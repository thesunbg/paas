package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

// Single sign-on with OpenID Connect (Google Workspace, Microsoft Entra ID,
// Keycloak, Authentik…). Instance administrators configure providers; each
// sign-in is an Authorization Code flow with PKCE (S256), a random state bound
// to the browser by a cookie and stored server-side (single use), and a nonce.
// The ID token is verified by go-oidc against the provider's JWKS (cached, and
// refetched when a token names an unknown key, so key rotation just works):
// signature, iss, aud and exp; this file checks nonce, azp and email_verified.
//
// Which account a provider identity signs in to (the account-takeover rules):
//
//  1. An identity already linked ((provider, sub), never the email: emails are
//     mutable and not unique at most IdPs) signs in to its account.
//  2. Otherwise the provider must report a *verified* email. If an account has
//     that email, it is linked automatically only when the email's domain is in
//     the provider's trusted_domains (the administrator asserts the IdP controls
//     who gets addresses there) and the account is not an instance
//     administrator. Anything else must be linked explicitly from a signed-in
//     session (Settings → Security), which proves control of both sides.
//     Without this rule, anyone able to register an arbitrary (or unverified)
//     email at some IdP could take over the account with that email here.
//  3. No account: one is created only when the provider has auto_provision on.
//
// allowed_domains, when set, restricts every sign-in through the provider to
// verified emails in those domains.
//
// Two-factor: when skip_totp is on (the default) an SSO sign-in does not ask
// for the account's TOTP code, because the IdP is expected to enforce MFA. Turn
// it off for providers that do not, and accounts with 2FA get the same code
// prompt as a password sign-in.
//
// Enforced SSO: password (and GitHub) sign-in is refused for emails in a
// provider's enforce_domains, except for the instance owner (the first
// administrator), who keeps password sign-in as a break-glass account for when
// the IdP is down or misconfigured. API tokens keep working.

const (
	oidcStateCookie = "paas_oidc_state"
	oidcCallback    = "/api/v1/auth/oidc/callback"
	oidcLoginTTL    = 10 * time.Minute
	oidcCacheTTL    = time.Hour
)

var oidcHTTP = &http.Client{Timeout: 15 * time.Second}

type oidcClient struct {
	key      string // issuer + client id the entry was built for
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	at       time.Time
}

// oidcCache holds discovery results and key sets per provider id.
type oidcCache struct {
	mu      sync.Mutex
	clients map[string]*oidcClient
}

func (c *oidcCache) forget(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.clients, id)
}

func oidcContext(ctx context.Context) context.Context {
	return oidc.ClientContext(ctx, oidcHTTP)
}

// oidcClientFor returns the discovered provider and an ID token verifier for p.
func (s *Server) oidcClientFor(ctx context.Context, p *store.OIDCProvider) (*oidcClient, error) {
	key := p.Issuer + "\x00" + p.ClientID
	s.oidc.mu.Lock()
	c := s.oidc.clients[p.ID]
	s.oidc.mu.Unlock()
	if c != nil && c.key == key && time.Since(c.at) < oidcCacheTTL {
		return c, nil
	}
	prov, err := oidc.NewProvider(oidcContext(ctx), p.Issuer)
	if err != nil {
		return nil, fmt.Errorf("discovery for %s failed: %w", p.Issuer, err)
	}
	c = &oidcClient{key: key, provider: prov, verifier: prov.Verifier(&oidc.Config{ClientID: p.ClientID}), at: time.Now()}
	s.oidc.mu.Lock()
	if s.oidc.clients == nil {
		s.oidc.clients = map[string]*oidcClient{}
	}
	s.oidc.clients[p.ID] = c
	s.oidc.mu.Unlock()
	return c, nil
}

