package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testApp(t *testing.T) (*App, *rsa.PrivateKey) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return &App{ID: 42, Slug: "paas-test", PEM: string(p)}, key
}

func TestJWT(t *testing.T) {
	app, key := testApp(t)
	c, err := NewClient(app)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := c.JWT()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatal("bad jwt")
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatal("signature does not verify")
	}
	claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var m map[string]any
	_ = json.Unmarshal(claims, &m)
	if m["iss"] != "42" {
		t.Fatalf("iss = %v", m["iss"])
	}
}

func TestTokenAndRepos(t *testing.T) {
	app, _ := testApp(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/7/access_tokens":
			calls++
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				t.Error("token exchange must use the app JWT")
			}
			w.Write([]byte(`{"token":"ghs_x","expires_at":"2099-01-01T00:00:00Z"}`))
		case r.URL.Path == "/installation/repositories":
			if r.Header.Get("Authorization") != "token ghs_x" {
				t.Error("repo listing must use the installation token")
			}
			w.Write([]byte(`{"total_count":1,"repositories":[{"full_name":"me/app","private":true,"default_branch":"main","clone_url":"https://github.com/me/app.git"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	API = srv.URL
	c, _ := NewClient(app)
	repos, err := c.Repos(t.Context(), 7)
	if err != nil || len(repos) != 1 || repos[0].FullName != "me/app" {
		t.Fatalf("repos %v %v", repos, err)
	}
	_, _ = c.Token(t.Context(), 7)
	if calls != 1 {
		t.Fatalf("token should be cached, exchanged %d times", calls)
	}
}

func TestRepoToken(t *testing.T) {
	app, _ := testApp(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var in struct {
			Repositories []string          `json:"repositories"`
			Permissions  map[string]string `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if len(in.Repositories) != 1 || in.Permissions["contents"] != "read" || len(in.Permissions) != 1 {
			t.Errorf("bad mint body %+v", in)
		}
		w.Write([]byte(`{"token":"ghs_` + in.Repositories[0] + `","expires_at":"2099-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()
	API = srv.URL
	c, _ := NewClient(app)
	for _, tc := range []struct{ repo, want string }{{"app", "ghs_app"}, {"other", "ghs_other"}, {"app", "ghs_app"}} {
		tok, err := c.RepoToken(t.Context(), 7, tc.repo)
		if err != nil || tok != tc.want {
			t.Fatalf("RepoToken(%s) = %q, %v", tc.repo, tok, err)
		}
	}
	if calls != 2 {
		t.Fatalf("want 2 mints (one per repo, second app cached), got %d", calls)
	}
}

func TestUserInstallationIDs(t *testing.T) {
	app, _ := testApp(t)
	app.ClientID, app.ClientSecret = "cid", "csecret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["code"] != "abc" || in["client_secret"] != "csecret" {
				t.Errorf("bad exchange %v", in)
			}
			w.Write([]byte(`{"access_token":"ghu_x"}`))
		case "/user/installations":
			if r.Header.Get("Authorization") != "token ghu_x" {
				t.Error("must use the user token")
			}
			w.Write([]byte(`{"installations":[{"id":7},{"id":9}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	API, Web = srv.URL, srv.URL
	c, _ := NewClient(app)
	tok, err := c.UserToken(t.Context(), "abc")
	if err != nil || tok != "ghu_x" {
		t.Fatalf("token %q %v", tok, err)
	}
	ids, err := c.UserInstallationIDs(t.Context(), tok)
	if err != nil || !ids[7] || !ids[9] || ids[8] {
		t.Fatalf("ids %v %v", ids, err)
	}
}

func TestManifest(t *testing.T) {
	m := Manifest("paas-x", "https://paas.example.com")
	if m["request_oauth_on_install"] != true || m["hook_attributes"].(map[string]any)["url"] != "https://paas.example.com/api/v1/webhooks/github-app" {
		t.Fatalf("manifest %v", m)
	}
}
