package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/totp"
	"github.com/thesunbg/paas/internal/util"
)

func mustLogin(t *testing.T, h http.Handler, email, pw string) string {
	t.Helper()
	code, tok := login(t, h, email, pw, "198.51.100.1")
	if code != 200 {
		t.Fatalf("login %s = %d", email, code)
	}
	return tok
}

func mkAPIToken(t *testing.T, h http.Handler, session string) string {
	t.Helper()
	w := do(t, h, "POST", "/api/v1/tokens", session, map[string]string{"name": "ci"})
	var out struct {
		Token string `json:"token"`
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Token == "" {
		t.Fatalf("create token = %d %s", w.Code, w.Body)
	}
	return out.Token
}

func wantStatus(t *testing.T, h http.Handler, tok string, want int, what string) {
	t.Helper()
	if w := do(t, h, "GET", "/api/v1/me", tok, nil); w.Code != want {
		t.Errorf("%s: /me = %d, want %d", what, w.Code, want)
	}
}

// enableTOTP stores a secret and turns 2FA on, returning the secret.
func enableTOTP(t *testing.T, s *Server, u *store.User) string {
	t.Helper()
	secret := totp.NewSecret()
	if err := s.store.SetTOTP(context.Background(), u.ID, s.box.MustEncrypt(secret), true); err != nil {
		t.Fatal(err)
	}
	return secret
}

func TestChangePasswordRevokesOthers(t *testing.T) {
	s, h := newTestServer(t)
	mkUser(t, s, "a@example.com", "password1", false)
	a, b := mustLogin(t, h, "a@example.com", "password1"), mustLogin(t, h, "a@example.com", "password1")
	api := mkAPIToken(t, h, a)
	w := do(t, h, "POST", "/api/v1/me/password", a, map[string]string{"current": "password1", "new": "password2"})
	if w.Code != 200 {
		t.Fatalf("change password = %d %s", w.Code, w.Body)
	}
	wantStatus(t, h, a, 200, "current session")
	wantStatus(t, h, b, 401, "other session")
	wantStatus(t, h, api, 401, "api token")
}

func TestAdminPasswordResetRevokesAll(t *testing.T) {
	s, h := newTestServer(t)
	mkUser(t, s, "admin@example.com", "password1", true)
	x := mkUser(t, s, "x@example.com", "password1", false)
	at := mustLogin(t, h, "admin@example.com", "password1")
	xa, xb := mustLogin(t, h, "x@example.com", "password1"), mustLogin(t, h, "x@example.com", "password1")
	api := mkAPIToken(t, h, xa)
	w := do(t, h, "PATCH", "/api/v1/users/"+x.ID, at, map[string]any{"reset_password": true})
	if w.Code != 200 {
		t.Fatalf("reset = %d %s", w.Code, w.Body)
	}
	wantStatus(t, h, at, 200, "admin session")
	for name, tok := range map[string]string{"session a": xa, "session b": xb, "api token": api} {
		wantStatus(t, h, tok, 401, name)
	}
}

func TestDisable2FARevokes(t *testing.T) {
	s, h := newTestServer(t)
	u := mkUser(t, s, "a@example.com", "password1", false)
	// Logging in with 2FA on needs a code, so open the sessions before enabling it.
	a, b := mustLogin(t, h, "a@example.com", "password1"), mustLogin(t, h, "a@example.com", "password1")
	api := mkAPIToken(t, h, a)
	secret := enableTOTP(t, s, u)
	code, _ := totp.Code(secret, time.Now())
	w := do(t, h, "POST", "/api/v1/me/2fa/disable", a, map[string]string{"code": code, "password": "password1"})
	if w.Code != 200 {
		t.Fatalf("self disable = %d %s", w.Code, w.Body)
	}
	wantStatus(t, h, a, 200, "self: current session")
	wantStatus(t, h, b, 401, "self: other session")
	wantStatus(t, h, api, 401, "self: api token")

	// Administrator disables it: everything goes.
	mkUser(t, s, "admin@example.com", "password1", true)
	enableTOTP(t, s, u)
	at := mustLogin(t, h, "admin@example.com", "password1")
	c := mustSession(t, s, u.ID)
	w = do(t, h, "PATCH", "/api/v1/users/"+u.ID, at, map[string]any{"disable_2fa": true})
	if w.Code != 200 {
		t.Fatalf("admin disable = %d %s", w.Code, w.Body)
	}
	wantStatus(t, h, at, 200, "admin session")
	wantStatus(t, h, a, 401, "admin: old session")
	wantStatus(t, h, c, 401, "admin: other session")
}

// mustSession inserts a session straight into the store (login would demand a 2FA code).
func mustSession(t *testing.T, s *Server, userID string) string {
	t.Helper()
	tok := "sess-" + userID
	if err := s.store.CreateSession(context.Background(), util.HashToken(tok), userID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestQueryTokenOnlyOnStreams(t *testing.T) {
	s, h := newTestServer(t)
	mkUser(t, s, "a@example.com", "password1", false)
	tok := mustLogin(t, h, "a@example.com", "password1")
	if w := do(t, h, "GET", "/api/v1/me?access_token="+tok, "", nil); w.Code != 401 {
		t.Errorf("/me with query token = %d, want 401", w.Code)
	}
	w := do(t, h, "GET", "/api/v1/services/00000000-0000-0000-0000-000000000000/logs/stream?access_token="+tok, "", nil)
	if w.Code == 401 {
		t.Errorf("log stream with query token = 401, want it past authentication")
	}
}
