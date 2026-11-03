package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/installer"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
	"github.com/thesunbg/paas/internal/webui"
)

type ctxKey int

const (
	userKey    ctxKey = 1
	sessionKey ctxKey = 2 // hash of the session token that authenticated the request
)

const sessionCookie = "paas_session"

var roleRank = map[string]int{"viewer": 0, "developer": 1, "admin": 2, "owner": 3}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	h := func(pattern string, fn http.HandlerFunc) { mux.HandleFunc(pattern, s.authed(fn)) }

	mux.HandleFunc("GET /install.sh", installer.Handler)
	mux.HandleFunc("GET /download/{file}", installer.BinaryHandler)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /api/v1/setup", s.handleSetupStatus)
	mux.HandleFunc("POST /api/v1/cluster/enroll", s.handleEnroll)
	mux.HandleFunc("POST /api/v1/auth/register", s.handleRegister)
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/v1/auth/github", s.handleGitHubLoginStart)
	mux.HandleFunc("POST /api/v1/auth/github/2fa", s.handleGitHub2FA)
	mux.HandleFunc("POST /api/v1/auth/2fa", s.handleGitHub2FA) // completes a GitHub or SSO sign-in with 2FA
	mux.HandleFunc("GET /api/v1/auth/oidc/callback", s.handleOIDCCallback)
	mux.HandleFunc("GET /api/v1/auth/oidc/{provider}", s.handleOIDCLoginStart)
	mux.HandleFunc("POST /api/v1/webhooks/github/{id}", s.handleGitHubWebhook)
	mux.HandleFunc("POST /api/v1/webhooks/github-app", s.handleGitHubAppWebhook)
	mux.HandleFunc("POST /api/v1/webhooks/gitlab/{id}", s.handleGitLabWebhook)
	mux.HandleFunc("POST /api/v1/webhooks/gitea/{id}", s.handleGiteaWebhook)
	mux.HandleFunc("POST /api/v1/webhooks/bitbucket/{id}", s.handleBitbucketWebhook)
	mux.HandleFunc("GET /api/v1/github/app/callback", s.handleGitHubAppCallback)
	mux.HandleFunc("GET /api/v1/github/installed", s.handleGitHubInstalled)

	h("GET /api/v1/me", s.handleMe)
	h("GET /api/v1/me/ip", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"ip": clientAddr(r)})
	})
	h("GET /api/v1/me/github/link", s.handleGitHubLinkStart)
	h("DELETE /api/v1/me/github", s.handleGitHubUnlink)
	h("GET /api/v1/me/oidc", s.handleMyOIDC)
	h("GET /api/v1/me/oidc/{provider}/link", s.handleOIDCLinkStart)
	h("DELETE /api/v1/me/oidc/{provider}", s.handleOIDCUnlink)
	h("POST /api/v1/me/password", s.handleChangePassword)
	h("POST /api/v1/me/2fa/setup", s.handle2FASetup)
	h("POST /api/v1/me/2fa/enable", s.handle2FAEnable)
	h("POST /api/v1/me/2fa/disable", s.handle2FADisable)
	h("GET /api/v1/tokens", s.handleListTokens)
	h("POST /api/v1/tokens", s.handleCreateToken)
	h("DELETE /api/v1/tokens/{id}", s.handleDeleteToken)
	h("GET /api/v1/overview", s.handleOverview)

	h("GET /api/v1/teams", s.handleListTeams)
	h("POST /api/v1/teams", s.handleCreateTeam)
	h("GET /api/v1/teams/{team}/members", s.handleListMembers)
	h("GET /api/v1/teams/{team}/quota", s.handleGetTeamQuota)
	h("PUT /api/v1/teams/{team}/quota", s.handlePutTeamQuota)
	h("POST /api/v1/teams/{team}/members", s.handleAddMember)
	h("DELETE /api/v1/teams/{team}/members/{user}", s.handleRemoveMember)
	h("GET /api/v1/teams/{team}/projects", s.handleListProjects)
	h("GET /api/v1/teams/{team}/github", s.handleTeamGitHub)
	h("GET /api/v1/teams/{team}/github/repos", s.handleGitHubRepos)
	h("GET /api/v1/teams/{team}/github/branches", s.handleGitHubBranches)
	h("DELETE /api/v1/teams/{team}/github/installations/{installation}", s.handleUnlinkGitHubInstallation)
	h("GET /api/v1/teams/{team}/git-credentials", s.handleListGitCredentials)
	h("POST /api/v1/teams/{team}/git-credentials", s.handleSetGitCredential)
	h("DELETE /api/v1/teams/{team}/git-credentials/{credential}", s.handleDeleteGitCredential)
	h("GET /api/v1/github/app", s.handleGitHubAppStatus)
	h("POST /api/v1/github/app/manifest", s.handleGitHubAppManifest)
	h("DELETE /api/v1/github/app", s.handleDeleteGitHubApp)
	h("POST /api/v1/teams/{team}/projects", s.handleCreateProject)

	h("GET /api/v1/projects/{id}", s.handleGetProject)
	h("DELETE /api/v1/projects/{id}", s.handleDeleteProject)
	h("PATCH /api/v1/projects/{id}", s.handleUpdateProject)
	h("POST /api/v1/projects/{id}/studio", s.handleStudio)
	h("POST /api/v1/projects/{id}/services", s.handleCreateService)
	h("POST /api/v1/projects/{id}/databases", s.handleCreateDatabase)
	h("POST /api/v1/projects/{id}/templates/{template}", s.handleInstallTemplate)
	h("POST /api/v1/projects/{id}/detect", s.handleDetect)
	h("POST /api/v1/projects/{id}/compose", s.handleImportCompose)
	h("POST /api/v1/projects/{id}/import-repo", s.handleImportRepo)
	h("GET /api/v1/templates", s.handleListTemplates)

	h("GET /api/v1/services/{id}", s.handleGetService)
	h("PATCH /api/v1/services/{id}", s.handleUpdateService)
	h("DELETE /api/v1/services/{id}", s.handleDeleteService)
	h("POST /api/v1/services/{id}/deploy", s.handleDeploy)
	h("GET /api/v1/services/{id}/webhook", s.handleServiceWebhook)
	h("POST /api/v1/services/{id}/restart", s.handleRestart)
	h("GET /api/v1/services/{id}/volume-backups", s.handleListVolumeBackups)
	h("POST /api/v1/services/{id}/volume-backups", s.handleCreateVolumeBackup)
	h("POST /api/v1/services/{id}/recover", s.handleRecoverService)
	h("GET /api/v1/services/{id}/move", s.handleVolumeMoveStatus)
	h("POST /api/v1/services/{id}/move", s.handleStartVolumeMove)
	h("DELETE /api/v1/services/{id}/move", s.handleAbortVolumeMove)
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
	h("GET /api/v1/services/{id}/terminal", s.handleTerminal)
	h("GET /api/v1/batches", s.handleListBatches)
	h("POST /api/v1/batches", s.handleCreateBatch)
	h("GET /api/v1/batches/estimate", s.handleBatchEstimate)
	h("GET /api/v1/batches/{id}", s.handleGetBatch)
	h("GET /api/v1/batches/{id}/csv", s.handleBatchCSV)
	h("DELETE /api/v1/batches/{id}", s.handleDeleteBatch)
	h("POST /api/v1/services/{id}/run", s.handleRunJob)
	h("GET /api/v1/services/{id}/runs", s.handleListRuns)
	h("GET /api/v1/services/{id}/metrics", s.handleServiceMetrics)
	h("GET /api/v1/nodes/{id}/metrics", s.handleNodeMetrics)
	h("GET /api/v1/projects/{id}/alerts", s.handleListAlerts)
	h("POST /api/v1/projects/{id}/alerts", s.handleCreateAlert)
	h("PATCH /api/v1/alerts/{id}", s.handleUpdateAlert)
	h("DELETE /api/v1/alerts/{id}", s.handleDeleteAlert)
	h("GET /api/v1/projects/{id}/uptime", s.handleListUptime)
	h("POST /api/v1/projects/{id}/uptime", s.handleCreateUptime)
	h("GET /api/v1/uptime/{id}", s.handleGetUptime)
	h("PATCH /api/v1/uptime/{id}", s.handleUpdateUptime)
	h("DELETE /api/v1/uptime/{id}", s.handleDeleteUptime)
	// Prometheus: PAAS_METRICS_TOKEN or an administrator's credentials (handlePrometheus authenticates).
	mux.HandleFunc("GET /metrics", s.handlePrometheus)
	mux.HandleFunc("GET /api/v1/metrics/prometheus", s.handlePrometheus)
	h("GET /api/v1/teams/{team}/notifications", s.handleListChannels)
	h("POST /api/v1/teams/{team}/notifications", s.handleCreateChannel)
	h("DELETE /api/v1/teams/{team}/notifications/{channel}", s.handleDeleteChannel)
	h("POST /api/v1/teams/{team}/notifications/{channel}/test", s.handleTestChannel)

	h("GET /api/v1/deployments/{id}", s.handleGetDeployment)
	h("GET /api/v1/deployments/{id}/logs", s.handleDeploymentLogs)
	h("GET /api/v1/deployments/{id}/logs/stream", s.handleDeploymentLogStream)
	h("POST /api/v1/deployments/{id}/rollback", s.handleRollback)
	h("POST /api/v1/deployments/{id}/cancel", s.handleCancelDeployment)
	h("GET /api/v1/deployments/{id}/diagnosis", s.handleGetDiagnosis)
	h("POST /api/v1/deployments/{id}/diagnose", s.handleDiagnose)
	h("GET /api/v1/deployments/{id}/scan", s.handleGetDeploymentScan)
	h("GET /api/v1/services/{id}/scan", s.handleGetServiceScan)
	h("GET /api/v1/projects/{id}/scan-policy", s.handleGetScanPolicy)
	h("PUT /api/v1/projects/{id}/scan-policy", s.handlePutScanPolicy)

	h("GET /api/v1/databases/{id}", s.handleGetDatabase)
	h("DELETE /api/v1/databases/{id}", s.handleDeleteDatabase)
	h("POST /api/v1/databases/{id}/links", s.handleLinkDatabase)
	h("DELETE /api/v1/databases/{id}/links/{service}", s.handleUnlinkDatabase)
	h("GET /api/v1/databases/{id}/backups", s.handleListBackups)
	h("POST /api/v1/databases/{id}/backups", s.handleCreateBackup)
	h("POST /api/v1/databases/{id}/backups/verify", s.handleVerifyBackup)
	h("POST /api/v1/databases/{id}/recover", s.handleRecoverDatabase)
	h("POST /api/v1/databases/{id}/backups/{backup}/restore", s.handleRestoreBackup)
	h("POST /api/v1/databases/{id}/upgrade", s.handleUpgradeDatabase)
	h("POST /api/v1/databases/{id}/version", s.handleDatabaseVersion)
	h("POST /api/v1/databases/{id}/update-minor", s.handleUpdateMinor)
	h("POST /api/v1/databases/{id}/retry", s.handleRetryDatabase)
	h("POST /api/v1/databases/{id}/import", s.handleDatabaseImport)
	h("POST /api/v1/databases/{id}/public", s.handleDatabasePublic)
	h("GET /api/v1/databases/{id}/metrics", s.handleDatabaseMetrics)
	h("PUT /api/v1/databases/{id}/quota", s.handleDatabaseQuota)
	h("POST /api/v1/databases/{id}/pooler", s.handleDatabasePooler)
	h("PUT /api/v1/databases/{id}/size", s.handleDatabaseResize)
	h("GET /api/v1/databases/{id}/pitr", s.handleGetPITR)
	h("GET /api/v1/databases/{id}/ha", s.handleGetHA)
	h("POST /api/v1/databases/{id}/ha", s.handleSetHA)
	h("POST /api/v1/databases/{id}/ha/switchover", s.handleSwitchover)
	h("POST /api/v1/databases/{id}/ha/failover", s.handleForceFailover)
	h("POST /api/v1/databases/{id}/pitr", s.handleSetPITR)
	h("POST /api/v1/databases/{id}/pitr/restore", s.handlePITRRestore)
	h("GET /api/v1/databases/{id}/users", s.handleListDBUsers)
	h("POST /api/v1/databases/{id}/users", s.handleCreateDBUser)
	h("DELETE /api/v1/databases/{id}/users/{user}", s.handleDeleteDBUser)
	h("POST /api/v1/databases/{id}/users/{user}/password", s.handleRotateDBUser)

	h("GET /api/v1/nodes", s.handleListNodes)
	h("PATCH /api/v1/nodes/{id}", s.handleUpdateNode)
	h("DELETE /api/v1/nodes/{id}", s.handleDeleteNode)
	h("GET /api/v1/nodes/{id}/stuck", s.handleNodeStuck)
	h("POST /api/v1/nodes/{id}/drain", s.handleDrainNode)
	h("POST /api/v1/nodes/{id}/recover", s.handleRecoverNode)
	h("POST /api/v1/cluster/join-tokens", s.handleCreateJoinToken)
	h("GET /api/v1/sizes", s.handleSizes)
	h("GET /api/v1/users", s.handleListUsers)
	h("POST /api/v1/users", s.handleCreateUser)
	h("PATCH /api/v1/users/{id}", s.handleUpdateUser)
	h("DELETE /api/v1/users/{id}", s.handleDeleteUser)
	h("GET /api/v1/admin/settings", s.handleGetSystemSettings)
	h("PUT /api/v1/admin/settings", s.handlePutSystemSettings)
	h("GET /api/v1/admin/oidc", s.handleListOIDCProviders)
	h("POST /api/v1/admin/oidc", s.handleCreateOIDCProvider)
	h("PATCH /api/v1/admin/oidc/{id}", s.handleUpdateOIDCProvider)
	h("DELETE /api/v1/admin/oidc/{id}", s.handleDeleteOIDCProvider)
	h("GET /api/v1/admin/storage", s.handleGetStorage)
	h("PUT /api/v1/admin/storage", s.handlePutStorage)
	h("GET /api/v1/admin/state-backups", s.handleListStateBackups)
	h("POST /api/v1/admin/state-backups", s.handleCreateStateBackup)
	h("GET /api/v1/admin/state-backups/{name}", s.handleDownloadStateBackup)
	h("GET /api/v1/admin/setup-checklist", s.handleGetSetupChecklist)
	h("PUT /api/v1/admin/setup-checklist", s.handlePutSetupChecklist)
	h("POST /api/v1/admin/setup-checklist/{step}/check", s.handleCheckSetupStep)
	h("GET /api/v1/admin/ai", s.handleGetAI)
	h("PUT /api/v1/admin/ai", s.handlePutAI)

	// MCP for AI agents: its tools call back into this mux with the caller's token.
	mux.HandleFunc("/mcp", s.handleMCP(mux))

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

