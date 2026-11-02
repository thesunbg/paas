package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/thesunbg/paas/internal/github"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/store"
)

var githubRepoRe = regexp.MustCompile(`github\.com[/:]([^/]+)/([^/]+?)(?:\.git)?/?$`)

// verifyGitHubSignature checks the X-Hub-Signature-256 HMAC.
func verifyGitHubSignature(secret string, body []byte, header string) bool {
	if !strings.HasPrefix(header, "sha256=") {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

type pushEvent struct {
	Ref        string       `json:"ref"`
	After      string       `json:"after"`
	Deleted    bool         `json:"deleted"`
	HeadCommit *headCommit  `json:"head_commit"`
	Commits    []pushCommit `json:"commits"`
}

type headCommit struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

type pushCommit struct {
	Added    []string `json:"added"`
	Removed  []string `json:"removed"`
	Modified []string `json:"modified"`
}

// handleGitHubWebhook triggers a deployment when the configured branch receives a push.
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	svc, err := s.store.ServiceByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "unknown service")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 5<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body")
		return
	}
	sig := r.Header.Get("X-Hub-Signature-256")
	if sig == "" || !verifyGitHubSignature(svc.WebhookSecret, body, sig) {
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}
	s.handleGitHubEvent(w, r, svc, r.Header.Get("X-GitHub-Event"), body)
}

// handleGitHubEvent acts on a verified push or pull_request event for one service.
func (s *Server) handleGitHubEvent(w http.ResponseWriter, r *http.Request, svc *store.Service, event string, body []byte) {
	if event == "ping" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "pong"})
		return
	}
	if event == "pull_request" {
		s.handlePullRequest(w, r, svc, body)
		return
	}
	if event != "" && event != "push" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "event": event})
		return
	}
	var ev pushEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		writeError(w, http.StatusBadRequest, "invalid payload")
		return
	}
	s.handlePush(w, r, svc, ev)
}

// handlePush deploys a verified push (from any provider, mapped onto GitHub's
// shape) when it updates the service's branch.
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request, svc *store.Service, ev pushEvent) {
	branch := strings.TrimPrefix(ev.Ref, "refs/heads/")
	want := svc.Source.Branch
	if want == "" {
		want = "main"
	}
	if ev.After != "" && !validSHA(ev.After) {
		writeError(w, http.StatusBadRequest, "invalid commit sha")
		return
	}
	if ev.Deleted || branch != want {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "branch " + branch})
		return
	}
	if !svc.Source.AutoDeploy {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "auto deploy disabled"})
		return
	}
	if root := strings.Trim(svc.Source.RootDir, "/"); root != "" && root != "." && !touchesPath(ev, root+"/") {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "no changes under " + root})
		return
	}
	msg := ""
	if ev.HeadCommit != nil {
		msg = strings.SplitN(ev.HeadCommit.Message, "\n", 2)[0]
	}
	d, err := s.QueueDeployment(r.Context(), svc, "webhook", ev.After, msg, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued", "deployment_id": d.ID})
}

func touchesPath(ev pushEvent, prefix string) bool {
	if len(ev.Commits) == 0 {
		return true
	}
	for _, c := range ev.Commits {
		for _, list := range [][]string{c.Added, c.Removed, c.Modified} {
			for _, f := range list {
				if strings.HasPrefix(f, prefix) {
					return true
				}
			}
		}
	}
	return false
}

