package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/config"
	"github.com/thesunbg/paas/internal/s3"
	"github.com/thesunbg/paas/internal/store"
)

// fakeLookup resolves from a table; a name not in it does not exist. Names
// under "*.<base>" match any label of that base (the wildcard).
func fakeLookup(table map[string][]string, calls *atomic.Int32) func(context.Context, string) ([]net.IP, error) {
	return func(_ context.Context, host string) ([]net.IP, error) {
		if calls != nil {
			calls.Add(1)
		}
		v, ok := table[host]
		if !ok {
			if i := strings.Index(host, "."); i > 0 {
				v, ok = table["*"+host[i:]]
			}
		}
		if !ok {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		var out []net.IP
		for _, s := range v {
			out = append(out, net.ParseIP(s))
		}
		return out, nil
	}
}

func ips(v ...string) []net.IP {
	var out []net.IP
	for _, s := range v {
		out = append(out, net.ParseIP(s))
	}
	return out
}

func TestCheckDNS(t *testing.T) {
	ctx := context.Background()
	own := ips("203.0.113.10", "2001:db8::10")
	cases := []struct {
		name    string
		table   map[string][]string
		base    string
		own     []net.IP
		want    string
		records int
	}{
		{"ok", map[string][]string{"*.apps.test": {"203.0.113.10"}, "paas.apps.test": {"203.0.113.10", "2001:db8::10"}}, "apps.test", own, setupOK, 0},
		{"wildcard missing", map[string][]string{"paas.apps.test": {"203.0.113.10"}}, "apps.test", own, setupFail, 2},
		{"points elsewhere", map[string][]string{"*.apps.test": {"198.51.100.7"}, "paas.apps.test": {"198.51.100.7"}}, "apps.test", own, setupFail, 4},
		{"extra address", map[string][]string{"*.apps.test": {"203.0.113.10", "198.51.100.7"}, "paas.apps.test": {"203.0.113.10"}}, "apps.test", own, setupWarn, 2},
		{"own unknown", map[string][]string{"*.apps.test": {"198.51.100.7"}, "paas.apps.test": {"198.51.100.7"}}, "apps.test", nil, setupWarn, 0},
		{"behind nat", map[string][]string{"*.apps.test": {"198.51.100.7"}, "paas.apps.test": {"198.51.100.7"}}, "apps.test", ips("10.0.0.5"), setupWarn, 0},
		{"no base domain", nil, "localhost", own, setupFail, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := checkDNS(ctx, fakeLookup(c.table, nil), c.base, "paas.apps.test", c.own)
			if r.Status != c.want {
				t.Fatalf("status %s, want %s: %s %v", r.Status, c.want, r.Summary, r.Details)
			}
			if len(r.Records) != c.records {
				t.Fatalf("records %+v, want %d", r.Records, c.records)
			}
		})
	}

	// The wildcard is tested with a random label, and records name both hosts with A and AAAA.
	var asked []string
	var askedMu sync.Mutex // lookups run concurrently
	lookup := func(_ context.Context, host string) ([]net.IP, error) {
		askedMu.Lock()
		defer askedMu.Unlock()
		asked = append(asked, host)
		return nil, errors.New("timeout")
	}
	r := checkDNS(ctx, lookup, "apps.test", "dash.example.org", own)
	if r.Status != setupFail {
		t.Fatalf("status %s", r.Status)
	}
	var labelled bool
	for _, h := range asked {
		if strings.HasPrefix(h, "paas-check-") && strings.HasSuffix(h, ".apps.test") {
			labelled = true
		}
	}
	if !labelled {
		t.Fatalf("wildcard not probed with a random label: %v", asked)
	}
	want := map[string]bool{"*.apps.test A 203.0.113.10": true, "*.apps.test AAAA 2001:db8::10": true,
		"dash.example.org A 203.0.113.10": true, "dash.example.org AAAA 2001:db8::10": true}
	for _, rec := range r.Records {
		delete(want, rec.Name+" "+rec.Type+" "+rec.Value)
	}
	if len(want) != 0 {
		t.Fatalf("missing records %v in %+v", want, r.Records)
	}
}

