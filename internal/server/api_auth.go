package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/totp"
	"github.com/thesunbg/paas/internal/util"
)

const sessionTTL = 30 * 24 * time.Hour

var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("not-a-real-password"), bcrypt.DefaultCost)

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	n, err := s.store.CountUsers(r.Context())
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	signup, _ := s.store.GetSetting(r.Context(), settingAllowSignup)
	_, ghErr := s.githubApp(r.Context())
	sso := []map[string]string{}
	if n > 0 {
		sso = s.ssoButtons(r.Context())
	}
	writeJSON(w, 200, map[string]any{"needs_setup": n == 0, "allow_signup": signup == "true", "base_domain": s.cfg.BaseDomain, "github_login": ghErr == nil && n > 0,
		"sso": sso})
}

type credentialsInput struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
	Code     string `json:"code"` // two-factor code
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var in credentialsInput
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	in.Email = strings.TrimSpace(strings.ToLower(in.Email))
	if _, err := mail.ParseAddress(in.Email); err != nil {
		writeError(w, 400, "invalid email")
		return
	}
	if len(in.Password) < 8 {
		writeError(w, 400, "password must be at least 8 characters")
		return
	}
	release := s.store.AdvisoryLock(r.Context(), "register")
	defer release()
	n, err := s.store.CountUsers(r.Context())
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	first := n == 0
	if !first {
		signup, _ := s.store.GetSetting(r.Context(), settingAllowSignup)
		if signup != "true" {
			writeError(w, 403, "registration is closed; ask an administrator to invite you")
			return
		}
		if p := s.ssoEnforced(r.Context(), in.Email); p != nil {
			writeError(w, 403, fmt.Sprintf("accounts for %s addresses are created by signing in with %s", emailDomain(in.Email), p.Name))
			return
		}
		if max := s.teamLimits(r.Context()).MaxPerIPPerDay; max > 0 && !signupGuard.take(clientAddr(r), max) {
			writeError(w, 429, "too many accounts created from this address today")
			return
		}
	}
	u, err := s.createUser(r, in, first)
	if err != nil {
		if store.IsUniqueViolation(err) {
			writeError(w, 409, "email already registered")
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	s.startSession(w, r, u)
}

func (s *Server) createUser(r *http.Request, in credentialsInput, admin bool) (*store.User, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	name := in.Name
	if name == "" {
		name = strings.SplitN(in.Email, "@", 2)[0]
	}
	u, err := s.store.CreateUser(r.Context(), in.Email, name, string(hash), admin)
	if err != nil {
		return nil, err
	}
	slug := util.Slugify(name) + "-" + util.RandomToken(2)
	if _, err := s.store.CreateTeam(r.Context(), name+"'s team", slug, u.ID, false); err != nil {
		return nil, err
	}
	return u, nil
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in credentialsInput
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	email, ip := strings.ToLower(strings.TrimSpace(in.Email)), clientAddr(r)
	pairKey, emailKey := email+"|"+ip, "email:"+email
	if !loginGuard.allowed(pairKey, maxPerEmailIP) || !loginGuard.allowed(ip, maxPerIP) || !loginGuard.allowed(emailKey, maxPerEmail) {
		writeError(w, 429, "too many failed attempts; try again in 15 minutes")
		return
	}
	failed := func() { loginGuard.fail(pairKey); loginGuard.fail(ip); loginGuard.fail(emailKey) }
	// Enforced SSO is decided by the email's domain alone, before looking the
	// account up, so the answer does not reveal whether an account exists.
	if p := s.ssoEnforced(r.Context(), email); p != nil {
		writeJSON(w, 403, map[string]any{"error": ssoEnforcedMessage(p, email), "sso": p.Slug})
		return
	}
	u, err := s.store.UserByEmail(r.Context(), strings.TrimSpace(in.Email))
	if err != nil {
		// Spend comparable time to avoid user enumeration.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(in.Password))
		failed()
		writeError(w, 401, "invalid email or password")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(in.Password)) != nil {
		failed()
		writeError(w, 401, "invalid email or password")
		return
	}
	if u.TOTPEnabled {
		secret, err := s.box.Decrypt(u.TOTPSecret)
		if err != nil {
			writeError(w, 500, "cannot read two-factor secret")
			return
		}
		if in.Code == "" {
			writeJSON(w, 401, map[string]any{"error": "two-factor code required", "two_factor": true})
			return
		}
		if !s.verifyTOTP(r.Context(), u.ID, secret, in.Code) {
			failed()
			writeJSON(w, 401, map[string]any{"error": "invalid two-factor code", "two_factor": true})
			return
		}
	}
	s.startSession(w, r, u)
}

// verifyTOTP checks a code and burns its time step so it cannot be replayed.
func (s *Server) verifyTOTP(ctx context.Context, userID, secret, code string) bool {
	step, ok := totp.Match(secret, code, time.Now())
	if !ok {
		return false
	}
	used, err := s.store.ConsumeTOTPStep(ctx, userID, step)
	return err == nil && used
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u *store.User) {
	tok, err := s.newSession(w, r, u)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"user": u, "token": tok})
}

