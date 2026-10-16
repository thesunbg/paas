package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/thesunbg/paas/internal/store"
)

// userWithTeam mirrors signup: the account owns a personal team.
func userWithTeam(t *testing.T, s *Server, email string, admin bool) string {
	t.Helper()
	u := mkUser(t, s, email, "password1", admin)
	if _, err := s.store.CreateTeam(context.Background(), "personal", "personal-"+u.ID, u.ID, false); err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func setLimits(t *testing.T, s *Server, mut func(*TeamLimits)) {
	t.Helper()
	l := defaultTeamLimits
	mut(&l)
	b, _ := json.Marshal(l)
	if err := s.store.SetSetting(context.Background(), settingTeamLimits, string(b)); err != nil {
		t.Fatal(err)
	}
}

func createTeam(t *testing.T, h http.Handler, tok, name string, want int) string {
	t.Helper()
	w := do(t, h, "POST", "/api/v1/teams", tok, map[string]string{"name": name})
	if w.Code != want {
		t.Fatalf("create team %q = %d %s, want %d", name, w.Code, w.Body, want)
	}
	var out struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out.ID
}

func TestCreateTeamLimit(t *testing.T) {
	s, h := newTestServer(t)
	userWithTeam(t, s, "u@example.com", false)
	userWithTeam(t, s, "admin@example.com", true)
	ut, at := mustLogin(t, h, "u@example.com", "password1"), mustLogin(t, h, "admin@example.com", "password1")
	createTeam(t, h, ut, "two", 201)
	createTeam(t, h, ut, "three", 201)
	createTeam(t, h, ut, "four", 403)
	createTeam(t, h, at, "admin-extra-1", 201)
	createTeam(t, h, at, "admin-extra-2", 201)
	createTeam(t, h, at, "admin-extra-3", 201)
	setLimits(t, s, func(l *TeamLimits) { l.MaxTeams = 0 })
	createTeam(t, h, ut, "unlimited", 201)
}

func TestAddMemberSignupClosed(t *testing.T) {
	s, h := newTestServer(t)
	userWithTeam(t, s, "u@example.com", false)
	userWithTeam(t, s, "admin@example.com", true)
	ut, at := mustLogin(t, h, "u@example.com", "password1"), mustLogin(t, h, "admin@example.com", "password1")
	team := createTeam(t, h, ut, "mine", 201)
	w := do(t, h, "POST", "/api/v1/teams/"+team+"/members", ut, map[string]string{"email": "new@example.com", "role": "viewer"})
	if w.Code != 403 {
		t.Fatalf("add new email = %d %s, want 403", w.Code, w.Body)
	}
	if _, err := s.store.UserByEmail(context.Background(), "new@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("account was created anyway: %v", err)
	}
	w = do(t, h, "POST", "/api/v1/teams/"+team+"/members", at, map[string]string{"email": "new@example.com", "role": "viewer"})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 201 || out["temporary_password"] == nil {
		t.Fatalf("admin add = %d %s, want 201 with temporary_password", w.Code, w.Body)
	}
}

func TestAddMemberSignupOpenRateLimited(t *testing.T) {
	s, h := newTestServer(t)
	userWithTeam(t, s, "u@example.com", false)
	ctx := context.Background()
	if err := s.store.SetSetting(ctx, settingAllowSignup, "true"); err != nil {
		t.Fatal(err)
	}
	setLimits(t, s, func(l *TeamLimits) { l.MaxPerIPPerDay = 1 })
	ut := mustLogin(t, h, "u@example.com", "password1")
	team := createTeam(t, h, ut, "mine", 201)
	for i, want := range []int{201, 429} {
		email := []string{"one@example.com", "two@example.com"}[i]
		if w := do(t, h, "POST", "/api/v1/teams/"+team+"/members", ut, map[string]string{"email": email, "role": "viewer"}); w.Code != want {
			t.Fatalf("add %s = %d %s, want %d", email, w.Code, w.Body, want)
		}
	}
}

func TestAddMemberNoUserObject(t *testing.T) {
	s, h := newTestServer(t)
	userWithTeam(t, s, "u@example.com", false)
	mkUser(t, s, "other@example.com", "password1", false)
	ut := mustLogin(t, h, "u@example.com", "password1")
	team := createTeam(t, h, ut, "mine", 201)
	w := do(t, h, "POST", "/api/v1/teams/"+team+"/members", ut, map[string]string{"email": "other@example.com", "role": "viewer"})
	if w.Code != 201 {
		t.Fatalf("add existing = %d %s", w.Code, w.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	for _, k := range []string{"user", "name", "is_admin", "totp_enabled"} {
		if _, ok := out[k]; ok {
			t.Errorf("response leaks %q", k)
		}
	}
	if out["email"] != "other@example.com" || out["role"] != "viewer" {
		t.Errorf("response = %v, want email and role", out)
	}
}
