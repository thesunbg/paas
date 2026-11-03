package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/thesunbg/paas/internal/github"
	"github.com/thesunbg/paas/internal/store"
)

func TestGithubRepo(t *testing.T) {
	for _, tc := range []struct {
		url, want string
		ok        bool
	}{
		{"https://gitlab.com/me/r", "", false},
		{"https://github.com/other/r", "", false},
		{"https://evil.com/github.com/me/r", "", false},
		{"http://github.com/me/r", "", false},
		{"https://www.github.com/me/r", "", false},
		{"https://github.com:8443/me/r", "", false},
		{"https://github.com/me/r/extra", "", false},
		{"https://github.com/me", "", false},
		{"https://github.com/Me/r.git", "r", true},
		{"https://github.com/me/r/", "r", true},
	} {
		got, err := githubRepo(tc.url, "me")
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("githubRepo(%q) = %q, %v", tc.url, got, err)
		}
	}
}

// A repo outside the installation's account must fail before any token is minted.
func TestRepoTokenMismatchDoesNotMint(t *testing.T) {
	mints := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mints++
		w.Write([]byte(`{"token":"x","expires_at":"2099-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()
	old := github.API
	github.API = srv.URL
	defer func() { github.API = old }()
	c, err := github.NewClient(&github.App{ID: 1, PEM: testPEM(t)})
	if err != nil {
		t.Fatal(err)
	}
	inst := &store.GitHubInstallation{InstallationID: 7, AccountLogin: "me"}
	if _, err := repoToken(t.Context(), c, inst, "https://gitlab.com/me/r.git"); err == nil {
		t.Fatal("gitlab repo must not get a token")
	}
	if mints != 0 {
		t.Fatalf("minted %d tokens for a mismatched repo", mints)
	}
	if tok, err := repoToken(t.Context(), c, inst, "https://github.com/me/r.git"); err != nil || tok != "x" || mints != 1 {
		t.Fatalf("matching repo: %q %v mints=%d", tok, err, mints)
	}
}

func testPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

// Disconnecting the GitHub App also forgets its installations: a new app gets new
// installation ids, and a stale row for the same account would be used and fail.
func TestDeleteGitHubAppForgetsInstallations(t *testing.T) {
	s, h := newTestServer(t)
	ctx := context.Background()
	admin := userWithTeam(t, s, "admin@example.com", true)
	tok := mustLogin(t, h, "admin@example.com", "password1")
	team := s.firstTeam(ctx, admin)
	if err := s.store.SetSetting(ctx, settingGitHubApp, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Q(ctx).Exec(ctx, `INSERT INTO github_installations (installation_id, team_id, account_login) VALUES (42, $1, 'me')`, team); err != nil {
		t.Fatal(err)
	}
	userWithTeam(t, s, "dev@example.com", false)
	if w := do(t, h, "DELETE", "/api/v1/github/app", mustLogin(t, h, "dev@example.com", "password1"), nil); w.Code != 403 {
		t.Fatalf("non-admin DELETE = %d", w.Code)
	}
	if w := do(t, h, "DELETE", "/api/v1/github/app", tok, nil); w.Code != 204 {
		t.Fatalf("DELETE = %d %s", w.Code, w.Body.String())
	}
	var n int
	_ = s.store.Q(ctx).QueryRow(ctx, `SELECT count(*) FROM github_installations`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d installations left", n)
	}
	if _, err := s.store.GetSetting(ctx, settingGitHubApp); err == nil {
		t.Fatal("the app is still configured")
	}
}
