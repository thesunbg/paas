package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/store"
)

// Git hosts other than the GitHub App: GitLab (gitlab.com or self-hosted),
// Gitea / Forgejo (self-hosted, codeberg.org) and Bitbucket Cloud. A team
// stores one access token per host (see git_credentials.go); clones, commit
// statuses and preview comments use it only for repositories on that host.

const (
	providerGitHub    = "github"
	providerGitLab    = "gitlab"
	providerGitea     = "gitea"
	providerBitbucket = "bitbucket"
)

var gitProviders = []string{providerGitHub, providerGitLab, providerGitea, providerBitbucket}

// knownGitHosts are public hosting services whose provider needs no guessing.
var knownGitHosts = map[string]string{
	"github.com":    providerGitHub,
	"gitlab.com":    providerGitLab,
	"bitbucket.org": providerBitbucket,
	"codeberg.org":  providerGitea,
	"gitea.com":     providerGitea,
}

// defaultGitHosts is the host of a credential that names none.
var defaultGitHosts = map[string]string{providerGitHub: "github.com", providerGitLab: "gitlab.com", providerBitbucket: "bitbucket.org"}

// defaultGitUser is the HTTP basic user sent with a token on clone. GitLab and
// Gitea accept any user with an access token; Bitbucket access tokens need
// x-token-auth (app passwords need the account's username instead).
func defaultGitUser(provider string) string {
	switch provider {
	case providerGitLab:
		return "oauth2"
	case providerBitbucket:
		return "x-token-auth"
	}
	return "x-access-token"
}

var gitHostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?$`)

// canonicalHost lowercases host[:port] and drops the default https port.
func canonicalHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(h), ":443")
}

// normalizeGitHost parses the host of a credential: "git.example.com",
// "git.example.com:8443" or "https://git.example.com/". Anything with a path,
// credentials or another scheme is rejected: the credential is bound to exactly
// this host.
func normalizeGitHost(in string) (string, error) {
	in = strings.TrimSpace(in)
	bad := errors.New("host must be a hostname such as gitlab.example.com (https only, no path)")
	if strings.Contains(in, "://") {
		u, err := url.Parse(in)
		if err != nil || u.Scheme != "https" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return "", bad
		}
		in = u.Host
	}
	h := canonicalHost(in)
	if !gitHostRe.MatchString(h) || strings.Contains(h, "..") {
		return "", bad
	}
	return h, nil
}

// repoHost is the canonical host of an https repository URL; ok is false for
// any other scheme or a URL carrying credentials, which never get a token.
func repoHost(repo string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(repo))
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || u.Opaque != "" {
		return "", false
	}
	h := canonicalHost(u.Host)
	if !gitHostRe.MatchString(h) {
		return "", false
	}
	return h, true
}

// repoPath is the repository path of an https URL without .git: "owner/name",
// or "group/subgroup/name" on GitLab.
func repoPath(repo string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(repo))
	if err != nil || u.Scheme != "https" {
		return "", errors.New("repository must be an https:// URL")
	}
	p := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	parts := strings.Split(p, "/")
	if len(parts) < 2 {
		return "", errors.New("repository path must be <owner>/<name>")
	}
	for _, s := range parts {
		if s == "" || s == "." || s == ".." {
			return "", errors.New("invalid repository path")
		}
	}
	return p, nil
}

// ownerRepo splits a two-segment repository path (GitHub, Gitea, Bitbucket).
func ownerRepo(p string) (string, string, error) {
	owner, name, ok := strings.Cut(p, "/")
	if !ok || strings.Contains(name, "/") {
		return "", "", errors.New("repository path must be <owner>/<name>")
	}
	return owner, name, nil
}

// gitAuth is the credential for one service's repository.
type gitAuth struct {
	Provider string // github | gitlab | gitea | bitbucket, or "" when unknown
	User     string
	Token    string
}

// teamGitAuth returns the team's credential for repo's host, or an empty token
// when the team has none: a credential is never used for another host.
func (s *Server) teamGitAuth(ctx context.Context, teamID, repo string) (gitAuth, error) {
	host, ok := repoHost(repo)
	if !ok {
		return gitAuth{}, nil
	}
	auth := gitAuth{Provider: knownGitHosts[host]}
	if teamID == "" {
		return auth, nil
	}
	c, err := s.store.GitCredentialForHost(ctx, teamID, host)
	if errors.Is(err, store.ErrNotFound) {
		return auth, nil
	}
	if err != nil {
		return gitAuth{}, err
	}
	tok, err := s.box.Decrypt(c.TokenEnc)
	if err != nil {
		return gitAuth{}, fmt.Errorf("decrypt git credential: %w", err)
	}
	return gitAuth{Provider: c.Provider, User: firstNonEmpty(c.Username, defaultGitUser(c.Provider)), Token: tok}, nil
}

// gitProviderOf names the provider of a service's repository for webhooks and
// instructions ("" for an image service or an unknown host).
func (s *Server) gitProviderOf(ctx context.Context, svc *store.Service, teamID string) string {
	if svc.Source.Kind != "git" {
		return ""
	}
	if svc.Source.InstallationID != 0 {
		return providerGitHub
	}
	host, ok := repoHost(svc.Source.Repo)
	if !ok {
		return ""
	}
	if p := knownGitHosts[host]; p != "" {
		return p
	}
	if c, err := s.store.GitCredentialForHost(ctx, teamID, host); err == nil {
		return c.Provider
	}
	return ""
}

// webhookURL is the push-to-deploy endpoint of a service for a provider (GitHub when unknown).
func (s *Server) webhookURL(svc *store.Service, provider string) string {
	return fmt.Sprintf("%s/api/v1/webhooks/%s/%s", s.cfg.PublicURL, firstNonEmpty(provider, providerGitHub), svc.ID)
}

// GET /api/v1/services/{id}/webhook: the webhook URL and secret to configure on the git host.
func (s *Server) handleServiceWebhook(w http.ResponseWriter, r *http.Request) {
	svc, p := s.serviceFor(w, r, "developer")
	if svc == nil {
		return
	}
	provider := s.gitProviderOf(r.Context(), svc, p.TeamID)
	urls := map[string]string{}
	for _, pv := range gitProviders {
		urls[pv] = s.webhookURL(svc, pv)
	}
	writeJSON(w, 200, map[string]any{
		"provider": provider,
		"url":      s.webhookURL(svc, provider),
		"secret":   svc.WebhookSecret,
		"urls":     urls,
	})
}

// Webhook verification.

// verifyGitLabToken compares the X-Gitlab-Token header with the service's secret.
func verifyGitLabToken(secret, header string) bool {
	return secret != "" && header != "" && subtle.ConstantTimeCompare([]byte(secret), []byte(header)) == 1
}

// verifyGiteaSignature checks X-Gitea-Signature / X-Forgejo-Signature: the hex
// HMAC-SHA256 of the body, without the "sha256=" prefix GitHub uses.
func verifyGiteaSignature(secret string, body []byte, header string) bool {
	if secret == "" || header == "" {
		return false
	}
	return verifyGitHubSignature(secret, body, "sha256="+strings.ToLower(strings.TrimSpace(header)))
}

// webhookBody loads the service of a webhook and reads its body.
func (s *Server) webhookBody(w http.ResponseWriter, r *http.Request) (*store.Service, []byte, bool) {
	svc, err := s.store.ServiceByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "unknown service")
		return nil, nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 5<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body")
		return nil, nil, false
	}
	return svc, body, true
}

func ignored(w http.ResponseWriter, reason string) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": reason})
}

// GitLab.

type gitlabPushEvent struct {
	Ref     string `json:"ref"`
	After   string `json:"after"`
	Commits []struct {
		ID       string   `json:"id"`
		Message  string   `json:"message"`
		Added    []string `json:"added"`
		Removed  []string `json:"removed"`
		Modified []string `json:"modified"`
	} `json:"commits"`
}

type gitlabMREvent struct {
	ObjectAttributes struct {
		IID             int    `json:"iid"`
		Action          string `json:"action"`
		Title           string `json:"title"`
		SourceBranch    string `json:"source_branch"`
		TargetBranch    string `json:"target_branch"`
		SourceProjectID int64  `json:"source_project_id"`
		TargetProjectID int64  `json:"target_project_id"`
		OldRev          string `json:"oldrev"`
		LastCommit      struct {
			ID string `json:"id"`
		} `json:"last_commit"`
		Source struct {
			GitHTTPURL string `json:"git_http_url"`
		} `json:"source"`
	} `json:"object_attributes"`
}

func zeroSHA(s string) bool { return s != "" && strings.Trim(s, "0") == "" }

// parseGitLabPush maps a GitLab "Push Hook" onto the GitHub push shape.
func parseGitLabPush(body []byte) (pushEvent, error) {
	var in gitlabPushEvent
	if err := json.Unmarshal(body, &in); err != nil {
		return pushEvent{}, err
	}
	ev := pushEvent{Ref: in.Ref, After: in.After, Deleted: zeroSHA(in.After)}
	for _, c := range in.Commits {
		ev.Commits = append(ev.Commits, pushCommit{Added: c.Added, Removed: c.Removed, Modified: c.Modified})
		if c.ID == in.After {
			ev.HeadCommit = &headCommit{ID: c.ID, Message: c.Message}
		}
	}
	return ev, nil
}

// parseGitLabMR maps a "Merge Request Hook" onto the GitHub pull_request shape;
// fork is true when the source branch lives in another project.
func parseGitLabMR(body []byte) (ev pullRequestEvent, fork bool, err error) {
	var in gitlabMREvent
	if err := json.Unmarshal(body, &in); err != nil {
		return ev, false, err
	}
	a := in.ObjectAttributes
	switch a.Action {
	case "open":
		ev.Action = "opened"
	case "reopen":
		ev.Action = "reopened"
	case "update":
		ev.Action = "edited"
		if a.OldRev != "" { // new commits were pushed
			ev.Action = "synchronize"
		}
	case "close", "merge":
		ev.Action = "closed"
	default:
		ev.Action = a.Action
	}
	ev.Number = a.IID
	ev.PullRequest.Title = a.Title
	ev.PullRequest.Head.Ref = a.SourceBranch
	ev.PullRequest.Head.SHA = a.LastCommit.ID
	ev.PullRequest.Head.Repo.CloneURL = a.Source.GitHTTPURL
	ev.PullRequest.Base.Ref = a.TargetBranch
	return ev, a.SourceProjectID != a.TargetProjectID, nil
}

// POST /api/v1/webhooks/gitlab/{id} (secret token in X-Gitlab-Token)
func (s *Server) handleGitLabWebhook(w http.ResponseWriter, r *http.Request) {
	svc, body, ok := s.webhookBody(w, r)
	if !ok {
		return
	}
	if !verifyGitLabToken(svc.WebhookSecret, r.Header.Get("X-Gitlab-Token")) {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}
	switch event := r.Header.Get("X-Gitlab-Event"); event {
	case "Push Hook":
		ev, err := parseGitLabPush(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid payload")
			return
		}
		s.handlePush(w, r, svc, ev)
	case "Merge Request Hook":
		ev, fork, err := parseGitLabMR(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid payload")
			return
		}
		if fork {
			ignored(w, "merge requests from forks do not get previews")
			return
		}
		s.handlePullRequestEvent(w, r, svc, ev)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "event": event})
	}
}

// Gitea / Forgejo: the payloads follow GitHub's.

// POST /api/v1/webhooks/gitea/{id} (HMAC in X-Gitea-Signature or X-Forgejo-Signature)
func (s *Server) handleGiteaWebhook(w http.ResponseWriter, r *http.Request) {
	svc, body, ok := s.webhookBody(w, r)
	if !ok {
		return
	}
	sig := firstNonEmpty(r.Header.Get("X-Gitea-Signature"), r.Header.Get("X-Forgejo-Signature"))
	if !verifyGiteaSignature(svc.WebhookSecret, body, sig) {
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}
	switch event := firstNonEmpty(r.Header.Get("X-Gitea-Event"), r.Header.Get("X-Forgejo-Event")); event {
	case "push":
		var ev pushEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			writeError(w, http.StatusBadRequest, "invalid payload")
			return
		}
		ev.Deleted = ev.Deleted || zeroSHA(ev.After)
		s.handlePush(w, r, svc, ev)
	case "pull_request":
		var ev pullRequestEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			writeError(w, http.StatusBadRequest, "invalid payload")
			return
		}
		if ev.Action == "synchronized" {
			ev.Action = "synchronize"
		}
		s.handlePullRequestEvent(w, r, svc, ev)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "event": event})
	}
}

// Bitbucket Cloud.

type bitbucketRef struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	Target struct {
		Hash    string `json:"hash"`
		Message string `json:"message"`
	} `json:"target"`
}

type bitbucketPushEvent struct {
	Push struct {
		Changes []struct {
			New    *bitbucketRef `json:"new"`
			Old    *bitbucketRef `json:"old"`
			Closed bool          `json:"closed"`
		} `json:"changes"`
	} `json:"push"`
}

type bitbucketBranchSide struct {
	Branch struct {
		Name string `json:"name"`
	} `json:"branch"`
	Commit struct {
		Hash string `json:"hash"`
	} `json:"commit"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type bitbucketPREvent struct {
	PullRequest struct {
		ID          int                 `json:"id"`
		Title       string              `json:"title"`
		Source      bitbucketBranchSide `json:"source"`
		Destination bitbucketBranchSide `json:"destination"`
	} `json:"pullrequest"`
}

