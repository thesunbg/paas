package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/thesunbg/paas/internal/bus"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/update"
)

// Standalone agents (nodes that joined a cluster) follow the control plane's
// binary: when it changes, they download it, verify the checksum, replace
// their own executable and re-exec. Containers keep running (they belong to
// containerd shims) and are recovered by the new process.

const (
	updateInterval = 5 * time.Minute
	maxPostpone    = 12
)

// work marks a command that a restart would lose (builds, jobs, transfers);
// updates wait until none is running, for at most maxPostpone checks. Container
// starts are not counted: the control plane retries them.
func (a *Agent) work() func() {
	a.busy.Add(1)
	return func() { a.busy.Add(-1) }
}

func (a *Agent) selfUpdateLoop(ctx context.Context) {
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if err := a.selfUpdate(ctx); err != nil {
			slog.Warn("self-update", "err", err)
		}
		timer.Reset(updateInterval)
	}
}

func (a *Agent) selfUpdate(ctx context.Context) error {
	var info proto.BinaryInfo
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	// The per-node subject lets the control plane hold this node back (a rollout in
	// progress or failed, a binary pinned by a rollback); control planes that predate
	// it only answer the shared subject.
	var err error = nats.ErrNoResponders
	if !a.legacy {
		err = bus.Request(rctx, a.nc, proto.SubjNodeBinary(a.nodeID), struct{}{}, &info)
	}
	if errors.Is(err, nats.ErrNoResponders) {
		err = bus.Request(rctx, a.nc, proto.SubjBinary, struct{}{}, &info)
	}
	cancel()
	if err != nil {
		return nil // control plane without self-update support, or unreachable
	}
	if info.SHA256 == "" || info.URL == "" || info.Arch != runtime.GOARCH {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	cur, err := fileSHA256(exe)
	if err != nil {
		return err
	}
	if cur == info.SHA256 {
		return nil
	}
	if n := a.busy.Load(); n > 0 && a.postponed < maxPostpone {
		a.postponed++
		slog.Info("self-update postponed: commands in progress", "count", n)
		return nil
	}
	slog.Info("downloading the control plane's paasd binary", "url", info.URL)
	tmp := exe + ".new"
	if err := download(ctx, info.URL, tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	got, err := fileSHA256(tmp)
	if err != nil || got != info.SHA256 {
		_ = os.Remove(tmp)
		return fmt.Errorf("checksum mismatch for downloaded binary (got %s, want %s)", got, info.SHA256)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	// Keeps the replaced binary as paasd.prev (POST /api/v1/nodes/{id}/rollback-binary).
	if err := update.Install(exe, tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	slog.Info("paasd updated; restarting", "sha256", info.SHA256[:12])
	// The edge proxy of this node runs from the same binary.
	if out, err := exec.Command("systemctl", "try-restart", "paasd-proxy.service").CombinedOutput(); err != nil {
		slog.Warn("restart edge proxy", "err", err, "out", string(out))
	}
	a.net.DNS.Shutdown()
	return syscall.Exec(exe, os.Args, os.Environ())
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func download(ctx context.Context, url, dest string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
