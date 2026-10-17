package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
)

func TestEnrollWritesIdentity(t *testing.T) {
	var got struct{ Token, Name, Nkey string }
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/cluster/enroll" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]string{"node_id": "n-1", "nats_url": "tls://cp:4222", "ca_pem": "CA-PEM"})
	}))
	defer ts.Close()
	dir := filepath.Join(t.TempDir(), "identity")
	id, err := enroll(context.Background(), ts.Client(), ts.URL, "tok", "w1", dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "tok" || got.Name != "w1" || !nkeys.IsValidPublicUserKey(got.Nkey) {
		t.Fatalf("bad request payload: %+v", got)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", st.Mode().Perm())
	}
	for _, f := range []string{"node.json", "seed.nk", "ca.pem"} {
		st, err := os.Stat(filepath.Join(dir, f))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", f, err, st)
		}
	}
	back, err := LoadIdentity(dir)
	if err != nil || back.NodeID != "n-1" || back.NATSURL != "tls://cp:4222" || string(back.CA) != "CA-PEM" || string(back.Seed) != string(id.Seed) {
		t.Fatalf("load: %v %+v", err, back)
	}
	kp, err := nkeys.FromSeed(back.Seed)
	if err != nil {
		t.Fatal(err)
	}
	if pub, _ := kp.PublicKey(); pub != got.Nkey {
		t.Fatal("stored seed does not match the enrolled public key")
	}
}

func TestEnrollRejectsHTTPAndFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("http url must not be contacted")
	}))
	defer ts.Close()
	dir := filepath.Join(t.TempDir(), "identity")
	if _, err := enroll(context.Background(), ts.Client(), ts.URL, "tok", "w1", dir); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("http url accepted: %v", err)
	}
	bad := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "invalid or expired join token", 401) }))
	defer bad.Close()
	if _, err := enroll(context.Background(), bad.Client(), bad.URL, "tok", "w1", dir); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("rejection not surfaced: %v", err)
	}
	if _, err := LoadIdentity(dir); !os.IsNotExist(err) {
		t.Fatalf("failed enroll left an identity: %v", err)
	}
}

func TestRemoveEnvVar(t *testing.T) {
	p := filepath.Join(t.TempDir(), "paasd.env")
	_ = os.WriteFile(p, []byte("PAAS_JOIN_URL=https://x\nPAAS_JOIN_TOKEN=abc\nPAAS_ROLES=worker\n"), 0o600)
	if err := removeEnvVar(p, "PAAS_JOIN_TOKEN"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "PAAS_JOIN_URL=https://x\nPAAS_ROLES=worker\n" {
		t.Fatalf("got %q", b)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatal("mode changed")
	}
}

func TestEnrollDoesNotFollowRedirect(t *testing.T) {
	hit := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer target.Close()
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer ts.Close()
	hc := *noRedirect
	hc.Transport = ts.Client().Transport
	dir := filepath.Join(t.TempDir(), "identity")
	if _, err := enroll(context.Background(), &hc, ts.URL, "tok", "w1", dir); err == nil {
		t.Fatal("enroll must fail on a redirect")
	}
	if hit {
		t.Fatal("redirect target was contacted")
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("identity written despite redirect")
	}
}
