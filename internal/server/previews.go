package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/resources"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

type pullRequestEvent struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Title string `json:"title"`
		Head  struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo struct {
				CloneURL string `json:"clone_url"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"pull_request"`
}

// handlePullRequest creates, updates or removes the preview deployment of a pull request.
func (s *Server) handlePullRequest(w http.ResponseWriter, r *http.Request, svc *store.Service, body []byte) {
	if !svc.Spec.Previews || svc.ParentID != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "previews disabled"})
		return
	}
	var ev pullRequestEvent
	if err := json.Unmarshal(body, &ev); err != nil || ev.Number == 0 {
		writeError(w, http.StatusBadRequest, "invalid payload")
		return
	}
	want := firstNonEmpty(svc.Source.Branch, "main")
	if ev.PullRequest.Base.Ref != want {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "pull request targets " + ev.PullRequest.Base.Ref})
		return
	}
	if u := ev.PullRequest.Head.Repo.CloneURL; u != "" && !sameRepo(u, svc.Source.Repo) {
		// Never run code from forks with this service's secrets and git token.
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "pull requests from forks do not get previews"})
		return
	}
	if !validSHA(ev.PullRequest.Head.SHA) {
		writeError(w, http.StatusBadRequest, "invalid head sha")
		return
	}
	ctx := context.WithoutCancel(r.Context())
	switch ev.Action {
	case "opened", "reopened", "synchronize", "ready_for_review":
		preview, created, err := s.ensurePreview(ctx, svc, ev)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		d, err := s.QueueDeployment(ctx, preview, "preview", ev.PullRequest.Head.SHA, ev.PullRequest.Title, nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if created {
			go s.commentPreview(svc, preview, ev.Number)
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued", "service_id": preview.ID, "deployment_id": d.ID})
	case "closed":
		preview, err := s.store.PreviewFor(ctx, svc.ID, ev.Number)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "no preview"})
			return
		}
		if err := s.deleteService(ctx, preview); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "action": ev.Action})
	}
}

// ensurePreview returns the preview service of a pull request, creating it from the parent.
func (s *Server) ensurePreview(ctx context.Context, parent *store.Service, ev pullRequestEvent) (*store.Service, bool, error) {
	repo := parent.Source.Repo
	if existing, err := s.store.PreviewFor(ctx, parent.ID, ev.Number); err == nil {
		existing.Source.Repo, existing.Source.Branch = repo, ev.PullRequest.Head.Ref
		if err := s.store.UpdateServiceConfig(ctx, existing); err != nil {
			return nil, false, err
		}
		if existing.Status == model.StatusStopped {
			_ = s.store.SetServiceStatus(ctx, existing.ID, model.StatusDeploying)
		}
		return existing, false, nil
	}
	proj, err := s.store.ProjectByID(ctx, parent.ProjectID)
	if err != nil {
		return nil, false, err
	}
	spec := parent.Spec
	spec.Replicas = 1
	spec.Previews = false
	spec.Autoscale = model.Autoscale{}
	spec.Volumes = nil
	if parent.Type == model.TypeWeb {
		// Previews are cheap: they sleep when idle and are the first to go under memory pressure.
		spec.Sleep = model.SleepPolicy{Enabled: true, IdleMinutes: 15}
	}
	size := resources.Size{MemoryLimitMB: spec.Resources.MemoryLimitMB, CPULimitMillis: spec.Resources.CPULimitMillis}
	resources.Apply(&spec, size, model.QoSBestEffort)
	pr := ev.Number
	prv := &store.Service{
		ProjectID: parent.ProjectID, Name: fmt.Sprintf("%s (PR #%d)", parent.Name, pr), Slug: fmt.Sprintf("%s-pr-%d", parent.Slug, pr),
		Type: parent.Type, Source: model.ServiceSource{Kind: "git", Repo: repo, Branch: ev.PullRequest.Head.Ref, RootDir: parent.Source.RootDir, Dockerfile: parent.Source.Dockerfile, InstallationID: parent.Source.InstallationID},
		Build: parent.Build, Spec: spec, GitTokenEnc: parent.GitTokenEnc, WebhookSecret: util.RandomToken(20),
		ParentID: &parent.ID, PreviewPR: &pr,
	}
	created, err := s.store.CreateService(ctx, prv)
	if err != nil {
		return nil, false, err
	}
	vars, _ := s.store.EnvVars(ctx, parent.ID)
	for _, v := range vars {
		_ = s.store.SetEnvVar(ctx, created.ID, v.Key, v.ValueEnc)
	}
	_ = s.store.SetEnvVar(ctx, created.ID, "PAAS_PREVIEW", s.box.MustEncrypt("true"))
	_ = s.store.SetEnvVar(ctx, created.ID, "PAAS_PULL_REQUEST", s.box.MustEncrypt(fmt.Sprint(pr)))
	if _, err := s.store.AddDomain(ctx, created.ID, s.generatedDomain(created.Slug, proj.Slug), true); err != nil {
		slog.Warn("preview domain", "err", err)
	}
	slog.Info("preview created", "service", created.Slug, "pr", pr)
	return created, true, nil
}

// commentPreview posts the preview URL on the pull request (needs GitHub access).
func (s *Server) commentPreview(parent, preview *store.Service, pr int) {
	m := githubRepoRe.FindStringSubmatch(parent.Source.Repo)
	if m == nil || (parent.GitTokenEnc == "" && parent.Source.InstallationID == 0) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	doms, _ := s.store.DomainsForService(ctx, preview.ID)
	var urls []string
	for _, d := range doms {
		urls = append(urls, s.urlFor(d.Hostname))
	}
	body := fmt.Sprintf("🚀 Preview deployment for **%s**: %s\n\nIt updates on every push and is removed when this pull request is closed.", parent.Name, strings.Join(urls, ", "))
	if err := s.githubPost(ctx, parent, fmt.Sprintf("/repos/%s/%s/issues/%d/comments", m[1], m[2], pr), map[string]string{"body": body}); err != nil {
		slog.Debug("github comment", "err", err)
	}
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

func validSHA(s string) bool { return shaRe.MatchString(s) }

// sameRepo compares two git URLs ignoring scheme, credentials, case and a .git suffix.
func sameRepo(a, b string) bool {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		if i := strings.Index(s, "://"); i >= 0 {
			s = s[i+3:]
		}
		if i := strings.LastIndex(s, "@"); i >= 0 {
			s = s[i+1:]
		}
		return strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	}
	return norm(a) == norm(b)
}
