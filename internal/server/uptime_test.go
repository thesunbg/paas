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
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/store"
)

// tlsServerExpiring serves h over TLS with a self-signed certificate valid until notAfter.
func tlsServerExpiring(t *testing.T, notAfter time.Time, h http.Handler) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "uptime.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, pool
}

func TestProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte("hello healthy world"))
		case "/fail":
			w.WriteHeader(503)
		case "/slow":
			time.Sleep(1500 * time.Millisecond)
		case "/created":
			w.WriteHeader(201)
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound)
		}
	}))
	defer srv.Close()
	client := newUptimeClient(true, nil)
	ctx := context.Background()
	for _, c := range []struct {
		name           string
		path, keyword  string
		expect, status int
		ok             bool
		errPart        string
	}{
		{"ok", "/ok", "", 0, 200, true, ""},
		{"keyword", "/ok", "healthy", 0, 200, true, ""},
		{"keyword missing", "/ok", "sad", 0, 200, false, "keyword"},
		{"5xx", "/fail", "", 0, 503, false, "status 503"},
		{"expected 503", "/fail", "", 503, 503, true, ""},
		{"expected 200 got 201", "/created", "", 200, 201, false, "expected 200"},
		{"redirect followed", "/redirect", "healthy", 200, 200, true, ""},
		{"timeout", "/slow", "", 0, 0, false, "deadline"},
	} {
		chk := &store.UptimeCheck{URL: srv.URL + c.path, Method: "GET", ExpectedStatus: c.expect, Keyword: c.keyword, TimeoutSeconds: 1}
		res, cert := probe(ctx, client, chk)
		if res.OK != c.ok || res.StatusCode != c.status || !strings.Contains(res.Error, c.errPart) {
			t.Errorf("%s: %+v", c.name, res)
		}
		if cert != nil {
			t.Errorf("%s: plain http has no certificate", c.name)
		}
		if c.ok && res.LatencyMS < 0 {
			t.Errorf("%s: latency %d", c.name, res.LatencyMS)
		}
	}

	// The SSRF guard refuses private addresses by default.
	res, _ := probe(ctx, newUptimeClient(false, nil), &store.UptimeCheck{URL: srv.URL + "/ok", Method: "GET", TimeoutSeconds: 2})
	if res.OK || !strings.Contains(res.Error, "not a public address") {
		t.Fatalf("guarded probe of loopback = %+v", res)
	}
}

