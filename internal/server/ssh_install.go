package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/thesunbg/paas/internal/remote"
	"github.com/thesunbg/paas/internal/store"
)

// Adding a node over SSH: the control plane connects to the machine with credentials the
// administrator gives (never stored), checks it, runs install.sh join with a fresh
// single-use join token and waits until the node has enrolled and reports in.

// Step time limits; variables so tests can shorten them.
var (
	sshConnectTimeout   = 30 * time.Second
	sshPreflightTimeout = 2 * time.Minute
	sshInstallTimeout   = 30 * time.Minute
	sshEnrollTimeout    = 5 * time.Minute
	sshPollInterval     = 2 * time.Second
)

const (
	maxSSHInstallsRunning = 5  // per control plane
	maxSSHInstallsPerUser = 10 // started per 15 minutes
	maxHostKeyProbes      = 30 // per 15 minutes
)

// sshGuard counts install starts and host key probes per administrator (in memory).
var sshGuard = &attemptGuard{failures: map[string][]time.Time{}}

// sshInstaller holds the attempts this control plane runs. Its zero value is ready.
type sshInstaller struct {
	// dial and probe are remote.Dial and remote.ProbeHostKey; tests swap them.
	dial  func(ctx context.Context, cfg remote.DialConfig) (remote.Runner, error)
	probe func(ctx context.Context, addr string) (ssh.PublicKey, error)

	mu      sync.Mutex
	running map[string]context.CancelFunc
	subs    map[string]map[chan struct{}]struct{}
}

func (x *sshInstaller) dialer() func(ctx context.Context, cfg remote.DialConfig) (remote.Runner, error) {
	if x.dial != nil {
		return x.dial
	}
	return func(ctx context.Context, cfg remote.DialConfig) (remote.Runner, error) { return remote.Dial(ctx, cfg) }
}

func (x *sshInstaller) prober() func(ctx context.Context, addr string) (ssh.PublicKey, error) {
	if x.probe != nil {
		return x.probe
	}
	return func(ctx context.Context, addr string) (ssh.PublicKey, error) {
		return remote.ProbeHostKey(ctx, addr, sshConnectTimeout)
	}
}

// start registers a running attempt; false when too many run already.
func (x *sshInstaller) start(id string, cancel context.CancelFunc) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.running == nil {
		x.running = map[string]context.CancelFunc{}
	}
	if len(x.running) >= maxSSHInstallsRunning {
		return false
	}
	x.running[id] = cancel
	return true
}

func (x *sshInstaller) done(id string) {
	x.mu.Lock()
	delete(x.running, id)
	x.mu.Unlock()
	x.notify(id)
}

func (x *sshInstaller) cancel(id string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	c, ok := x.running[id]
	if ok {
		c()
	}
	return ok
}

func (x *sshInstaller) subscribe(id string) (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	x.mu.Lock()
	if x.subs == nil {
		x.subs = map[string]map[chan struct{}]struct{}{}
	}
	if x.subs[id] == nil {
		x.subs[id] = map[chan struct{}]struct{}{}
	}
	x.subs[id][ch] = struct{}{}
	x.mu.Unlock()
	return ch, func() {
		x.mu.Lock()
		delete(x.subs[id], ch)
		if len(x.subs[id]) == 0 {
			delete(x.subs, id)
		}
		x.mu.Unlock()
	}
}