// parseBitbucketPush picks the change of branch want from a "repo:push" event
// (one push can update several refs). Bitbucket sends no file lists, so a
// root directory filter cannot skip it.
func parseBitbucketPush(body []byte, want string) (pushEvent, error) {
	var in bitbucketPushEvent
	if err := json.Unmarshal(body, &in); err != nil {
		return pushEvent{}, err
	}
	var first *pushEvent
	for _, c := range in.Push.Changes {
		ref := c.New
		if ref == nil {
			ref = c.Old
		}
		if ref == nil || (ref.Type != "branch" && ref.Type != "named_branch") {
			continue
		}
		ev := pushEvent{Ref: "refs/heads/" + ref.Name, Deleted: c.Closed || c.New == nil}
		if c.New != nil {
			ev.After = c.New.Target.Hash
			ev.HeadCommit = &headCommit{ID: c.New.Target.Hash, Message: c.New.Target.Message}
		}
		if ref.Name == want {
			return ev, nil
		}
		if first == nil {
			first = &ev
		}
	}
	if first == nil {
		return pushEvent{}, nil
	}
	return *first, nil
}

// parseBitbucketPR maps a pullrequest:* event; fork is true when the source
// branch lives in another repository.
func parseBitbucketPR(body []byte, key string) (ev pullRequestEvent, fork bool, err error) {
	var in bitbucketPREvent
	if err := json.Unmarshal(body, &in); err != nil {
		return ev, false, err
	}
	switch key {
	case "pullrequest:created":
		ev.Action = "opened"
	case "pullrequest:updated":
		ev.Action = "synchronize"
	case "pullrequest:fulfilled", "pullrequest:rejected":
		ev.Action = "closed"
	default:
		ev.Action = key
	}
	pr := in.PullRequest
	ev.Number = pr.ID
	ev.PullRequest.Title = pr.Title
	ev.PullRequest.Head.Ref = pr.Source.Branch.Name
	ev.PullRequest.Head.SHA = pr.Source.Commit.Hash
	ev.PullRequest.Base.Ref = pr.Destination.Branch.Name
	fork = !strings.EqualFold(pr.Source.Repository.FullName, pr.Destination.Repository.FullName)
	return ev, fork, nil
}

