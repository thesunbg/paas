package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/store"
)

func TestMetricValue(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	at := func(minAgo int) time.Time { return now.Add(-time.Duration(minAgo) * time.Minute) }
	res := model.Resources{CPULimitMillis: 500, MemoryLimitMB: 256}
	samples := []metricSample{
		{TS: at(10), Instances: 2, CPUMillis: 100, MemoryMB: 100, Restarts: 0, Requests: 1000, Errors: 1000}, // outside the window
		{TS: at(4), Instances: 2, CPUMillis: 200, MemoryMB: 200, Restarts: 1, Requests: 100, Errors: 5},
		{TS: at(3), Instances: 2, CPUMillis: 300, MemoryMB: 300, Restarts: 3, Requests: 100, Errors: 0},
		{TS: at(2), Instances: 1, CPUMillis: 0, MemoryMB: 0, Restarts: 0, Requests: 0, Errors: 0}, // replaced instance: counter dropped
		{TS: at(1), Instances: 2, CPUMillis: 400, MemoryMB: 128, Restarts: 2, Requests: 200, Errors: 15},
	}
	for _, c := range []struct {
		metric string
		want   float64
		ok     bool
	}{
		{"cpu_percent", 40, true},    // 400m / 2 instances / 500m limit
		{"memory_percent", 25, true}, // 128 MB / 2 / 256
		{"restarts", 4, true},        // +2, (drop ignored), +2
		{"error_rate", 5, true},      // 20 / 400
		{"instances", 2, true},       // latest
		{"latency_p95", 0, false},    // unknown metric
	} {
		got, ok := metricValue(c.metric, samples, res, now)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("%s = %v,%v; want %v,%v", c.metric, got, ok, c.want, c.ok)
		}
	}

	// Stale data: no current CPU, zero instances, no traffic.
	stale := []metricSample{{TS: at(4), Instances: 1, CPUMillis: 900}}
	if _, ok := metricValue("cpu_percent", stale, res, now); ok {
		t.Error("a 4-minute-old sample is no current CPU value")
	}
	if v, ok := metricValue("instances", nil, res, now); !ok || v != 0 {
		t.Errorf("no sample = no instance, got %v,%v", v, ok)
	}
	if _, ok := metricValue("error_rate", stale, res, now); ok {
		t.Error("no requests: no error rate")
	}
	// No limits: CPU against one core, memory against the request, or nothing.
	one := []metricSample{{TS: at(1), Instances: 1, CPUMillis: 500, MemoryMB: 64}}
	if v, _ := metricValue("cpu_percent", one, model.Resources{}, now); v != 50 {
		t.Errorf("cpu without limit = %v, want 50", v)
	}
	if v, _ := metricValue("memory_percent", one, model.Resources{MemoryRequestMB: 128}, now); v != 50 {
		t.Errorf("memory against request = %v, want 50", v)
	}
	if _, ok := metricValue("memory_percent", one, model.Resources{}, now); ok {
		t.Error("memory without limit or request has no percentage")
	}
}

func TestStepAlert(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }
	forDur, cool := 5*time.Minute, 30*time.Minute
	st := store.AlertState{State: "ok"}
	step := func(cond bool, at time.Time, wantState, wantNotify string) {
		t.Helper()
		var n string
		st, n = stepAlert(st, cond, at, forDur, cool)
		if st.State != wantState || n != wantNotify {
			t.Fatalf("at %s cond=%v: state %s notify %q, want %s %q", at.Sub(t0), cond, st.State, n, wantState, wantNotify)
		}
	}
	// The condition must hold for 5 minutes.
	step(true, at(0), "pending", "")
	step(true, at(4), "pending", "")
	step(false, at(5), "ok", "") // a blip resets the clock
	step(true, at(6), "pending", "")
	step(true, at(10), "pending", "")
	step(true, at(11), "firing", "firing")
	step(true, at(12), "firing", "") // no repeat while firing
	step(false, at(13), "ok", "resolved")
	if st.ResolvedAt == nil || !st.ResolvedAt.Equal(at(13)) {
		t.Fatalf("resolved_at = %v", st.ResolvedAt)
	}
	// Fires again within the cooldown: silently, and its resolve is silent too.
	step(true, at(14), "pending", "")
	step(true, at(19), "firing", "")
	if st.Notified {
		t.Fatal("a firing within the cooldown is not notified")
	}
	step(false, at(20), "ok", "")
	// After the cooldown it notifies again.
	step(true, at(40), "pending", "")
	step(true, at(45), "firing", "firing")
	step(false, at(46), "ok", "resolved")

	// for 0 fires on the first evaluation.
	st = store.AlertState{State: "ok"}
	forDur = 0
	step(true, at(100), "firing", "firing")
}

// alertEnv is a project with a running web service, users of each role and a webhook channel.
type alertEnv struct {
	s     *Server
	h     http.Handler
	tok   map[string]string
	team  *store.Team
	proj  *store.Project
	svc   *store.Service
	hooks chan map[string]any
	chID  string
}

