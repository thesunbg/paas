package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/templates"
)

func TestBatchPacingDefaults(t *testing.T) {
	no, yes := false, true
	cases := []struct {
		in       batchInput
		parallel int
		sleep    bool
		idle     int
	}{
		{batchInput{}, batchParallelDef, true, batchIdleDef},
		{batchInput{Parallel: 5, Sleep: &yes, IdleMinutes: 10}, 5, true, 10},
		{batchInput{Parallel: 50}, batchParallelMax, true, batchIdleDef},
		{batchInput{Sleep: &no, IdleMinutes: 10}, batchParallelDef, false, 0},
	}
	for _, c := range cases {
		p, sl := c.in.pacing()
		if p != c.parallel || sl.Enabled != c.sleep || sl.IdleMinutes != c.idle {
			t.Errorf("%+v: got parallel %d sleep %+v", c.in, p, sl)
		}
	}
}

// One small web service (request 160 MB, limit 512 MB) and a dedicated database (96 / 256 MB).
var estimateTpl = &templates.Template{
	ID:        "est",
	Services:  []templates.Service{{Name: "app", Size: "small", Port: 80}},
	Databases: []templates.Database{{Name: "app", Engine: "postgres", Mode: "dedicated"}},
}

func TestBatchEstimateWithoutSleep(t *testing.T) {
	e := computeBatchEstimate(estimateTpl, 30, 2, false, 20000, 16000)
	if e.PerCopyRequestMB != 256 || e.NeedRequestMB != 30*256 {
		t.Fatalf("request: per copy %d, need %d", e.PerCopyRequestMB, e.NeedRequestMB)
	}
	if e.StartupPeakMB != 2*(512-160) {
		t.Fatalf("startup peak = %d", e.StartupPeakMB)
	}
	if !e.Fits || e.Blocked {
		t.Fatalf("fits %v blocked %v: %s", e.Fits, e.Blocked, e.Warning)
	}
	// The scheduler would place them (overcommit), but the memory is not there: refused even with force.
	e = computeBatchEstimate(estimateTpl, 30, 2, false, 12000, 6000)
	if !e.Fits || !e.Blocked || !strings.Contains(e.Warning, "out of memory") {
		t.Fatalf("fits %v blocked %v: %s", e.Fits, e.Blocked, e.Warning)
	}
	// Physical memory not reported yet: only the reservation check applies.
	if e = computeBatchEstimate(estimateTpl, 30, 2, false, 12000, 0); e.Blocked {
		t.Fatal("blocked without a memory report")
	}
	// More parallel starts, higher peak.
	if e = computeBatchEstimate(estimateTpl, 30, 10, false, 20000, 16000); e.StartupPeakMB != 10*(512-160) {
		t.Fatalf("startup peak with 10 = %d", e.StartupPeakMB)
	}
}

func TestBatchEstimateWithSleep(t *testing.T) {
	// Asleep only the databases reserve memory, plus the copies starting at once.
	e := computeBatchEstimate(estimateTpl, 30, 2, true, 12000, 6000)
	if e.NeedRequestMB != 30*96+2*160 {
		t.Fatalf("need = %d", e.NeedRequestMB)
	}
	if !e.Fits || e.Blocked {
		t.Fatalf("fits %v blocked %v: %s", e.Fits, e.Blocked, e.Warning)
	}
	if want := (12000 - 30*96) / 160; e.MaxAwake != min(30, want) {
		t.Fatalf("max awake = %d, want %d", e.MaxAwake, min(30, want))
	}
	// Room for only a few awake at once: a warning, not a refusal.
	e = computeBatchEstimate(estimateTpl, 30, 2, true, 3500, 6000)
	if e.Blocked || !e.Fits || e.MaxAwake >= 30 || !strings.Contains(e.Warning, "awake at the same time") {
		t.Fatalf("fits %v blocked %v awake %d: %s", e.Fits, e.Blocked, e.MaxAwake, e.Warning)
	}
	// Even asleep the databases alone do not fit in memory: refused.
	if e = computeBatchEstimate(estimateTpl, 100, 2, true, 50000, 5000); !e.Blocked {
		t.Fatalf("not blocked: %+v", e)
	}
}

