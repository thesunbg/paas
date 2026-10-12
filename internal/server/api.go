package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/installer"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
	"github.com/thesunbg/paas/internal/webui"
)

type ctxKey int

const userKey ctxKey = 1

const sessionCookie = "paas_session"

var roleRank = map[string]int{"viewer": 0, "developer": 1, "admin": 2, "owner": 3}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	h := func(pattern string, fn http.HandlerFunc) { mux.HandleFunc(pattern, s.authed(fn)) }

	mux.HandleFunc("GET /install.sh", installer.Handler)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /api/v1/setup", s.handleSetupStatus)
	mux.HandleFunc("POST /api/v1/auth/register", s.handleRegister)
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)
	mux.HandleFunc("POST /api/v1/webhooks/github/{id}", s.handleGitHubWebhook)

	h("GET /api/v1/me", s.handleMe)
	h("POST /api/v1/me/password", s.handleChangePassword)
	h("GET /api/v1/tokens", s.handleListTokens)
	h("POST /api/v1/tokens", s.handleCreateToken)
	h("DELETE /api/v1/tokens/{id}", s.handleDeleteToken)
	h("GET /api/v1/overview", s.handleOverview)

	h("GET /api/v1/teams", s.handleListTeams)
	h("POST /api/v1/teams", s.handleCreateTeam)
	h("GET /api/v1/teams/{team}/members", s.handleListMembers)
	h("POST /api/v1/teams/{team}/members", s.handleAddMember)
	h("DELETE /api/v1/teams/{team}/members/{user}", s.handleRemoveMember)
	h("GET /api/v1/teams/{team}/projects", s.handleListProjects)
	h("POST /api/v1/teams/{team}/projects", s.handleCreateProject)

	h("GET /api/v1/projects/{id}", s.handleGetProject)
	h("DELETE /api/v1/projects/{id}", s.handleDeleteProject)
	h("POST /api/v1/projects/{id}/services", s.handleCreateService)
	h("POST /api/v1/projects/{id}/databases", s.handleCreateDatabase)

	h("GET /api/v1/services/{id}", s.handleGetService)
	h("PATCH /api/v1/services/{id}", s.handleUpdateService)
	h("DELETE /api/v1/services/{id}", s.handleDeleteService)
	h("POST /api/v1/services/{id}/deploy", s.handleDeploy)
	h("POST /api/v1/services/{id}/restart", s.handleRestart)
	h("POST /api/v1/services/{id}/stop", s.handleStop)
	h("POST /api/v1/services/{id}/start", s.handleStart)
	h("POST /api/v1/services/{id}/scale", s.handleScale)
	h("GET /api/v1/services/{id}/deployments", s.handleListDeployments)
	h("GET /api/v1/services/{id}/env", s.handleGetEnv)
	h("PUT /api/v1/services/{id}/env", s.handlePutEnv)
	h("DELETE /api/v1/services/{id}/env/{key}", s.handleDeleteEnv)
	h("GET /api/v1/services/{id}/domains", s.handleListDomains)
	h("POST /api/v1/services/{id}/domains", s.handleAddDomain)
	h("DELETE /api/v1/services/{id}/domains/{domain}", s.handleDeleteDomain)
	h("GET /api/v1/services/{id}/logs", s.handleServiceLogs)
	h("GET /api/v1/services/{id}/logs/stream", s.handleServiceLogStream)
	h("POST /api/v1/services/{id}/exec", s.handleExec)

	h("GET /api/v1/deployments/{id}", s.handleGetDeployment)
	h("GET /api/v1/deployments/{id}/logs", s.handleDeploymentLogs)
	h("GET /api/v1/deployments/{id}/logs/stream", s.handleDeploymentLogStream)
	h("POST /api/v1/deployments/{id}/rollback", s.handleRollback)
	h("POST /api/v1/deployments/{id}/cancel", s.handleCancelDeployment)

	h("GET /api/v1/databases/{id}", s.handleGetDatabase)
	h("DELETE /api/v1/databases/{id}", s.handleDeleteDatabase)
	h("POST /api/v1/databases/{id}/links", s.handleLinkDatabase)
	h("DELETE /api/v1/databases/{id}/links/{service}", s.handleUnlinkDatabase)
	h("GET /api/v1/databases/{id}/backups", s.handleListBackups)
	h("POST /api/v1/databases/{id}/backups", s.handleCreateBackup)
	h("POST /api/v1/databases/{id}/backups/{backup}/restore", s.handleRestoreBackup)
	h("POST /api/v1/databases/{id}/upgrade", s.handleUpgradeDatabase)
	h("POST /api/v1/databases/{id}/retry", s.handleRetryDatabase)

	h("GET /api/v1/nodes", s.handleListNodes)
	h("PATCH /api/v1/nodes/{id}", s.handleUpdateNode)
	h("DELETE /api/v1/nodes/{id}", s.handleDeleteNode)
	h("GET /api/v1/cluster/join", s.handleJoinInfo)
	h("GET /api/v1/sizes", s.handleSizes)

	mux.Handle("/", webui.Handler())
	return logRequests(securityHeaders(mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) { r.status = code; r.ResponseWriter.WriteHeader(code) }
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		if strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasSuffix(r.URL.Path, "/stream") {
			slog.Debug("http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "took", time.Since(start))
		}
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// authed resolves the user from a session cookie, session bearer token or API token.
func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := ""
		if c, err := r.Cookie(sessionCookie); err == nil {
			tok = c.Value
		}
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			tok = strings.TrimPrefix(h, "Bearer ")
		}
		if tok == "" {
			tok = r.URL.Query().Get("access_token") // EventSource cannot set headers
		}
		if tok == "" {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		var user *store.User
		var err error
		if strings.HasPrefix(tok, "paas_") {
			user, err = s.store.UserByAPIToken(r.Context(), util.HashToken(tok))
		} else {
			user, err = s.store.UserBySession(r.Context(), util.HashToken(tok))
		}
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid or expired credentials")
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("Authorization") == "" && !sameOrigin(r) {
			writeError(w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, user)))
	}
}