func newAlertEnv(t *testing.T) *alertEnv {
	t.Helper()
	s, h := newTestServer(t)
	ctx := context.Background()
	e := &alertEnv{s: s, h: h, tok: map[string]string{}, hooks: make(chan map[string]any, 32)}
	var err error
	if e.team, err = s.store.CreateTeam(ctx, "t", "t", "", false); err != nil {
		t.Fatal(err)
	}
	for _, u := range []struct{ name, role string }{{"dev", "developer"}, {"viewer", "viewer"}} {
		usr := mkUser(t, s, u.name+"@example.com", "password1", false)
		if err := s.store.AddMember(ctx, e.team.ID, usr.ID, u.role); err != nil {
			t.Fatal(err)
		}
		e.tok[u.name] = mustLogin(t, h, u.name+"@example.com", "password1")
	}
	mkUser(t, s, "outsider@example.com", "password1", false)
	e.tok["outsider"] = mustLogin(t, h, "outsider@example.com", "password1")
	if e.proj, err = s.store.CreateProject(ctx, e.team.ID, "p", "p"); err != nil {
		t.Fatal(err)
	}
	if e.svc, err = s.store.CreateService(ctx, &store.Service{ProjectID: e.proj.ID, Name: "web", Slug: "web", Type: model.TypeWeb,
		Spec: model.ServiceSpec{Replicas: 1, Port: 8080, Resources: model.Resources{CPULimitMillis: 1000, MemoryLimitMB: 256}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetServiceStatus(ctx, e.svc.ID, model.StatusRunning); err != nil {
		t.Fatal(err)
	}

	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		e.hooks <- m
	}))
	t.Cleanup(hook.Close)
	prev := AllowPrivateNotifyTargets
	AllowPrivateNotifyTargets = true
	t.Cleanup(func() { AllowPrivateNotifyTargets = prev })
	ch, err := s.store.CreateChannel(ctx, &store.NotificationChannel{TeamID: e.team.ID, Name: "hook", Kind: "webhook",
		TargetEnc: s.box.MustEncrypt(hook.URL), Events: []string{"deploy.failed"}}) // not subscribed to alert.*: chosen by id
	if err != nil {
		t.Fatal(err)
	}
	e.chID = ch.ID
	return e
}

// sample inserts a per-minute metric row for the service.
func (e *alertEnv) sample(t *testing.T, ts time.Time, cpuMillis int) {
	t.Helper()
	if _, err := e.s.store.Q(context.Background()).Exec(context.Background(), `INSERT INTO service_metrics (service_id, ts, instances, memory_mb, cpu_millis, requests, errors_5xx, restarts)
		VALUES ($1,$2,1,64,$3,0,0,0)`, e.svc.ID, ts, cpuMillis); err != nil {
		t.Fatal(err)
	}
}

func (e *alertEnv) expectHook(t *testing.T, event string) map[string]any {
	t.Helper()
	select {
	case m := <-e.hooks:
		if m["event"] != event {
			t.Fatalf("webhook event = %v, want %s (%v)", m["event"], event, m)
		}
		return m
	case <-time.After(5 * time.Second):
		t.Fatalf("no %s notification", event)
	}
	return nil
}