func (s *Server) oauthConfig(p *store.OIDCProvider, c *oidcClient, redirect string) (*oauth2.Config, error) {
	secret := ""
	if p.ClientSecretEnc != "" {
		var err error
		if secret, err = s.box.Decrypt(p.ClientSecretEnc); err != nil {
			return nil, errors.New("cannot read the client secret")
		}
	}
	return &oauth2.Config{ClientID: p.ClientID, ClientSecret: secret, Endpoint: c.provider.Endpoint(), RedirectURL: redirect, Scopes: p.Scopes}, nil
}

// GET /api/v1/auth/oidc/{provider} starts a sign-in.
func (s *Server) handleOIDCLoginStart(w http.ResponseWriter, r *http.Request) {
	s.startOIDC(w, r, nil, "/#/login")
}

// GET /api/v1/me/oidc/{provider}/link links a provider to the signed-in account.
func (s *Server) handleOIDCLinkStart(w http.ResponseWriter, r *http.Request) {
	id := currentUser(r).ID
	s.startOIDC(w, r, &id, "/#/settings/security")
}

func (s *Server) startOIDC(w http.ResponseWriter, r *http.Request, linkUser *string, failPath string) {
	ctx := r.Context()
	fail := func(msg string) { http.Redirect(w, r, withQuery(failPath, "sso_error", msg), http.StatusFound) }
	p, err := s.store.OIDCProviderBySlug(ctx, r.PathValue("provider"))
	if err != nil || !p.Enabled {
		fail("unknown sign-in provider")
		return
	}
	c, err := s.oidcClientFor(ctx, p)
	if err != nil {
		slog.Warn("oidc discovery", "provider", p.Slug, "err", err)
		fail(p.Name + " is not reachable right now")
		return
	}
	redirect := s.baseURL(r) + oidcCallback
	cfg, err := s.oauthConfig(p, c, redirect)
	if err != nil {
		fail(err.Error())
		return
	}
	state, nonce, verifier := util.RandomToken(32), util.RandomToken(16), oauth2.GenerateVerifier()
	if err := s.store.CreateOIDCLogin(ctx, util.HashToken(state), &store.OIDCLogin{ProviderID: p.ID, Nonce: nonce, Verifier: verifier,
		RedirectURI: redirect, LinkUserID: linkUser}, time.Now().Add(oidcLoginTTL)); err != nil {
		fail(err.Error())
		return
	}
	// The cookie binds the state to this browser, so an attacker cannot make a
	// victim finish a sign-in the attacker started (login CSRF). Lax: the
	// provider's redirect back is a top-level GET.
	http.SetCookie(w, &http.Cookie{Name: oidcStateCookie, Value: state, Path: oidcCallback, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https", MaxAge: int(oidcLoginTTL.Seconds())})
	http.Redirect(w, r, cfg.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

// oidcResult is what a verified ID token says about the user.
type oidcResult struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	Groups        []string
}

// GET /api/v1/auth/oidc/callback is the redirect URI of every provider.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	failPath := "/#/login"
	fail := func(msg string) { http.Redirect(w, r, withQuery(failPath, "sso_error", msg), http.StatusFound) }
	http.SetCookie(w, &http.Cookie{Name: oidcStateCookie, Value: "", Path: oidcCallback, MaxAge: -1})
	state := q.Get("state")
	c, err := r.Cookie(oidcStateCookie)
	if state == "" || err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		fail("the sign-in did not start in this browser or expired; please try again")
		return
	}
	login, err := s.store.TakeOIDCLogin(ctx, util.HashToken(state))
	if err != nil {
		fail("the sign-in expired; please try again")
		return
	}
	if login.LinkUserID != nil {
		failPath = "/#/settings/security"
	}
	p, err := s.store.OIDCProviderByID(ctx, login.ProviderID)
	if err != nil || !p.Enabled {
		fail("unknown sign-in provider")
		return
	}
	res, err := s.oidcExchange(ctx, p, login, q)
	if err != nil {
		slog.Warn("oidc sign-in", "provider", p.Slug, "err", err)
		fail(err.Error())
		return
	}
	if login.LinkUserID != nil {
		if err := s.linkOIDC(ctx, p, res, *login.LinkUserID); err != nil {
			fail(err.Error())
			return
		}
		s.store.Audit(ctx, login.LinkUserID, nil, "oidc.link", p.Slug)
		http.Redirect(w, r, "/#/settings/security?sso=linked", http.StatusFound)
		return
	}
	u, err := s.oidcUser(r, p, res)
	if err != nil {
		fail(err.Error())
		return
	}
	s.applyGroupRoles(ctx, p, u, res.Groups)
	if u.TOTPEnabled && !p.SkipTOTP {
		pending, err := s.signState(ctx, ghState{Kind: "2fa", UserID: u.ID})
		if err != nil {
			fail(err.Error())
			return
		}
		http.Redirect(w, r, withQuery("/#/login", "sso_2fa", pending), http.StatusFound)
		return
	}
	if _, err := s.newSession(w, r, u); err != nil {
		fail(err.Error())
		return
	}
	s.store.Audit(ctx, &u.ID, nil, "login.oidc", p.Slug)
	http.Redirect(w, r, "/#/", http.StatusFound)
}

// oidcExchange trades the code for tokens and verifies the ID token.
func (s *Server) oidcExchange(ctx context.Context, p *store.OIDCProvider, login *store.OIDCLogin, q url.Values) (*oidcResult, error) {
	if e := q.Get("error"); e != "" {
		return nil, fmt.Errorf("%s: %s", p.Name, firstNonEmpty(q.Get("error_description"), e))
	}
	// RFC 9207: a provider that names itself must be the one this sign-in went to (mix-up attacks).
	if iss := q.Get("iss"); iss != "" && iss != p.Issuer {
		return nil, errors.New("the response came from another issuer")
	}
	code := q.Get("code")
	if code == "" {
		return nil, errors.New("the provider did not return an authorization code")
	}
	c, err := s.oidcClientFor(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("%s is not reachable right now", p.Name)
	}
	cfg, err := s.oauthConfig(p, c, login.RedirectURI)
	if err != nil {
		return nil, err
	}
	tok, err := cfg.Exchange(oidcContext(ctx), code, oauth2.VerifierOption(login.Verifier))
	if err != nil {
		slog.Warn("oidc token exchange", "provider", p.Slug, "err", err)
		return nil, errors.New("the provider refused the authorization code")
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return nil, errors.New("the provider returned no ID token")
	}
	idt, err := c.verifier.Verify(oidcContext(ctx), raw)
	if err != nil {
		slog.Warn("oidc id token", "provider", p.Slug, "err", err)
		return nil, errors.New("the ID token is invalid")
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(login.Nonce)) != 1 {
		return nil, errors.New("the ID token is invalid (nonce)")
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return nil, errors.New("the ID token is invalid")
	}
	// With several audiences, the token must have been issued to us.
	if azp, _ := claims["azp"].(string); len(idt.Audience) > 1 && azp != p.ClientID {
		return nil, errors.New("the ID token is invalid (azp)")
	}
	res := &oidcResult{Subject: idt.Subject}
	res.Email, _ = claims["email"].(string)
	res.Email = strings.ToLower(strings.TrimSpace(res.Email))
	// Some providers send "true" as a string. A missing claim is "not verified".
	switch v := claims["email_verified"].(type) {
	case bool:
		res.EmailVerified = v
	case string:
		res.EmailVerified = v == "true"
	}
	res.Name, _ = claims["name"].(string)
	if res.Name == "" {
		res.Name, _ = claims["preferred_username"].(string)
	}
	switch g := claims[firstNonEmpty(p.GroupsClaim, "groups")].(type) {
	case []any:
		for _, v := range g {
			if s, ok := v.(string); ok {
				res.Groups = append(res.Groups, s)
			}
		}
	case string:
		res.Groups = []string{g}
	}
	if res.Subject == "" {
		return nil, errors.New("the ID token has no subject")
	}
	return res, nil
}

func emailDomain(email string) string {
	if i := strings.LastIndexByte(email, '@'); i >= 0 {
		return strings.ToLower(email[i+1:])
	}
	return ""
}

func inDomains(email string, domains []string) bool {
	d := emailDomain(email)
	return d != "" && slices.Contains(domains, d)
}

// checkAllowed applies the provider's allowed_domains.
func checkAllowed(p *store.OIDCProvider, res *oidcResult) error {
	if len(p.AllowedDomains) == 0 {
		return nil
	}
	if !res.EmailVerified || !inDomains(res.Email, p.AllowedDomains) {
		return fmt.Errorf("%s sign-in is limited to %s addresses", p.Name, strings.Join(p.AllowedDomains, ", "))
	}
	return nil
}

// oidcUser resolves (or creates) the account an identity signs in to; see the rules at the top.
func (s *Server) oidcUser(r *http.Request, p *store.OIDCProvider, res *oidcResult) (*store.User, error) {
	ctx := r.Context()
	if err := checkAllowed(p, res); err != nil {
		return nil, err
	}
	u, err := s.store.UserByOIDCSubject(ctx, p.ID, res.Subject)
	if err == nil {
		if res.EmailVerified {
			s.store.TouchOIDCEmail(ctx, p.ID, res.Subject, res.Email)
		}
		return u, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if res.Email == "" || !res.EmailVerified {
		return nil, fmt.Errorf("%s did not report a verified email address; sign in with your password and link %s under Settings → Security", p.Name, p.Name)
	}
	existing, err := s.store.UserByEmail(ctx, res.Email)
	switch {
	case err == nil:
		explicit := fmt.Errorf("an account with %s already exists: sign in with your password and link %s under Settings → Security", res.Email, p.Name)
		if !inDomains(res.Email, p.TrustedDomains) || existing.IsAdmin {
			return nil, explicit
		}
		if err := s.store.LinkOIDC(ctx, p.ID, res.Subject, existing.ID, res.Email); err != nil {
			return nil, err
		}
		slog.Info("oidc identity linked by trusted domain", "provider", p.Slug, "email", res.Email)
		return existing, nil
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}
	if !p.AutoProvision {
		return nil, fmt.Errorf("there is no account for %s; ask an administrator to create one", res.Email)
	}
	release := s.store.AdvisoryLock(ctx, "register")
	defer release()
	nu, err := s.createUser(r, credentialsInput{Email: res.Email, Name: strings.TrimSpace(res.Name), Password: util.RandomToken(24)}, false)
	if err != nil {
		if store.IsUniqueViolation(err) {
			return nil, errors.New("this account was just created; please sign in again")
		}
		return nil, err
	}
	if p.DefaultTeamID != nil {
		if err := s.store.AddMember(ctx, *p.DefaultTeamID, nu.ID, firstNonEmpty(p.DefaultRole, "developer")); err != nil {
			slog.Warn("oidc default team", "provider", p.Slug, "err", err)
		}
	}
	if err := s.store.LinkOIDC(ctx, p.ID, res.Subject, nu.ID, res.Email); err != nil {
		return nil, err
	}
	s.store.Audit(ctx, &nu.ID, nil, "user.create.oidc", p.Slug+" "+res.Email)
	slog.Info("account created with oidc", "provider", p.Slug, "email", res.Email)
	return nu, nil
}

// linkOIDC attaches an identity to the signed-in account that started the flow.
// The user proved control of both the account (session) and the identity (IdP
// sign-in), so the emails need not match.
func (s *Server) linkOIDC(ctx context.Context, p *store.OIDCProvider, res *oidcResult, userID string) error {
	if err := checkAllowed(p, res); err != nil {
		return err
	}
	if other, err := s.store.UserByOIDCSubject(ctx, p.ID, res.Subject); err == nil && other.ID != userID {
		return fmt.Errorf("this %s account is already linked to another user", p.Name)
	}
	return s.store.LinkOIDC(ctx, p.ID, res.Subject, userID, res.Email)
}

// applyGroupRoles sets the team roles the provider's groups map to. The
// highest mapped role per team wins; team owners are never changed, and
// memberships are not removed when a group disappears.
func (s *Server) applyGroupRoles(ctx context.Context, p *store.OIDCProvider, u *store.User, groups []string) {
	want := map[string]string{}
	for _, m := range p.GroupRoles {
		if !slices.Contains(groups, m.Group) {
			continue
		}
		if cur, ok := want[m.TeamID]; !ok || roleRank[m.Role] > roleRank[cur] {
			want[m.TeamID] = m.Role
		}
	}
	for team, role := range want {
		cur, err := s.store.MemberRole(ctx, team, u.ID)
		if err == nil && (cur == "owner" || cur == role) {
			continue
		}
		if err := s.store.AddMember(ctx, team, u.ID, role); err != nil {
			slog.Warn("oidc group role", "provider", p.Slug, "team", team, "err", err)
		}
	}
}

// ssoEnforced returns the provider that must be used instead of a password for
// this email, or nil. The instance owner is never enforced (break-glass).
func (s *Server) ssoEnforced(ctx context.Context, email string) *store.OIDCProvider {
	email = strings.ToLower(strings.TrimSpace(email))
	ps, err := s.store.OIDCProviders(ctx)
	if err != nil {
		return nil
	}
	for i := range ps {
		p := &ps[i]
		if !p.Enabled || !inDomains(email, p.EnforceDomains) {
			continue
		}
		if id, err := s.store.InstanceOwnerID(ctx); err == nil {
			if owner, err := s.store.UserByID(ctx, id); err == nil && owner.Email == email {
				return nil
			}
		}
		return p
	}
	return nil
}

func ssoEnforcedMessage(p *store.OIDCProvider, email string) string {
	return fmt.Sprintf("password sign-in is disabled for %s addresses: use Sign in with %s", emailDomain(email), p.Name)
}

// GET /api/v1/me/oidc lists the providers linked to the signed-in account.
func (s *Server) handleMyOIDC(w http.ResponseWriter, r *http.Request) {
	ids, err := s.store.OIDCIdentitiesForUser(r.Context(), currentUser(r).ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, ids)
}

// DELETE /api/v1/me/oidc/{provider}
func (s *Server) handleOIDCUnlink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := currentUser(r)
	p, err := s.store.OIDCProviderBySlug(ctx, r.PathValue("provider"))
	if err != nil {
		writeError(w, 404, "unknown provider")
		return
	}
	if s.ssoEnforced(ctx, u.Email) != nil {
		ids, _ := s.store.OIDCIdentitiesForUser(ctx, u.ID)
		if len(ids) <= 1 {
			writeError(w, 400, "password sign-in is disabled for your domain: this is your only way to sign in")
			return
		}
	}
	if err := s.store.UnlinkOIDC(ctx, p.ID, u.ID); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	s.store.Audit(ctx, &u.ID, nil, "oidc.unlink", p.Slug)
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// ssoButtons are the enabled providers, for the sign-in page.
func (s *Server) ssoButtons(ctx context.Context) []map[string]string {
	out := []map[string]string{}
	ps, err := s.store.OIDCProviders(ctx)
	if err != nil {
		return out
	}
	for _, p := range ps {
		if p.Enabled {
			out = append(out, map[string]string{"slug": p.Slug, "name": p.Name})
		}
	}
	return out
}

// Administration.

type oidcProviderInput struct {
	Slug           *string                `json:"slug"`
	Name           *string                `json:"name"`
	Issuer         *string                `json:"issuer"`
	ClientID       *string                `json:"client_id"`
	ClientSecret   *string                `json:"client_secret"` // write-only; omitted or "" keeps the current one on update
	Scopes         *[]string              `json:"scopes"`
	Enabled        *bool                  `json:"enabled"`
	TrustedDomains *[]string              `json:"trusted_domains"`
	AllowedDomains *[]string              `json:"allowed_domains"`
	EnforceDomains *[]string              `json:"enforce_domains"`
	AutoProvision  *bool                  `json:"auto_provision"`
	DefaultTeamID  *string                `json:"default_team_id"` // "" clears it
	DefaultRole    *string                `json:"default_role"`
	GroupsClaim    *string                `json:"groups_claim"`
	GroupRoles     *[]store.OIDCGroupRole `json:"group_roles"`
	SkipTOTP       *bool                  `json:"skip_totp"`
}

var (
	oidcSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	domainRe   = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9-]{2,63}$`)
)

func cleanDomains(in []string) ([]string, error) {
	out := []string{}
	for _, d := range in {
		d = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(d)), "@")
		if d == "" {
			continue
		}
		if !domainRe.MatchString(d) {
			return nil, fmt.Errorf("invalid domain %q", d)
		}
		if !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out, nil
}

// apply merges input into p and validates the result.
func (s *Server) applyOIDCInput(ctx context.Context, p *store.OIDCProvider, in *oidcProviderInput) error {
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = strings.TrimSpace(*v)
		}
	}
	set(&p.Name, in.Name)
	set(&p.Slug, in.Slug)
	set(&p.Issuer, in.Issuer)
	set(&p.ClientID, in.ClientID)
	set(&p.DefaultRole, in.DefaultRole)
	set(&p.GroupsClaim, in.GroupsClaim)
	if p.Name == "" {
		return errors.New("name is required")
	}
	if p.Slug == "" {
		p.Slug = util.Slugify(p.Name)
	}
	if !oidcSlugRe.MatchString(p.Slug) {
		return errors.New("slug: lowercase letters, digits and dashes")
	}
	if u, err := url.Parse(p.Issuer); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("issuer must be an http(s) URL, e.g. https://accounts.google.com")
	}
	if p.ClientID == "" {
		return errors.New("client_id is required")
	}
	if in.ClientSecret != nil && *in.ClientSecret != "" {
		enc, err := s.box.Encrypt(*in.ClientSecret)
		if err != nil {
			return err
		}
		p.ClientSecretEnc = enc
	}
	if in.Scopes != nil {
		p.Scopes = nil
		for _, sc := range *in.Scopes {
			if sc = strings.TrimSpace(sc); sc != "" && !slices.Contains(p.Scopes, sc) {
				p.Scopes = append(p.Scopes, sc)
			}
		}
	}
	if len(p.Scopes) == 0 {
		p.Scopes = []string{"openid", "email", "profile"}
	}
	if !slices.Contains(p.Scopes, oidc.ScopeOpenID) {
		p.Scopes = append([]string{oidc.ScopeOpenID}, p.Scopes...)
	}
	var err error
	for _, d := range []struct {
		dst *[]string
		in  *[]string
	}{{&p.TrustedDomains, in.TrustedDomains}, {&p.AllowedDomains, in.AllowedDomains}, {&p.EnforceDomains, in.EnforceDomains}} {
		if d.in != nil {
			if *d.dst, err = cleanDomains(*d.in); err != nil {
				return err
			}
		}
		if *d.dst == nil {
			*d.dst = []string{}
		}
	}
	if in.Enabled != nil {
		p.Enabled = *in.Enabled
	}
	if in.AutoProvision != nil {
		p.AutoProvision = *in.AutoProvision
	}
	if in.SkipTOTP != nil {
		p.SkipTOTP = *in.SkipTOTP
	}
	if in.DefaultTeamID != nil {
		p.DefaultTeamID = nil
		if *in.DefaultTeamID != "" {
			p.DefaultTeamID = in.DefaultTeamID
		}
	}
	if p.DefaultRole == "" {
		p.DefaultRole = "developer"
	}
	if p.GroupsClaim == "" {
		p.GroupsClaim = "groups"
	}
	if in.GroupRoles != nil {
		p.GroupRoles = *in.GroupRoles
	}
	assignable := func(role string) bool { _, ok := roleRank[role]; return ok && role != "owner" }
	if !assignable(p.DefaultRole) {
		return errors.New("default_role must be admin, developer or viewer")
	}
	teamOK := func(id string) bool {
		var ok bool
		_ = s.store.Q(ctx).QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM teams WHERE id::text=$1 AND NOT is_system)`, id).Scan(&ok)
		return ok
	}
	if p.DefaultTeamID != nil && !teamOK(*p.DefaultTeamID) {
		return errors.New("default_team_id: team not found")
	}
	for i, m := range p.GroupRoles {
		p.GroupRoles[i].Group = strings.TrimSpace(m.Group)
		if p.GroupRoles[i].Group == "" || !assignable(m.Role) || !teamOK(m.TeamID) {
			return fmt.Errorf("group_roles[%d]: needs a group, an existing team_id and a role (admin, developer or viewer)", i)
		}
	}
	return nil
}

