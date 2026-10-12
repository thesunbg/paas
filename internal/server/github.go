package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

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
	if !(sig != "" && verifyGitHubSignature(svc.WebhookSecret, body, sig)) && !hmac.Equal([]byte(r.URL.Query().Get("secret")), []byte(svc.WebhookSecret)) {
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	if event == "ping" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "pong"})
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

// reportCommitStatus posts a commit status to GitHub when the service has a token.
func (s *Server) reportCommitStatus(ctx context.Context, svc *store.Service, d *store.Deployment, state string) {
	if svc == nil || d == nil || svc.GitTokenEnc == "" || d.CommitSHA == "" || svc.Source.Kind != "git" {
		return
	}
	m := githubRepoRe.FindStringSubmatch(svc.Source.Repo)
	if m == nil {
		return
	}
	token, err := s.box.Decrypt(svc.GitTokenEnc)
	if err != nil || token == "" {
		return
	}
	desc := map[string]string{"pending": "Deploying…", "success": "Deployed", "failure": "Deployment failed"}[state]
	payload, _ := json.Marshal(map[string]string{
		"state": state, "context": "paas/" + svc.Slug, "description": desc,
		"target_url": fmt.Sprintf("%s/#/services/%s/deployments/%s", s.cfg.PublicURL, svc.ID, d.ID),
	})
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		url := fmt.Sprintf("https://api.github.com/repos/%s/%s/statuses/%s", m[1], m[2], d.CommitSHA)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			slog.Debug("github status", "err", err)
			return
		}
		resp.Body.Close()
	}()
}

var _ = model.TypeWeb
