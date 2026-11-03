package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/thesunbg/paas/internal/remote"
	"github.com/thesunbg/paas/internal/store"
)

// fakeMachine stands in for a server reached over SSH: it answers the preflight and the
// installer, and the installer "enrolls" a node with the token it was given.
type fakeMachine struct {
	t        *testing.T
	s        *Server
	hostKey  ssh.PublicKey
	systemd  string // "yes" or "no"
	mu       sync.Mutex
	cmds     []string
	dialCfgs []remote.DialConfig
	block    chan struct{} // when set, the installer waits on it (or on cancel)
}

func newFakeMachine(t *testing.T, s *Server) *fakeMachine {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	m := &fakeMachine{t: t, s: s, hostKey: signer.PublicKey(), systemd: "yes"}
	s.ssh.dial = m.dial
	s.ssh.probe = func(context.Context, string) (ssh.PublicKey, error) { return m.hostKey, nil }
	return m
}

func (m *fakeMachine) dial(_ context.Context, cfg remote.DialConfig) (remote.Runner, error) {
	m.mu.Lock()
	m.dialCfgs = append(m.dialCfgs, cfg)
	m.mu.Unlock()
	if cfg.HostKey == nil {
		return nil, fmt.Errorf("no host key check")
	}
	// The real client checks the host key during the handshake, before authenticating.
	if err := cfg.HostKey("203.0.113.10:22", &net.TCPAddr{IP: net.ParseIP("203.0.113.10"), Port: 22}, m.hostKey); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *fakeMachine) Close() error { return nil }

var tokenRE = regexp.MustCompile(`--token ([0-9a-f]{64})`)

