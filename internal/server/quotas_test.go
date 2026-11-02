package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/resources"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/templates"
	"github.com/thesunbg/paas/internal/util"
)

func sized(name, qos string, replicas int) model.ServiceSpec {
	spec := model.ServiceSpec{Replicas: replicas}
	size, _ := resources.SizeByName(name)
	resources.Apply(&spec, size, qos)
	return spec
}

func mkService(t *testing.T, s *Server, projectID, slug, typ string, spec model.ServiceSpec, parent *string) *store.Service {
	t.Helper()
	svc := &store.Service{ProjectID: projectID, Name: slug, Slug: slug, Type: typ, Source: model.ServiceSource{Kind: "image", Image: "nginx"},
		Spec: spec, WebhookSecret: util.RandomToken(8), ParentID: parent}
	if parent != nil {
		pr := 7
		svc.PreviewPR = &pr
	}
	out, err := s.store.CreateService(context.Background(), svc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestQuotaAccounting(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()
	team, _ := s.store.CreateTeam(ctx, "T", "t", "", false)
	p, err := s.store.CreateProject(ctx, team.ID, "P", "p")
	if err != nil {
		t.Fatal(err)
	}
	s.store.CreateProject(ctx, team.ID, "Q", "q")
	// small burstable: request 160 MB / 100 m, limit 512 MB.
	web := mkService(t, s, p.ID, "web", model.TypeWeb, sized("small", model.QoSBurstable, 2), nil) // 2 instances
	auto := sized("small", model.QoSBurstable, 1)
	auto.Autoscale = model.Autoscale{Enabled: true, Min: 1, Max: 3}
	mkService(t, s, p.ID, "auto", model.TypeWeb, auto, nil)                                      // up to 3 instances
	mkService(t, s, p.ID, "job", model.TypeCron, sized("small", model.QoSBurstable, 0), nil)     // one run at a time
	mkService(t, s, p.ID, "site", model.TypeStatic, sized("small", model.QoSBurstable, 0), nil)  // served by the edge
	mkService(t, s, p.ID, "pr", model.TypeWeb, sized("small", model.QoSBestEffort, 1), &web.ID)  // preview: reserves nothing
	mkService(t, s, p.ID, "pg", model.TypeDatabase, sized("micro", model.QoSGuaranteed, 1), nil) // a dedicated server: 256/256
	if _, err := s.store.AddDomain(ctx, web.ID, "www.example.com", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.AddDomain(ctx, web.ID, "web-p.apps.test", true); err != nil {
		t.Fatal(err)
	}
	shared, _ := s.store.CreateDatabase(ctx, &store.Database{ProjectID: p.ID, Name: "a", Engine: "postgres", Mode: "shared", SizePreset: "micro"})
	ded, _ := s.store.CreateDatabase(ctx, &store.Database{ProjectID: p.ID, Name: "b", Engine: "postgres", Mode: "dedicated", SizePreset: "small"})
	if shared == nil || ded == nil {
		t.Fatal("create databases")
	}
	if _, err := s.store.UpdateDatabaseOptions(ctx, ded.ID, func(o *store.DBOptions) error { o.HA = true; return nil }); err != nil {
		t.Fatal(err)
	}
	u, err := s.teamUsage(ctx, team.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := TeamUsage{
		Projects: 2, Services: 4, Databases: 2, Previews: 1, Domains: 1,
		MemoryMB:      160*2 + 160*3 + 160 + 0 + 256,
		MemoryLimitMB: 512*2 + 512*3 + 512 + 512 + 256,
		CPUMillis:     100*2 + 100*3 + 100 + 0 + 250,
		DiskMB:        1024 + 5120*2, // shared default + small preset, twice with the HA standby
	}
	if u != want {
		t.Fatalf("usage\n got %+v\nwant %+v", u, want)
	}
	// Another team's resources are not counted.
	other, _ := s.store.CreateTeam(ctx, "O", "o", "", false)
	if u, _ := s.teamUsage(ctx, other.ID); u != (TeamUsage{}) {
		t.Fatalf("empty team usage = %+v", u)
	}
}

func TestQuotaFitsMessage(t *testing.T) {
	q := TeamQuota{MaxMemoryMB: 4096, MaxServices: 5}
	err := q.fits(TeamUsage{MemoryMB: 3584}, TeamUsage{MemoryMB: 1024})
	if err == nil || !strings.HasPrefix(err.Error(), "quota exceeded: memory 3.5 GiB of 4 GiB used, this needs 1 GiB more") {
		t.Fatalf("err = %v", err)
	}
	if err := q.fits(TeamUsage{MemoryMB: 9000, Services: 9}, TeamUsage{MemoryMB: -100}); err != nil {
		t.Fatalf("a decrease over quota was refused: %v", err)
	}
	if err := q.fits(TeamUsage{Services: 5}, TeamUsage{Services: 1}); err == nil || !strings.Contains(err.Error(), "services 5 of 5 used") {
		t.Fatalf("err = %v", err)
	}
	if err := (TeamQuota{}).fits(TeamUsage{MemoryMB: 1 << 20}, TeamUsage{MemoryMB: 1 << 20}); err != nil {
		t.Fatal("0 must mean unlimited")
	}
}

func setTeamQuota(t *testing.T, s *Server, team string, q map[string]int) {
	t.Helper()
	b, _ := json.Marshal(q)
	if err := s.store.SetTeamQuota(context.Background(), team, b); err != nil {
		t.Fatal(err)
	}
}

func wantQuota(t *testing.T, what string, code int, body, msg string) {
	t.Helper()
	if code != 403 || !strings.Contains(body, msg) {
		t.Fatalf("%s = %d %s, want 403 with %q", what, code, body, msg)
	}
}

func TestQuotaEnforcement(t *testing.T) {
	s := newTestServerBus(t)
	h := s.routes()
	ctx := context.Background()
	// An administrator: the sign-up limits do not apply, the team quota does.
	admin := userWithTeam(t, s, "admin@example.com", true)
	tok := mustLogin(t, h, "admin@example.com", "password1")
	team := s.firstTeam(ctx, admin)
	setTeamQuota(t, s, team, map[string]int{"max_projects": 1, "max_memory_mb": 512, "max_domains": 1, "max_disk_mb": 1000})

	w := do(t, h, "POST", "/api/v1/teams/"+team+"/projects", tok, map[string]string{"name": "one"})
	if w.Code != 201 {
		t.Fatalf("project = %d %s", w.Code, w.Body)
	}
	var proj store.Project
	_ = json.Unmarshal(w.Body.Bytes(), &proj)
	w = do(t, h, "POST", "/api/v1/teams/"+team+"/projects", tok, map[string]string{"name": "two"})
	wantQuota(t, "second project", w.Code, w.Body.String(), "quota exceeded: projects 1 of 1 used")

	// Services: 3 small replicas reserve 480 MiB of 512.
	svcBody := func(name string, replicas int) map[string]any {
		return map[string]any{"name": name, "source": map[string]string{"image": "nginx"}, "size": "small", "replicas": replicas, "deploy": false}
	}
	w = do(t, h, "POST", "/api/v1/projects/"+proj.ID+"/services", tok, svcBody("web", 3))
	if w.Code != 201 {
		t.Fatalf("service = %d %s", w.Code, w.Body)
	}
	var created struct {
		Service store.Service `json:"service"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	svc := created.Service
	w = do(t, h, "POST", "/api/v1/projects/"+proj.ID+"/services", tok, svcBody("api", 1))
	wantQuota(t, "second service", w.Code, w.Body.String(), "quota exceeded: memory 480 MiB of 512 MiB used, this needs 160 MiB more")
	// Scale and resize up are refused; scaling down is allowed (also when over quota).
	w = do(t, h, "POST", "/api/v1/services/"+svc.ID+"/scale", tok, map[string]int{"replicas": 4})
	wantQuota(t, "scale up", w.Code, w.Body.String(), "quota exceeded: memory")
	w = do(t, h, "PATCH", "/api/v1/services/"+svc.ID, tok, map[string]any{"size": "medium"})
	wantQuota(t, "resize up", w.Code, w.Body.String(), "quota exceeded: memory")
	setTeamQuota(t, s, team, map[string]int{"max_projects": 1, "max_memory_mb": 256, "max_domains": 1, "max_disk_mb": 1000})
	if w := do(t, h, "POST", "/api/v1/services/"+svc.ID+"/scale", tok, map[string]int{"replicas": 2}); w.Code != 200 {
		t.Fatalf("scale down over quota = %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "PATCH", "/api/v1/services/"+svc.ID, tok, map[string]any{"name": "web2"}); w.Code != 200 {
		t.Fatalf("an update that adds nothing = %d %s", w.Code, w.Body)
	}
	setTeamQuota(t, s, team, map[string]int{"max_projects": 1, "max_memory_mb": 512, "max_domains": 1, "max_disk_mb": 1000})

	// Custom domains.
	if w := do(t, h, "POST", "/api/v1/services/"+svc.ID+"/domains", tok, map[string]string{"hostname": "a.example.com"}); w.Code != 201 {
		t.Fatalf("domain = %d %s", w.Code, w.Body)
	}
	w = do(t, h, "POST", "/api/v1/services/"+svc.ID+"/domains", tok, map[string]string{"hostname": "b.example.com"})
	wantQuota(t, "second domain", w.Code, w.Body.String(), "quota exceeded: custom domains 1 of 1 used")

	// Databases: a shared database has a 1 GiB storage quota.
	w = do(t, h, "POST", "/api/v1/projects/"+proj.ID+"/databases", tok, map[string]string{"name": "db", "engine": "postgres", "mode": "shared"})
	wantQuota(t, "database", w.Code, w.Body.String(), "quota exceeded: disk 0 MiB of 1000 MiB used, this needs 1 GiB more")
	// A dedicated one also needs its server's memory.
	setTeamQuota(t, s, team, map[string]int{"max_memory_mb": 512})
	w = do(t, h, "POST", "/api/v1/projects/"+proj.ID+"/databases", tok, map[string]string{"name": "db", "engine": "postgres", "mode": "dedicated", "size": "small"})
	wantQuota(t, "dedicated database", w.Code, w.Body.String(), "quota exceeded: memory")

	// Templates and compose files are checked as a whole.
	tpl := plainTemplate(t)
	per := planUsage(tpl.Services, tpl.Databases, nil, "")
	setTeamQuota(t, s, team, map[string]int{"max_memory_limit_mb": 1024 + per.MemoryLimitMB - 1}) // web2 uses 2×512 limits
	w = do(t, h, "POST", "/api/v1/projects/"+proj.ID+"/templates/"+tpl.ID, tok, map[string]any{})
	wantQuota(t, "template", w.Code, w.Body.String(), "quota exceeded: memory limits")
	compose := "services:\n  app:\n    image: nginx\n    ports: [\"80:80\"]\n"
	setTeamQuota(t, s, team, map[string]int{"max_services": 1})
	w = do(t, h, "POST", "/api/v1/projects/"+proj.ID+"/compose", tok, map[string]string{"compose": compose})
	wantQuota(t, "compose", w.Code, w.Body.String(), "quota exceeded: services 1 of 1 used")

	// Batches: the whole class is checked up front, nothing is created.
	setTeamQuota(t, s, team, map[string]int{"max_memory_mb": 320 + per.MemoryMB*2})
	batch := map[string]any{"name": "class", "template": tpl.ID, "mode": "instances", "count": 3, "project_id": proj.ID, "force": true}
	w = do(t, h, "POST", "/api/v1/batches", tok, batch)
	wantQuota(t, "batch of 3", w.Code, w.Body.String(), "quota exceeded: memory")
	batch["count"] = 2
	delete(batch, "project_id")
	batch["team_id"] = team
	setTeamQuota(t, s, team, map[string]int{"max_projects": 1})
	w = do(t, h, "POST", "/api/v1/batches", tok, batch)
	wantQuota(t, "batch in a new project", w.Code, w.Body.String(), "quota exceeded: projects")
	var n int
	_ = s.store.Q(ctx).QueryRow(ctx, `SELECT count(*) FROM batches`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d batches were created", n)
	}
	// Accounts mode: each learner's new team gets the default quota.
	setTeamQuota(t, s, team, map[string]int{})
	if err := s.store.SetSetting(ctx, settingTeamQuota, `{"max_memory_mb": 1}`); err != nil {
		t.Fatal(err)
	}
	w = do(t, h, "POST", "/api/v1/batches", tok, map[string]any{"name": "class", "template": tpl.ID, "mode": "accounts", "force": true,
		"learners": []map[string]string{{"name": "A", "email": "a@learners.test"}}})
	wantQuota(t, "accounts batch", w.Code, w.Body.String(), "each learner's team: quota exceeded: memory")
	if _, err := s.store.UserByEmail(ctx, "a@learners.test"); err == nil {
		t.Fatal("a learner account was created")
	}
}

// plainTemplate is a template with services, no databases and no required inputs.
func plainTemplate(t *testing.T) *templates.Template {
	t.Helper()
outer:
	for i := range templates.All {
		tpl := &templates.All[i]
		if len(tpl.Databases) > 0 || tpl.Repo != "" || len(tpl.Services) == 0 {
			continue
		}
		for _, ts := range tpl.Services {
			for _, in := range ts.Inputs {
				if in.Required {
					continue outer
				}
			}
		}
		return tpl
	}
	t.Skip("no plain template")
	return nil
}

func TestQuotaPreviews(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()
	team, _ := s.store.CreateTeam(ctx, "T", "t", "", false)
	p, _ := s.store.CreateProject(ctx, team.ID, "P", "p")
	spec := sized("small", model.QoSBurstable, 1)
	spec.Previews = true
	parent := mkService(t, s, p.ID, "web", model.TypeWeb, spec, nil)
	setTeamQuota(t, s, team.ID, map[string]int{"max_previews": 1})
	ev := func(n int) pullRequestEvent {
		var e pullRequestEvent
		e.Number = n
		e.PullRequest.Head.Ref = "feature"
		return e
	}
	if _, created, err := s.ensurePreview(ctx, parent, ev(1)); err != nil || !created {
		t.Fatalf("first preview: %v", err)
	}
	// Updating an existing preview is not a new one.
	if _, _, err := s.ensurePreview(ctx, parent, ev(1)); err != nil {
		t.Fatalf("update preview: %v", err)
	}
	_, _, err := s.ensurePreview(ctx, parent, ev(2))
	if err == nil || !strings.Contains(err.Error(), "quota exceeded: preview deployments 1 of 1 used") {
		t.Fatalf("second preview: %v", err)
	}
}

func TestQuotaAPI(t *testing.T) {
	s, h := newTestServer(t)
	ctx := context.Background()
	uid := userWithTeam(t, s, "u@example.com", false)
	userWithTeam(t, s, "admin@example.com", true)
	ut, at := mustLogin(t, h, "u@example.com", "password1"), mustLogin(t, h, "admin@example.com", "password1")
	team := s.firstTeam(ctx, uid)
	if w := do(t, h, "PUT", "/api/v1/teams/"+team+"/quota", ut, map[string]any{"quota": map[string]int{"max_projects": 99}}); w.Code != 403 {
		t.Fatalf("member sets quota = %d", w.Code)
	}
	if w := do(t, h, "PUT", "/api/v1/admin/settings", at, map[string]any{"quota": map[string]int{"max_projects": 2, "max_memory_mb": 1024}}); w.Code != 200 {
		t.Fatalf("default quota = %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "PUT", "/api/v1/teams/"+team+"/quota", at, map[string]any{"quota": map[string]int{"max_memory_mb": -1}}); w.Code != 400 {
		t.Fatalf("negative quota = %d", w.Code)
	}
	if w := do(t, h, "PUT", "/api/v1/teams/"+team+"/quota", at, map[string]any{"quota": map[string]int{"bogus": 1}}); w.Code != 400 {
		t.Fatalf("unknown key = %d", w.Code)
	}
	if w := do(t, h, "PUT", "/api/v1/teams/"+team+"/quota", at, map[string]any{"quota": map[string]int{"max_memory_mb": 4096, "max_domains": 0}}); w.Code != 200 {
		t.Fatalf("override = %d %s", w.Code, w.Body)
	}
	w := do(t, h, "GET", "/api/v1/teams/"+team+"/quota", ut, nil)
	var out struct {
		Quota   TeamQuota `json:"quota"`
		Default TeamQuota `json:"default"`
		Usage   TeamUsage `json:"usage"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("get quota = %d %s", w.Code, w.Body)
	}
	if out.Quota.MaxProjects != 2 || out.Quota.MaxMemoryMB != 4096 || out.Default.MaxMemoryMB != 1024 {
		t.Fatalf("effective quota = %+v (default %+v)", out.Quota, out.Default)
	}
	other := userWithTeam(t, s, "o@example.com", false)
	if w := do(t, h, "GET", "/api/v1/teams/"+s.firstTeam(ctx, other)+"/quota", ut, nil); w.Code != 403 {
		t.Fatalf("another team's quota = %d", w.Code)
	}
}

func TestQuotaDatabaseGrowth(t *testing.T) {
	s, h := newTestServer(t)
	ctx := context.Background()
	admin := userWithTeam(t, s, "admin@example.com", true)
	tok := mustLogin(t, h, "admin@example.com", "password1")
	team := s.firstTeam(ctx, admin)
	p, _ := s.store.CreateProject(ctx, team, "P", "p")
	ded, _ := s.store.CreateDatabase(ctx, &store.Database{ProjectID: p.ID, Name: "big", Engine: "postgres", Mode: "dedicated", SizePreset: "micro"})
	shared, _ := s.store.CreateDatabase(ctx, &store.Database{ProjectID: p.ID, Name: "small", Engine: "postgres", Mode: "shared", SizePreset: "micro"})
	for _, id := range []string{ded.ID, shared.ID} {
		_ = s.store.SetDatabaseStatus(ctx, id, "ready", "")
	}
	// Usage: disk 2048 (micro dedicated) + 1024 (shared).
	setTeamQuota(t, s, team, map[string]int{"max_disk_mb": 4000})
	w := do(t, h, "PUT", "/api/v1/databases/"+ded.ID+"/size", tok, map[string]string{"size": "small"})
	wantQuota(t, "resize", w.Code, w.Body.String(), "quota exceeded: disk 3 GiB of 3.9 GiB used, this needs 3 GiB more")
	w = do(t, h, "POST", "/api/v1/databases/"+shared.ID+"/upgrade", tok, map[string]string{})
	wantQuota(t, "upgrade to dedicated", w.Code, w.Body.String(), "quota exceeded: disk")
	w = do(t, h, "PUT", "/api/v1/databases/"+shared.ID+"/quota", tok, map[string]int{"quota_mb": 4096})
	wantQuota(t, "storage quota", w.Code, w.Body.String(), "quota exceeded: disk")
	// Lowering is always fine.
	if w := do(t, h, "PUT", "/api/v1/databases/"+shared.ID+"/quota", tok, map[string]int{"quota_mb": 512}); w.Code != 200 {
		t.Fatalf("lower storage quota = %d %s", w.Code, w.Body)
	}
}
