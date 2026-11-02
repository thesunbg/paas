package server

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

// fakeIdP is an in-process OpenID provider: discovery, JWKS and a token
// endpoint that checks PKCE and returns the ID token the test prepared.
type fakeIdP struct {
	srv *httptest.Server
	key *rsa.PrivateKey
	kid string

	mu    sync.Mutex
	codes map[string]fakeGrant
}

type fakeGrant struct {
	challenge string
	idToken   string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{key: key, kid: "k1", codes: map[string]fakeGrant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/auth", "token_endpoint": f.srv.URL + "/token",
			"jwks_uri": f.srv.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		pub := f.key.PublicKey
		writeJSON(w, 200, map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": f.kid, "use": "sig", "alg": "RS256",
			"n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		id, secret, ok := r.BasicAuth()
		if !ok {
			id, secret = r.Form.Get("client_id"), r.Form.Get("client_secret")
		}
		if id != "client-1" || secret != "s3cret" {
			writeJSON(w, 401, map[string]string{"error": "invalid_client"})
			return
		}
		f.mu.Lock()
		g, found := f.codes[r.Form.Get("code")]
		delete(f.codes, r.Form.Get("code"))
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !found || b64(sum[:]) != g.challenge {
			writeJSON(w, 400, map[string]string{"error": "invalid_grant"})
			return
		}
		writeJSON(w, 200, map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 300, "id_token": g.idToken})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// sign makes an RS256 JWT with the given key (the IdP's unless overridden).
func (f *fakeIdP) sign(t *testing.T, claims map[string]any, key *rsa.PrivateKey) string {
	t.Helper()
	if key == nil {
		key = f.key
	}
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": f.kid})
	c, _ := json.Marshal(claims)
	in := b64(h) + "." + b64(c)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return in + "." + b64(sig)
}

type oidcCase struct {
	claims   func(c map[string]any) // changes the default claims
	key      *rsa.PrivateKey        // signs with another key
	cookie   string                 // replaces the state cookie
	state    string                 // replaces the state in the callback
	callback func(q url.Values)     // changes the callback query
	start    string                 // start path (default: the login start)
	token    string                 // bearer token for the start request
}

// run goes through start → (provider) → callback and returns the callback's response.
func (f *fakeIdP) run(t *testing.T, h http.Handler, slug, sub, email string, tc oidcCase) *httptest.ResponseRecorder {
	t.Helper()
	start := firstNonEmpty(tc.start, "/api/v1/auth/oidc/"+slug)
	w := do(t, h, "GET", start, tc.token, nil)
	if w.Code != http.StatusFound {
		t.Fatalf("start = %d %s", w.Code, w.Body)
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	if !strings.HasPrefix(loc.String(), f.srv.URL+"/auth") {
		t.Fatalf("start redirected to %s", loc)
	}
	aq := loc.Query()
	if aq.Get("code_challenge_method") != "S256" || aq.Get("code_challenge") == "" || aq.Get("nonce") == "" || aq.Get("state") == "" {
		t.Fatalf("authorization request lacks PKCE/nonce/state: %s", loc)
	}
	var cookie string
	for _, c := range w.Result().Cookies() {
		if c.Name == oidcStateCookie {
			cookie = c.Value
		}
	}
	claims := map[string]any{
		"iss": f.srv.URL, "aud": "client-1", "sub": sub, "email": email, "email_verified": true, "name": "Ada",
		"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(), "nonce": aq.Get("nonce"),
	}
	if tc.claims != nil {
		tc.claims(claims)
	}
	code := util.RandomToken(8)
	f.mu.Lock()
	f.codes[code] = fakeGrant{challenge: aq.Get("code_challenge"), idToken: f.sign(t, claims, tc.key)}
	f.mu.Unlock()
	q := url.Values{"code": {code}, "state": {firstNonEmpty(tc.state, aq.Get("state"))}}
	if tc.callback != nil {
		tc.callback(q)
	}
	r := httptest.NewRequest("GET", oidcCallback+"?"+q.Encode(), nil)
	if tc.cookie != "" {
		cookie = tc.cookie
	}
	r.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: cookie})
	cw := httptest.NewRecorder()
	h.ServeHTTP(cw, r)
	return cw
}

func sessionOf(w *httptest.ResponseRecorder) string {
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

// wantSignedIn checks the callback started a session and returns its token.
func wantSignedIn(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	if loc := w.Header().Get("Location"); w.Code != http.StatusFound || loc != "/#/" {
		t.Fatalf("callback = %d %s, want a redirect to /#/", w.Code, loc)
	}
	tok := sessionOf(w)
	if tok == "" {
		t.Fatal("no session cookie")
	}
	return tok
}

// wantRefused checks the callback failed with an error containing msg and no session.
func wantRefused(t *testing.T, w *httptest.ResponseRecorder, msg string) {
	t.Helper()
	loc := w.Header().Get("Location")
	u, _ := url.Parse(strings.Replace(loc, "#", "", 1))
	if w.Code != http.StatusFound || !strings.Contains(u.Query().Get("sso_error"), msg) {
		t.Fatalf("callback = %d %s, want sso_error containing %q", w.Code, loc, msg)
	}
	if sessionOf(w) != "" {
		t.Fatal("a session was started")
	}
}

func addProvider(t *testing.T, s *Server, f *fakeIdP, mut func(p *store.OIDCProvider)) *store.OIDCProvider {
	t.Helper()
	p := &store.OIDCProvider{Slug: "corp", Name: "Corp", Issuer: f.srv.URL, ClientID: "client-1", ClientSecretEnc: s.box.MustEncrypt("s3cret"),
		Scopes: []string{"openid", "email"}, Enabled: true, TrustedDomains: []string{}, AllowedDomains: []string{}, EnforceDomains: []string{},
		DefaultRole: "developer", GroupsClaim: "groups", SkipTOTP: true}
	if mut != nil {
		mut(p)
	}
	saved, err := s.store.SaveOIDCProvider(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func TestOIDCProvisionAndSignIn(t *testing.T) {
	s, h := newTestServer(t)
	f := newFakeIdP(t)
	mkUser(t, s, "owner@example.com", "password1", true)
	team, err := s.store.CreateTeam(context.Background(), "Staff", "staff", "", false)
	if err != nil {
		t.Fatal(err)
	}
	addProvider(t, s, f, func(p *store.OIDCProvider) {
		p.AutoProvision, p.DefaultTeamID, p.DefaultRole = true, &team.ID, "viewer"
	})

	// The provider is offered on the sign-in page.
	w := do(t, h, "GET", "/api/v1/setup", "", nil)
	if !strings.Contains(w.Body.String(), `"slug":"corp"`) {
		t.Fatalf("setup does not list the provider: %s", w.Body)
	}
	tok := wantSignedIn(t, f.run(t, h, "corp", "sub-1", "ada@corp.test", oidcCase{}))
	if w := do(t, h, "GET", "/api/v1/me", tok, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "ada@corp.test") {
		t.Fatalf("me = %d %s", w.Code, w.Body)
	}
	u, err := s.store.UserByEmail(context.Background(), "ada@corp.test")
	if err != nil {
		t.Fatal(err)
	}
	if role, _ := s.store.MemberRole(context.Background(), team.ID, u.ID); role != "viewer" {
		t.Fatalf("default team role = %q", role)
	}
	// Signing in again (even with a changed email) finds the account by subject.
	wantSignedIn(t, f.run(t, h, "corp", "sub-1", "ada.lovelace@corp.test", oidcCase{}))
	if n, _ := s.store.CountUsers(context.Background()); n != 2 {
		t.Fatalf("users = %d, want 2", n)
	}
	// Key rotation: a token signed with a new key (new kid) refetches the JWKS.
	f.key, _ = rsa.GenerateKey(rand.Reader, 2048)
	f.kid = "k2"
	wantSignedIn(t, f.run(t, h, "corp", "sub-1", "ada@corp.test", oidcCase{}))
}

func TestOIDCTokenChecks(t *testing.T) {
	s, h := newTestServer(t)
	f := newFakeIdP(t)
	mkUser(t, s, "owner@example.com", "password1", true)
	addProvider(t, s, f, func(p *store.OIDCProvider) { p.AutoProvision = true })
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	cases := map[string]oidcCase{
		"bad signature":   {key: other},
		"wrong audience":  {claims: func(c map[string]any) { c["aud"] = "someone-else" }},
		"wrong issuer":    {claims: func(c map[string]any) { c["iss"] = "https://evil.test" }},
		"expired":         {claims: func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }},
		"nonce mismatch":  {claims: func(c map[string]any) { c["nonce"] = "other" }},
		"azp mismatch":    {claims: func(c map[string]any) { c["aud"] = []string{"client-1", "x"}; c["azp"] = "x" }},
		"unverified":      {claims: func(c map[string]any) { c["email_verified"] = false }},
		"no verification": {claims: func(c map[string]any) { delete(c, "email_verified") }},
		"idp error":       {callback: func(q url.Values) { q.Del("code"); q.Set("error", "access_denied") }},
		"mix-up":          {callback: func(q url.Values) { q.Set("iss", "https://other-idp.test") }},
	}
	want := map[string]string{"unverified": "verified email", "no verification": "verified email", "idp error": "access_denied", "mix-up": "another issuer"}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			wantRefused(t, f.run(t, h, "corp", "sub-"+name, "x@corp.test", tc), firstNonEmpty(want[name], "invalid"))
		})
	}
	if n, _ := s.store.CountUsers(context.Background()); n != 1 {
		t.Fatalf("users = %d: a refused sign-in created an account", n)
	}
}