// newSession creates a session and sets its cookie.
func (s *Server) newSession(w http.ResponseWriter, r *http.Request, u *store.User) (string, error) {
	tok := util.RandomToken(32)
	if err := s.store.CreateSession(r.Context(), util.HashToken(tok), u.ID, time.Now().Add(sessionTTL)); err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https", MaxAge: int(sessionTTL.Seconds()),
	})
	return tok, nil
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.store.DeleteSession(r.Context(), util.HashToken(c.Value))
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		_ = s.store.DeleteSession(r.Context(), util.HashToken(strings.TrimPrefix(h, "Bearer ")))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	teams, err := s.store.TeamsForUser(r.Context(), u.ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"user": u, "teams": teams})
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	u := currentUser(r)
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(in.Current)) != nil {
		writeError(w, 400, "current password is wrong")
		return
	}
	if len(in.New) < 8 {
		writeError(w, 400, "password must be at least 8 characters")
		return
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte(in.New), bcrypt.DefaultCost)
	if err := s.store.UpdatePassword(r.Context(), u.ID, string(hash)); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if err := s.store.RevokeCredentials(r.Context(), u.ID, currentSessionHash(r)); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	toks, err := s.store.ListAPITokens(r.Context(), currentUser(r).ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, toks)
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil || strings.TrimSpace(in.Name) == "" {
		writeError(w, 400, "name required")
		return
	}
	tok := "paas_" + util.RandomToken(24)
	t, err := s.store.CreateAPIToken(r.Context(), currentUser(r).ID, in.Name, util.HashToken(tok))
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"token": tok, "info": t})
}

func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteAPIToken(r.Context(), currentUser(r).ID, r.PathValue("id")); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// Teams.

func (s *Server) handleListTeams(w http.ResponseWriter, r *http.Request) {
	teams, err := s.store.TeamsForUser(r.Context(), currentUser(r).ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, teams)
}

func (s *Server) handleCreateTeam(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil || strings.TrimSpace(in.Name) == "" {
		writeError(w, 400, "name required")
		return
	}
	if l := s.teamLimits(r.Context()); limited(r) && l.MaxTeams > 0 {
		var n int
		err := s.store.Q(r.Context()).QueryRow(r.Context(), `SELECT count(*) FROM memberships WHERE user_id=$1 AND role='owner'`, currentUser(r).ID).Scan(&n)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		if n >= l.MaxTeams {
			writeError(w, 403, fmt.Sprintf("you can own at most %d teams; ask an administrator for more", l.MaxTeams))
			return
		}
	}
	t, err := s.store.CreateTeam(r.Context(), in.Name, util.Slugify(in.Name)+"-"+util.RandomToken(2), currentUser(r).ID, false)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, t)
}

func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")
	if !s.requireTeam(w, r, team, "viewer") {
		return
	}
	m, err := s.store.TeamMembers(r.Context(), team)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, m)
}

// handleAddMember adds an existing user, or creates an account with a temporary password.
func (s *Server) handleAddMember(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")
	if !s.requireTeam(w, r, team, "admin") {
		return
	}
	var in struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if _, ok := roleRank[in.Role]; !ok || in.Role == "owner" {
		writeError(w, 400, "role must be admin, developer or viewer")
		return
	}
	resp := map[string]any{}
	if existing, err := s.store.UserByEmail(r.Context(), in.Email); err == nil {
		if role, err := s.store.MemberRole(r.Context(), team, existing.ID); err == nil && role == "owner" {
			writeError(w, 403, "the team owner's role cannot be changed")
			return
		}
	}
	u, err := s.store.UserByEmail(r.Context(), in.Email)
	if errors.Is(err, store.ErrNotFound) {
		if _, perr := mail.ParseAddress(in.Email); perr != nil {
			writeError(w, 400, "invalid email")
			return
		}
		// Without this, AddMember would sidestep the registration switch and per-IP cap.
		if !currentUser(r).IsAdmin {
			if signup, _ := s.store.GetSetting(r.Context(), settingAllowSignup); signup != "true" {
				writeError(w, 403, "registration is closed; ask an administrator to create the account")
				return
			}
			if max := s.teamLimits(r.Context()).MaxPerIPPerDay; max > 0 && !signupGuard.take(clientAddr(r), max) {
				writeError(w, 429, "too many accounts created from this address today")
				return
			}
		}
		pass := util.RandomPassword(16)
		u, err = s.createUser(r, credentialsInput{Email: in.Email, Password: pass}, false)
		resp["temporary_password"] = pass
	}
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if err := s.store.AddMember(r.Context(), team, u.ID, in.Role); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	// Only echo what the caller supplied: an existing account's profile is not theirs to read.
	resp["email"], resp["role"] = u.Email, in.Role
	writeJSON(w, 201, resp)
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")
	if !s.requireTeam(w, r, team, "admin") {
		return
	}
	if r.PathValue("user") == currentUser(r).ID {
		writeError(w, 400, "you cannot remove yourself")
		return
	}
	if role, err := s.store.MemberRole(r.Context(), team, r.PathValue("user")); err == nil && role == "owner" {
		writeError(w, 403, "the team owner cannot be removed")
		return
	}
	if err := s.store.RemoveMember(r.Context(), team, r.PathValue("user")); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "removed"})
}

