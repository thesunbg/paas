package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func newClientKey(t *testing.T) ([]byte, ssh.PublicKey) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block), s.PublicKey()
}

// echoShell answers id -u with uid and prints every other command back.
func echoShell(uid string) fakeShell {
	return func(cmd, stdin string, out io.Writer, closed <-chan struct{}) int {
		switch {
		case cmd == "id -u":
			fmt.Fprintln(out, uid)
		case cmd == "command -v sudo":
			fmt.Fprintln(out, "/usr/bin/sudo")
		case cmd == "sudo -n true":
			fmt.Fprintln(out, "sudo: a password is required")
			return 1
		case cmd == "sudo -S -p '' true":
			if stdin != "s3cret-pw\n" {
				fmt.Fprintln(out, "Sorry, try again.")
				return 1
			}
		case strings.HasPrefix(cmd, "sleep"):
			<-closed
			return 137
		default:
			fmt.Fprintln(out, "ran:", cmd)
		}
		return 0
	}
}

func TestHostKeyMismatchRefusedBeforeAuth(t *testing.T) {
	ts := newTestSSHServer(t, "root", "s3cret-pw", nil, echoShell("0"))
	ctx := context.Background()
	key, err := ProbeHostKey(ctx, ts.addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if Fingerprint(key) != Fingerprint(ts.hostKey.PublicKey()) {
		t.Fatalf("probe got %s", Fingerprint(key))
	}
	if ts.authHits.Load() != 0 {
		t.Fatal("the probe authenticated")
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	os1, _ := ssh.NewSignerFromKey(other)
	_, err = Dial(ctx, DialConfig{Addr: ts.addr, User: "root", Password: "s3cret-pw", HostKey: FingerprintCallback(Fingerprint(os1.PublicKey()))})
	var mm *HostKeyMismatchError
	if err == nil || !errors.As(err, &mm) {
		t.Fatalf("want host key mismatch, got %v", err)
	}
	if mm.Got != Fingerprint(key) {
		t.Fatalf("mismatch reports %s", mm.Got)
	}
	if ts.authHits.Load() != 0 {
		t.Fatal("the password was offered to a server with the wrong host key")
	}
	if _, err := Dial(ctx, DialConfig{Addr: ts.addr, User: "root", Password: "x"}); err == nil {
		t.Fatal("dial without a host key check must fail")
	}
}

func TestFingerprintConfirmationFormats(t *testing.T) {
	ts := newTestSSHServer(t, "root", "s3cret-pw", nil, echoShell("0"))
	fp := Fingerprint(ts.hostKey.PublicKey())
	for _, f := range []string{fp, strings.TrimPrefix(fp, "SHA256:"), "sha256:" + strings.TrimPrefix(fp, "SHA256:") + "=", "  " + fp + "\n"} {
		c, err := Dial(context.Background(), DialConfig{Addr: ts.addr, User: "root", Password: "s3cret-pw", HostKey: FingerprintCallback(f)})
		if err != nil {
			t.Fatalf("fingerprint %q: %v", f, err)
		}
		_ = c.Close()
	}
}

func TestPasswordAndKeyAuth(t *testing.T) {
	keyPEM, pub := newClientKey(t)
	ts := newTestSSHServer(t, "deploy", "s3cret-pw", pub, echoShell("1000"))
	hk := FingerprintCallback(Fingerprint(ts.hostKey.PublicKey()))
	ctx := context.Background()

	c, err := Dial(ctx, DialConfig{Addr: ts.addr, User: "deploy", PrivateKey: keyPEM, HostKey: hk})
	if err != nil {
		t.Fatalf("key auth: %v", err)
	}
	out, err := Output(ctx, c, "hello", nil)
	_ = c.Close()
	if err != nil || strings.TrimSpace(out) != "ran: hello" {
		t.Fatalf("key auth run: %q %v", out, err)
	}

	c, err = Dial(ctx, DialConfig{Addr: ts.addr, User: "deploy", Password: "s3cret-pw", HostKey: hk})
	if err != nil {
		t.Fatalf("password auth: %v", err)
	}
	_ = c.Close()

	if _, err := Dial(ctx, DialConfig{Addr: ts.addr, User: "deploy", Password: "wrong", HostKey: hk}); err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("wrong password: %v", err)
	}
	other, _ := newClientKey(t)
	if _, err := Dial(ctx, DialConfig{Addr: ts.addr, User: "deploy", PrivateKey: other, HostKey: hk}); err == nil {
		t.Fatal("unknown key accepted")
	}
	// An encrypted key needs its passphrase.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("pass"))
	if _, err := ParsePrivateKey(pem.EncodeToMemory(block), ""); err == nil || !strings.Contains(err.Error(), "passphrase") {
		t.Fatalf("encrypted key without passphrase: %v", err)
	}
	if _, err := ParsePrivateKey(pem.EncodeToMemory(block), "pass"); err != nil {
		t.Fatal(err)
	}
}

func TestSudoPath(t *testing.T) {
	ts := newTestSSHServer(t, "deploy", "s3cret-pw", nil, echoShell("1000"))
	hk := FingerprintCallback(Fingerprint(ts.hostKey.PublicKey()))
	ctx := context.Background()
	c, err := Dial(ctx, DialConfig{Addr: ts.addr, User: "deploy", Password: "s3cret-pw", HostKey: hk})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	h := &Host{R: c, User: "deploy", Password: "s3cret-pw"}
	if err := h.Privileges(ctx); err != nil {
		t.Fatalf("privileges: %v", err)
	}
	if h.Root() {
		t.Fatal("uid 1000 is not root")
	}
	var lines []string
	lw := NewLineWriter(func(l string) { lines = append(lines, l) }, h.Password)
	if err := h.RunRoot(ctx, "install it", lw); err != nil {
		t.Fatal(err)
	}
	_ = lw.Close()
	cmds, stdins := ts.seen()
	last := cmds[len(cmds)-1]
	if last != "sudo -S -p '' sh -c 'install it'" || stdins[len(stdins)-1] != "s3cret-pw\n" {
		t.Fatalf("sudo command %q stdin %q", last, stdins[len(stdins)-1])
	}
	for _, c := range cmds {
		if strings.Contains(c, "s3cret-pw") {
			t.Fatalf("the password is on a command line: %q", c)
		}
	}

	// Wrong password for sudo.
	h2 := &Host{R: c, User: "deploy", Password: "not-it"}
	if err := h2.Privileges(ctx); err == nil || !strings.Contains(err.Error(), "sudo refused") {
		t.Fatalf("wrong sudo password: %v", err)
	}
	// Key auth without a password cannot answer sudo's prompt.
	h3 := &Host{R: c, User: "deploy"}
	if err := h3.Privileges(ctx); err == nil || !strings.Contains(err.Error(), "NOPASSWD") {
		t.Fatalf("no password: %v", err)
	}
}

func TestNoSudo(t *testing.T) {
	ts := newTestSSHServer(t, "deploy", "pw-pw-pw", nil, func(cmd, _ string, out io.Writer, _ <-chan struct{}) int {
		if cmd == "id -u" {
			fmt.Fprintln(out, "1000")
			return 0
		}
		return 1 // command -v sudo: not found
	})
	c, err := Dial(context.Background(), DialConfig{Addr: ts.addr, User: "deploy", Password: "pw-pw-pw", HostKey: FingerprintCallback(Fingerprint(ts.hostKey.PublicKey()))})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	h := &Host{R: c, User: "deploy", Password: "pw-pw-pw"}
	if err := h.Privileges(context.Background()); !errors.Is(err, ErrNoSudo) {
		t.Fatalf("want ErrNoSudo, got %v", err)
	}
}

func TestRootRunsWithoutSudo(t *testing.T) {
	ts := newTestSSHServer(t, "root", "s3cret-pw", nil, echoShell("0"))
	c, err := Dial(context.Background(), DialConfig{Addr: ts.addr, User: "root", Password: "s3cret-pw", HostKey: FingerprintCallback(Fingerprint(ts.hostKey.PublicKey()))})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	h := &Host{R: c, User: "root", Password: "s3cret-pw"}
	if err := h.Privileges(context.Background()); err != nil || !h.Root() {
		t.Fatalf("root: %v", err)
	}
	cmd, stdin := h.RootCommand("echo it's")
	if cmd != `sh -c 'echo it'\''s'` || stdin != nil {
		t.Fatalf("root command %q", cmd)
	}
}

func TestMaskingInLogs(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	ts := newTestSSHServer(t, "root", "s3cret-pw", nil, func(cmd, _ string, out io.Writer, _ <-chan struct{}) int {
		// Echo the command (it carries the token), split across writes, with colours and a progress bar.
		fmt.Fprint(out, "\x1b[1;34m==>\x1b[0m running ")
		fmt.Fprint(out, cmd[:len(cmd)/2])
		fmt.Fprintln(out, cmd[len(cmd)/2:])
		fmt.Fprint(out, "progress 10%\rprogress 100%\n")
		fmt.Fprint(out, "password was s3cret-pw")
		return 0
	})
	c, err := Dial(context.Background(), DialConfig{Addr: ts.addr, User: "root", Password: "s3cret-pw", HostKey: FingerprintCallback(Fingerprint(ts.hostKey.PublicKey()))})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var mu sync.Mutex
	var lines []string
	lw := NewLineWriter(func(l string) { mu.Lock(); lines = append(lines, l); mu.Unlock() }, "s3cret-pw", token)
	if err := c.Run(context.Background(), "install --token "+token, nil, lw); err != nil {
		t.Fatal(err)
	}
	_ = lw.Close()
	all := strings.Join(lines, "\n")
	if strings.Contains(all, token) || strings.Contains(all, "s3cret-pw") {
		t.Fatalf("secret in log: %q", all)
	}
	want := []string{"==> running install --token ********", "progress 100%", "password was ********"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("lines %q", lines)
	}
	if MaskText("abc", []string{"ab"}) != "abc" {
		t.Fatal("short secrets must not be masked")
	}
}

func TestStepTimeout(t *testing.T) {
	ts := newTestSSHServer(t, "root", "s3cret-pw", nil, echoShell("0"))
	c, err := Dial(context.Background(), DialConfig{Addr: ts.addr, User: "root", Password: "s3cret-pw", HostKey: FingerprintCallback(Fingerprint(ts.hostKey.PublicKey()))})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	err = WithTimeout(context.Background(), "install", 300*time.Millisecond, func(ctx context.Context) error {
		return c.Run(ctx, "sleep 600", nil, io.Discard)
	})
	if err == nil || err.Error() != "install timed out after 300ms" {
		t.Fatalf("timeout error: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the step did not stop at its timeout")
	}
	// The connection still works for the next step.
	if out, err := Output(context.Background(), c, "next", nil); err != nil || !strings.Contains(out, "ran: next") {
		t.Fatalf("after timeout: %q %v", out, err)
	}
	// Exit codes surface as *ExitError.
	var ee *ExitError
	if err := c.Run(context.Background(), "sudo -n true", nil, io.Discard); !errors.As(err, &ee) || ee.Code != 1 {
		t.Fatalf("exit error: %v", err)
	}
}

const (
	unameOut   = "Linux x86_64"
	osRelease  = "PRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nNAME=\"Ubuntu\"\nID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"24.04\"\n"
	freeOut    = "               total        used        free      shared  buff/cache   available\nMem:            3911         512        2800           1         600        3300\nSwap:              0           0           0\n"
	dfOut      = "Filesystem     1048576-blocks  Used Available Capacity Mounted on\n/dev/sda1               80000 10000     65000      14% /\n"
	portsOut   = "0.0.0.0:22\n[::]:22\n127.0.0.53%lo:53\n"
	reachOK    = "200"
	preflightT = "@@uname\n%s\n@@os-release\n%s\n@@free\n%s\n@@df\n%s\n@@systemd\n%s\n@@pkg\n%s\n@@curl\n/usr/bin/curl\n@@cgroup\ncgroup2fs\n@@installed\n%s\n@@ports\n%s\n@@hostname\nnode-1\n@@ips\n10.0.0.5 fd00::5\n@@reach\n%s\n@@end\n"
)

func preflightOut(uname, osr, free, df, systemd, pkg, installed, ports, reach string) string {
	return fmt.Sprintf(preflightT, uname, osr, free, df, systemd, pkg, installed, ports, reach)
}

func statusOf(r Report, id string) string {
	for _, c := range r.Checks {
		if c.ID == id {
			return c.Status
		}
	}
	return ""
}

func TestParsePreflight(t *testing.T) {
	join := PreflightOptions{Mode: "join", Roles: []string{"worker", "edge"}, ControlPlaneURL: "https://paas.example.com"}
	r := ParsePreflight(preflightOut(unameOut, osRelease, freeOut, dfOut, "yes", "/usr/bin/apt-get", "", portsOut, reachOK), join)
	if r.Failed() {
		t.Fatalf("healthy machine failed: %+v", r.Checks)
	}
	if r.Arch != "amd64" || r.OS != "Ubuntu 24.04.1 LTS" || r.MemoryMB != 3911 || r.DiskFreeMB != 65000 || r.Hostname != "node-1" || len(r.IPs) != 2 {
		t.Fatalf("report %+v", r)
	}
	for _, id := range []string{"os", "arch", "systemd", "curl", "cgroup", "memory", "disk", "existing", "ports", "control-plane"} {
		if statusOf(r, id) != CheckOK {
			t.Fatalf("check %s = %q", id, statusOf(r, id))
		}
	}

	cases := []struct {
		name, out, id, want string
		opts                PreflightOptions
	}{
		{"arch", preflightOut("Linux riscv64", osRelease, freeOut, dfOut, "yes", "/usr/bin/apt-get", "", portsOut, reachOK), "arch", CheckFail, join},
		{"darwin", preflightOut("Darwin arm64", "", freeOut, dfOut, "no", "none", "", portsOut, reachOK), "os", CheckFail, join},
		{"no pkg", preflightOut(unameOut, "ID=alpine\nPRETTY_NAME=\"Alpine\"", freeOut, dfOut, "yes", "none", "", portsOut, reachOK), "os", CheckFail, join},
		{"untested", preflightOut(unameOut, "ID=arch-ish", freeOut, dfOut, "yes", "/usr/bin/dnf", "", portsOut, reachOK), "os", CheckWarn, join},
		{"rocky", preflightOut(unameOut, "ID=rocky\nID_LIKE=\"rhel centos fedora\"", freeOut, dfOut, "yes", "/usr/bin/dnf", "", portsOut, reachOK), "os", CheckOK, join},
		{"no systemd", preflightOut(unameOut, osRelease, freeOut, dfOut, "no", "/usr/bin/apt-get", "", portsOut, reachOK), "systemd", CheckFail, join},
		{"tiny ram", preflightOut(unameOut, osRelease, "Mem: 400 100 300 0 0 300", dfOut, "yes", "/usr/bin/apt-get", "", portsOut, reachOK), "memory", CheckFail, join},
		{"small ram server", preflightOut(unameOut, osRelease, "Mem: 1500 100 300 0 0 300", dfOut, "yes", "/usr/bin/apt-get", "", portsOut, ""), "memory", CheckWarn, PreflightOptions{Mode: "server"}},
		{"full disk", preflightOut(unameOut, osRelease, freeOut, "Filesystem 1M-blocks Used Available Capacity Mounted\n/dev/vda1 20000 19000 1000 95% /", "yes", "/usr/bin/apt-get", "", portsOut, reachOK), "disk", CheckFail, join},
		{"installed", preflightOut(unameOut, osRelease, freeOut, dfOut, "yes", "/usr/bin/apt-get", "binary\nunit", portsOut, reachOK), "existing", CheckFail, join},
		{"reinstall", preflightOut(unameOut, osRelease, freeOut, dfOut, "yes", "/usr/bin/apt-get", "binary\nunit", portsOut, reachOK), "existing", CheckWarn, PreflightOptions{Mode: "join", Reinstall: true}},
		{"port 80 busy edge", preflightOut(unameOut, osRelease, freeOut, dfOut, "yes", "/usr/bin/apt-get", "", portsOut+"0.0.0.0:80\n*:443\n", reachOK), "ports", CheckFail, join},
		{"port 80 busy worker", preflightOut(unameOut, osRelease, freeOut, dfOut, "yes", "/usr/bin/apt-get", "", portsOut+"0.0.0.0:80\n", reachOK), "ports", "", PreflightOptions{Mode: "join", Roles: []string{"worker"}}},
		{"server 8080 busy", preflightOut(unameOut, osRelease, freeOut, dfOut, "yes", "/usr/bin/apt-get", "", portsOut+"[::]:8080\n", ""), "ports", CheckFail, PreflightOptions{Mode: "server"}},
		{"unreachable", preflightOut(unameOut, osRelease, freeOut, dfOut, "yes", "/usr/bin/apt-get", "", portsOut, "curl: (6) Could not resolve host\n000"), "control-plane", CheckFail, join},
		{"server skips reach", preflightOut(unameOut, osRelease, freeOut, dfOut, "yes", "/usr/bin/apt-get", "", portsOut, ""), "control-plane", "", PreflightOptions{Mode: "server"}},
	}
	for _, c := range cases {
		r := ParsePreflight(c.out, c.opts)
		if got := statusOf(r, c.id); got != c.want {
			t.Errorf("%s: check %s = %q, want %q (%+v)", c.name, c.id, got, c.want, r.Checks)
		}
	}
	if !strings.Contains(PreflightScript(join), "'https://paas.example.com/healthz'") {
		t.Fatal("the script does not probe the control plane")
	}
}

func TestPreflightOverSSH(t *testing.T) {
	ts := newTestSSHServer(t, "root", "s3cret-pw", nil, func(cmd, _ string, out io.Writer, _ <-chan struct{}) int {
		switch {
		case cmd == "id -u":
			fmt.Fprintln(out, "0")
		case strings.HasPrefix(cmd, "sh -c ") && strings.Contains(cmd, "@@uname"):
			fmt.Fprint(out, preflightOut(unameOut, osRelease, freeOut, dfOut, "yes", "/usr/bin/apt-get", "", portsOut, reachOK))
		default:
			return 127
		}
		return 0
	})
	c, err := Dial(context.Background(), DialConfig{Addr: ts.addr, User: "root", Password: "s3cret-pw", HostKey: FingerprintCallback(Fingerprint(ts.hostKey.PublicKey()))})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := Preflight(context.Background(), &Host{R: c, User: "root", Password: "s3cret-pw"}, PreflightOptions{Mode: "join", ControlPlaneURL: "https://cp"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Failed() || statusOf(*r, "root") != CheckOK {
		t.Fatalf("checks %+v", r.Checks)
	}
}

func TestCheckKnownHosts(t *testing.T) {
	ts := newTestSSHServer(t, "root", "pw-pw-pw", nil, echoShell("0"))
	key := ts.hostKey.PublicKey()
	dir := t.TempDir()
	path := dir + "/known_hosts"
	if known, err := CheckKnownHosts([]string{path}, ts.addr, key); known || err != nil {
		t.Fatalf("missing file: %v %v", known, err)
	}
	if err := os.WriteFile(path, []byte(KnownHostsLine(ts.addr, key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if known, err := CheckKnownHosts([]string{path}, ts.addr, key); !known || err != nil {
		t.Fatalf("listed key: %v %v", known, err)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	os2, _ := ssh.NewSignerFromKey(other)
	var mm *HostKeyMismatchError
	if _, err := CheckKnownHosts([]string{path}, ts.addr, os2.PublicKey()); !errors.As(err, &mm) {
		t.Fatalf("changed key: %v", err)
	}
	if known, err := CheckKnownHosts([]string{path}, "10.9.9.9:22", key); known || err != nil {
		t.Fatalf("other host: %v %v", known, err)
	}
}