func TestOIDCStateBinding(t *testing.T) {
	s, h := newTestServer(t)
	f := newFakeIdP(t)
	mkUser(t, s, "owner@example.com", "password1", true)
	addProvider(t, s, f, func(p *store.OIDCProvider) { p.AutoProvision = true })
	// A state from another browser (login CSRF) or a forged one is refused.
	wantRefused(t, f.run(t, h, "corp", "s1", "a@corp.test", oidcCase{cookie: "not-this-browser"}), "did not start in this browser")
	wantRefused(t, f.run(t, h, "corp", "s1", "a@corp.test", oidcCase{state: "forged", cookie: "forged"}), "expired")
	// A state works once.
	var q url.Values
	w := f.run(t, h, "corp", "s1", "a@corp.test", oidcCase{callback: func(v url.Values) { q = v }})
	wantSignedIn(t, w)
	r := httptest.NewRequest("GET", oidcCallback+"?"+q.Encode(), nil)
	r.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: q.Get("state")})
	again := httptest.NewRecorder()
	h.ServeHTTP(again, r)
	wantRefused(t, again, "expired")
	// Disabled providers cannot be used.
	if _, err := s.store.Q(context.Background()).Exec(context.Background(), `UPDATE oidc_providers SET enabled=false`); err != nil {
		t.Fatal(err)
	}
	if w := do(t, h, "GET", "/api/v1/auth/oidc/corp", "", nil); !strings.Contains(w.Header().Get("Location"), "sso_error") {
		t.Fatalf("disabled provider start = %d %s", w.Code, w.Header().Get("Location"))
	}
}

