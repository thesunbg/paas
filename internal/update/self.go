package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/thesunbg/paas/internal/util"
)

// Self updates the binary of the process it runs in: a control plane (for its own
// machine) or an agent (for a joined node). The caller replies to the controller
// first and then calls Restart.
//
// Restart mechanism. paasd.service (install.sh) has Restart=always, RestartSec=2 and
// KillMode=process, so when paasd exits for any reason other than `systemctl stop`
// systemd starts the binary at /usr/local/bin/paasd again, and containers (owned by
// containerd shims) keep running. The update therefore only renames the new binary into
// place and makes the process exit cleanly (SIGTERM to itself, the same graceful path as
// `systemctl stop`). The alternative, `systemd-run --on-active=1 systemctl restart
// paasd`, needs a transient unit and a second process for what Restart=always already
// does; exiting has no moving part that can fail half way. Outside systemd (development)
// the process re-executes itself instead.
//
// Local rollback guard. A binary that crashes on start can never report back, so before
// restarting, Apply arms a transient systemd timer (systemd-run) that runs a few lines of
// /bin/sh after the step timeout: unless the new process confirmed it came up (Confirm
// removes the pending file), it moves paasd.prev back over paasd and restarts the unit.
// It is a shell script on purpose: it must not depend on either binary working.
type Self struct {
	DataDir string       // /var/lib/paas: holds the pending-update file
	Version string       // version of the running binary
	Client  *http.Client // downloads
	Exe     string       // binary to replace; empty: the running executable

	// systemdRun runs systemd-run with the given arguments (tests replace it).
	systemdRun func(args ...string) error
}

const pendingFile = "update-pending.json"

type pendingUpdate struct {
	Version  string    `json:"version"`
	Token    string    `json:"token"`
	Exe      string    `json:"exe"`
	Deadline time.Time `json:"deadline"`
}

// ErrRejected marks a refusal that retrying cannot fix (bad checksum, unknown
// architecture, missing checksum): the controller fails the step at once instead of
// waiting for the machine.
var ErrRejected = errors.New("update rejected")

func (s *Self) exe() (string, error) {
	if s.Exe != "" {
		return s.Exe, nil
	}
	return Executable()
}

// Apply installs version from urls[GOARCH], verified against sums[GOARCH] (empty only
// with allowUnverified), and arms the local rollback guard to fire after guardAfter
// (0: no guard). restart is false when this binary already is that version: nothing
// changed and the process must keep running.
func (s *Self) Apply(ctx context.Context, version string, urls, sums map[string]string, allowUnverified bool, guardAfter time.Duration) (restart bool, err error) {
	if Same(s.Version, version) {
		return false, nil
	}
	arch := runtime.GOARCH
	url, want := urls[arch], sums[arch]
	if url == "" {
		return false, fmt.Errorf("%w: no %s binary for linux-%s", ErrRejected, version, arch)
	}
	if want == "" && !allowUnverified {
		return false, fmt.Errorf("%w: no SHA-256 checksum for %s", ErrRejected, BinaryName(arch))
	}
	exe, err := s.exe()
	if err != nil {
		return false, err
	}
	// Asked again after the binary was swapped but before the restart (the controller
	// retries after losing its state): installing again would make .prev the new binary.
	if cur, err := FileSHA256(exe); err == nil && want != "" && strings.EqualFold(cur, want) {
		return true, nil
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Minute}
	}
	staged, err := Download(ctx, client, url, filepath.Dir(exe), want)
	if err != nil {
		if errors.Is(err, ErrChecksumMismatch) {
			err = fmt.Errorf("%w: %w", ErrRejected, err)
		}
		return false, err
	}
	if err := Install(exe, staged); err != nil {
		_ = os.Remove(staged)
		return false, err
	}
	if guardAfter > 0 {
		if err := s.armGuard(exe, version, guardAfter); err != nil {
			slog.Warn("update: no local rollback guard", "err", err)
		}
	}
	slog.Info("paasd binary replaced; restarting", "from", s.Version, "to", version)
	return true, nil
}

// Rollback puts the previous binary back and disarms the guard; the caller restarts.
func (s *Self) Rollback() error {
	exe, err := s.exe()
	if err != nil {
		return err
	}
	if err := Rollback(exe); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(s.DataDir, pendingFile))
	slog.Warn("paasd binary rolled back to the previous one; restarting")
	return nil
}

func (s *Self) armGuard(exe, version string, after time.Duration) error {
	p := pendingUpdate{Version: version, Token: util.RandomToken(8), Exe: exe, Deadline: time.Now().Add(after)}
	path := filepath.Join(s.DataDir, pendingFile)
	b, _ := json.Marshal(p)
	if err := writeFileSync(path, b); err != nil {
		return err
	}
	run := s.systemdRun
	if run == nil {
		if os.Getenv("INVOCATION_ID") == "" {
			return errors.New("not running under systemd")
		}
		if _, err := exec.LookPath("systemd-run"); err != nil {
			return err
		}
		run = func(args ...string) error {
			out, err := exec.Command("systemd-run", args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("systemd-run: %w: %s", err, strings.TrimSpace(string(out)))
			}
			return nil
		}
	}
	script, err := guardScript(exe, path, p.Token)
	if err != nil {
		return err
	}
	return run("--unit=paasd-update-guard-"+strings.ToLower(p.Token), fmt.Sprintf("--on-active=%ds", int(after.Seconds())),
		"--timer-property=AccuracySec=1s", "/bin/sh", "-c", script)
}

// guardScript restores exe.prev unless the pending file (removed by Confirm or a
// rollback) still holds token.
func guardScript(exe, pending, token string) (string, error) {
	for _, s := range []string{exe, pending, token} {
		if strings.ContainsAny(s, "'\n") {
			return "", fmt.Errorf("unsupported character in %q", s)
		}
	}
	return fmt.Sprintf(`grep -qs '%[3]s' '%[2]s' || exit 0
mv -f '%[1]s%[4]s' '%[1]s' || exit 1
rm -f '%[2]s'
systemctl restart paasd.service
systemctl try-restart paasd-proxy.service
exit 0`, exe, pending, token, PrevSuffix), nil
}

// Confirm tells the local guard that this process, running version, came up healthy.
// Safe to call any number of times, from any component of the process.
func Confirm(dataDir, version string) {
	path := filepath.Join(dataDir, pendingFile)
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var p pendingUpdate
	if json.Unmarshal(b, &p) != nil || Same(p.Version, version) {
		if os.Remove(path) == nil {
			slog.Info("update confirmed", "version", version)
		}
	}
}

// Restart makes the process start again from the binary now at exe (see Self).
func Restart(exe string) {
	if os.Getenv("INVOCATION_ID") != "" { // started by systemd
		// The edge proxy of a joined node runs from the same binary in its own unit.
		if out, err := exec.Command("systemctl", "try-restart", "paasd-proxy.service").CombinedOutput(); err != nil {
			slog.Warn("restart edge proxy", "err", err, "out", string(out))
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		// Restart=always starts it again however it ends; do not hang in shutdown.
		time.AfterFunc(time.Minute, func() { os.Exit(1) })
		return
	}
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		slog.Error("re-exec after update", "err", err)
		os.Exit(1)
	}
}

func writeFileSync(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