func TestCopySettled(t *testing.T) {
	dep := "d1"
	cases := []struct {
		svc  store.Service
		want bool
	}{
		{store.Service{Status: model.StatusCreated}, false},
		{store.Service{Status: model.StatusDeploying}, false},
		{store.Service{Status: model.StatusRunning}, false}, // no deployment live yet
		{store.Service{Status: model.StatusRunning, CurrentDeploymentID: &dep}, true},
		{store.Service{Status: model.StatusFailed}, true},
		{store.Service{Status: model.StatusStopped}, true},
		{store.Service{Status: model.StatusSleeping}, true},
	}
	for _, c := range cases {
		if got := copySettled(&c.svc); got != c.want {
			t.Errorf("%s (deployment %v) = %v", c.svc.Status, c.svc.CurrentDeploymentID != nil, got)
		}
	}
}

func TestTemplateSleepOnlyForPublicWeb(t *testing.T) {
	s := newTestServerBus(t)
	ctx := context.Background()
	team, err := s.store.CreateTeam(ctx, "T", "t", "", false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.store.CreateProject(ctx, team.ID, "class", "class")
	if err != nil {
		t.Fatal(err)
	}
	tpl := &templates.Template{ID: "sl", Services: []templates.Service{
		{Name: "web", Image: "nginx", Port: 80},
		{Name: "api", Image: "nginx", Port: 8080, Private: true},
		{Name: "worker", Image: "busybox", Type: "worker"},
	}}
	svcs, _, err := s.installTemplate(ctx, p, tpl, "", "", nil, model.SleepPolicy{Enabled: true, IdleMinutes: 15})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]model.SleepPolicy{}
	for _, svc := range svcs {
		got[svc.Slug] = svc.Spec.Sleep
	}
	if !got["web"].Enabled || got["web"].IdleMinutes != 15 {
		t.Fatalf("web: %+v", got["web"])
	}
	if got["api"].Enabled || got["worker"].Enabled {
		t.Fatalf("a private service or a worker sleeps: %+v", got)
	}
	// A plain install (not a batch) never sleeps.
	svcs, _, err = s.installTemplate(ctx, p, &templates.Template{ID: "x", Services: []templates.Service{{Name: "site", Image: "nginx", Port: 80}}}, "", "", nil, model.SleepPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if svcs[0].Spec.Sleep.Enabled {
		t.Fatal("a single install sleeps")
	}
}

func TestBatchCreateBlockedEvenWithForce(t *testing.T) {
	s, h := newTestServer(t)
	ctx := context.Background()
	userWithTeam(t, s, "admin@example.com", true)
	tok := mustLogin(t, h, "admin@example.com", "password1")
	// A worker that the scheduler would still place copies on, but with little memory left.
	n, err := s.store.UpsertNodeLegacy(ctx, &proto.RegisterRequest{Name: "w1", Roles: []string{"worker"}, CPUCores: 4, MemoryMB: 8192, Capabilities: map[string]string{}}, 250)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.NodeHeartbeat(ctx, n.ID, proto.NodeUsage{MemoryAvailableMB: 900}); err != nil {
		t.Fatal(err)
	}
	tpl := plainTemplate(t)
	off := false
	w := do(t, h, "POST", "/api/v1/batches", tok, map[string]any{"name": "class", "template": tpl.ID, "mode": "instances", "count": 20, "force": true, "sleep": off})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "out of memory") {
		t.Fatalf("POST batches = %d %s", w.Code, w.Body.String())
	}
	var count int
	_ = s.store.Q(ctx).QueryRow(ctx, `SELECT count(*) FROM batches`).Scan(&count)
	if count != 0 {
		t.Fatalf("%d batches created", count)
	}
	// The estimate says so too, and that sleep makes it fit.
	w = do(t, h, "GET", "/api/v1/batches/estimate?template="+tpl.ID+"&count=20&parallel=2&sleep=false", tok, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"blocked":true`) {
		t.Fatalf("estimate = %d %s", w.Code, w.Body.String())
	}
	w = do(t, h, "GET", "/api/v1/batches/estimate?template="+tpl.ID+"&count=20&parallel=2", tok, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"blocked":true`) || !strings.Contains(w.Body.String(), `"sleep":true`) {
		t.Fatalf("estimate with sleep = %d %s", w.Code, w.Body.String())
	}
	// Out-of-range pacing is refused.
	w = do(t, h, "POST", "/api/v1/batches", tok, map[string]any{"name": "class", "template": tpl.ID, "mode": "instances", "count": 2, "parallel": 11})
	if w.Code != 400 {
		t.Fatalf("parallel 11 = %d", w.Code)
	}
}