func TestOIDCLinkingRules(t *testing.T) {
	s, h := newTestServer(t)
	f := newFakeIdP(t)
	mkUser(t, s, "owner@trusted.test", "password1", true)
	mkUser(t, s, "bob@untrusted.test", "password1", false)
	alice := mkUser(t, s, "alice@trusted.test", "password1", false)
	addProvider(t, s, f, func(p *store.OIDCProvider) { p.TrustedDomains = []string{"trusted.test"}; p.AutoProvision = true })

	// Untrusted domain: an existing account is never taken over by email.
	wantRefused(t, f.run(t, h, "corp", "attacker-1", "bob@untrusted.test", oidcCase{}), "already exists")
	// Administrators are never linked automatically, even in a trusted domain.
	wantRefused(t, f.run(t, h, "corp", "attacker-2", "owner@trusted.test", oidcCase{}), "already exists")
	// Trusted domain and verified email: linked to the existing account.
	tok := wantSignedIn(t, f.run(t, h, "corp", "alice-sub", "alice@trusted.test", oidcCase{}))
	if w := do(t, h, "GET", "/api/v1/me", tok, nil); !strings.Contains(w.Body.String(), alice.ID) {
		t.Fatalf("signed in as someone else: %s", w.Body)
	}
	// Trusted domain but unverified email: refused.
	wantRefused(t, f.run(t, h, "corp", "x-sub", "carol@trusted.test", oidcCase{claims: func(c map[string]any) { c["email_verified"] = "false" }}), "verified email")

	// Explicit link from a signed-in session works for any email, then signs in.
	bobTok := mustLogin(t, h, "bob@untrusted.test", "password1")
	w := f.run(t, h, "corp", "bob-sub", "robert@elsewhere.test", oidcCase{start: "/api/v1/me/oidc/corp/link", token: bobTok,
		claims: func(c map[string]any) { c["email_verified"] = false }})
	if loc := w.Header().Get("Location"); loc != "/#/settings/security?sso=linked" {
		t.Fatalf("link = %d %s", w.Code, loc)
	}
	tok = wantSignedIn(t, f.run(t, h, "corp", "bob-sub", "robert@elsewhere.test", oidcCase{claims: func(c map[string]any) { c["email_verified"] = false }}))
	if w := do(t, h, "GET", "/api/v1/me", tok, nil); !strings.Contains(w.Body.String(), "bob@untrusted.test") {
		t.Fatalf("linked sign-in = %s", w.Body)
	}
	if w := do(t, h, "GET", "/api/v1/me/oidc", bobTok, nil); !strings.Contains(w.Body.String(), `"provider_slug":"corp"`) {
		t.Fatalf("identities = %s", w.Body)
	}
	// The same identity cannot be linked to a second account.
	ownerTok := mustLogin(t, h, "owner@trusted.test", "password1")
	w = f.run(t, h, "corp", "bob-sub", "robert@elsewhere.test", oidcCase{start: "/api/v1/me/oidc/corp/link", token: ownerTok})
	wantRefused(t, w, "already linked to another user")
	// Unlink.
	if w := do(t, h, "DELETE", "/api/v1/me/oidc/corp", bobTok, nil); w.Code != 200 {
		t.Fatalf("unlink = %d %s", w.Code, w.Body)
	}
	wantRefused(t, f.run(t, h, "corp", "bob-sub", "robert@elsewhere.test", oidcCase{claims: func(c map[string]any) { c["email_verified"] = false }}), "verified email")
}