// sameOrigin protects cookie-authenticated mutations against CSRF.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return r.Header.Get("Sec-Fetch-Site") != "cross-site"
	}
	o := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	return o == r.Host
}

func currentUser(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey).(*store.User)
	return u
}

var errForbidden = errors.New("forbidden")

// teamRole returns the caller's role in a team (system admins are owners everywhere).
func (s *Server) teamRole(r *http.Request, teamID string) (string, error) {
	u := currentUser(r)
	if u.IsAdmin {
		return "owner", nil
	}
	role, err := s.store.MemberRole(r.Context(), teamID, u.ID)
	if errors.Is(err, store.ErrNotFound) {
		return "", errForbidden
	}
	return role, err
}

func (s *Server) requireTeam(w http.ResponseWriter, r *http.Request, teamID, min string) bool {
	if teamID == s.systemTeamID && !currentUser(r).IsAdmin {
		writeError(w, http.StatusForbidden, "forbidden")
		return false
	}
	role, err := s.teamRole(r, teamID)
	if err != nil {
		writeError(w, http.StatusForbidden, "you are not a member of this team")
		return false
	}
	if roleRank[role] < roleRank[min] {
		writeError(w, http.StatusForbidden, "requires role "+min)
		return false
	}
	return true
}

func (s *Server) projectFor(w http.ResponseWriter, r *http.Request, projectID, min string) *store.Project {
	p, err := s.store.ProjectByID(r.Context(), projectID)
	if err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return nil
	}
	if !s.requireTeam(w, r, p.TeamID, min) {
		return nil
	}
	return p
}

func (s *Server) serviceFor(w http.ResponseWriter, r *http.Request, min string) (*store.Service, *store.Project) {
	svc, err := s.store.ServiceByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "service not found")
		return nil, nil
	}
	p := s.projectFor(w, r, svc.ProjectID, min)
	if p == nil {
		return nil, nil
	}
	return svc, p
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !currentUser(r).IsAdmin {
		writeError(w, http.StatusForbidden, "administrator only")
		return false
	}
	return true
}