func (s *Server) oidcAdminList(ctx context.Context, r *http.Request) (map[string]any, error) {
	ps, err := s.store.OIDCProviders(ctx)
	if err != nil {
		return nil, err
	}
	type out struct {
		store.OIDCProvider
		HasSecret bool `json:"has_secret"`
	}
	list := []out{}
	for _, p := range ps {
		list = append(list, out{OIDCProvider: p, HasSecret: p.ClientSecretEnc != ""})
	}
	return map[string]any{"providers": list, "redirect_uri": s.baseURL(r) + oidcCallback}, nil
}

// GET /api/v1/admin/oidc
func (s *Server) handleListOIDCProviders(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	out, err := s.oidcAdminList(r.Context(), r)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, out)
}

// POST /api/v1/admin/oidc
func (s *Server) handleCreateOIDCProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in oidcProviderInput
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	p := &store.OIDCProvider{Enabled: true, SkipTOTP: true}
	s.saveOIDCProvider(w, r, p, &in, 201)
}

// PATCH /api/v1/admin/oidc/{id}
func (s *Server) handleUpdateOIDCProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	p, err := s.store.OIDCProviderByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "provider not found")
		return
	}
	var in oidcProviderInput
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	s.saveOIDCProvider(w, r, p, &in, 200)
}