// POST /api/v1/webhooks/bitbucket/{id} (HMAC in X-Hub-Signature: sha256=…)
func (s *Server) handleBitbucketWebhook(w http.ResponseWriter, r *http.Request) {
	svc, body, ok := s.webhookBody(w, r)
	if !ok {
		return
	}
	// Every service has a secret, so the signature is mandatory: a hook
	// without one could be sent by anyone who knows the URL.
	sig := r.Header.Get("X-Hub-Signature")
	if sig == "" || !verifyGitHubSignature(svc.WebhookSecret, body, sig) {
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}
	switch key := r.Header.Get("X-Event-Key"); key {
	case "diagnostics:ping":
		writeJSON(w, http.StatusOK, map[string]string{"status": "pong"})
	case "repo:push":
		ev, err := parseBitbucketPush(body, firstNonEmpty(svc.Source.Branch, "main"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid payload")
			return
		}
		s.handlePush(w, r, svc, ev)
	case "pullrequest:created", "pullrequest:updated", "pullrequest:fulfilled", "pullrequest:rejected":
		ev, fork, err := parseBitbucketPR(body, key)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid payload")
			return
		}
		if fork {
			ignored(w, "pull requests from forks do not get previews")
			return
		}
		s.handlePullRequestEvent(w, r, svc, ev)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "event": key})
	}
}

// Provider API calls: commit statuses and preview comments.

// providerAPIBase is the REST API root of a provider on host (a variable for tests).
var providerAPIBase = func(provider, host string) string {
	switch provider {
	case providerGitHub:
		return "https://api.github.com"
	case providerGitLab:
		return "https://" + host + "/api/v4"
	case providerGitea:
		return "https://" + host + "/api/v1"
	case providerBitbucket:
		return "https://api.bitbucket.org/2.0"
	}
	return ""
}