// handle2FASetup creates a new (not yet enabled) TOTP secret.
func (s *Server) handle2FASetup(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.TOTPEnabled {
		writeError(w, 400, "two-factor authentication is already enabled")
		return
	}
	secret := totp.NewSecret()
	if err := s.store.SetTOTP(r.Context(), u.ID, s.box.MustEncrypt(secret), false); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"secret": secret, "otpauth_url": totp.URI("paas", u.Email, secret)})
}

func (s *Server) handle2FAEnable(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code string `json:"code"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	u := currentUser(r)
	secret, err := s.box.Decrypt(u.TOTPSecret)
	if err != nil || secret == "" {
		writeError(w, 400, "call /me/2fa/setup first")
		return
	}
	key := "2fa:" + u.ID
	if !loginGuard.allowed(key, maxPer2FA) {
		writeError(w, 429, "too many failed attempts; try again in 15 minutes")
		return
	}
	if !s.verifyTOTP(r.Context(), u.ID, secret, in.Code) {
		loginGuard.fail(key)
		writeError(w, 400, "invalid code")
		return
	}
	if err := s.store.SetTOTP(r.Context(), u.ID, u.TOTPSecret, true); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "enabled"})
}

func (s *Server) handle2FADisable(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code     string `json:"code"`
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	u := currentUser(r)
	if !u.TOTPEnabled {
		writeError(w, 400, "two-factor authentication is not enabled")
		return
	}
	key := "2fa:" + u.ID
	if !loginGuard.allowed(key, maxPer2FA) {
		writeError(w, 429, "too many failed attempts; try again in 15 minutes")
		return
	}
	secret, _ := s.box.Decrypt(u.TOTPSecret)
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(in.Password)) != nil || !s.verifyTOTP(r.Context(), u.ID, secret, in.Code) {
		loginGuard.fail(key)
		writeError(w, 400, "password or code is wrong")
		return
	}
	if err := s.store.SetTOTP(r.Context(), u.ID, "", false); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if err := s.store.RevokeCredentials(r.Context(), u.ID, currentSessionHash(r)); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "disabled"})
}

// attemptGuard locks out a key after repeated failures. Login counts three keys
// so one attacker cannot lock a victim out: email+IP protects a single account
// from a single address, IP stops spraying passwords across accounts yet
// tolerates a NAT (hence the higher cap), and email alone is a ceiling against
// a distributed attack.
// In-memory per control plane; remove when the counters move to a table.
type attemptGuard struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

var loginGuard = &attemptGuard{failures: map[string][]time.Time{}}

const (
	maxPerEmailIP = 10
	maxPerIP      = 50
	maxPerEmail   = 100
	maxPer2FA     = 10
	failureWindow = 15 * time.Minute
)

func (g *attemptGuard) recent(key string) []time.Time {
	var keep []time.Time
	for _, t := range g.failures[key] {
		if time.Since(t) < failureWindow {
			keep = append(keep, t)
		}
	}
	g.failures[key] = keep
	return keep
}

func (g *attemptGuard) allowed(key string, max int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.recent(key)) < max
}

func (g *attemptGuard) fail(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failures[key] = append(g.recent(key), time.Now())
	if len(g.failures) > 100000 {
		g.failures = map[string][]time.Time{}
	}
}

// clientAddr is the caller's IP. Behind the local edge proxy (a loopback peer)
// the last X-Forwarded-For entry, which that proxy appends, is the real client.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if last := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(last) != nil {
				return last
			}
		}
	}
	return host
}

// handleDeleteUser removes an account (administrators only, not themselves).
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == currentUser(r).ID {
		writeError(w, 400, "you cannot delete your own account")
		return
	}
	if _, err := s.store.UserByID(r.Context(), id); err != nil {
		writeError(w, 404, "user not found")
		return
	}
	if _, err := s.store.Q(r.Context()).Exec(r.Context(), `DELETE FROM users WHERE id=$1`, id); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// signupGuard limits account creation per client address per day.
var signupGuard = &dailyCounter{hits: map[string][]time.Time{}}

type dailyCounter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func (c *dailyCounter) take(key string, max int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	var keep []time.Time
	for _, t := range c.hits[key] {
		if time.Since(t) < 24*time.Hour {
			keep = append(keep, t)
		}
	}
	if len(keep) >= max {
		c.hits[key] = keep
		return false
	}
	c.hits[key] = append(keep, time.Now())
	if len(c.hits) > 100000 {
		c.hits = map[string][]time.Time{}
	}
	return true
}