func (s *Server) saveOIDCProvider(w http.ResponseWriter, r *http.Request, p *store.OIDCProvider, in *oidcProviderInput, status int) {
	ctx := r.Context()
	if err := s.applyOIDCInput(ctx, p, in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	// Check discovery now rather than at the first sign-in.
	if _, err := oidc.NewProvider(oidcContext(ctx), p.Issuer); err != nil {
		writeError(w, 400, fmt.Sprintf("discovery failed (%s/.well-known/openid-configuration): %v", strings.TrimSuffix(p.Issuer, "/"), err))
		return
	}
	saved, err := s.store.SaveOIDCProvider(ctx, p)
	if err != nil {
		if store.IsUniqueViolation(err) {
			writeError(w, 409, "a provider with this slug already exists")
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	s.oidc.forget(saved.ID)
	u := currentUser(r)
	s.store.Audit(ctx, &u.ID, nil, "oidc.provider.save", saved.Slug)
	writeJSON(w, status, map[string]any{"provider": saved, "has_secret": saved.ClientSecretEnc != ""})
}

// DELETE /api/v1/admin/oidc/{id}
func (s *Server) handleDeleteOIDCProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	p, err := s.store.OIDCProviderByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "provider not found")
		return
	}
	if err := s.store.DeleteOIDCProvider(r.Context(), p.ID); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	s.oidc.forget(p.ID)
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, nil, "oidc.provider.delete", p.Slug)
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}
