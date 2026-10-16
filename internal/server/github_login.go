package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/totp"
	"github.com/thesunbg/paas/internal/util"
)

// "Sign in with GitHub" uses the user authorization of the GitHub App created
// in Settings → GitHub, so no separate OAuth app is needed.

// GET /api/v1/auth/github starts the sign-in.
func (s *Server) handleGitHubLoginStart(w http.ResponseWriter, r *http.Request) {
	c, err := s.githubApp(r.Context())
	if err != nil {
		http.Redirect(w, r, withQuery("/#/login", "github_error", err.Error()), http.StatusFound)
		return
	}
	st, err := s.signState(r.Context(), ghState{Kind: "login"})
	if err != nil {
		http.Redirect(w, r, withQuery("/#/login", "github_error", err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, c.AuthorizeURL(st), http.StatusFound)
}

// GET /api/v1/me/github/link connects GitHub to the signed-in account.
func (s *Server) handleGitHubLinkStart(w http.ResponseWriter, r *http.Request) {
	c, err := s.githubApp(r.Context())
	if err != nil {
		http.Redirect(w, r, withQuery("/#/settings/security", "github_error", err.Error()), http.StatusFound)
		return
	}
	st, err := s.signState(r.Context(), ghState{Kind: "link", UserID: currentUser(r).ID})
	if err != nil {
		http.Redirect(w, r, withQuery("/#/settings/security", "github_error", err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, c.AuthorizeURL(st), http.StatusFound)
}

// DELETE /api/v1/me/github
func (s *Server) handleGitHubUnlink(w http.ResponseWriter, r *http.Request) {
	if err := s.store.LinkGitHub(r.Context(), currentUser(r).ID, 0, ""); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) finishGitHubLink(w http.ResponseWriter, r *http.Request, st *ghState) {
	ctx := r.Context()
	fail := func(msg string) {
		http.Redirect(w, r, withQuery("/#/settings/security", "github_error", msg), http.StatusFound)
	}
	gu, err := s.githubUserFromCode(r)
	if err != nil {
		fail(err.Error())
		return
	}
	if other, err := s.store.UserByGitHubID(ctx, gu.ID); err == nil && other.ID != st.UserID {
		fail("this GitHub account is already connected to another user")
		return
	}
	if err := s.store.LinkGitHub(ctx, st.UserID, gu.ID, gu.Login); err != nil {
		fail(err.Error())
		return
	}
	http.Redirect(w, r, "/#/settings/security?github=linked", http.StatusFound)
}

func (s *Server) finishGitHubLogin(w http.ResponseWriter, r *http.Request, _ *ghState) {
	ctx := r.Context()
	fail := func(msg string) {
		http.Redirect(w, r, withQuery("/#/login", "github_error", msg), http.StatusFound)
	}
	gu, err := s.githubUserFromCode(r)
	if err != nil {
		fail(err.Error())
		return
	}
	u, err := s.store.UserByGitHubID(ctx, gu.ID)
	if errors.Is(err, store.ErrNotFound) && gu.Email != "" {
		// GitHub only exposes verified addresses here, so the account can be linked by email.
		if byEmail, err2 := s.store.UserByEmail(ctx, gu.Email); err2 == nil {
			if err := s.store.LinkGitHub(ctx, byEmail.ID, gu.ID, gu.Login); err != nil {
				fail(err.Error())
				return
			}
			u, err = byEmail, nil
		}
	}
	if errors.Is(err, store.ErrNotFound) {
		u, err = s.signupWithGitHub(r, gu.ID, gu.Login, gu.Name, gu.Email)
		if err != nil {
			fail(err.Error())
			return
		}
	}
	if err != nil {
		fail(err.Error())
		return
	}
	if u.TOTPEnabled {
		pending, err := s.signState(ctx, ghState{Kind: "2fa", UserID: u.ID})
		if err != nil {
			fail(err.Error())
			return
		}
		http.Redirect(w, r, withQuery("/#/login", "github_2fa", pending), http.StatusFound)
		return
	}
	if _, err := s.newSession(w, r, u); err != nil {
		fail(err.Error())
		return
	}
	s.store.Audit(ctx, &u.ID, nil, "login.github", gu.Login)
	http.Redirect(w, r, "/#/", http.StatusFound)
}

type githubIdentity struct {
	ID                 int64
	Login, Name, Email string
}

func (s *Server) githubUserFromCode(r *http.Request) (*githubIdentity, error) {
	code := r.URL.Query().Get("code")
	if code == "" {
		return nil, errors.New(firstNonEmpty(r.URL.Query().Get("error_description"), "GitHub did not return an authorization code"))
	}
	c, err := s.githubApp(r.Context())
	if err != nil {
		return nil, err
	}
	tok, err := c.UserToken(r.Context(), code)
	if err != nil {
		return nil, err
	}
	gu, err := c.CurrentUser(r.Context(), tok)
	if err != nil {
		return nil, err
	}
	return &githubIdentity{ID: gu.ID, Login: gu.Login, Name: gu.Name, Email: strings.ToLower(gu.Email)}, nil
}

// signupWithGitHub creates an account for a new GitHub user when sign-up is open.
func (s *Server) signupWithGitHub(r *http.Request, id int64, login, name, email string) (*store.User, error) {
	ctx := r.Context()
	if v, _ := s.store.GetSetting(ctx, settingAllowSignup); v != "true" {
		return nil, errors.New("no account is connected to this GitHub user and sign-up is closed; sign in with your password and connect GitHub in Settings → Security")
	}
	if max := s.teamLimits(ctx).MaxPerIPPerDay; max > 0 && !signupGuard.take(clientAddr(r), max) {
		return nil, errors.New("too many accounts created from this address today")
	}
	if email == "" {
		// No visible verified email: use GitHub's private relay address.
		email = strings.ToLower(login) + "@users.noreply.github.com"
	}
	release := s.store.AdvisoryLock(ctx, "register")
	defer release()
	u, err := s.createUser(r, credentialsInput{Email: email, Name: firstNonEmpty(name, login), Password: util.RandomToken(24)}, false)
	if err != nil {
		if store.IsUniqueViolation(err) {
			return nil, errors.New("an account with this email already exists; sign in with your password and connect GitHub in Settings")
		}
		return nil, err
	}
	if err := s.store.LinkGitHub(ctx, u.ID, id, login); err != nil {
		return nil, err
	}
	slog.Info("account created with github", "login", login)
	return s.store.UserByID(ctx, u.ID)
}

// POST /api/v1/auth/github/2fa {pending, code} completes a GitHub sign-in with 2FA.
func (s *Server) handleGitHub2FA(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Pending string `json:"pending"`
		Code    string `json:"code"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	st, err := s.verifyState(r.Context(), in.Pending, "2fa")
	if err != nil {
		writeError(w, 401, "the sign-in expired, please try again")
		return
	}
	key := "2fa:" + st.UserID
	if !loginGuard.allowed(key) || !loginGuard.allowed(clientAddr(r)) {
		writeError(w, 429, "too many failed attempts; try again in 15 minutes")
		return
	}
	u, err := s.store.UserByID(r.Context(), st.UserID)
	if err != nil {
		writeError(w, 401, "unknown user")
		return
	}
	secret, err := s.box.Decrypt(u.TOTPSecret)
	if err != nil || !u.TOTPEnabled || !totp.Verify(secret, strings.TrimSpace(in.Code), time.Now()) {
		loginGuard.fail(key)
		loginGuard.fail(clientAddr(r))
		writeJSON(w, 401, map[string]any{"error": "invalid two-factor code", "two_factor": true})
		return
	}
	s.startSession(w, r, u)
}