func TestResumeStaleBatches(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()
	var oldID, freshID string
	// Created before batches kept their input, and its runner is gone.
	if err := s.store.Q(ctx).QueryRow(ctx, `INSERT INTO batches (name, template, mode, heartbeat_at) VALUES ('old', 'n8n', 'instances', now() - interval '10 minutes') RETURNING id`).Scan(&oldID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Q(ctx).Exec(ctx, `INSERT INTO batch_items (batch_id, idx, label) VALUES ($1, 0, 'n8n-01')`, oldID); err != nil {
		t.Fatal(err)
	}
	// Still being created by a live runner.
	if err := s.store.Q(ctx).QueryRow(ctx, `INSERT INTO batches (name, template, mode) VALUES ('fresh', 'n8n', 'instances') RETURNING id`).Scan(&freshID); err != nil {
		t.Fatal(err)
	}
	s.resumeStaleBatches(ctx)
	var status, itemErr string
	_ = s.store.Q(ctx).QueryRow(ctx, `SELECT status FROM batches WHERE id=$1`, oldID).Scan(&status)
	_ = s.store.Q(ctx).QueryRow(ctx, `SELECT error FROM batch_items WHERE batch_id=$1`, oldID).Scan(&itemErr)
	if status != "ready" || !strings.Contains(itemErr, "interrupted") {
		t.Fatalf("old batch: %s, item error %q", status, itemErr)
	}
	_ = s.store.Q(ctx).QueryRow(ctx, `SELECT status FROM batches WHERE id=$1`, freshID).Scan(&status)
	if status != "creating" {
		t.Fatalf("a batch with a live runner was touched: %s", status)
	}
}

// A batch starts `parallel` copies, and the next ones only once those are up.
func TestBatchStartsCopiesInGroups(t *testing.T) {
	oldPoll := batchPoll
	batchPoll = 20 * time.Millisecond
	t.Cleanup(func() { batchPoll = oldPoll })
	s := newTestServerBus(t)
	h := s.routes()
	ctx := context.Background()
	userWithTeam(t, s, "admin@example.com", true)
	tok := mustLogin(t, h, "admin@example.com", "password1")
	tpl := plainTemplate(t)
	w := do(t, h, "POST", "/api/v1/batches", tok, map[string]any{"name": "class", "template": tpl.ID, "mode": "instances",
		"count": 5, "parallel": 2, "sleep": false, "force": true})
	if w.Code != 202 && w.Code != 201 {
		t.Fatalf("POST batches = %d %s", w.Code, w.Body.String())
	}
	var batchID string
	_ = s.store.Q(ctx).QueryRow(ctx, `SELECT id FROM batches`).Scan(&batchID)
	created := func() []string {
		rows, err := s.store.Q(ctx).Query(ctx, `SELECT service_id FROM batch_items WHERE batch_id=$1 AND service_id IS NOT NULL ORDER BY idx`, batchID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var id string
			_ = rows.Scan(&id)
			ids = append(ids, id)
		}
		return ids
	}
	waitFor := func(n int) []string {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if ids := created(); len(ids) >= n {
				return ids
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("only %d copies created, want %d", len(created()), n)
		return nil
	}
	settle := func(ids []string) {
		for _, id := range ids {
			if err := s.store.SetServiceStatus(ctx, id, model.StatusStopped); err != nil {
				t.Fatal(err)
			}
		}
	}
	ids := waitFor(2)
	time.Sleep(200 * time.Millisecond)
	if got := created(); len(got) != 2 {
		t.Fatalf("%d copies started while the first two are not up", len(got))
	}
	settle(ids[:1]) // one slot frees: one more copy
	ids = waitFor(3)
	time.Sleep(200 * time.Millisecond)
	if got := created(); len(got) != 3 {
		t.Fatalf("%d copies after one slot freed, want 3", len(got))
	}
	settle(ids)
	ids = waitFor(5)
	settle(ids)
	deadline := time.Now().Add(10 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		_ = s.store.Q(ctx).QueryRow(ctx, `SELECT status FROM batches WHERE id=$1`, batchID).Scan(&status)
		if status == "ready" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status != "ready" {
		t.Fatalf("batch is %s", status)
	}
}