// tlsServer serves cert (the httptest default when nil) and returns it with
// its address.
func tlsServer(t *testing.T, cert *tls.Certificate) (*httptest.Server, *x509.Certificate) {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	if cert != nil {
		srv.TLS = &tls.Config{Certificates: []tls.Certificate{*cert}}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, srv.Certificate()
}

// selfSigned makes a CA-capable self-signed certificate for host valid in [from, to].
func selfSigned(t *testing.T, host string, from, to time.Time) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: from, NotAfter: to, IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func dialTo(addr string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
}

func TestInspectCert(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	srv, cert := tlsServer(t, nil) // httptest: valid for example.com
	trusted := x509.NewCertPool()
	trusted.AddCert(cert)
	addr := srv.Listener.Addr().String()

	state := func(dial func(context.Context, string, string) (net.Conn, error), host string) tls.ConnectionState {
		cs, err := tlsState(ctx, dial, addr, host)
		if err != nil {
			t.Fatal(err)
		}
		return cs
	}
	if r := inspectCert(state(dialTo(addr), "example.com"), "example.com", trusted, now); r.Status != setupOK {
		t.Fatalf("valid: %s %s", r.Status, r.Summary)
	}
	if r := inspectCert(state(dialTo(addr), "paas.other.test"), "paas.other.test", trusted, now); r.Status != setupFail || !strings.Contains(r.Summary, "not for") {
		t.Fatalf("wrong host: %s %s", r.Status, r.Summary)
	}
	if r := inspectCert(state(dialTo(addr), "example.com"), "example.com", x509.NewCertPool(), now); r.Status != setupFail || !strings.Contains(r.Summary, "not trusted") {
		t.Fatalf("untrusted: %s %s", r.Status, r.Summary)
	}
	if r := inspectCert(state(dialTo(addr), "example.com"), "example.com", trusted, cert.NotAfter.Add(-5*24*time.Hour)); r.Status != setupWarn {
		t.Fatalf("expiring: %s %s", r.Status, r.Summary)
	}

	expired := selfSigned(t, "paas.apps.test", now.Add(-48*time.Hour), now.Add(-time.Hour))
	esrv, ecert := tlsServer(t, expired)
	pool := x509.NewCertPool()
	pool.AddCert(ecert)
	cs, err := tlsState(ctx, dialTo(esrv.Listener.Addr().String()), "x", "paas.apps.test")
	if err != nil {
		t.Fatal(err)
	}
	if r := inspectCert(cs, "paas.apps.test", pool, now); r.Status != setupFail || !strings.Contains(r.Summary, "expired") {
		t.Fatalf("expired: %s %s", r.Status, r.Summary)
	}
}

func TestCheckHTTPSFallsBackToLocalProxy(t *testing.T) {
	srv, cert := tlsServer(t, nil)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	local := srv.Listener.Addr().String()
	s := &Server{cfg: testCfg("example.com", "https://example.com")}
	p := &setupProbe{roots: pool, now: time.Now, proxyHTTPS: local, proxyTLS: true,
		dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr != local {
				return nil, &net.DNSError{Err: "no such host", Name: addr, IsNotFound: true}
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}
	r := s.checkHTTPS(context.Background(), p)
	if r.Status != setupOK || r.Info["address"] != local {
		t.Fatalf("got %s %s %v", r.Status, r.Summary, r.Info)
	}

	// Nothing listening: fail with HTTPS on, warn ("HTTPS is off") without.
	srv.Close()
	if r := s.checkHTTPS(context.Background(), p); r.Status != setupFail {
		t.Fatalf("no listener: %s %s", r.Status, r.Summary)
	}
	s.cfg.PublicURL = "http://example.com"
	p.proxyTLS = false
	if r := s.checkHTTPS(context.Background(), p); r.Status != setupWarn || !strings.Contains(r.Summary, "HTTPS is off") {
		t.Fatalf("https off: %s %s", r.Status, r.Summary)
	}
}

func testCfg(dashboard, publicURL string) config.Server {
	return config.Server{BaseDomain: "apps.test", DashboardDomain: dashboard, PublicURL: publicURL}
}

// listenAddr returns the address of an open listener and of a closed port.
func listenAddrs(t *testing.T) (open, closed string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed = ln2.Addr().String()
	ln2.Close()
	return ln.Addr().String(), closed
}

// setupTestServer is a DB-backed server with fake probes: DNS from table, the
// edge proxy at the given addresses, a controllable clock.
func setupTestServer(t *testing.T, table map[string][]string) (*Server, http.Handler, *setupProbe, *atomic.Int32, *time.Time) {
	t.Helper()
	s, h := newTestServer(t)
	s.cfg.DashboardDomain = "paas.apps.test"
	s.cfg.PublicURL = "https://paas.apps.test"
	s.cfg.PublicIPs = []string{"203.0.113.10"}
	open, _ := listenAddrs(t)
	calls := &atomic.Int32{}
	now := time.Now()
	var mu sync.Mutex
	p := &setupProbe{
		lookup: fakeLookup(table, calls),
		dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if strings.HasPrefix(addr, "paas.apps.test") {
				return nil, errors.New("connection refused")
			}
			return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, addr)
		},
		now:          func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
		proxyHTTP:    open,
		proxyHTTPS:   open,
		proxyTLS:     true,
		storageCheck: func(*s3.Config) error { return nil },
	}
	s.setup.probe = p
	return s, h, p, calls, &now
}