func TestOIDCAllowedDomainsAndNoProvisioning(t *testing.T) {
	s, h := newTestServer(t)
	f := newFakeIdP(t)
	mkUser(t, s, "owner@example.com", "password1", true)
	p := addProvider(t, s, f, func(p *store.OIDCProvider) { p.AllowedDomains = []string{"corp.test"}; p.AutoProvision = true })
	wantRefused(t, f.run(t, h, "corp", "s1", "eve@gmail.test", oidcCase{}), "limited to corp.test")
	wantSignedIn(t, f.run(t, h, "corp", "s2", "ann@corp.test", oidcCase{}))
	p.AutoProvision = false
	if _, err := s.store.SaveOIDCProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	wantRefused(t, f.run(t, h, "corp", "s3", "new@corp.test", oidcCase{}), "no account")
}

func TestOIDCGroupRoles(t *testing.T) {
	s, h := newTestServer(t)
	f := newFakeIdP(t)
	mkUser(t, s, "owner@example.com", "password1", true)
	ctx := context.Background()
	ops, _ := s.store.CreateTeam(ctx, "Ops", "ops", "", false)
	dev, _ := s.store.CreateTeam(ctx, "Dev", "dev", "", false)
	addProvider(t, s, f, func(p *store.OIDCProvider) {
		p.AutoProvision = true
		p.GroupsClaim = "roles"
		p.GroupRoles = []store.OIDCGroupRole{{Group: "ops-admins", TeamID: ops.ID, Role: "admin"}, {Group: "everyone", TeamID: ops.ID, Role: "viewer"},
			{Group: "devs", TeamID: dev.ID, Role: "developer"}}
	})
	wantSignedIn(t, f.run(t, h, "corp", "s1", "g@corp.test", oidcCase{claims: func(c map[string]any) { c["roles"] = []string{"everyone", "ops-admins"} }}))
	u, _ := s.store.UserByEmail(ctx, "g@corp.test")
	if r, _ := s.store.MemberRole(ctx, ops.ID, u.ID); r != "admin" {
		t.Fatalf("ops role = %q, want admin (highest mapped)", r)
	}
	if _, err := s.store.MemberRole(ctx, dev.ID, u.ID); err == nil {
		t.Fatal("added to a team without the group")
	}
}

