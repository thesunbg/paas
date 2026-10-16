package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/totp"
)

func loginWithCode(t *testing.T, h http.Handler, email, pw, code string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, h, "POST", "/api/v1/auth/login", "", map[string]string{"email": email, "password": pw, "code": code})
}

func TestTOTPReplayRejected(t *testing.T) {
	s, h := newTestServer(t)
	u := mkUser(t, s, "a@example.com", "password1", false)
	secret := enableTOTP(t, s, u)
	code, _ := totp.Code(secret, time.Now())
	if w := loginWithCode(t, h, "a@example.com", "password1", code); w.Code != 200 {
		t.Fatalf("first login = %d %s", w.Code, w.Body)
	}
	w := loginWithCode(t, h, "a@example.com", "password1", code)
	if w.Code != 401 || !strings.Contains(w.Body.String(), "invalid two-factor code") {
		t.Fatalf("replayed code = %d %s, want 401 invalid two-factor code", w.Code, w.Body)
	}
}

func TestTOTPOlderStepRejected(t *testing.T) {
	s, h := newTestServer(t)
	u := mkUser(t, s, "a@example.com", "password1", false)
	secret := enableTOTP(t, s, u)
	cur, _ := totp.Code(secret, time.Now())
	if w := loginWithCode(t, h, "a@example.com", "password1", cur); w.Code != 200 {
		t.Fatalf("login = %d %s", w.Code, w.Body)
	}
	prev, _ := totp.Code(secret, time.Now().Add(-30*time.Second))
	if prev == cur {
		t.Skip("adjacent steps produced the same code")
	}
	if w := loginWithCode(t, h, "a@example.com", "password1", prev); w.Code != 401 {
		t.Fatalf("older step = %d, want 401", w.Code)
	}
}

func Test2FAEnableRateLimited(t *testing.T) {
	s, h := newTestServer(t)
	mkUser(t, s, "a@example.com", "password1", false)
	tok := mustLogin(t, h, "a@example.com", "password1")
	w := do(t, h, "POST", "/api/v1/me/2fa/setup", tok, nil)
	if w.Code != 200 {
		t.Fatalf("setup = %d %s", w.Code, w.Body)
	}
	u, _ := s.store.UserByEmail(t.Context(), "a@example.com")
	secret, _ := s.box.Decrypt(u.TOTPSecret)
	for i := 0; i < maxPer2FA; i++ {
		if w := do(t, h, "POST", "/api/v1/me/2fa/enable", tok, map[string]string{"code": "000000"}); w.Code != 400 {
			t.Fatalf("wrong code %d = %d, want 400", i, w.Code)
		}
	}
	code, _ := totp.Code(secret, time.Now())
	if w := do(t, h, "POST", "/api/v1/me/2fa/enable", tok, map[string]string{"code": code}); w.Code != 429 {
		t.Fatalf("correct code after lockout = %d, want 429", w.Code)
	}
}

func Test2FADisableRateLimited(t *testing.T) {
	s, h := newTestServer(t)
	u := mkUser(t, s, "a@example.com", "password1", false)
	tok := mustLogin(t, h, "a@example.com", "password1")
	secret := enableTOTP(t, s, u)
	code, _ := totp.Code(secret, time.Now())
	for i := 0; i < maxPer2FA; i++ {
		if w := do(t, h, "POST", "/api/v1/me/2fa/disable", tok, map[string]string{"code": code, "password": "wrong-password"}); w.Code != 400 {
			t.Fatalf("wrong password %d = %d, want 400", i, w.Code)
		}
	}
	if w := do(t, h, "POST", "/api/v1/me/2fa/disable", tok, map[string]string{"code": code, "password": "password1"}); w.Code != 429 {
		t.Fatalf("correct credentials after lockout = %d, want 429", w.Code)
	}
}