func decodeChecklist(t *testing.T, body []byte) setupChecklist {
	t.Helper()
	var cl setupChecklist
	if err := json.Unmarshal(body, &cl); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	return cl
}

func stepOf(cl setupChecklist, id string) setupStepView {
	for _, st := range cl.Steps {
		if st.ID == id {
			return st
		}
	}
	return setupStepView{}
}

func TestSetupChecklistAggregationAndCache(t *testing.T) {
	s, h, _, calls, now := setupTestServer(t, map[string][]string{"*.apps.test": {"203.0.113.10"}})
	ctx := context.Background()
	admin := mkUser(t, s, "admin@example.com", "password1", true)
	tok := mustLogin(t, h, "admin@example.com", "password1")

	w := do(t, h, "GET", "/api/v1/admin/setup-checklist", tok, nil)
	if w.Code != 200 {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}
	cl := decodeChecklist(t, w.Body.Bytes())
	if len(cl.Steps) != len(setupSteps) || !cl.Pending {
		t.Fatalf("steps %d pending %v", len(cl.Steps), cl.Pending)
	}
	want := map[string]string{"admin": setupWarn, "dns": setupOK, "https": setupFail, "ports": setupOK, "github": setupWarn,
		"backups": setupWarn, "notifications": setupWarn, "first_app": setupWarn}
	for id, st := range want {
		if got := stepOf(cl, id); got.Status != st || got.Summary == "" || got.HowTo == "" {
			t.Errorf("%s: %s (%s), want %s", id, got.Status, got.Summary, st)
		}
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("lookups %d, want 2", n)
	}

	// Cached for 30 s: no new lookups, even after the state changed.
	team, err := s.store.CreateTeam(ctx, "ops", "ops", admin.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.CreateChannel(ctx, &store.NotificationChannel{TeamID: team.ID, Name: "ops", Kind: "webhook", TargetEnc: s.box.MustEncrypt("https://example.com"), Events: []string{"node.offline"}}); err != nil {
		t.Fatal(err)
	}
	cl = decodeChecklist(t, do(t, h, "GET", "/api/v1/admin/setup-checklist", tok, nil).Body.Bytes())
	if calls.Load() != 2 || stepOf(cl, "notifications").Status != setupWarn {
		t.Fatalf("not cached: lookups %d, notifications %s", calls.Load(), stepOf(cl, "notifications").Status)
	}

	// Check again runs that one step only.
	w = do(t, h, "POST", "/api/v1/admin/setup-checklist/notifications/check", tok, nil)
	var st setupStepView
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if w.Code != 200 || st.Status != setupOK || calls.Load() != 2 {
		t.Fatalf("check again: %d %s lookups %d", w.Code, st.Status, calls.Load())
	}
	if w := do(t, h, "POST", "/api/v1/admin/setup-checklist/nope/check", tok, nil); w.Code != 404 {
		t.Fatalf("unknown step: %d", w.Code)
	}

	// After the TTL everything runs again.
	*now = now.Add(setupCacheTTL + time.Second)
	decodeChecklist(t, do(t, h, "GET", "/api/v1/admin/setup-checklist", tok, nil).Body.Bytes())
	if n := calls.Load(); n != 4 {
		t.Fatalf("lookups after ttl %d, want 4", n)
	}
	decodeChecklist(t, do(t, h, "GET", "/api/v1/admin/setup-checklist?refresh=1", tok, nil).Body.Bytes())
	if n := calls.Load(); n != 6 {
		t.Fatalf("lookups after refresh %d, want 6", n)
	}
}

func TestSetupChecklistBackupsAndTimeout(t *testing.T) {
	s, h, p, _, _ := setupTestServer(t, nil)
	mkUser(t, s, "admin@example.com", "password1", true)
	tok := mustLogin(t, h, "admin@example.com", "password1")
	s.s3.Store(&s3.Config{Endpoint: "https://s3.example.com", Bucket: "b", AccessKey: "a", SecretKey: "s"})
	check := func() setupStepView {
		var st setupStepView
		w := do(t, h, "POST", "/api/v1/admin/setup-checklist/backups/check", tok, nil)
		_ = json.Unmarshal(w.Body.Bytes(), &st)
		return st
	}
	if st := check(); st.Status != setupOK {
		t.Fatalf("storage ok: %s %s", st.Status, st.Summary)
	}
	p.storageCheck = func(*s3.Config) error { return errors.New("upload: 403 AccessDenied") }
	if st := check(); st.Status != setupFail || !strings.Contains(st.Summary, "AccessDenied") {
		t.Fatalf("storage failing: %s %s", st.Status, st.Summary)
	}
	old := setupCheckTimeout
	setupCheckTimeout = 50 * time.Millisecond
	defer func() { setupCheckTimeout = old }()
	block := make(chan struct{})
	defer close(block)
	p.storageCheck = func(*s3.Config) error { <-block; return nil }
	if st := check(); st.Status != setupFail || !strings.Contains(st.Summary, "timed out") {
		t.Fatalf("storage hanging: %s %s", st.Status, st.Summary)
	}
}

func TestCheckPorts(t *testing.T) {
	s, _, p, _, _ := setupTestServer(t, nil)
	if r := s.checkPorts(context.Background(), p); r.Status != setupOK {
		t.Fatalf("listening: %s %v", r.Status, r.Details)
	}
	open, closed := listenAddrs(t)
	p.proxyHTTP, p.proxyHTTPS = open, closed
	r := s.checkPorts(context.Background(), p)
	if r.Status != setupFail || !strings.Contains(strings.Join(r.Details, " "), "not listening") {
		t.Fatalf("closed 443: %s %v", r.Status, r.Details)
	}
}

func TestSetupChecklistAdminOnly(t *testing.T) {
	s, h, _, _, _ := setupTestServer(t, nil)
	mkUser(t, s, "admin@example.com", "password1", true)
	mkUser(t, s, "dev@example.com", "password1", false)
	tok := mustLogin(t, h, "dev@example.com", "password1")
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/admin/setup-checklist"},
		{"PUT", "/api/v1/admin/setup-checklist"},
		{"POST", "/api/v1/admin/setup-checklist/dns/check"},
	} {
		if w := do(t, h, c.method, c.path, tok, "{}"); w.Code != 403 {
			t.Errorf("%s %s as member: %d", c.method, c.path, w.Code)
		}
		if w := do(t, h, c.method, c.path, "", "{}"); w.Code != 401 {
			t.Errorf("%s %s anonymous: %d", c.method, c.path, w.Code)
		}
	}
	var me map[string]any
	_ = json.Unmarshal(do(t, h, "GET", "/api/v1/me", tok, nil).Body.Bytes(), &me)
	if _, ok := me["setup_pending"]; ok {
		t.Fatalf("setup_pending shown to a member: %v", me)
	}
}