// providerRequest builds an authenticated API request to a provider.
func providerRequest(ctx context.Context, auth gitAuth, host, method, path string, body any) (*http.Request, error) {
	base := providerAPIBase(auth.Provider, host)
	if base == "" || auth.Token == "" {
		return nil, errors.New("no provider API")
	}
	if auth.Provider == providerBitbucket && host != "bitbucket.org" {
		return nil, errors.New("only Bitbucket Cloud is supported")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	switch auth.Provider {
	case providerGitLab:
		req.Header.Set("PRIVATE-TOKEN", auth.Token)
	case providerGitea:
		req.Header.Set("Authorization", "token "+auth.Token)
	case providerBitbucket:
		if auth.User == "" || auth.User == "x-token-auth" {
			req.Header.Set("Authorization", "Bearer "+auth.Token)
		} else {
			req.SetBasicAuth(auth.User, auth.Token)
		}
	default:
		req.Header.Set("Authorization", "Bearer "+auth.Token)
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	return req, nil
}

// statusCall returns the API path and body that set a commit status.
// state is GitHub's: pending, success, failure or error.
func statusCall(provider, repo, sha, state, name, desc, target string) (string, any, error) {
	p, err := repoPath(repo)
	if err != nil {
		return "", nil, err
	}
	switch provider {
	case providerGitLab:
		st := map[string]string{"pending": "running", "success": "success", "failure": "failed", "error": "canceled"}[state]
		return "/projects/" + url.PathEscape(p) + "/statuses/" + sha,
			map[string]string{"state": st, "name": name, "description": desc, "target_url": target}, nil
	case providerBitbucket:
		owner, name2, err := ownerRepo(p)
		if err != nil {
			return "", nil, err
		}
		st := map[string]string{"pending": "INPROGRESS", "success": "SUCCESSFUL", "failure": "FAILED", "error": "STOPPED"}[state]
		key := name
		if len(key) > 40 {
			key = key[:40]
		}
		return "/repositories/" + url.PathEscape(owner) + "/" + url.PathEscape(name2) + "/commit/" + sha + "/statuses/build",
			map[string]string{"key": key, "state": st, "name": name, "description": desc, "url": target}, nil
	case providerGitea, providerGitHub:
		owner, name2, err := ownerRepo(p)
		if err != nil {
			return "", nil, err
		}
		return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name2) + "/statuses/" + sha,
			map[string]string{"state": state, "context": name, "description": desc, "target_url": target}, nil
	}
	return "", nil, fmt.Errorf("unsupported provider %q", provider)
}

// commentCall returns the API path and body that comment on a pull / merge request.
func commentCall(provider, repo string, number int, text string) (string, any, error) {
	p, err := repoPath(repo)
	if err != nil {
		return "", nil, err
	}
	switch provider {
	case providerGitLab:
		return fmt.Sprintf("/projects/%s/merge_requests/%d/notes", url.PathEscape(p), number), map[string]string{"body": text}, nil
	case providerBitbucket:
		owner, name, err := ownerRepo(p)
		if err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("/repositories/%s/%s/pullrequests/%d/comments", url.PathEscape(owner), url.PathEscape(name), number),
			map[string]any{"content": map[string]string{"raw": text}}, nil
	case providerGitea, providerGitHub:
		owner, name, err := ownerRepo(p)
		if err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("/repos/%s/%s/issues/%d/comments", url.PathEscape(owner), url.PathEscape(name), number), map[string]string{"body": text}, nil
	}
	return "", nil, fmt.Errorf("unsupported provider %q", provider)
}

// providerPost sends one call for a service's repository with its credential.
// The host is tenant-chosen, so the call goes through the SSRF-guarded client.
func (s *Server) providerPost(ctx context.Context, svc *store.Service, build func(provider string) (string, any, error)) error {
	auth, err := s.gitToken(ctx, svc)
	if err != nil {
		return err
	}
	host, ok := repoHost(svc.Source.Repo)
	if !ok || auth.Token == "" || auth.Provider == "" {
		return errors.New("no credential for this repository")
	}
	path, body, err := build(auth.Provider)
	if err != nil {
		return err
	}
	req, err := providerRequest(ctx, auth, host, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	resp, err := notifyClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d", auth.Provider, path, resp.StatusCode)
	}
	return nil
}

// reportProviderStatus posts a commit status with the team's credential for the
// repository's host (GitLab, Gitea, Bitbucket, or GitHub with a team token).
func (s *Server) reportProviderStatus(ctx context.Context, svc *store.Service, sha, state, desc, target string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		err := s.providerPost(ctx, svc, func(provider string) (string, any, error) {
			return statusCall(provider, svc.Source.Repo, sha, state, "paas/"+svc.Slug, desc, target)
		})
		if err != nil {
			slog.Debug("commit status", "service", svc.Slug, "err", err)
		}
	}()
}