func (m *fakeMachine) Run(ctx context.Context, cmd string, _ io.Reader, out io.Writer) error {
	m.mu.Lock()
	m.cmds = append(m.cmds, cmd)
	m.mu.Unlock()
	switch {
	case cmd == "id -u":
		fmt.Fprintln(out, "0")
	case strings.Contains(cmd, "@@uname"):
		fmt.Fprintf(out, "@@uname\nLinux x86_64\n@@os-release\nID=debian\nPRETTY_NAME=\"Debian 12\"\n@@free\nMem: 4000 1 1 1 1 1\n@@df\n"+
			"Filesystem 1M-blocks Used Available Capacity Mounted\n/dev/sda1 80000 1000 70000 2%% /\n@@systemd\n%s\n@@pkg\n/usr/bin/apt-get\n"+
			"@@curl\n/usr/bin/curl\n@@cgroup\ncgroup2fs\n@@installed\n@@ports\n0.0.0.0:22\n@@hostname\nfresh\n@@ips\n203.0.113.10\n@@reach\n200\n@@end\n", m.systemd)
	case strings.Contains(cmd, "install.sh | sh -s -- join"):
		tok := tokenRE.FindStringSubmatch(cmd)
		if tok == nil {
			return fmt.Errorf("no token in %q", cmd)
		}
		fmt.Fprintf(out, "\x1b[1;34m==>\x1b[0m installing paas (join, roles: worker) token %s\n", tok[1])
		if m.block != nil {
			select {
			case <-m.block:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		fmt.Fprintln(out, "==> done.")
		// paasd starts and enrolls with the token, then registers its resources.
		n, _, err := m.s.store.EnrollNode(context.Background(), hashJoinToken(tok[1]), "fresh", "UNKEY"+tok[1][:20], 100)
		if err != nil {
			return err
		}
		_, err = m.s.store.Q(context.Background()).Exec(context.Background(), `UPDATE nodes SET memory_mb=4000, cpu_cores=2 WHERE id=$1`, n.ID)
		return err
	default:
		return &remote.ExitError{Code: 127}
	}
	return nil
}

func sshInstallServer(t *testing.T) (*Server, string, string, *fakeMachine) {
	t.Helper()
	s, h := newTestServer(t)
	s.cfg.PublicURL = "https://192.0.2.50"
	s.cfg.ServerName = "cp1"
	sshPollInterval = 20 * time.Millisecond
	sshGuard.mu.Lock()
	sshGuard.failures = map[string][]time.Time{}
	sshGuard.mu.Unlock()
	mkUser(t, s, "root@example.com", "password1", true)
	mkUser(t, s, "user@example.com", "password1", false)
	_, admin := login(t, h, "root@example.com", "password1", "198.51.100.1")
	_, user := login(t, h, "user@example.com", "password1", "198.51.100.2")
	return s, admin, user, newFakeMachine(t, s)
}

func waitInstall(t *testing.T, s *Server, admin, id string) map[string]any {
	t.Helper()
	h := s.routes()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		w := do(t, h, "GET", "/api/v1/cluster/ssh-installs/"+id, admin, nil)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		if out["status"] != "running" {
			// The goroutine may still be closing its log: wait until it is done.
			for time.Now().Before(deadline) {
				s.ssh.mu.Lock()
				_, running := s.ssh.running[id]
				s.ssh.mu.Unlock()
				if !running {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			w = do(t, h, "GET", "/api/v1/cluster/ssh-installs/"+id, admin, nil)
			out = map[string]any{}
			_ = json.Unmarshal(w.Body.Bytes(), &out)
			return out
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("install %s did not finish", id)
	return nil
}

func TestSSHInstallAdminOnly(t *testing.T) {
	s, _, user, _ := sshInstallServer(t)
	h := s.routes()
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/v1/cluster/ssh-host-key"},
		{"GET", "/api/v1/cluster/ssh-installs"},
		{"POST", "/api/v1/cluster/ssh-installs"},
		{"GET", "/api/v1/cluster/ssh-installs/00000000-0000-0000-0000-000000000000"},
		{"GET", "/api/v1/cluster/ssh-installs/00000000-0000-0000-0000-000000000000/logs/stream"},
		{"POST", "/api/v1/cluster/ssh-installs/00000000-0000-0000-0000-000000000000/cancel"},
	} {
		if w := do(t, h, c.method, c.path, user, `{"host":"203.0.113.10"}`); w.Code != 403 {
			t.Errorf("%s %s as non-admin = %d", c.method, c.path, w.Code)
		}
		if w := do(t, h, c.method, c.path, "", `{}`); w.Code != 401 {
			t.Errorf("%s %s anonymous = %d", c.method, c.path, w.Code)
		}
	}
}

func TestSSHInstallLifecycle(t *testing.T) {
	s, admin, _, m := sshInstallServer(t)
	h := s.routes()
	ctx := context.Background()

	w := do(t, h, "POST", "/api/v1/cluster/ssh-host-key", admin, map[string]any{"host": "203.0.113.10", "port": 22})
	var hk struct {
		Fingerprint  string `json:"fingerprint"`
		HostKey      string `json:"host_key"`
		ControlPlane bool   `json:"control_plane"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &hk)
	if w.Code != 200 || hk.Fingerprint != remote.Fingerprint(m.hostKey) || hk.ControlPlane || !strings.HasPrefix(hk.HostKey, "ssh-ed25519 ") {
		t.Fatalf("host key = %d %s", w.Code, w.Body)
	}

	const password = "hunter2-very-secret"
	body := map[string]any{"host": "203.0.113.10", "user": "root", "auth": "password", "password": password,
		"roles": []string{"worker"}, "name": "fresh", "fingerprint": hk.Fingerprint}

	// The host key must be confirmed first.
	noFP := map[string]any{"host": "203.0.113.10", "auth": "password", "password": password}
	if w := do(t, h, "POST", "/api/v1/cluster/ssh-installs", admin, noFP); w.Code != 400 || !strings.Contains(w.Body.String(), "fingerprint") {
		t.Fatalf("without fingerprint = %d %s", w.Code, w.Body)
	}
	// The token would travel in clear text over http.
	s.cfg.PublicURL = "http://192.0.2.50"
	if w := do(t, h, "POST", "/api/v1/cluster/ssh-installs", admin, body); w.Code != 409 {
		t.Fatalf("http public URL = %d %s", w.Code, w.Body)
	}
	s.cfg.PublicURL = "https://192.0.2.50"

	w = do(t, h, "POST", "/api/v1/cluster/ssh-installs", admin, body)
	if w.Code != 202 {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	var created store.SSHInstall
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if created.Status != "running" || created.AuthMethod != "password" || created.HostKeyFingerprint != hk.Fingerprint {
		t.Fatalf("created %+v", created)
	}
	out := waitInstall(t, s, admin, created.ID)
	if out["status"] != "succeeded" || out["step"] != "done" || out["node_id"] == nil {
		t.Fatalf("finished %v", out)
	}
	nodeID := out["node_id"].(string)
	if out["host_key"] != hk.HostKey {
		t.Fatalf("stored host key %v", out["host_key"])
	}
	pf, _ := out["preflight"].(map[string]any)
	if pf == nil || pf["hostname"] != "fresh" || len(pf["checks"].([]any)) < 8 {
		t.Fatalf("preflight %v", out["preflight"])
	}
	var nodeKey string
	if err := s.store.Q(ctx).QueryRow(ctx, `SELECT ssh_host_key FROM nodes WHERE id=$1`, nodeID).Scan(&nodeKey); err != nil || nodeKey != hk.HostKey {
		t.Fatalf("node host key %q %v", nodeKey, err)
	}
	if !strings.Contains(m.cmds[len(m.cmds)-1], `--name '\''fresh'\''`) || !strings.HasPrefix(m.cmds[len(m.cmds)-1], "sh -c ") {
		t.Fatalf("install command %q", m.cmds[len(m.cmds)-1])
	}

	// The log streams, credentials and the token masked.
	w = do(t, h, "GET", "/api/v1/cluster/ssh-installs/"+created.ID+"/logs/stream", admin, nil)
	stream := w.Body.String()
	if !strings.Contains(stream, "event: done\ndata: succeeded") || !strings.Contains(stream, "data: ==> installing paas (join, roles: worker) token ********") ||
		!strings.Contains(stream, "[ok] Operating system: Debian 12") || !strings.Contains(stream, "node fresh is online") {
		t.Fatalf("stream:\n%s", stream)
	}
	tok := tokenRE.FindStringSubmatch(m.cmds[len(m.cmds)-1])[1]

	// Nothing secret reached the database: not the password, not the join token.
	var dump string
	err := s.store.Q(ctx).QueryRow(ctx, `SELECT
		(SELECT COALESCE(string_agg(row_to_json(i)::text, ''), '') FROM ssh_installs i) ||
		(SELECT COALESCE(string_agg(line, ''), '') FROM ssh_install_logs) ||
		(SELECT COALESCE(string_agg(action || target, ''), '') FROM audit_logs)`).Scan(&dump)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dump, password) || strings.Contains(dump, tok) {
		t.Fatal("a credential or the join token was stored")
	}
	if !strings.Contains(dump, "node.ssh_install.succeeded") || !strings.Contains(dump, "node.ssh_install") {
		t.Fatal("no audit entries")
	}
	// The token was used once, by this node.
	if id, err := s.store.JoinTokenNode(ctx, hashJoinToken(tok)); err != nil || id == nil || *id != nodeID {
		t.Fatalf("token node %v %v", id, err)
	}
	// The listing has it.
	w = do(t, h, "GET", "/api/v1/cluster/ssh-installs", admin, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), created.ID) || strings.Contains(w.Body.String(), password) {
		t.Fatalf("list = %d %s", w.Code, w.Body)
	}
	w = do(t, h, "GET", "/api/v1/nodes", admin, nil)
	if !strings.Contains(w.Body.String(), `"ssh_host_key":"ssh-ed25519 `) {
		t.Fatalf("nodes list lacks the host key: %s", w.Body)
	}
}

func TestSSHInstallKeyAuthNeverStored(t *testing.T) {
	s, admin, _, m := sshInstallServer(t)
	h := s.routes()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKey(priv, "")
	keyPEM := string(pem.EncodeToMemory(block))
	w := do(t, h, "POST", "/api/v1/cluster/ssh-installs", admin, map[string]any{"host": "203.0.113.10", "user": "root", "auth": "key",
		"private_key": keyPEM, "fingerprint": remote.Fingerprint(m.hostKey), "preflight_only": true})
	if w.Code != 202 {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	var created store.SSHInstall
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	out := waitInstall(t, s, admin, created.ID)
	if out["status"] != "succeeded" || out["step"] != "preflight" || out["node_id"] != nil {
		t.Fatalf("preflight only: %v", out)
	}
	if len(m.dialCfgs) != 1 || len(m.dialCfgs[0].PrivateKey) == 0 || m.dialCfgs[0].Password != "" {
		t.Fatal("key auth not used")
	}
	// The key was wiped from memory once the attempt finished.
	for _, b := range m.dialCfgs[0].PrivateKey {
		if b != 0 {
			t.Fatal("the private key is still in memory")
		}
	}
	for _, c := range m.cmds {
		if strings.Contains(c, "install.sh") {
			t.Fatal("preflight only ran the installer")
		}
	}
	var dump string
	_ = s.store.Q(context.Background()).QueryRow(context.Background(), `SELECT string_agg(row_to_json(i)::text, '') FROM ssh_installs i`).Scan(&dump)
	if strings.Contains(dump, "PRIVATE KEY") || strings.Contains(dump, strings.Split(keyPEM, "\n")[1]) {
		t.Fatal("the private key was stored")
	}
	// A bad key is refused up front.
	if w := do(t, h, "POST", "/api/v1/cluster/ssh-installs", admin, map[string]any{"host": "203.0.113.10", "auth": "key",
		"private_key": "nope", "fingerprint": remote.Fingerprint(m.hostKey)}); w.Code != 400 {
		t.Fatalf("bad key = %d", w.Code)
	}
}

func TestSSHInstallPreflightFailure(t *testing.T) {
	s, admin, _, m := sshInstallServer(t)
	m.systemd = "no"
	h := s.routes()
	w := do(t, h, "POST", "/api/v1/cluster/ssh-installs", admin, map[string]any{"host": "203.0.113.10", "auth": "password",
		"password": "pw-pw-pw-pw", "fingerprint": remote.Fingerprint(m.hostKey)})
	var created store.SSHInstall
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	out := waitInstall(t, s, admin, created.ID)
	if out["status"] != "failed" || out["step"] != "preflight" || !strings.Contains(out["error"].(string), "systemd") {
		t.Fatalf("preflight failure: %v", out)
	}
	var tokens int
	_ = s.store.Q(context.Background()).QueryRow(context.Background(), `SELECT count(*) FROM join_tokens`).Scan(&tokens)
	if tokens != 0 {
		t.Fatal("a join token was issued although the preflight failed")
	}
}

func TestSSHInstallHostKeyMismatch(t *testing.T) {
	s, admin, _, m := sshInstallServer(t)
	h := s.routes()
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	os, _ := ssh.NewSignerFromKey(other)
	w := do(t, h, "POST", "/api/v1/cluster/ssh-installs", admin, map[string]any{"host": "203.0.113.10", "auth": "password",
		"password": "pw-pw-pw-pw", "fingerprint": remote.Fingerprint(os.PublicKey())})
	var created store.SSHInstall
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	out := waitInstall(t, s, admin, created.ID)
	if out["status"] != "failed" || out["step"] != "connect" || !strings.Contains(out["error"].(string), "host key mismatch") {
		t.Fatalf("mismatch: %v", out)
	}
	if len(m.cmds) != 0 {
		t.Fatal("commands ran on a server with the wrong host key")
	}
}

func TestSSHInstallGuardsAndCancel(t *testing.T) {
	s, admin, _, m := sshInstallServer(t)
	h := s.routes()
	fp := remote.Fingerprint(m.hostKey)
	// The control plane itself is refused unless explicitly allowed.
	body := map[string]any{"host": "127.0.0.1", "auth": "password", "password": "pw-pw-pw-pw", "fingerprint": fp}
	if w := do(t, h, "POST", "/api/v1/cluster/ssh-installs", admin, body); w.Code != 400 || !strings.Contains(w.Body.String(), "control plane") {
		t.Fatalf("self = %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/v1/cluster/ssh-host-key", admin, map[string]any{"host": "127.0.0.1"}); !strings.Contains(w.Body.String(), `"control_plane":true`) {
		t.Fatalf("host key of self: %s", w.Body)
	}
	body["allow_control_plane"] = true
	m.block = make(chan struct{})
	w := do(t, h, "POST", "/api/v1/cluster/ssh-installs", admin, body)
	if w.Code != 202 {
		t.Fatalf("self allowed = %d %s", w.Code, w.Body)
	}
	var created store.SSHInstall
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	// Wait until the installer runs, then cancel it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		inst, _ := s.store.SSHInstallByID(context.Background(), created.ID)
		if inst.Step == store.SSHStepInstall {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never reached install: %+v", inst)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if w := do(t, h, "POST", "/api/v1/cluster/ssh-installs/"+created.ID+"/cancel", admin, nil); w.Code != 202 {
		t.Fatalf("cancel = %d %s", w.Code, w.Body)
	}
	out := waitInstall(t, s, admin, created.ID)
	if out["status"] != "failed" || out["error"] != "cancelled" {
		t.Fatalf("cancelled: %v", out)
	}
	// Its unused join token is revoked.
	var tokens int
	_ = s.store.Q(context.Background()).QueryRow(context.Background(), `SELECT count(*) FROM join_tokens`).Scan(&tokens)
	if tokens != 0 {
		t.Fatal("the cancelled install's join token is still valid")
	}
	if w := do(t, h, "POST", "/api/v1/cluster/ssh-installs/"+created.ID+"/cancel", admin, nil); w.Code != 409 {
		t.Fatalf("cancel twice = %d", w.Code)
	}
	// Rate limit: ten starts per 15 minutes per administrator (one was used above).
	body["host"] = "203.0.113.10"
	delete(body, "allow_control_plane")
	m.block = nil
	codes := []int{}
	for range 10 {
		w := do(t, h, "POST", "/api/v1/cluster/ssh-installs", admin, body)
		codes = append(codes, w.Code)
		if w.Code == 202 {
			var c store.SSHInstall
			_ = json.Unmarshal(w.Body.Bytes(), &c)
			waitInstall(t, s, admin, c.ID)
		}
	}
	if codes[8] != 202 || codes[9] != 429 {
		t.Fatalf("rate limit codes %v", codes)
	}
}

func TestSSHInstallResume(t *testing.T) {
	s, admin, _, _ := sshInstallServer(t)
	ctx := context.Background()
	u, _ := s.store.UserByEmail(ctx, "root@example.com")
	mk := func(step string, tok string) *store.SSHInstall {
		inst, err := s.store.CreateSSHInstall(ctx, &store.SSHInstall{Host: "203.0.113.10", Port: 22, User: "root", AuthMethod: "password",
			Roles: []string{"worker"}, ServerName: "cp1", CreatedBy: &u.ID})
		if err != nil {
			t.Fatal(err)
		}
		_ = s.store.SetSSHInstallStep(ctx, inst.ID, step)
		if tok != "" {
			_, _ = s.store.CreateJoinToken(ctx, hashJoinToken(tok), []string{"worker"}, time.Hour, "")
			_ = s.store.SetSSHInstallToken(ctx, inst.ID, hashJoinToken(tok))
		}
		return inst
	}
	interrupted := mk(store.SSHStepInstall, "tok-a")
	waiting := mk(store.SSHStepEnroll, "tok-b")
	s.resumeSSHInstalls(ctx)
	if out := waitInstall(t, s, admin, interrupted.ID); out["status"] != "failed" || !strings.Contains(out["error"].(string), "interrupted") {
		t.Fatalf("interrupted: %v", out)
	}
	// The waiting one finishes once its node enrolls.
	n, _, err := s.store.EnrollNode(ctx, hashJoinToken("tok-b"), "late", "UNKEYlate", 100)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.store.Q(ctx).Exec(ctx, `UPDATE nodes SET memory_mb=1000 WHERE id=$1`, n.ID)
	if out := waitInstall(t, s, admin, waiting.ID); out["status"] != "succeeded" || out["node_id"] != n.ID {
		t.Fatalf("resumed: %v", out)
	}
}