func TestSetupChecklistProgressPersists(t *testing.T) {
	s, h, _, _, _ := setupTestServer(t, nil)
	mkUser(t, s, "admin@example.com", "password1", true)
	tok := mustLogin(t, h, "admin@example.com", "password1")
	pending := func() any {
		var me map[string]any
		_ = json.Unmarshal(do(t, h, "GET", "/api/v1/me", tok, nil).Body.Bytes(), &me)
		return me["setup_pending"]
	}
	if pending() != true {
		t.Fatal("a new instance should have the wizard pending")
	}
	w := do(t, h, "PUT", "/api/v1/admin/setup-checklist", tok, map[string]any{"completed": []string{"dns", "https", "dns"}, "skipped": []string{"github"}})
	if w.Code != 200 {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "PUT", "/api/v1/admin/setup-checklist", tok, map[string]any{"completed": []string{"bogus"}}); w.Code != 400 {
		t.Fatalf("unknown step: %d", w.Code)
	}
	// A partial update keeps the other fields.
	if w := do(t, h, "PUT", "/api/v1/admin/setup-checklist", tok, map[string]any{"dismissed": true}); w.Code != 200 {
		t.Fatalf("dismiss: %d", w.Code)
	}
	cl := decodeChecklist(t, do(t, h, "GET", "/api/v1/admin/setup-checklist", tok, nil).Body.Bytes())
	if !cl.Dismissed || cl.Pending || strings.Join(cl.Completed, ",") != "dns,https" || strings.Join(cl.Skipped, ",") != "github" {
		t.Fatalf("progress: %+v", cl.setupProgress)
	}
	if !stepOf(cl, "dns").Completed || !stepOf(cl, "github").Skipped || stepOf(cl, "ports").Completed {
		t.Fatal("step flags not applied")
	}
	if pending() != false {
		t.Fatal("dismissed wizard still pending")
	}
	// Re-opened from Settings: not dismissed any more, progress kept.
	do(t, h, "PUT", "/api/v1/admin/setup-checklist", tok, map[string]any{"dismissed": false})
	if p, _ := s.setupProgress(context.Background()); !p.pending() || len(p.Completed) != 2 {
		t.Fatalf("reopened: %+v", p)
	}
}
