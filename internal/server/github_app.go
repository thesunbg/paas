package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/github"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

// GitHub App integration: the administrator creates a private GitHub App from a
// manifest in one click, teams install it on their accounts and pick repositories
// from a list. Clones, commit statuses, PR comments and webhooks then use short-lived
// installation tokens, so nobody has to paste personal tokens or webhook URLs.

const (
	settingGitHubApp      = "github_app"
	settingGitHubStateKey = "github_state_key"
)

var errNoGitHubApp = errors.New("the GitHub App is not configured (Settings → GitHub)")

// githubApp returns the cached app client, loading it from the settings table.
func (s *Server) githubApp(ctx context.Context) (*github.Client, error) {
	s.ghMu.Lock()
	defer s.ghMu.Unlock()
	if s.gh != nil {
		return s.gh, nil
	}
	enc, err := s.store.GetSetting(ctx, settingGitHubApp)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errNoGitHubApp
	}
	if err != nil {
		return nil, err
	}
	plain, err := s.box.Decrypt(enc)
	if err != nil {
		return nil, err
	}
	var app github.App
	if err := json.Unmarshal([]byte(plain), &app); err != nil {
		return nil, err
	}
	c, err := github.NewClient(&app)
	if err != nil {
		return nil, err
	}
	s.gh = c
	return c, nil
}

// OAuth-style state carried through GitHub redirects, signed with a server key.
type ghState struct {
	Kind   string `json:"k"` // manifest | install
	UserID string `json:"u"`
	TeamID string `json:"t,omitempty"`
	Return string `json:"r,omitempty"`
	Exp    int64  `json:"e"`
}

func (s *Server) ghStateKey(ctx context.Context) ([]byte, error) {
	enc, err := s.store.GetSetting(ctx, settingGitHubStateKey)
	if errors.Is(err, store.ErrNotFound) {
		key := util.RandomToken(32)
		if err := s.store.SetSetting(ctx, settingGitHubStateKey, s.box.MustEncrypt(key)); err != nil {
			return nil, err
		}
		enc, err = s.store.GetSetting(ctx, settingGitHubStateKey)
	}
	if err != nil {
		return nil, err
	}
	key, err := s.box.Decrypt(enc)
	return []byte(key), err
}

func (s *Server) signState(ctx context.Context, st ghState) (string, error) {
	key, err := s.ghStateKey(ctx)
	if err != nil {
		return "", err
	}
	st.Exp = time.Now().Add(30 * time.Minute).Unix()
	b, _ := json.Marshal(st)
	p := base64.RawURLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(p))
	return p + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *Server) verifyState(ctx context.Context, raw, kind string) (*ghState, error) {
	p, sig, ok := strings.Cut(raw, ".")
	if !ok {
		return nil, errors.New("invalid state")
	}
	key, err := s.ghStateKey(ctx)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(p))
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, mac.Sum(nil)) {
		return nil, errors.New("invalid state")
	}
	b, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return nil, errors.New("invalid state")
	}
	var st ghState
	if err := json.Unmarshal(b, &st); err != nil || st.Kind != kind {
		return nil, errors.New("invalid state")
	}
	if time.Now().Unix() > st.Exp {
		return nil, errors.New("this link expired, please try again")
	}
	return &st, nil
}

// safeReturn only allows redirects back into the dashboard.
func safeReturn(r string) string {
	if strings.HasPrefix(r, "/#/") && !strings.ContainsAny(r, "\r\n\\") {
		return r
	}
	return "/#/settings/github"
}

func withQuery(path, key, value string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + key + "=" + url.QueryEscape(value)
}

func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// GET /api/v1/github/app
func (s *Server) handleGitHubAppStatus(w http.ResponseWriter, r *http.Request) {
	c, err := s.githubApp(r.Context())
	if errors.Is(err, errNoGitHubApp) {
		writeJSON(w, 200, map[string]any{"configured": false, "can_configure": currentUser(r).IsAdmin, "base_url": s.baseURL(r)})
		return
	}
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	app := c.App()
	writeJSON(w, 200, map[string]any{
		"configured": true, "can_configure": currentUser(r).IsAdmin, "base_url": s.baseURL(r),
		"id": app.ID, "slug": app.Slug, "name": app.Name, "html_url": app.HTMLURL, "owner": app.Owner.Login,
	})
}

var ghOrgRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

// POST /api/v1/github/app/manifest returns the form the browser submits to GitHub.
func (s *Server) handleGitHubAppManifest(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in struct {
		Name         string `json:"name"`
		Organization string `json:"organization"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	base := s.baseURL(r)
	if in.Name == "" {
		host := strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
		in.Name = "paas-" + strings.TrimPrefix(util.Slugify(host), "paas-")
		if len(in.Name) > 28 {
			in.Name = in.Name[:28]
		}
		in.Name = strings.TrimRight(in.Name, "-") + "-" + strings.ToLower(util.RandomToken(2))
	}
	state, err := s.signState(r.Context(), ghState{Kind: "manifest", UserID: currentUser(r).ID})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	action := github.Web + "/settings/apps/new"
	if in.Organization != "" {
		if !ghOrgRe.MatchString(in.Organization) {
			writeError(w, 400, "invalid organization name")
			return
		}
		action = github.Web + "/organizations/" + in.Organization + "/settings/apps/new"
	}
	manifest, _ := json.Marshal(github.Manifest(in.Name, base))
	writeJSON(w, 200, map[string]string{"action_url": action + "?state=" + url.QueryEscape(state), "manifest": string(manifest)})
}

// GET /api/v1/github/app/callback?code=&state= (GitHub redirects here after creating the app)
func (s *Server) handleGitHubAppCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.verifyState(ctx, r.URL.Query().Get("state"), "manifest")
	if err != nil {
		http.Redirect(w, r, withQuery("/#/settings/github", "github_error", err.Error()), http.StatusFound)
		return
	}
	if u, err := s.store.UserByID(ctx, st.UserID); err != nil || !u.IsAdmin {
		http.Redirect(w, r, withQuery("/#/settings/github", "github_error", "administrator only"), http.StatusFound)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" || !regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`).MatchString(code) {
		http.Redirect(w, r, withQuery("/#/settings/github", "github_error", "missing code"), http.StatusFound)
		return
	}
	app, err := github.ConvertManifest(ctx, code)
	if err != nil {
		http.Redirect(w, r, withQuery("/#/settings/github", "github_error", err.Error()), http.StatusFound)
		return
	}
	if _, err := github.NewClient(app); err != nil {
		http.Redirect(w, r, withQuery("/#/settings/github", "github_error", err.Error()), http.StatusFound)
		return
	}
	b, _ := json.Marshal(app)
	if err := s.store.SetSetting(ctx, settingGitHubApp, s.box.MustEncrypt(string(b))); err != nil {
		http.Redirect(w, r, withQuery("/#/settings/github", "github_error", err.Error()), http.StatusFound)
		return
	}
	s.ghMu.Lock()
	s.gh = nil
	s.ghMu.Unlock()
	s.store.Audit(ctx, &st.UserID, nil, "github.app.create", app.Slug)
	slog.Info("github app configured", "slug", app.Slug)
	http.Redirect(w, r, "/#/settings/github?github=created", http.StatusFound)
}