// Hijack lets WebSockets (the terminal) take over the connection.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijacking not supported")
	}
	r.status = http.StatusSwitchingProtocols
	return h.Hijack()
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

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

// writeUpdateError maps the store's compare-and-swap errors (412 for a failed
// If-Match, 409 when concurrent writers kept winning) and falls back to 500.
func writeUpdateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrStale):
		writeError(w, http.StatusPreconditionFailed, err.Error())
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// ifMatch reads the If-Match header as a row version. ok is false without the
// header; an unparseable value yields -1, which never matches.
func ifMatch(r *http.Request) (version int64, ok bool) {
	h := strings.TrimSpace(r.Header.Get("If-Match"))
	if h == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.Trim(strings.TrimPrefix(h, "W/"), `"`), 10, 64)
	if err != nil {
		return -1, true
	}
	return v, true
}

// checkIfMatch is store.ErrStale when the request carries an If-Match that is not the current version.
func checkIfMatch(r *http.Request, current int64) error {
	if v, ok := ifMatch(r); ok && v != current {
		return store.ErrStale
	}
	return nil
}

func setETag(w http.ResponseWriter, version int64) {
	w.Header().Set("ETag", `"`+strconv.FormatInt(version, 10)+`"`)
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
		if tok == "" && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/logs/stream") {
			tok = r.URL.Query().Get("access_token") // EventSource cannot set headers; URLs leak into logs, so only log streams accept it
		}
		if tok == "" {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		var user *store.User
		var err error
		sessionHash := ""
		if strings.HasPrefix(tok, "paas_") {
			user, err = s.store.UserByAPIToken(r.Context(), util.HashToken(tok))
		} else {
			sessionHash = util.HashToken(tok)
			user, err = s.store.UserBySession(r.Context(), sessionHash)
		}
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid or expired credentials")
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("Authorization") == "" && !sameOrigin(r) {
			writeError(w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		ctx := context.WithValue(r.Context(), userKey, user)
		next(w, r.WithContext(context.WithValue(ctx, sessionKey, sessionHash)))
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

// currentSessionHash is the authenticating session's token hash, empty for API tokens.
func currentSessionHash(r *http.Request) string {
	h, _ := r.Context().Value(sessionKey).(string)
	return h
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