func (x *sshInstaller) notify(id string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for ch := range x.subs[id] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// installLog batches an attempt's output lines into the database and wakes live viewers.
type installLog struct {
	s     *Server
	id    string
	lines chan string
	done  chan struct{}
}

func (s *Server) newInstallLog(id string) *installLog {
	l := &installLog{s: s, id: id, lines: make(chan string, 4096), done: make(chan struct{})}
	go l.loop()
	return l
}

func (l *installLog) loop() {
	defer close(l.done)
	var batch []string
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := l.s.store.AppendSSHInstallLog(ctx, l.id, batch); err != nil {
			slog.Warn("ssh install log", "install", l.id, "err", err)
		}
		cancel()
		batch = batch[:0]
		l.s.ssh.notify(l.id)
	}
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case line, ok := <-l.lines:
			if !ok {
				flush()
				return
			}
			if len(line) > 4000 {
				line = line[:4000] + "…"
			}
			batch = append(batch, line)
			if len(batch) >= 200 {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}

// Line logs one line (already masked).
func (l *installLog) Line(format string, args ...any) { l.lines <- fmt.Sprintf(format, args...) }

func (l *installLog) emit(line string) { l.lines <- line }

// Close flushes and waits.
func (l *installLog) Close() {
	close(l.lines)
	<-l.done
}

// sshCreds are the credentials of one attempt, in memory only.
type sshCreds struct {
	Password   string
	PrivateKey []byte
	Passphrase string
}

func (c *sshCreds) wipe() {
	for i := range c.PrivateKey {
		c.PrivateKey[i] = 0
	}
	*c = sshCreds{}
}

var (
	sshUserRE  = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,31}$`)
	nodeNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]{0,62}$`)
	hostRE     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.:-]{0,252}$`)
)

func validSSHHost(h string) bool {
	h = strings.Trim(h, "[]")
	return net.ParseIP(h) != nil || hostRE.MatchString(h)
}

// isControlPlaneHost reports whether host resolves to this control plane: one of its own
// addresses or the address of its public URL.
func (s *Server) isControlPlaneHost(ctx context.Context, host string) (bool, []string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, strings.Trim(host, "[]"))
	if err != nil {
		return false, nil
	}
	var mine []net.IP
	if u, err := url.Parse(s.cfg.PublicURL); err == nil && u.Hostname() != "" {
		if own, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname()); err == nil {
			for _, a := range own {
				mine = append(mine, a.IP)
			}
		}
	}
	var addrs []string
	self := false
	for _, a := range ips {
		addrs = append(addrs, a.IP.String())
		if remote.IsSelf(a.IP) {
			self = true
		}
		for _, m := range mine {
			if m.Equal(a.IP) {
				self = true
			}
		}
	}
	return self, addrs
}

// POST /api/v1/cluster/ssh-host-key: the SSH host key of a machine, read without logging in,
// for the administrator to confirm before any credential is sent.
func (s *Server) handleSSHHostKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	in.Host = strings.TrimSpace(in.Host)
	if !validSSHHost(in.Host) || in.Port < 0 || in.Port > 65535 {
		writeError(w, 400, "a valid host and port are required")
		return
	}
	key := "ssh-probe|" + currentUser(r).ID
	if !sshGuard.allowed(key, maxHostKeyProbes) {
		writeError(w, 429, "too many host key lookups; try again in 15 minutes")
		return
	}
	sshGuard.fail(key)
	addr := remote.Addr(in.Host, in.Port)
	k, err := s.ssh.prober()(r.Context(), addr)
	if err != nil {
		writeError(w, 502, err.Error())
		return
	}
	self, ips := s.isControlPlaneHost(r.Context(), in.Host)
	writeJSON(w, 200, map[string]any{
		"address":       addr,
		"ips":           ips,
		"key_type":      k.Type(),
		"fingerprint":   remote.Fingerprint(k),
		"host_key":      strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))),
		"control_plane": self,
	})
}

type sshInstallInput struct {
	Host              string   `json:"host"`
	Port              int      `json:"port"`
	User              string   `json:"user"`
	Auth              string   `json:"auth"` // key | password
	PrivateKey        string   `json:"private_key"`
	Passphrase        string   `json:"passphrase"`
	Password          string   `json:"password"` // login password, or sudo password with key auth
	Roles             []string `json:"roles"`
	Name              string   `json:"name"`
	Fingerprint       string   `json:"fingerprint"` // the host key the administrator accepted
	Reinstall         bool     `json:"reinstall"`
	PreflightOnly     bool     `json:"preflight_only"`
	AllowControlPlane bool     `json:"allow_control_plane"`
}

// POST /api/v1/cluster/ssh-installs starts an attempt; it runs in the background.
func (s *Server) handleCreateSSHInstall(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in sshInstallInput
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	creds := &sshCreds{Password: in.Password, PrivateKey: []byte(in.PrivateKey), Passphrase: in.Passphrase}
	in.Password, in.PrivateKey, in.Passphrase = "", "", ""
	in.Host, in.User, in.Name = strings.TrimSpace(in.Host), strings.TrimSpace(in.User), strings.TrimSpace(in.Name)
	if in.Port == 0 {
		in.Port = 22
	}
	if in.User == "" {
		in.User = "root"
	}
	if len(in.Roles) == 0 {
		in.Roles = []string{"worker"}
	}
	switch {
	case !validSSHHost(in.Host):
		writeError(w, 400, "a valid host name or IP address is required")
		return
	case in.Port < 1 || in.Port > 65535:
		writeError(w, 400, "port must be between 1 and 65535")
		return
	case !sshUserRE.MatchString(in.User):
		writeError(w, 400, "invalid user name")
		return
	case in.Name != "" && !nodeNameRE.MatchString(in.Name):
		writeError(w, 400, "node name: letters, digits, dots and dashes")
		return
	case in.Auth != "key" && in.Auth != "password":
		writeError(w, 400, "auth must be key or password")
		return
	case in.Auth == "key" && len(creds.PrivateKey) == 0:
		writeError(w, 400, "private_key is required for key authentication")
		return
	case in.Auth == "password" && creds.Password == "":
		writeError(w, 400, "password is required for password authentication")
		return
	case strings.TrimSpace(in.Fingerprint) == "":
		writeError(w, 400, "fingerprint is required: confirm the server's SSH host key first")
		return
	}
	for _, role := range in.Roles {
		if role != "worker" && role != "build" && role != "edge" {
			writeError(w, 400, "role must be worker, build or edge")
			return
		}
	}
	if in.Auth == "key" {
		if _, err := remote.ParsePrivateKey(creds.PrivateKey, creds.Passphrase); err != nil {
			writeError(w, 400, err.Error())
			return
		}
	}
	if !strings.HasPrefix(s.cfg.PublicURL, "https://") {
		writeError(w, 409, "PAAS_PUBLIC_URL must be https:// to join nodes: the join token would travel in clear text")
		return
	}
	if self, _ := s.isControlPlaneHost(r.Context(), in.Host); self && !in.AllowControlPlane {
		writeError(w, 400, in.Host+" is this control plane: installing a node agent over it replaces its configuration; set allow_control_plane to do it anyway")
		return
	}
	u := currentUser(r)
	key := "ssh-install|" + u.ID
	if !sshGuard.allowed(key, maxSSHInstallsPerUser) {
		writeError(w, 429, "too many SSH installs started; try again in 15 minutes")
		return
	}
	inst, err := s.store.CreateSSHInstall(r.Context(), &store.SSHInstall{
		Host: in.Host, Port: in.Port, User: in.User, AuthMethod: in.Auth, Roles: in.Roles, NodeName: in.Name,
		HostKeyFingerprint: remote.NormalizeFingerprint(in.Fingerprint), Reinstall: in.Reinstall, PreflightOnly: in.PreflightOnly,
		ServerName: s.cfg.ServerName, CreatedBy: &u.ID, CreatedByEmail: u.Email,
	})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	if !s.ssh.start(inst.ID, cancel) {
		cancel()
		_ = s.store.FinishSSHInstall(r.Context(), inst.ID, store.SSHFailed, "too many installs are running on this control plane", nil)
		writeError(w, 429, fmt.Sprintf("%d installs are already running; wait for one to finish", maxSSHInstallsRunning))
		return
	}
	sshGuard.fail(key)
	what := "node.ssh_install"
	if in.PreflightOnly {
		what = "node.ssh_preflight"
	}
	s.store.Audit(r.Context(), &u.ID, nil, what, fmt.Sprintf("%s@%s:%d roles=%s host_key=%s", in.User, in.Host, in.Port, strings.Join(in.Roles, ","), inst.HostKeyFingerprint))
	go s.runSSHInstall(ctx, inst, creds)
	writeJSON(w, 202, inst)
}

// runSSHInstall drives one attempt to its end. creds are wiped when it no longer needs them.
func (s *Server) runSSHInstall(ctx context.Context, inst *store.SSHInstall, creds *sshCreds) {
	defer s.ssh.done(inst.ID)
	defer creds.wipe()
	bg := context.WithoutCancel(ctx)
	log := s.newInstallLog(inst.ID)
	defer log.Close()
	nodeID, err := s.sshInstallSteps(ctx, inst, creds, log)
	if err == nil && !inst.PreflightOnly {
		creds.wipe() // the SSH part is over: nothing below needs a credential
		nodeID, err = s.waitEnrolled(ctx, inst, log)
	}
	if err != nil {
		if ctx.Err() != nil && !errors.Is(err, context.DeadlineExceeded) {
			err = errors.New("cancelled")
		}
		log.Line("==> failed: %s", err)
		if inst.TokenHash != nil {
			_ = s.store.RevokeJoinToken(bg, inst.TokenHash)
		}
		_ = s.store.FinishSSHInstall(bg, inst.ID, store.SSHFailed, err.Error(), nodeID)
		s.store.Audit(bg, inst.CreatedBy, nil, "node.ssh_install.failed", fmt.Sprintf("%s:%d %s", inst.Host, inst.Port, err))
		slog.Warn("ssh install failed", "install", inst.ID, "host", inst.Host, "err", err)
		return
	}
	if inst.PreflightOnly {
		log.Line("==> preflight passed")
	}
	_ = s.store.FinishSSHInstall(bg, inst.ID, store.SSHSucceeded, "", nodeID)
	if !inst.PreflightOnly {
		s.store.Audit(bg, inst.CreatedBy, nil, "node.ssh_install.succeeded", fmt.Sprintf("%s:%d node=%s", inst.Host, inst.Port, deref(nodeID)))
		slog.Info("node installed over ssh", "install", inst.ID, "host", inst.Host, "node", deref(nodeID))
	}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// sshInstallSteps connects, runs the preflight checks and the installer.
func (s *Server) sshInstallSteps(ctx context.Context, inst *store.SSHInstall, creds *sshCreds, log *installLog) (*string, error) {
	secrets := []string{creds.Password, creds.Passphrase}

	// Connect: the host key must be the one the administrator accepted, checked before auth.
	addr := remote.Addr(inst.Host, inst.Port)
	log.Line("==> connecting to %s@%s (host key %s, %s authentication)", inst.User, addr, inst.HostKeyFingerprint, inst.AuthMethod)
	verify := remote.FingerprintCallback(inst.HostKeyFingerprint)
	var seen string
	cfg := remote.DialConfig{Addr: addr, User: inst.User, Timeout: sshConnectTimeout,
		HostKey: func(host string, a net.Addr, k ssh.PublicKey) error {
			if err := verify(host, a, k); err != nil {
				return err
			}
			seen = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
			return nil
		}}
	if inst.AuthMethod == "key" {
		cfg.PrivateKey, cfg.Passphrase = creds.PrivateKey, creds.Passphrase
	} else {
		cfg.Password = creds.Password
	}
	var conn remote.Runner
	err := remote.WithTimeout(ctx, "connect", sshConnectTimeout, func(ctx context.Context) error {
		var err error
		conn, err = s.ssh.dialer()(ctx, cfg)
		return err
	})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if seen != "" {
		_ = s.store.SetSSHInstallHostKey(ctx, inst.ID, seen)
	}
	log.Line("connected")

	// Preflight.
	_ = s.store.SetSSHInstallStep(ctx, inst.ID, store.SSHStepPreflight)
	log.Line("==> preflight checks")
	host := &remote.Host{R: conn, User: inst.User, Password: creds.Password}
	var rep *remote.Report
	err = remote.WithTimeout(ctx, "preflight", sshPreflightTimeout, func(ctx context.Context) error {
		var err error
		rep, err = remote.Preflight(ctx, host, remote.PreflightOptions{Mode: "join", Roles: inst.Roles, ControlPlaneURL: s.cfg.PublicURL, Reinstall: inst.Reinstall})
		return err
	})
	if err != nil {
		return nil, err
	}
	_ = s.store.SetSSHInstallPreflight(ctx, inst.ID, rep)
	for _, c := range rep.Checks {
		log.Line("[%s] %s: %s", c.Status, c.Title, remote.MaskText(c.Detail, secrets))
	}
	if rep.Failed() {
		return nil, fmt.Errorf("preflight failed: %s", strings.Join(rep.Failures(), "; "))
	}
	if inst.PreflightOnly {
		return nil, nil
	}

	// Install with a fresh single-use join token.
	_ = s.store.SetSSHInstallStep(ctx, inst.ID, store.SSHStepInstall)
	var raw [32]byte
	_, _ = rand.Read(raw[:])
	tok := hex.EncodeToString(raw[:])
	inst.TokenHash = hashJoinToken(tok)
	if _, err := s.store.CreateJoinToken(ctx, inst.TokenHash, inst.Roles, joinTokenTTL, deref(inst.CreatedBy)); err != nil {
		return nil, err
	}
	if err := s.store.SetSSHInstallToken(ctx, inst.ID, inst.TokenHash); err != nil {
		return nil, err
	}
	cmd := fmt.Sprintf("curl -fsSL %s/install.sh | sh -s -- join --url %s --token %s --roles %s",
		remote.Quote(s.cfg.PublicURL), remote.Quote(s.cfg.PublicURL), tok, strings.Join(inst.Roles, ","))
	if inst.NodeName != "" {
		cmd += " --name " + remote.Quote(inst.NodeName)
	}
	tw := remote.NewLineWriter(log.emit, append(secrets, tok)...)
	log.Line("==> running the installer (%s/install.sh join, roles %s)", s.cfg.PublicURL, strings.Join(inst.Roles, ","))
	err = remote.WithTimeout(ctx, "install", sshInstallTimeout, func(ctx context.Context) error {
		return host.RunRoot(ctx, cmd, tw)
	})
	_ = tw.Close()
	if err != nil {
		var ee *remote.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("the installer exited with status %d (see the log)", ee.Code)
		}
		return nil, err
	}
	return nil, nil
}

// waitEnrolled waits for the attempt's join token to enroll a node and for that node to report in.
func (s *Server) waitEnrolled(ctx context.Context, inst *store.SSHInstall, log *installLog) (*string, error) {
	_ = s.store.SetSSHInstallStep(ctx, inst.ID, store.SSHStepEnroll)
	log.Line("==> waiting for the node to enroll and report in (up to %s)", sshEnrollTimeout)
	ctx, cancel := context.WithTimeout(ctx, sshEnrollTimeout)
	defer cancel()
	var nodeID *string
	t := time.NewTicker(sshPollInterval)
	defer t.Stop()
	for {
		if nodeID == nil {
			if id, err := s.store.JoinTokenNode(ctx, inst.TokenHash); err == nil && id != nil {
				nodeID = id
				n, _ := s.store.NodeByID(ctx, *id)
				if n != nil {
					log.Line("node %s enrolled (%s)", n.Name, n.ID)
				}
			}
		}
		if nodeID != nil {
			n, err := s.store.NodeByID(ctx, *nodeID)
			if err == nil && n.Status == "online" && n.MemoryMB > 0 && time.Since(n.LastSeenAt) < time.Minute {
				log.Line("==> node %s is online: %d CPU cores, %d MB memory, roles %s", n.Name, n.CPUCores, n.MemoryMB, strings.Join(n.Roles, ","))
				return nodeID, nil
			}
		}
		select {
		case <-ctx.Done():
			if nodeID == nil {
				return nil, fmt.Errorf("the installer finished but the node did not enroll within %s (on the machine: journalctl -u paasd)", sshEnrollTimeout)
			}
			return nodeID, fmt.Errorf("the node enrolled but did not report in within %s (on the machine: journalctl -u paasd)", sshEnrollTimeout)
		case <-t.C:
		}
	}
}

// resumeSSHInstalls handles the attempts this control plane was running when it stopped:
// one that was waiting for its node to enroll keeps waiting (that needs no credentials);
// one that was still using SSH failed, since its credentials are gone.
func (s *Server) resumeSSHInstalls(ctx context.Context) {
	list, err := s.store.RunningSSHInstalls(ctx, s.cfg.ServerName)
	if err != nil {
		slog.Warn("resume ssh installs", "err", err)
		return
	}
	for i := range list {
		inst := &list[i]
		if inst.Step == store.SSHStepEnroll && inst.TokenHash != nil {
			ictx, cancel := context.WithCancel(ctx)
			if s.ssh.start(inst.ID, cancel) {
				go s.resumeEnroll(ictx, inst)
				continue
			}
			cancel()
		}
		if inst.TokenHash != nil {
			_ = s.store.RevokeJoinToken(ctx, inst.TokenHash)
		}
		_ = s.store.FinishSSHInstall(ctx, inst.ID, store.SSHFailed,
			"interrupted: the control plane restarted during step "+inst.Step+"; credentials are never kept, so start the install again", nil)
	}
}

func (s *Server) resumeEnroll(ctx context.Context, inst *store.SSHInstall) {
	defer s.ssh.done(inst.ID)
	log := s.newInstallLog(inst.ID)
	defer log.Close()
	log.Line("==> the control plane restarted; still waiting for the node")
	nodeID, err := s.waitEnrolled(ctx, inst, log)
	bg := context.WithoutCancel(ctx)
	if err != nil {
		log.Line("==> failed: %s", err)
		_ = s.store.RevokeJoinToken(bg, inst.TokenHash)
		_ = s.store.FinishSSHInstall(bg, inst.ID, store.SSHFailed, err.Error(), nodeID)
		return
	}
	_ = s.store.FinishSSHInstall(bg, inst.ID, store.SSHSucceeded, "", nodeID)
	s.store.Audit(bg, inst.CreatedBy, nil, "node.ssh_install.succeeded", fmt.Sprintf("%s:%d node=%s", inst.Host, inst.Port, deref(nodeID)))
}

type sshInstallOut struct {
	*store.SSHInstall
	LogTail []string `json:"log_tail"`
}

// GET /api/v1/cluster/ssh-installs
func (s *Server) handleListSSHInstalls(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	list, err := s.store.SSHInstalls(r.Context(), 50)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if list == nil {
		list = []store.SSHInstall{}
	}
	writeJSON(w, 200, list)
}

// GET /api/v1/cluster/ssh-installs/{id}
func (s *Server) handleGetSSHInstall(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	inst, err := s.store.SSHInstallByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "install not found")
		return
	}
	tail, _ := s.store.SSHInstallLogTail(r.Context(), inst.ID, 30)
	if tail == nil {
		tail = []string{}
	}
	writeJSON(w, 200, sshInstallOut{SSHInstall: inst, LogTail: tail})
}

// GET /api/v1/cluster/ssh-installs/{id}/logs?after=ID
func (s *Server) handleSSHInstallLogs(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	lines, err := s.store.SSHInstallLogs(r.Context(), r.PathValue("id"), after, 5000)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if lines == nil {
		lines = []store.LogLine{}
	}
	writeJSON(w, 200, lines)
}

// POST /api/v1/cluster/ssh-installs/{id}/cancel
func (s *Server) handleCancelSSHInstall(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	inst, err := s.store.SSHInstallByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "install not found")
		return
	}
	if inst.Status != store.SSHRunning {
		writeError(w, 409, "the install is not running")
		return
	}
	if !s.ssh.cancel(inst.ID) {
		writeError(w, 409, "the install runs on control plane "+inst.ServerName+": cancel it there")
		return
	}
	s.store.Audit(r.Context(), &currentUser(r).ID, nil, "node.ssh_install.cancel", inst.Host)
	writeJSON(w, 202, map[string]string{"status": "cancelling"})
}

// GET /api/v1/cluster/ssh-installs/{id}/logs/stream replays the log, then streams new lines
// until the attempt ends (event "done" carries its status).
func (s *Server) handleSSHInstallLogStream(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	inst, err := s.store.SSHInstallByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "install not found")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, unsub := s.ssh.subscribe(inst.ID)
	defer unsub()
	var last int64
	if v, err := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64); err == nil {
		last = v
	}
	sendStored := func() {
		for {
			lines, _ := s.store.SSHInstallLogs(r.Context(), inst.ID, last, 1000)
			for _, l := range lines {
				fmt.Fprintf(w, "id: %d\n", l.ID)
				writeSSE(w, "log", l.Line)
				last = l.ID
			}
			if len(lines) < 1000 {
				return
			}
		}
	}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		sendStored()
		cur, err := s.store.SSHInstallByID(r.Context(), inst.ID)
		if err != nil {
			return
		}
		if cur.Status != store.SSHRunning {
			sendStored()
			writeSSE(w, "done", cur.Status)
			flusher.Flush()
			return
		}
		flusher.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-ch:
		case <-t.C:
			// Also covers an attempt run by another control plane (no local wake-ups).
		}
	}
}