// DELETE /api/v1/github/app forgets the app (it stays on GitHub until deleted there).
func (s *Server) handleDeleteGitHubApp(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if _, err := s.store.Q(r.Context()).Exec(r.Context(), `DELETE FROM settings WHERE key=$1`, settingGitHubApp); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	s.ghMu.Lock()
	s.gh = nil
	s.ghMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/v1/teams/{team}/github
func (s *Server) handleTeamGitHub(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")
	if !s.requireTeam(w, r, team, "viewer") {
		return
	}
	insts, err := s.store.GitHubInstallationsForTeam(r.Context(), team)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	out := map[string]any{"configured": false, "installations": insts}
	c, err := s.githubApp(r.Context())
	if err == nil {
		out["configured"] = true
		out["app_slug"] = c.App().Slug
		role, _ := s.teamRole(r, team)
		if roleRank[role] >= roleRank["developer"] {
			st, err := s.signState(r.Context(), ghState{Kind: "install", UserID: currentUser(r).ID, TeamID: team, Return: safeReturn(r.URL.Query().Get("return"))})
			if err != nil {
				writeError(w, 500, err.Error())
				return
			}
			out["install_url"] = c.App().InstallURL(st)
		}
	}
	writeJSON(w, 200, out)
}

// GET /api/v1/github/installed?installation_id=&setup_action=&code=&state=
// GitHub sends the user here after they install the app and authorize it.
func (s *Server) handleGitHubInstalled(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	fail := func(ret, msg string) {
		http.Redirect(w, r, withQuery(ret, "github_error", msg), http.StatusFound)
	}
	// "Sign in with GitHub" and account linking return to the same callback URL.
	if st, err := s.verifyState(ctx, q.Get("state"), "login"); err == nil {
		s.finishGitHubLogin(w, r, st)
		return
	}
	if st, err := s.verifyState(ctx, q.Get("state"), "link"); err == nil {
		s.finishGitHubLink(w, r, st)
		return
	}
	st, err := s.verifyState(ctx, q.Get("state"), "install")
	if err != nil {
		// Installed or changed from GitHub's own settings page: nothing to link.
		if q.Get("setup_action") == "update" {
			http.Redirect(w, r, "/#/settings/github?github=updated", http.StatusFound)
			return
		}
		fail("/#/settings/github", err.Error())
		return
	}
	ret := safeReturn(st.Return)
	id, err := strconv.ParseInt(q.Get("installation_id"), 10, 64)
	if err != nil || id <= 0 {
		fail(ret, "missing installation id")
		return
	}
	role, err := s.store.MemberRole(ctx, st.TeamID, st.UserID)
	if u, uerr := s.store.UserByID(ctx, st.UserID); uerr == nil && u.IsAdmin {
		role, err = "owner", nil
	}
	if err != nil || roleRank[role] < roleRank["developer"] {
		fail(ret, "you are not allowed to connect GitHub for this team")
		return
	}
	c, err := s.githubApp(ctx)
	if err != nil {
		fail(ret, err.Error())
		return
	}
	// Prove the person coming back can access this installation, so nobody can
	// attach someone else's installation (and its private repositories) by id.
	code := q.Get("code")
	if code == "" {
		fail(ret, "GitHub did not return an authorization code; enable \"Request user authorization (OAuth) during installation\" on the app")
		return
	}
	userToken, err := c.UserToken(ctx, code)
	if err != nil {
		fail(ret, err.Error())
		return
	}
	ids, err := c.UserInstallationIDs(ctx, userToken)
	if err != nil {
		fail(ret, err.Error())
		return
	}
	if !ids[id] {
		fail(ret, "this GitHub installation is not accessible to your GitHub account")
		return
	}
	inst, err := c.GetInstallation(ctx, id)
	if err != nil {
		fail(ret, err.Error())
		return
	}
	if err := s.store.UpsertGitHubInstallation(ctx, store.GitHubInstallation{
		InstallationID: id, TeamID: st.TeamID, AccountLogin: inst.Account.Login, AccountType: inst.Account.Type,
	}); err != nil {
		fail(ret, err.Error())
		return
	}
	s.store.Audit(ctx, &st.UserID, &st.TeamID, "github.install", inst.Account.Login)
	http.Redirect(w, r, withQuery(ret, "github", "connected"), http.StatusFound)
}

// DELETE /api/v1/teams/{team}/github/installations/{installation}
func (s *Server) handleUnlinkGitHubInstallation(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")
	if !s.requireTeam(w, r, team, "admin") {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("installation"), 10, 64)
	inst, err := s.store.GitHubInstallation(r.Context(), id)
	if err != nil || inst.TeamID != team {
		writeError(w, 404, "installation not found")
		return
	}
	if err := s.store.DeleteGitHubInstallation(r.Context(), id); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// checkInstallation makes sure a service only uses its own team's installation.
func (s *Server) checkInstallation(ctx context.Context, teamID string, id int64) error {
	if id == 0 {
		return nil
	}
	inst, err := s.store.GitHubInstallation(ctx, id)
	if err != nil || inst.TeamID != teamID {
		return errors.New("unknown GitHub installation for this team")
	}
	return nil
}

type ghRepo struct {
	github.Repo
	InstallationID int64  `json:"installation_id"`
	Account        string `json:"account"`
}

// GET /api/v1/teams/{team}/github/repos
func (s *Server) handleGitHubRepos(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")
	if !s.requireTeam(w, r, team, "developer") {
		return
	}
	c, err := s.githubApp(r.Context())
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	insts, err := s.store.GitHubInstallationsForTeam(r.Context(), team)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	out := []ghRepo{}
	var errs []string
	for _, in := range insts {
		repos, err := c.Repos(r.Context(), in.InstallationID)
		if err != nil {
			errs = append(errs, in.AccountLogin+": "+err.Error())
			continue
		}
		for _, rp := range repos {
			out = append(out, ghRepo{Repo: rp, InstallationID: in.InstallationID, Account: in.AccountLogin})
		}
	}
	writeJSON(w, 200, map[string]any{"repos": out, "errors": errs})
}

var ghFullNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// GET /api/v1/teams/{team}/github/branches?installation_id=&repo=owner/name
func (s *Server) handleGitHubBranches(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")
	if !s.requireTeam(w, r, team, "developer") {
		return
	}
	id, _ := strconv.ParseInt(r.URL.Query().Get("installation_id"), 10, 64)
	if err := s.checkInstallation(r.Context(), team, id); err != nil || id == 0 {
		writeError(w, 400, "unknown GitHub installation for this team")
		return
	}
	repo := r.URL.Query().Get("repo")
	if !ghFullNameRe.MatchString(repo) {
		writeError(w, 400, "repo must be owner/name")
		return
	}
	c, err := s.githubApp(r.Context())
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	branches, err := c.Branches(r.Context(), id, repo)
	if err != nil {
		writeError(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"branches": branches})
}

type appEvent struct {
	Action       string `json:"action"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		CloneURL string `json:"clone_url"`
		HTMLURL  string `json:"html_url"`
	} `json:"repository"`
}

// POST /api/v1/webhooks/github-app receives every event of the app and routes
// it to the services connected to that installation and repository.
func (s *Server) handleGitHubAppWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, err := s.githubApp(ctx)
	if err != nil {
		writeError(w, 404, "github app not configured")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 5<<20))
	if err != nil {
		writeError(w, 400, "read body")
		return
	}
	if !verifyGitHubSignature(c.App().WebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		writeError(w, 401, "invalid signature")
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	var ev appEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		writeError(w, 400, "invalid payload")
		return
	}
	switch event {
	case "ping":
		writeJSON(w, 200, map[string]string{"status": "pong"})
		return
	case "installation":
		if ev.Action == "deleted" {
			_ = s.store.DeleteGitHubInstallation(ctx, ev.Installation.ID)
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	case "push", "pull_request":
	default:
		writeJSON(w, 200, map[string]string{"status": "ignored", "event": event})
		return
	}
	if ev.Installation.ID == 0 || ev.Repository.CloneURL == "" {
		writeJSON(w, 200, map[string]string{"status": "ignored"})
		return
	}
	inst, err := s.store.GitHubInstallation(ctx, ev.Installation.ID)
	if err != nil {
		writeJSON(w, 200, map[string]string{"status": "ignored", "reason": "installation not linked"})
		return
	}
	services, err := s.store.AllServices(ctx)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	results := []map[string]any{}
	for i := range services {
		svc := &services[i]
		if svc.ParentID != nil || svc.Source.Kind != "git" || svc.Source.InstallationID != inst.InstallationID {
			continue
		}
		if !sameRepo(svc.Source.Repo, ev.Repository.CloneURL) && !sameRepo(svc.Source.Repo, ev.Repository.HTMLURL) {
			continue
		}
		rec := httptest.NewRecorder()
		s.handleGitHubEvent(rec, r, svc, event, body)
		var res map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &res)
		if res == nil {
			res = map[string]any{}
		}
		res["service"] = svc.Slug
		res["http_status"] = rec.Code
		results = append(results, res)
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "results": results, "matched": fmt.Sprint(len(results))})
}
