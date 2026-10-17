package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/thesunbg/paas/internal/bus"
	"github.com/thesunbg/paas/internal/config"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/secrets"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

// newTestServer gives each test its own migrated database (cheap enough: one
// CREATE DATABASE from template1 plus the migrations) and the real route mux.
// Skips unless PAAS_TEST_DATABASE_URL points at a Postgres we may create databases in.
func newTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	adminURL := testDatabaseURL(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect admin db: %v", err)
	}
	defer admin.Close(ctx)
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "paas_test_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	u, _ := url.Parse(adminURL)
	u.Path = "/" + name
	st, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		st.Close()
		c, err := pgx.Connect(ctx, adminURL)
		if err != nil {
			return
		}
		defer c.Close(ctx)
		_, _ = c.Exec(ctx, `DROP DATABASE `+name+` WITH (FORCE)`)
	})
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	box, err := secrets.New(key)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: config.Server{BaseDomain: "apps.test"}, store: st, box: box}
	// loginGuard and signupGuard are package-level: start every test clean.
	loginGuard.mu.Lock()
	loginGuard.failures = map[string][]time.Time{}
	loginGuard.mu.Unlock()
	signupGuard.mu.Lock()
	signupGuard.hits = map[string][]time.Time{}
	signupGuard.mu.Unlock()
	return s, s.routes()
}

// newTestServerBus is newTestServer plus what deploy and reconcile need: lock and
// scheduling state, a cluster token, an embedded NATS server and the log hub.
func newTestServerBus(t *testing.T) *Server {
	t.Helper()
	s, _ := newTestServer(t)
	s.locks = map[string]*sync.Mutex{}
	s.evictedOn = map[string]time.Time{}
	s.wake = newWakeGroup()
	s.traffic = newTraffic()
	s.scale = map[string]*scaleState{}
	s.ClusterToken = util.RandomToken(24)
	var err error
	if err = s.initTrust(context.Background()); err != nil {
		t.Fatalf("init trust: %v", err)
	}
	if s.ns, err = bus.StartServer("127.0.0.1:-1", s.auth, s.ca.ServerTLS(), t.TempDir()); err != nil {
		t.Fatalf("start nats: %v", err)
	}
	if s.nc, err = bus.ConnectInProcess(s.ns, s.internalSecret, "test"); err != nil {
		t.Fatalf("connect nats: %v", err)
	}
	s.hub = NewLogHub(s.store, s.nc)
	t.Cleanup(func() {
		s.nc.Close()
		s.ns.Shutdown()
	})
	return s
}

// fakeBuildAgent answers build requests for nodeID: each request reports its
// deployment id on started, then blocks until release is called.
func fakeBuildAgent(t *testing.T, s *Server, nodeID string) (started <-chan string, release func()) {
	t.Helper()
	ch := make(chan string, 16)
	gate := make(chan struct{})
	var once sync.Once
	_, err := bus.Handle(s.nc, proto.AgentSubject(nodeID, proto.CmdBuild), func(_ context.Context, r *proto.BuildRequest) *proto.BuildReply {
		ch <- r.DeploymentID
		<-gate
		return &proto.BuildReply{Image: "img:test", Detected: "go"}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.nc.Flush(); err != nil {
		t.Fatal(err)
	}
	rel := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(rel)
	return ch, rel
}

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	v := os.Getenv("PAAS_TEST_DATABASE_URL")
	if v == "" {
		t.Skip("PAAS_TEST_DATABASE_URL not set")
	}
	return v
}

func mkUser(t *testing.T, s *Server, email, pw string, admin bool) *store.User {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.store.CreateUser(context.Background(), email, email, string(hash), admin)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

// login posts credentials from the given client IP and returns the status and session token.
func login(t *testing.T, h http.Handler, email, pw, ip string) (int, string) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"email": email, "password": pw})
	r := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(string(b)))
	r.RemoteAddr = ip + ":1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out.Token
}

// do calls the API with a Bearer token (which skips the CSRF origin check). body may be nil, a string or a JSON-able value.
func do(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	switch v := body.(type) {
	case nil:
		rd = strings.NewReader("")
	case string:
		rd = strings.NewReader(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		rd = strings.NewReader(string(b))
	}
	r := httptest.NewRequest(method, path, rd)
	r.RemoteAddr = "192.0.2.1:1234"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHarnessSmoke(t *testing.T) {
	s, h := newTestServer(t)
	mkUser(t, s, "a@example.com", "password1", false)
	code, tok := login(t, h, "a@example.com", "password1", "198.51.100.1")
	if code != 200 || tok == "" {
		t.Fatalf("login = %d token=%q", code, tok)
	}
	if w := do(t, h, "GET", "/api/v1/me", tok, nil); w.Code != 200 {
		t.Fatalf("me = %d %s", w.Code, w.Body)
	}
}