func TestProbeTLSCertExpiry(t *testing.T) {
	expiry := time.Now().Add(5 * 24 * time.Hour).Truncate(time.Second)
	srv, pool := tlsServerExpiring(t, expiry, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	client := newUptimeClient(true, &tls.Config{RootCAs: pool})
	chk := &store.UptimeCheck{ID: "c", URL: srv.URL, Method: "GET", TimeoutSeconds: 5, FailureThreshold: 3, State: "unknown"}
	res, cert := probe(context.Background(), client, chk)
	if !res.OK || cert == nil || !cert.Equal(expiry) {
		t.Fatalf("probe = %+v cert=%v, want ok with expiry %v", res, cert, expiry)
	}
	now := time.Now()
	events := applyUptimeResult(chk, res, cert, now)
	if len(events) != 1 || events[0] != "uptime.cert" || chk.State != "up" {
		t.Fatalf("first result: events %v state %s", events, chk.State)
	}
	// Warned once per certificate.
	if events := applyUptimeResult(chk, res, cert, now.Add(time.Minute)); len(events) != 0 {
		t.Fatalf("second result: events %v", events)
	}
	// A renewed certificate that still expires soon warns again; a far one does not.
	renewed := expiry.Add(24 * time.Hour)
	if events := applyUptimeResult(chk, res, &renewed, now.Add(2*time.Minute)); len(events) != 1 {
		t.Fatalf("renewed soon-expiring cert: events %v", events)
	}
	far := now.Add(60 * 24 * time.Hour)
	if events := applyUptimeResult(chk, res, &far, now.Add(3*time.Minute)); len(events) != 0 || !chk.CertExpiresAt.Equal(far) {
		t.Fatalf("far cert: events %v", events)
	}

	// An expired certificate fails the check.
	old, oldPool := tlsServerExpiring(t, time.Now().Add(-time.Hour), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	res, _ = probe(context.Background(), newUptimeClient(true, &tls.Config{RootCAs: oldPool}), &store.UptimeCheck{URL: old.URL, Method: "GET", TimeoutSeconds: 5})
	if res.OK || !strings.Contains(res.Error, "certificate") {
		t.Fatalf("expired cert = %+v", res)
	}
}

func TestApplyUptimeResult(t *testing.T) {
	c := &store.UptimeCheck{State: "unknown", FailureThreshold: 3}
	now := time.Now()
	ok := store.UptimeResult{OK: true, StatusCode: 200}
	bad := store.UptimeResult{Error: "status 500", StatusCode: 500}
	step := func(r store.UptimeResult, wantState string, wantEvents ...string) {
		t.Helper()
		now = now.Add(time.Minute)
		ev := applyUptimeResult(c, r, nil, now)
		if c.State != wantState || strings.Join(ev, ",") != strings.Join(wantEvents, ",") {
			t.Fatalf("state %s events %v, want %s %v", c.State, ev, wantState, wantEvents)
		}
	}
	step(ok, "up") // unknown -> up is no recovery
	step(bad, "up")
	step(bad, "up")
	step(ok, "up") // the streak resets
	step(bad, "up")
	step(bad, "up")
	step(bad, "down", "uptime.down")
	if c.ConsecutiveFailures != 3 || c.LastError != "status 500" {
		t.Fatalf("check = %+v", c)
	}
	step(bad, "down") // no repeat
	step(ok, "up", "uptime.up")
	if c.ConsecutiveFailures != 0 || c.LastError != "" {
		t.Fatalf("after recovery: %+v", c)
	}
}

func TestUptimeDB(t *testing.T) {
	e := newAlertEnv(t)
	ctx := context.Background()
	var healthy atomic.Bool
	healthy.Store(true)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(500)
		}
	}))
	defer target.Close()
	base := "/api/v1/projects/" + e.proj.ID + "/uptime"

	for name, bad := range map[string]map[string]any{
		"interval": {"url": target.URL, "interval_seconds": 10},
		"scheme":   {"url": "ftp://example.com"},
		"method":   {"url": target.URL, "method": "DELETE"},
		"head kw":  {"url": target.URL, "method": "HEAD", "keyword": "x"},
		"status":   {"url": target.URL, "expected_status": 99},
	} {
		if w := do(t, e.h, "POST", base, e.tok["dev"], bad); w.Code != 400 {
			t.Errorf("%s: create = %d %s, want 400", name, w.Code, w.Body)
		}
	}
	if w := do(t, e.h, "POST", base, e.tok["viewer"], map[string]any{"url": target.URL}); w.Code != 403 {
		t.Fatalf("viewer create = %d", w.Code)
	}
	w := do(t, e.h, "POST", base, e.tok["dev"], map[string]any{"url": target.URL, "service_id": e.svc.ID, "failure_threshold": 2,
		"interval_seconds": 30, "channel_ids": []string{e.chID}})
	if w.Code != 201 {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	var chk store.UptimeCheck
	_ = json.Unmarshal(w.Body.Bytes(), &chk)
	if chk.Name == "" || chk.Method != "GET" || chk.TimeoutSeconds != 10 || chk.State != "unknown" {
		t.Fatalf("defaults: %+v", chk)
	}

	e.s.uptime.client = newUptimeClient(true, nil)
	run := func() {
		t.Helper()
		c, err := e.s.store.UptimeCheckByID(ctx, chk.ID)
		if err != nil {
			t.Fatal(err)
		}
		e.s.runUptimeCheck(ctx, c)
		time.Sleep(5 * time.Millisecond) // distinct result timestamps
	}
	run()
	healthy.Store(false)
	run()
	e.expectNoHook(t)
	run()
	e.expectHook(t, "uptime.down")
	healthy.Store(true)
	run()
	e.expectHook(t, "uptime.up")

	w = do(t, e.h, "GET", "/api/v1/uptime/"+chk.ID, e.tok["viewer"], nil)
	var got struct {
		Check   uptimeView           `json:"check"`
		Results []store.UptimeResult `json:"results"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("get = %d %s", w.Code, w.Body)
	}
	if len(got.Results) != 4 || !got.Results[0].OK || got.Check.State != "up" || got.Check.Stats.Uptime24h == nil || *got.Check.Stats.Uptime24h != 50 ||
		got.Check.Stats.Checks24h != 4 || got.Check.Stats.P95Latency24 == nil {
		t.Fatalf("get = %s", w.Body)
	}
	w = do(t, e.h, "GET", base+"?service="+e.svc.ID, e.tok["viewer"], nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), chk.ID) {
		t.Fatalf("list = %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, "GET", "/api/v1/uptime/"+chk.ID, e.tok["outsider"], nil); w.Code != 403 {
		t.Fatalf("outsider get = %d", w.Code)
	}

	// A new URL starts over.
	w = do(t, e.h, "PATCH", "/api/v1/uptime/"+chk.ID, e.tok["dev"], map[string]any{"url": target.URL + "/other", "interval_seconds": 120})
	var upd store.UptimeCheck
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &upd) != nil || upd.State != "unknown" || upd.IntervalSeconds != 120 {
		t.Fatalf("patch = %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, "DELETE", "/api/v1/uptime/"+chk.ID, e.tok["viewer"], nil); w.Code != 403 {
		t.Fatalf("viewer delete = %d", w.Code)
	}
	if w := do(t, e.h, "DELETE", "/api/v1/uptime/"+chk.ID, e.tok["dev"], nil); w.Code != 200 {
		t.Fatalf("delete = %d", w.Code)
	}
}
