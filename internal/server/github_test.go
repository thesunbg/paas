package server

import (
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