func (e *alertEnv) expectNoHook(t *testing.T) {
	t.Helper()
	select {
	case m := <-e.hooks:
		t.Fatalf("unexpected notification %v", m)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestAlertEvaluationDB(t *testing.T) {
	e := newAlertEnv(t)
	ctx := context.Background()
	w := do(t, e.h, "POST", "/api/v1/projects/"+e.proj.ID+"/alerts", e.tok["dev"], map[string]any{
		"name": "hot", "metric": "cpu_percent", "comparator": ">", "threshold": 80, "for_seconds": 120, "cooldown_seconds": 600, "channel_ids": []string{e.chID}})
	if w.Code != 201 {
		t.Fatalf("create rule = %d %s", w.Code, w.Body)
	}
	var rule store.AlertRule
	_ = json.Unmarshal(w.Body.Bytes(), &rule)

	base := time.Now().UTC().Truncate(time.Minute)
	state := func() store.AlertState {
		t.Helper()
		sts, err := e.s.store.AlertStates(ctx, []string{rule.ID})
		if err != nil || len(sts) != 1 {
			t.Fatalf("states = %v %v", sts, err)
		}
		return sts[0]
	}
	eval := func(at time.Time) {
		t.Helper()
		if err := e.s.evaluateAlerts(ctx, at); err != nil {
			t.Fatal(err)
		}
	}
	e.sample(t, base, 950)
	eval(base)
	if st := state(); st.State != "pending" || st.Value == nil || *st.Value != 95 {
		t.Fatalf("after first hot minute: %+v", st)
	}
	e.sample(t, base.Add(time.Minute), 960)
	eval(base.Add(time.Minute))
	e.expectNoHook(t)
	e.sample(t, base.Add(2*time.Minute), 970)
	eval(base.Add(2 * time.Minute))
	if st := state(); st.State != "firing" || !st.Notified {
		t.Fatalf("after 2 hot minutes: %+v", st)
	}
	m := e.expectHook(t, "alert.firing")
	if d, _ := m["data"].(map[string]any); d["service"] != "web" || d["rule"] != "hot" {
		t.Fatalf("payload = %v", m)
	}
	// Still hot: no repeat.
	e.sample(t, base.Add(3*time.Minute), 990)
	eval(base.Add(3 * time.Minute))
	e.expectNoHook(t)
	// Cool down: resolve.
	e.sample(t, base.Add(4*time.Minute), 100)
	eval(base.Add(4 * time.Minute))
	e.expectHook(t, "alert.resolved")
	if st := state(); st.State != "ok" || st.ResolvedAt == nil {
		t.Fatalf("after resolve: %+v", st)
	}
	// Stopped services are not judged (and resolve).
	_ = e.s.store.SetServiceStatus(ctx, e.svc.ID, model.StatusStopped)
	e.sample(t, base.Add(5*time.Minute), 999)
	eval(base.Add(5 * time.Minute))
	if st := state(); st.State != "ok" || st.Value != nil {
		t.Fatalf("stopped service: %+v", st)
	}

	// The list shows the state with the service name; the prometheus export the rule.
	w = do(t, e.h, "GET", "/api/v1/projects/"+e.proj.ID+"/alerts?service="+e.svc.ID, e.tok["viewer"], nil)
	var list struct {
		Rules []alertRuleView `json:"rules"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Rules) != 1 || len(list.Rules[0].States) != 1 || list.Rules[0].States[0].ServiceName != "web" {
		t.Fatalf("list = %d %s", w.Code, w.Body)
	}
}

func TestAlertAPI(t *testing.T) {
	e := newAlertEnv(t)
	base := "/api/v1/projects/" + e.proj.ID + "/alerts"
	rule := map[string]any{"metric": "memory_percent", "comparator": ">=", "threshold": 90, "service_id": e.svc.ID}

	if w := do(t, e.h, "POST", base, e.tok["viewer"], rule); w.Code != 403 {
		t.Fatalf("viewer create = %d, want 403", w.Code)
	}
	if w := do(t, e.h, "GET", base, e.tok["outsider"], nil); w.Code != 403 {
		t.Fatalf("outsider list = %d, want 403", w.Code)
	}
	for name, bad := range map[string]map[string]any{
		"metric":     {"metric": "latency", "comparator": ">", "threshold": 1},
		"comparator": {"metric": "cpu_percent", "comparator": "!=", "threshold": 1},
		"for":        {"metric": "cpu_percent", "comparator": ">", "threshold": 1, "for_seconds": -1},
		"channel":    {"metric": "cpu_percent", "comparator": ">", "threshold": 1, "channel_ids": []string{"00000000-0000-0000-0000-000000000000"}},
		"service":    {"metric": "cpu_percent", "comparator": ">", "threshold": 1, "service_id": "00000000-0000-0000-0000-000000000000"},
		"unknown":    {"metric": "cpu_percent", "comparator": ">", "threshold": 1, "bogus": true},
	} {
		if w := do(t, e.h, "POST", base, e.tok["dev"], bad); w.Code != 400 {
			t.Errorf("%s: create = %d %s, want 400", name, w.Code, w.Body)
		}
	}
	w := do(t, e.h, "POST", base, e.tok["dev"], rule)
	if w.Code != 201 {
		t.Fatalf("dev create = %d %s", w.Code, w.Body)
	}
	var created store.AlertRule
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if created.ForSeconds != 300 || created.CooldownSeconds != 1800 || !created.Enabled || created.Name == "" || created.ServiceID == nil {
		t.Fatalf("defaults: %+v", created)
	}

	if w := do(t, e.h, "PATCH", "/api/v1/alerts/"+created.ID, e.tok["viewer"], map[string]any{"enabled": false}); w.Code != 403 {
		t.Fatalf("viewer patch = %d", w.Code)
	}
	w = do(t, e.h, "PATCH", "/api/v1/alerts/"+created.ID, e.tok["dev"], map[string]any{"enabled": false, "threshold": 95})
	var upd store.AlertRule
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &upd) != nil || upd.Enabled || upd.Threshold != 95 || upd.Metric != "memory_percent" {
		t.Fatalf("patch = %d %s", w.Code, w.Body)
	}
	if w := do(t, e.h, "GET", base, e.tok["viewer"], nil); w.Code != 200 {
		t.Fatalf("viewer list = %d", w.Code)
	}
	if w := do(t, e.h, "DELETE", "/api/v1/alerts/"+created.ID, e.tok["viewer"], nil); w.Code != 403 {
		t.Fatalf("viewer delete = %d", w.Code)
	}
	if w := do(t, e.h, "DELETE", "/api/v1/alerts/"+created.ID, e.tok["dev"], nil); w.Code != 200 {
		t.Fatalf("dev delete = %d", w.Code)
	}
	if w := do(t, e.h, "DELETE", "/api/v1/alerts/"+created.ID, e.tok["dev"], nil); w.Code != 404 {
		t.Fatalf("delete again = %d", w.Code)
	}
}
