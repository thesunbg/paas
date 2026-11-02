package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/store"
)

func TestNormalizeGitHost(t *testing.T) {
	for in, want := range map[string]string{
		"gitlab.com":                  "gitlab.com",
		"GitLab.Example.com":          "gitlab.example.com",
		"https://git.example.com/":    "git.example.com",
		"https://git.example.com:443": "git.example.com",
		"git.example.com:8443":        "git.example.com:8443",
		"http://git.example.com":      "",
		"https://git.example.com/sub": "",
		"https://u:p@git.example.com": "",
		"git.example.com/group":       "",
		"":                            "",
		"-bad.com":                    "",
		"a..b":                        "",
		"git example.com":             "",
	} {
		got, err := normalizeGitHost(in)
		if (err == nil) != (want != "") || got != want {
			t.Errorf("normalizeGitHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestRepoHostAndPath(t *testing.T) {
	for in, want := range map[string]string{
		"https://gitlab.com/g/sub/r.git":   "gitlab.com",
		"https://GitLab.com:443/g/r":       "gitlab.com",
		"https://git.example.com:8443/o/r": "git.example.com:8443",
		"http://gitlab.com/g/r":            "",
		"https://tok@gitlab.com/g/r":       "",
		"git@gitlab.com:g/r.git":           "",
		"https:gitlab.com/g/r":             "",
		"/srv/repo":                        "",
	} {
		got, ok := repoHost(in)
		if ok != (want != "") || got != want {
			t.Errorf("repoHost(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for in, want := range map[string]string{
		"https://gitlab.com/g/sub/r.git": "g/sub/r",
		"https://codeberg.org/o/r/":      "o/r",
		"https://gitlab.com/r":           "",
		"https://gitlab.com/o//r":        "",
		"https://gitlab.com/o/../r":      "",
		"http://gitlab.com/o/r":          "",
	} {
		got, err := repoPath(in)
		if (err == nil) != (want != "") || got != want {
			t.Errorf("repoPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestWebhookVerification(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	if !verifyGitLabToken("s3cret", "s3cret") || verifyGitLabToken("s3cret", "other") || verifyGitLabToken("", "") || verifyGitLabToken("s3cret", "") {
		t.Fatal("gitlab token check")
	}
	sig := hmacHex("s3cret", body)
	if !verifyGiteaSignature("s3cret", body, sig) || !verifyGiteaSignature("s3cret", body, strings.ToUpper(sig)) {
		t.Fatal("valid gitea signature rejected")
	}
	if verifyGiteaSignature("s3cret", body, "sha256="+sig) || verifyGiteaSignature("other", body, sig) || verifyGiteaSignature("s3cret", body, "") ||
		verifyGiteaSignature("", body, hmacHex("", body)) || verifyGiteaSignature("s3cret", []byte("x"), sig) {
		t.Fatal("bad gitea signature accepted")
	}
	// Bitbucket uses GitHub's "sha256=<hex>" form in X-Hub-Signature.
	if !verifyGitHubSignature("s3cret", body, "sha256="+sig) || verifyGitHubSignature("s3cret", body, sig) {
		t.Fatal("bitbucket signature check")
	}
}

func TestParseProviderEvents(t *testing.T) {
	after := strings.Repeat("ab", 20)
	ev, err := parseGitLabPush([]byte(`{"object_kind":"push","ref":"refs/heads/main","after":"` + after + `","commits":[
		{"id":"1111111","message":"old","added":["a/x"]},{"id":"` + after + `","message":"fix: thing\nbody","modified":["b/y"]}]}`))
	if err != nil || ev.Ref != "refs/heads/main" || ev.After != after || ev.Deleted || ev.HeadCommit == nil || ev.HeadCommit.Message != "fix: thing\nbody" {
		t.Fatalf("gitlab push = %+v, %v", ev, err)
	}
	if !touchesPath(ev, "b/") || touchesPath(ev, "c/") {
		t.Fatal("gitlab push file lists not mapped")
	}
	if ev, _ := parseGitLabPush([]byte(`{"ref":"refs/heads/main","after":"0000000000000000000000000000000000000000"}`)); !ev.Deleted {
		t.Fatal("branch deletion not detected")
	}

	mr := `{"object_kind":"merge_request","object_attributes":{"iid":7,"action":"update","oldrev":"abc","title":"T","source_branch":"feat","target_branch":"main",
		"source_project_id":1,"target_project_id":1,"last_commit":{"id":"` + after + `"},"source":{"git_http_url":"https://gitlab.com/g/r.git"}}}`
	pr, fork, err := parseGitLabMR([]byte(mr))
	if err != nil || fork || pr.Action != "synchronize" || pr.Number != 7 || pr.PullRequest.Head.Ref != "feat" || pr.PullRequest.Base.Ref != "main" || pr.PullRequest.Head.SHA != after {
		t.Fatalf("gitlab mr = %+v fork=%v %v", pr, fork, err)
	}
	if pr, fork, _ := parseGitLabMR([]byte(strings.Replace(strings.Replace(mr, `"oldrev":"abc",`, "", 1), `"source_project_id":1`, `"source_project_id":2`, 1))); !fork || pr.Action != "edited" {
		t.Fatalf("gitlab fork mr = %+v fork=%v", pr, fork)
	}
	for action, want := range map[string]string{"open": "opened", "reopen": "reopened", "merge": "closed", "close": "closed"} {
		if pr, _, _ := parseGitLabMR([]byte(`{"object_attributes":{"iid":1,"action":"` + action + `"}}`)); pr.Action != want {
			t.Errorf("gitlab action %s = %s", action, pr.Action)
		}
	}

	bb := `{"push":{"changes":[
		{"new":{"type":"branch","name":"dev","target":{"hash":"` + strings.Repeat("1", 40) + `","message":"dev"}},"closed":false},
		{"new":{"type":"branch","name":"main","target":{"hash":"` + after + `","message":"ship it"}},"closed":false},
		{"new":null,"old":{"type":"branch","name":"gone"},"closed":true}]}}`
	if ev, err := parseBitbucketPush([]byte(bb), "main"); err != nil || ev.Ref != "refs/heads/main" || ev.After != after || ev.HeadCommit.Message != "ship it" || ev.Deleted {
		t.Fatalf("bitbucket push = %+v, %v", ev, err)
	}
	if ev, _ := parseBitbucketPush([]byte(bb), "gone"); !ev.Deleted {
		t.Fatal("bitbucket deleted branch not detected")
	}
	if ev, _ := parseBitbucketPush([]byte(bb), "other"); ev.Ref != "refs/heads/dev" {
		t.Fatalf("bitbucket push for another branch = %q", ev.Ref)
	}
	bpr := `{"pullrequest":{"id":3,"title":"T","source":{"branch":{"name":"feat"},"commit":{"hash":"abcdef123456"},"repository":{"full_name":"ws/r"}},
		"destination":{"branch":{"name":"main"},"repository":{"full_name":"WS/r"}}}}`
	if pr, fork, err := parseBitbucketPR([]byte(bpr), "pullrequest:updated"); err != nil || fork || pr.Action != "synchronize" || pr.Number != 3 || pr.PullRequest.Head.SHA != "abcdef123456" {
		t.Fatalf("bitbucket pr = %+v fork=%v %v", pr, fork, err)
	}
	if _, fork, _ := parseBitbucketPR([]byte(strings.Replace(bpr, `"full_name":"ws/r"`, `"full_name":"fork/r"`, 1)), "pullrequest:created"); !fork {
		t.Fatal("bitbucket fork not detected")
	}
}

func TestProviderCalls(t *testing.T) {
	sha := strings.Repeat("a", 40)
	path, body, err := statusCall(providerGitLab, "https://gitlab.com/g/sub/r.git", sha, "failure", "paas/web", "d", "https://x")
	if err != nil || path != "/projects/g%2Fsub%2Fr/statuses/"+sha || body.(map[string]string)["state"] != "failed" {
		t.Fatalf("gitlab status = %q %v %v", path, body, err)
	}
	path, body, err = statusCall(providerGitea, "https://codeberg.org/o/r", sha, "pending", "paas/web", "d", "https://x")
	if err != nil || path != "/repos/o/r/statuses/"+sha || body.(map[string]string)["context"] != "paas/web" {
		t.Fatalf("gitea status = %q %v %v", path, body, err)
	}
	path, body, err = statusCall(providerBitbucket, "https://bitbucket.org/ws/r.git", sha, "success", "paas/web", "d", "https://x")
	if err != nil || path != "/repositories/ws/r/commit/"+sha+"/statuses/build" || body.(map[string]string)["state"] != "SUCCESSFUL" {
		t.Fatalf("bitbucket status = %q %v %v", path, body, err)
	}
	if _, _, err := statusCall(providerGitea, "https://gitea.example.com/g/sub/r", sha, "success", "n", "d", "t"); err == nil {
		t.Fatal("gitea accepted a three-segment path")
	}
	if path, _, err := commentCall(providerGitLab, "https://gitlab.com/g/r", 4, "hi"); err != nil || path != "/projects/g%2Fr/merge_requests/4/notes" {
		t.Fatalf("gitlab comment = %q %v", path, err)
	}

	ctx := context.Background()
	req, err := providerRequest(ctx, gitAuth{Provider: providerGitLab, Token: "glpat"}, "gitlab.example.com", "POST", "/projects/g%2Fr/statuses/"+sha, nil)
	if err != nil || req.URL.String() != "https://gitlab.example.com/api/v4/projects/g%2Fr/statuses/"+sha || req.Header.Get("PRIVATE-TOKEN") != "glpat" {
		t.Fatalf("gitlab request = %v %v", req, err)
	}
	if req, _ := providerRequest(ctx, gitAuth{Provider: providerGitea, Token: "tk"}, "codeberg.org", "POST", "/x", nil); req.Header.Get("Authorization") != "token tk" || req.URL.Host != "codeberg.org" {
		t.Fatalf("gitea request = %v", req)
	}
	if req, _ := providerRequest(ctx, gitAuth{Provider: providerBitbucket, User: "x-token-auth", Token: "tk"}, "bitbucket.org", "POST", "/x", nil); req.Header.Get("Authorization") != "Bearer tk" || req.URL.Host != "api.bitbucket.org" {
		t.Fatalf("bitbucket token request = %v", req)
	}
	if req, _ := providerRequest(ctx, gitAuth{Provider: providerBitbucket, User: "alice", Token: "pw"}, "bitbucket.org", "POST", "/x", nil); !strings.HasPrefix(req.Header.Get("Authorization"), "Basic ") {
		t.Fatalf("bitbucket app password request = %v", req)
	}
	if _, err := providerRequest(ctx, gitAuth{Provider: providerGitLab}, "gitlab.com", "POST", "/x", nil); err == nil {
		t.Fatal("request without a token")
	}
}

func TestValidateGitCredential(t *testing.T) {
	for _, tc := range []struct {
		provider, host, user, token string
		wantHost                    string
	}{
		{"gitlab", "", "", "glpat-x", "gitlab.com"},
		{"GitLab", "https://gitlab.example.com/", "", "glpat-x", "gitlab.example.com"},
		{"gitea", "codeberg.org", "", "t", "codeberg.org"},
		{"bitbucket", "", "alice", "pw", "bitbucket.org"},
		{"github", "", "", "ghp_x", "github.com"},
		{"gitea", "", "", "t", ""},                   // no default host
		{"gitea", "gitlab.com", "", "t", ""},         // known host of another provider
		{"bitbucket", "bb.example.com", "", "t", ""}, // only Bitbucket Cloud
		{"svn", "x.com", "", "t", ""},
		{"gitlab", "", "", "", ""},
		{"gitlab", "", "", "a b", ""},
		{"gitlab", "", "", "a\nb", ""},
		{"gitlab", "", "bad user", "t", ""},
	} {
		c, err := validateGitCredential(tc.provider, tc.host, tc.user, tc.token, "")
		if (err == nil) != (tc.wantHost != "") || (err == nil && c.Host != tc.wantHost) {
			t.Errorf("validate(%+v) = %+v, %v", tc, c, err)
		}
	}
}

// gitEnv is a team with an owner, a developer and a viewer, and a project.
type gitEnv struct {
	s                *Server
	h                http.Handler
	team             string
	owner, dev, view string
	proj             *store.Project
}

func newGitEnv(t *testing.T) *gitEnv {
	t.Helper()
	s := newTestServerBus(t)
	h := s.routes()
	ctx := context.Background()
	owner := mkUser(t, s, "owner@example.com", "password1", false)
	team, err := s.store.CreateTeam(ctx, "t", "t", owner.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for email, role := range map[string]string{"dev@example.com": "developer", "view@example.com": "viewer"} {
		u := mkUser(t, s, email, "password1", false)
		if err := s.store.AddMember(ctx, team.ID, u.ID, role); err != nil {
			t.Fatal(err)
		}
	}
	proj, err := s.store.CreateProject(ctx, team.ID, "p", "p")
	if err != nil {
		t.Fatal(err)
	}
	return &gitEnv{s: s, h: h, team: team.ID, proj: proj,
		owner: mustLogin(t, h, "owner@example.com", "password1"),
		dev:   mustLogin(t, h, "dev@example.com", "password1"),
		view:  mustLogin(t, h, "view@example.com", "password1")}
}

func (e *gitEnv) service(t *testing.T, slug, repo string) *store.Service {
	t.Helper()
	svc, err := e.s.store.CreateService(context.Background(), &store.Service{ProjectID: e.proj.ID, Name: slug, Slug: slug, Type: model.TypeWeb,
		Source: model.ServiceSource{Kind: "git", Repo: repo, Branch: "main", AutoDeploy: true}, WebhookSecret: "hooksecret"})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestGitCredentialsAPI(t *testing.T) {
	e := newGitEnv(t)
	base := "/api/v1/teams/" + e.team + "/git-credentials"
	in := map[string]string{"provider": "gitlab", "host": "gitlab.example.com", "token": "glpat-secret", "name": "work"}
	if w := do(t, e.h, "POST", base, e.dev, in); w.Code != 403 {
		t.Fatalf("developer create = %d %s", w.Code, w.Body)
	}
	w := do(t, e.h, "POST", base, e.owner, in)
	if w.Code != 201 || strings.Contains(w.Body.String(), "glpat-secret") {
		t.Fatalf("owner create = %d %s", w.Code, w.Body)
	}
	var c store.GitCredential
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	if c.Host != "gitlab.example.com" || c.Provider != "gitlab" || c.ID == "" {
		t.Fatalf("created = %+v", c)
	}
	// Same host again replaces the token.
	in["token"] = "glpat-rotated"
	if w := do(t, e.h, "POST", base, e.owner, in); w.Code != 201 {
		t.Fatalf("rotate = %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, "POST", base, e.owner, map[string]string{"provider": "gitea", "token": "x"}); w.Code != 400 {
		t.Fatalf("gitea without host = %d", w.Code)
	}
	w = do(t, e.h, "GET", base, e.view, nil)
	var list struct {
		Credentials []store.GitCredential `json:"credentials"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if w.Code != 200 || len(list.Credentials) != 1 || strings.Contains(w.Body.String(), "glpat") || strings.Contains(w.Body.String(), "token_enc") {
		t.Fatalf("list = %d %s", w.Code, w.Body)
	}
	cred, err := e.s.store.GitCredentialForHost(context.Background(), e.team, "gitlab.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if tok, _ := e.s.box.Decrypt(cred.TokenEnc); tok != "glpat-rotated" || cred.TokenEnc == "glpat-rotated" {
		t.Fatalf("stored token = %q (enc %q)", tok, cred.TokenEnc)
	}

	// Another team's admin cannot see or delete it.
	mkUser(t, e.s, "other@example.com", "password1", false)
	other := mustLogin(t, e.h, "other@example.com", "password1")
	if w := do(t, e.h, "GET", base, other, nil); w.Code != 403 {
		t.Fatalf("outsider list = %d", w.Code)
	}
	if w := do(t, e.h, "DELETE", base+"/"+c.ID, other, nil); w.Code != 403 {
		t.Fatalf("outsider delete = %d", w.Code)
	}
	if w := do(t, e.h, "DELETE", base+"/"+c.ID, e.dev, nil); w.Code != 403 {
		t.Fatalf("developer delete = %d", w.Code)
	}
	if w := do(t, e.h, "DELETE", base+"/not-a-uuid", e.owner, nil); w.Code != 404 {
		t.Fatalf("bad id delete = %d", w.Code)
	}
	if w := do(t, e.h, "DELETE", base+"/"+c.ID, e.owner, nil); w.Code != 200 {
		t.Fatalf("delete = %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, "DELETE", base+"/"+c.ID, e.owner, nil); w.Code != 404 {
		t.Fatalf("second delete = %d", w.Code)
	}
}

// A team credential is sent only for a repository on its own host, and only
// for the team's own services.
func TestGitCredentialHostScoping(t *testing.T) {
	e := newGitEnv(t)
	ctx := context.Background()
	if w := do(t, e.h, "POST", "/api/v1/teams/"+e.team+"/git-credentials", e.owner,
		map[string]string{"provider": "gitlab", "host": "gitlab.com", "token": "glpat-secret"}); w.Code != 201 {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	for repo, want := range map[string]string{
		"https://gitlab.com/g/r.git":          "glpat-secret",
		"https://GITLAB.com:443/g/sub/r":      "glpat-secret",
		"https://gitlab.com.evil.com/g/r":     "",
		"https://evil.com/gitlab.com/g/r":     "",
		"https://gitlab.com:8443/g/r":         "",
		"http://gitlab.com/g/r":               "",
		"https://user@gitlab.com/g/r":         "",
		"https://github.com/gitlab.com/g/r":   "",
		"https://evil.com#@gitlab.com/g/r":    "",
		"https://evil.com\\@gitlab.com/g/r":   "",
		"https://evil.com/?h=gitlab.com/g/r":  "",
		"https://gitlab.com@evil.com/g/r.git": "",
	} {
		svc := &store.Service{ProjectID: e.proj.ID, Source: model.ServiceSource{Kind: "git", Repo: repo}}
		auth, err := e.s.gitToken(ctx, svc)
		if err != nil || auth.Token != want {
			t.Errorf("gitToken(%q) = %+v, %v; want token %q", repo, auth, err, want)
		}
		if want != "" && (auth.User != "oauth2" || auth.Provider != providerGitLab) {
			t.Errorf("gitToken(%q) user/provider = %+v", repo, auth)
		}
	}
	// A service of another team never gets it.
	owner2 := mkUser(t, e.s, "o2@example.com", "password1", false)
	team2, _ := e.s.store.CreateTeam(ctx, "t2", "t2", owner2.ID, false)
	p2, _ := e.s.store.CreateProject(ctx, team2.ID, "p2", "p2")
	if auth, err := e.s.gitToken(ctx, &store.Service{ProjectID: p2.ID, Source: model.ServiceSource{Kind: "git", Repo: "https://gitlab.com/g/r"}}); err != nil || auth.Token != "" {
		t.Fatalf("other team got %+v, %v", auth, err)
	}
	// The service's own token wins over the team credential.
	svc := &store.Service{ProjectID: e.proj.ID, GitTokenEnc: e.s.box.MustEncrypt("own"), Source: model.ServiceSource{Kind: "git", Repo: "https://gitlab.com/g/r"}}
	if auth, _ := e.s.gitToken(ctx, svc); auth.Token != "own" {
		t.Fatalf("own token = %+v", auth)
	}
}

func TestProviderWebhooks(t *testing.T) {
	e := newGitEnv(t)
	gl := e.service(t, "gl", "https://gitlab.com/g/r.git")
	gt := e.service(t, "gt", "https://codeberg.org/o/r.git")
	bb := e.service(t, "bb", "https://bitbucket.org/ws/r.git")
	sha := strings.Repeat("c", 40)
	post := func(path string, hdr map[string]string, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		return w
	}
	status := func(w *httptest.ResponseRecorder) string {
		var out map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out["status"]
	}

	// GitLab
	push := `{"ref":"refs/heads/main","after":"` + sha + `","commits":[{"id":"` + sha + `","message":"m"}]}`
	if w := post("/api/v1/webhooks/gitlab/"+gl.ID, map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": "wrong"}, push); w.Code != 401 {
		t.Fatalf("gitlab bad token = %d", w.Code)
	}
	if w := post("/api/v1/webhooks/gitlab/"+gl.ID, map[string]string{"X-Gitlab-Event": "Push Hook"}, push); w.Code != 401 {
		t.Fatalf("gitlab no token = %d", w.Code)
	}
	if w := post("/api/v1/webhooks/gitlab/"+gl.ID, map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": "hooksecret"},
		strings.Replace(push, "refs/heads/main", "refs/heads/dev", 1)); w.Code != 200 || status(w) != "ignored" {
		t.Fatalf("gitlab other branch = %d %s", w.Code, w.Body)
	}
	if w := post("/api/v1/webhooks/gitlab/"+gl.ID, map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": "hooksecret"}, push); w.Code != 202 || status(w) != "queued" {
		t.Fatalf("gitlab push = %d %s", w.Code, w.Body)
	}
	// A GitLab token on another provider's endpoint does not work.
	if w := post("/api/v1/webhooks/gitea/"+gl.ID, map[string]string{"X-Gitea-Event": "push", "X-Gitlab-Token": "hooksecret"}, push); w.Code != 401 {
		t.Fatalf("gitea endpoint with gitlab token = %d", w.Code)
	}

	// Gitea / Forgejo
	gpush := `{"ref":"refs/heads/main","after":"` + sha + `","head_commit":{"id":"` + sha + `","message":"m"}}`
	if w := post("/api/v1/webhooks/gitea/"+gt.ID, map[string]string{"X-Gitea-Event": "push", "X-Gitea-Signature": hmacHex("nope", []byte(gpush))}, gpush); w.Code != 401 {
		t.Fatalf("gitea bad signature = %d", w.Code)
	}
	if w := post("/api/v1/webhooks/gitea/"+gt.ID, map[string]string{"X-Forgejo-Event": "push", "X-Forgejo-Signature": hmacHex("hooksecret", []byte(gpush))}, gpush); w.Code != 202 {
		t.Fatalf("forgejo push = %d %s", w.Code, w.Body)
	}
	// Previews are off: pull requests are ignored.
	gpr := `{"action":"opened","number":1,"pull_request":{"head":{"ref":"f","sha":"` + sha + `"},"base":{"ref":"main"}}}`
	if w := post("/api/v1/webhooks/gitea/"+gt.ID, map[string]string{"X-Gitea-Event": "pull_request", "X-Gitea-Signature": hmacHex("hooksecret", []byte(gpr))}, gpr); w.Code != 200 || status(w) != "ignored" {
		t.Fatalf("gitea pr = %d %s", w.Code, w.Body)
	}

	// Bitbucket
	bpush := `{"push":{"changes":[{"new":{"type":"branch","name":"main","target":{"hash":"` + sha + `","message":"m"}}}]}}`
	if w := post("/api/v1/webhooks/bitbucket/"+bb.ID, map[string]string{"X-Event-Key": "repo:push"}, bpush); w.Code != 401 {
		t.Fatalf("bitbucket unsigned = %d", w.Code)
	}
	if w := post("/api/v1/webhooks/bitbucket/"+bb.ID, map[string]string{"X-Event-Key": "repo:push", "X-Hub-Signature": "sha256=" + hmacHex("hooksecret", []byte(bpush))}, bpush); w.Code != 202 {
		t.Fatalf("bitbucket push = %d %s", w.Code, w.Body)
	}

	deps, err := e.s.store.DeploymentsForService(context.Background(), gl.ID, 10)
	if err != nil || len(deps) != 1 || deps[0].CommitSHA != sha || deps[0].Trigger != "webhook" {
		t.Fatalf("gitlab deployments = %+v, %v", deps, err)
	}
}

// Preview environments work from a GitLab merge request, and forks are refused.
func TestGitLabMergeRequestPreview(t *testing.T) {
	e := newGitEnv(t)
	svc := e.service(t, "web", "https://gitlab.com/g/r.git")
	if _, err := e.s.store.UpdateService(context.Background(), svc.ID, func(f *store.Service) error { f.Spec.Previews = true; return nil }); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("d", 40)
	mr := func(action string, src int) string {
		return `{"object_attributes":{"iid":5,"action":"` + action + `","title":"T","source_branch":"feat","target_branch":"main","source_project_id":` +
			strconv.Itoa(src) + `,"target_project_id":1,"last_commit":{"id":"` + sha + `"},"source":{"git_http_url":"https://gitlab.com/g/r.git"}}}`
	}
	send := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/webhooks/gitlab/"+svc.ID, strings.NewReader(body))
		r.Header.Set("X-Gitlab-Event", "Merge Request Hook")
		r.Header.Set("X-Gitlab-Token", "hooksecret")
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		return w
	}
	if w := send(mr("open", 2)); w.Code != 200 || !strings.Contains(w.Body.String(), "forks") {
		t.Fatalf("fork mr = %d %s", w.Code, w.Body)
	}
	if w := send(mr("open", 1)); w.Code != 202 {
		t.Fatalf("open mr = %d %s", w.Code, w.Body)
	}
	pv, err := e.s.store.PreviewFor(context.Background(), svc.ID, 5)
	if err != nil || pv.Source.Branch != "feat" {
		t.Fatalf("preview = %+v, %v", pv, err)
	}
}

func TestServiceWebhookInfo(t *testing.T) {
	e := newGitEnv(t)
	e.s.cfg.PublicURL = "https://paas.test"
	svc := e.service(t, "web", "https://git.example.com/o/r.git")
	path := "/api/v1/services/" + svc.ID + "/webhook"
	if w := do(t, e.h, "GET", path, e.view, nil); w.Code != 403 {
		t.Fatalf("viewer = %d", w.Code)
	}
	var out struct {
		Provider, URL, Secret string
	}
	w := do(t, e.h, "GET", path, e.dev, nil)
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 || out.Provider != "" || out.URL != "https://paas.test/api/v1/webhooks/github/"+svc.ID || out.Secret != "hooksecret" {
		t.Fatalf("unknown host = %d %s", w.Code, w.Body)
	}
	// A credential for the host names its provider.
	if w := do(t, e.h, "POST", "/api/v1/teams/"+e.team+"/git-credentials", e.owner,
		map[string]string{"provider": "gitea", "host": "git.example.com", "token": "t"}); w.Code != 201 {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	w = do(t, e.h, "GET", path, e.dev, nil)
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out.Provider != "gitea" || out.URL != "https://paas.test/api/v1/webhooks/gitea/"+svc.ID {
		t.Fatalf("gitea host = %s", w.Body)
	}
	w = do(t, e.h, "GET", "/api/v1/services/"+svc.ID, e.dev, nil)
	if !strings.Contains(w.Body.String(), `"git_provider":"gitea"`) || !strings.Contains(w.Body.String(), "/webhooks/gitea/") {
		t.Fatalf("get service = %s", w.Body)
	}
}

// Commit statuses go to the provider's API with the team credential.
func TestProviderCommitStatus(t *testing.T) {
	e := newGitEnv(t)
	got := make(chan *http.Request, 1)
	bodies := make(chan string, 1)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- r
		bodies <- string(b)
		w.WriteHeader(201)
	}))
	defer api.Close()
	oldBase, oldPriv := providerAPIBase, AllowPrivateNotifyTargets
	providerAPIBase = func(provider, host string) string { return api.URL + "/" + provider + "/" + host }
	AllowPrivateNotifyTargets = true
	defer func() { providerAPIBase, AllowPrivateNotifyTargets = oldBase, oldPriv }()

	if w := do(t, e.h, "POST", "/api/v1/teams/"+e.team+"/git-credentials", e.owner,
		map[string]string{"provider": "gitlab", "token": "glpat-x"}); w.Code != 201 {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	svc := e.service(t, "web", "https://gitlab.com/g/r.git")
	sha := strings.Repeat("e", 40)
	e.s.reportCommitStatus(context.Background(), svc, &store.Deployment{ID: "d1", CommitSHA: sha}, "success")
	r := <-got
	body := <-bodies
	if r.URL.EscapedPath() != "/gitlab/gitlab.com/projects/g%2Fr/statuses/"+sha || r.Header.Get("PRIVATE-TOKEN") != "glpat-x" || !strings.Contains(body, `"state":"success"`) {
		t.Fatalf("status call %s %v %s", r.URL.EscapedPath(), r.Header, body)
	}
}
