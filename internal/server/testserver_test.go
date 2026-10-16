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
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/thesunbg/paas/internal/config"
	"github.com/thesunbg/paas/internal/secrets"
	"github.com/thesunbg/paas/internal/store"
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