func TestOIDCTwoFactor(t *testing.T) {
	s, h := newTestServer(t)
	f := newFakeIdP(t)
	mkUser(t, s, "owner@example.com", "password1", true)
	u := mkUser(t, s, "t@corp.test", "password1", false)
	if err := s.store.SetTOTP(context.Background(), u.ID, s.box.MustEncrypt("JBSWY3DPEHPK3PXP"), true); err != nil {
		t.Fatal(err)
	}
	p := addProvider(t, s, f, func(p *store.OIDCProvider) { p.TrustedDomains = []string{"corp.test"} })
	// skip_totp (default): the IdP is trusted to have done MFA.
	wantSignedIn(t, f.run(t, h, "corp", "t-sub", "t@corp.test", oidcCase{}))
	p.SkipTOTP = false
	if _, err := s.store.SaveOIDCProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	w := f.run(t, h, "corp", "t-sub", "t@corp.test", oidcCase{})
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/#/login?sso_2fa=") || sessionOf(w) != "" {
		t.Fatalf("2fa = %d %s, want the code prompt and no session", w.Code, loc)
	}
	pending, _ := url.QueryUnescape(strings.TrimPrefix(w.Header().Get("Location"), "/#/login?sso_2fa="))
	if w := do(t, h, "POST", "/api/v1/auth/2fa", "", map[string]string{"pending": pending, "code": "000000"}); w.Code != 401 {
		t.Fatalf("wrong code = %d", w.Code)
	}
}