// reportCommitStatus posts a commit status to the repository's host when the
// service has credentials for it (GitHub App, its own token or a team credential).
func (s *Server) reportCommitStatus(ctx context.Context, svc *store.Service, d *store.Deployment, state string) {
	if svc == nil || d == nil || d.CommitSHA == "" || svc.Source.Kind != "git" || !validSHA(d.CommitSHA) {
		return
	}
	desc := map[string]string{"pending": "Deploying…", "success": "Deployed", "failure": "Deployment failed", "error": "Deployment cancelled"}[state]
	target := fmt.Sprintf("%s/#/services/%s/deployments/%s", s.cfg.PublicURL, svc.ID, d.ID)
	m := githubRepoRe.FindStringSubmatch(svc.Source.Repo)
	if m == nil || (svc.GitTokenEnc == "" && svc.Source.InstallationID == 0) {
		s.reportProviderStatus(ctx, svc, d.CommitSHA, state, desc, target)
		return
	}
	payload := map[string]string{"state": state, "context": "paas/" + svc.Slug, "description": desc, "target_url": target}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if err := s.githubPost(ctx, svc, fmt.Sprintf("/repos/%s/%s/statuses/%s", m[1], m[2], d.CommitSHA), payload); err != nil {
			slog.Debug("github status", "err", err)
		}
	}()
}

// githubPost calls the GitHub API for a service with its app installation or personal token.
func (s *Server) githubPost(ctx context.Context, svc *store.Service, path string, body any) error {
	if svc.Source.InstallationID != 0 {
		c, err := s.githubApp(ctx)
		if err != nil {
			return err
		}
		return c.Post(ctx, svc.Source.InstallationID, path, body)
	}
	token, err := s.box.Decrypt(svc.GitTokenEnc)
	if err != nil || token == "" {
		return errors.New("no git token")
	}
	payload, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.github.com"+path, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("github %s: HTTP %d", path, resp.StatusCode)
	}
	return nil
}

// gitToken returns the credential used to clone a service's repository: the
// GitHub App installation, the service's own token, or the team's credential
// for the repository's host (never a credential of another host).
func (s *Server) gitToken(ctx context.Context, svc *store.Service) (gitAuth, error) {
	if svc.Source.InstallationID != 0 {
		c, err := s.githubApp(ctx)
		if err != nil {
			return gitAuth{}, err
		}
		inst, err := s.store.GitHubInstallation(ctx, svc.Source.InstallationID)
		if err != nil {
			return gitAuth{}, err
		}
		tok, err := repoToken(ctx, c, inst, svc.Source.Repo)
		if err != nil {
			return gitAuth{}, err
		}
		return gitAuth{Provider: providerGitHub, User: defaultGitUser(providerGitHub), Token: tok}, nil
	}
	if svc.GitTokenEnc != "" {
		token, err := s.box.Decrypt(svc.GitTokenEnc)
		if err != nil {
			return gitAuth{}, fmt.Errorf("decrypt git token: %w", err)
		}
		if token != "" {
			host, _ := repoHost(svc.Source.Repo)
			p := knownGitHosts[host]
			return gitAuth{Provider: p, User: defaultGitUser(p), Token: token}, nil
		}
	}
	if svc.ProjectID == "" {
		return gitAuth{}, nil
	}
	return s.teamGitAuth(ctx, s.teamOfService(ctx, svc), svc.Source.Repo)
}

// repoToken mints a token limited to repoURL, which must belong to the
// installation's own account; builds run untrusted code and must not receive
// access to the account's other repositories.
func repoToken(ctx context.Context, c *github.Client, inst *store.GitHubInstallation, repoURL string) (string, error) {
	name, err := githubRepo(repoURL, inst.AccountLogin)
	if err != nil {
		return "", err
	}
	return c.RepoToken(ctx, inst.InstallationID, name)
}

// githubRepo returns the repository name of an https://github.com/<account>/<name>
// URL, rejecting any other host or owner.
func githubRepo(repoURL, account string) (string, error) {
	bad := fmt.Errorf("repository must be github.com/%s/…", account)
	u, err := url.Parse(repoURL)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "github.com") {
		return "", bad
	}
	owner, name, ok := strings.Cut(strings.Trim(strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".git"), "/"), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") || !strings.EqualFold(owner, account) {
		return "", bad
	}
	return name, nil
}

var _ = model.TypeWeb

func hmacHex(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
