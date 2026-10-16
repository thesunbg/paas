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
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	HeadCommit *struct {
		ID      string `json:"id"`
		Message string `json:"message"`
	} `json:"head_commit"`
	Commits []struct {
		Added    []string `json:"added"`
		Removed  []string `json:"removed"`
		Modified []string `json:"modified"`
	} `json:"commits"`
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

// reportCommitStatus posts a commit status to GitHub when the service has GitHub access.
func (s *Server) reportCommitStatus(ctx context.Context, svc *store.Service, d *store.Deployment, state string) {
	if svc == nil || d == nil || d.CommitSHA == "" || svc.Source.Kind != "git" || !validSHA(d.CommitSHA) {
		return
	}
	if svc.GitTokenEnc == "" && svc.Source.InstallationID == 0 {
		return
	}
	m := githubRepoRe.FindStringSubmatch(svc.Source.Repo)
	if m == nil {
		return
	}
	desc := map[string]string{"pending": "Deploying…", "success": "Deployed", "failure": "Deployment failed"}[state]
	payload := map[string]string{
		"state": state, "context": "paas/" + svc.Slug, "description": desc,
		"target_url": fmt.Sprintf("%s/#/services/%s/deployments/%s", s.cfg.PublicURL, svc.ID, d.ID),
	}
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

// gitToken returns the credential used to clone a service's repository.
func (s *Server) gitToken(ctx context.Context, svc *store.Service) (string, error) {
	if svc.Source.InstallationID != 0 {
		c, err := s.githubApp(ctx)
		if err != nil {
			return "", err
		}
		inst, err := s.store.GitHubInstallation(ctx, svc.Source.InstallationID)
		if err != nil {
			return "", err
		}
		return repoToken(ctx, c, inst, svc.Source.Repo)
	}
	token, err := s.box.Decrypt(svc.GitTokenEnc)
	if err != nil {
		return "", fmt.Errorf("decrypt git token: %w", err)
	}
	return token, nil
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