func TestEnforceSSOWithOwnerBreakGlass(t *testing.T) {
	s, h := newTestServer(t)
	f := newFakeIdP(t)
	mkUser(t, s, "root@corp.test", "password1", true) // the instance owner
	mkUser(t, s, "second-admin@corp.test", "password1", true)
	mkUser(t, s, "staff@corp.test", "password1", false)
	mkUser(t, s, "guest@other.test", "password1", false)
	addProvider(t, s, f, func(p *store.OIDCProvider) {
		p.EnforceDomains = []string{"corp.test"}
		p.TrustedDomains = []string{"corp.test"}
	})

	for _, email := range []string{"staff@corp.test", "second-admin@corp.test", "nobody@corp.test"} {
		if code, _ := login(t, h, email, "password1", "198.51.100.7"); code != 403 {
			t.Fatalf("password login %s = %d, want 403", email, code)
		}
	}
	if code, _ := login(t, h, "root@corp.test", "password1", "198.51.100.7"); code != 200 {
		t.Fatalf("owner break-glass login = %d, want 200", code)
	}
	if code, _ := login(t, h, "guest@other.test", "password1", "198.51.100.7"); code != 200 {
		t.Fatalf("other domain login = %d, want 200", code)
	}
	_ = s.store.SetSetting(context.Background(), settingAllowSignup, "true")
	if w := do(t, h, "POST", "/api/v1/auth/register", "", map[string]string{"email": "new@corp.test", "password": "password1"}); w.Code != 403 {
		t.Fatalf("register in enforced domain = %d", w.Code)
	}
	// SSO itself works for the enforced domain.
	tok := wantSignedIn(t, f.run(t, h, "corp", "staff-sub", "staff@corp.test", oidcCase{}))
	// The only way in cannot be unlinked.
	if w := do(t, h, "DELETE", "/api/v1/me/oidc/corp", tok, nil); w.Code != 400 {
		t.Fatalf("unlink last identity = %d %s", w.Code, w.Body)
	}
}

func TestOIDCProviderAdmin(t *testing.T) {
	s, h := newTestServer(t)
	f := newFakeIdP(t)
	mkUser(t, s, "owner@example.com", "password1", true)
	mkUser(t, s, "u@example.com", "password1", false)
	at, ut := mustLogin(t, h, "owner@example.com", "password1"), mustLogin(t, h, "u@example.com", "password1")
	body := map[string]any{"name": "Keycloak", "issuer": f.srv.URL, "client_id": "client-1", "client_secret": "s3cret", "allowed_domains": []string{"@Corp.Test"}}
	if w := do(t, h, "POST", "/api/v1/admin/oidc", ut, body); w.Code != 403 {
		t.Fatalf("non-admin create = %d", w.Code)
	}
	w := do(t, h, "POST", "/api/v1/admin/oidc", at, body)
	if w.Code != 201 || strings.Contains(w.Body.String(), "s3cret") {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	var out struct {
		Provider store.OIDCProvider `json:"provider"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	p := out.Provider
	if p.Slug != "keycloak" || p.AllowedDomains[0] != "corp.test" || p.Scopes[0] != "openid" || !p.SkipTOTP {
		t.Fatalf("provider = %+v", p)
	}
	stored, _ := s.store.OIDCProviderByID(context.Background(), p.ID)
	if sec, _ := s.box.Decrypt(stored.ClientSecretEnc); sec != "s3cret" {
		t.Fatal("client secret not stored encrypted")
	}
	// Updating without a secret keeps it.
	if w := do(t, h, "PATCH", "/api/v1/admin/oidc/"+p.ID, at, map[string]any{"name": "KC"}); w.Code != 200 {
		t.Fatalf("update = %d %s", w.Code, w.Body)
	}
	if again, _ := s.store.OIDCProviderByID(context.Background(), p.ID); again.ClientSecretEnc != stored.ClientSecretEnc || again.Name != "KC" {
		t.Fatal("update lost the secret or the name")
	}
	if w := do(t, h, "POST", "/api/v1/admin/oidc", at, map[string]any{"name": "Bad", "issuer": "http://127.0.0.1:1", "client_id": "x"}); w.Code != 400 {
		t.Fatalf("unreachable issuer = %d", w.Code)
	}
	if w := do(t, h, "GET", "/api/v1/admin/oidc", at, nil); w.Code != 200 || !strings.Contains(w.Body.String(), oidcCallback) || !strings.Contains(w.Body.String(), `"has_secret":true`) {
		t.Fatalf("list = %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "DELETE", "/api/v1/admin/oidc/"+p.ID, at, nil); w.Code != 200 {
		t.Fatalf("delete = %d", w.Code)
	}
}
