package server

import (
	"context"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
)

var (
	promSampleRe = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{([a-zA-Z_][a-zA-Z0-9_]*="([^"\\\n]|\\.)*")(,[a-zA-Z_][a-zA-Z0-9_]*="([^"\\\n]|\\.)*")*\})? (NaN|[+-]Inf|-?[0-9.eE+-]+)$`)
	promTypeRe   = regexp.MustCompile(`^# TYPE ([a-zA-Z_:][a-zA-Z0-9_:]*) (gauge|counter|untyped)$`)
	promHelpRe   = regexp.MustCompile(`^# HELP ([a-zA-Z_:][a-zA-Z0-9_:]*) .*$`)
)

// checkPromFormat validates the text exposition format: every sample belongs to
// a family declared once, right before its samples.
func checkPromFormat(t *testing.T, body string) map[string][]string {
	t.Helper()
	samples := map[string][]string{}
	declared := map[string]bool{}
	current := ""
	for i, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		switch {
		case promHelpRe.MatchString(line):
		case promTypeRe.MatchString(line):
			name := promTypeRe.FindStringSubmatch(line)[1]
			if declared[name] {
				t.Fatalf("line %d: family %s declared twice", i+1, name)
			}
			declared[name], current = true, name
		case promSampleRe.MatchString(line):
			name := promSampleRe.FindStringSubmatch(line)[1]
			if name != current {
				t.Fatalf("line %d: sample of %s outside its family (%s)", i+1, name, current)
			}
			samples[name] = append(samples[name], line)
		default:
			t.Fatalf("line %d is not valid exposition format: %q", i+1, line)
		}
	}
	return samples
}

func TestPromWriterEscaping(t *testing.T) {
	var p promWriter
	p.family("x_total", "gauge", "help with \\ and\nnewline")
	p.sample("x_total", 1.5, "a", `quo"te`, "b", "back\\slash\nnl")
	p.sample("x_total", 3)
	want := "# HELP x_total help with \\\\ and\\nnewline\n# TYPE x_total gauge\nx_total{a=\"quo\\\"te\",b=\"back\\\\slash\\nnl\"} 1.5\nx_total 3\n"
	if got := p.b.String(); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	checkPromFormat(t, p.b.String())
}

func TestPrometheusEndpoint(t *testing.T) {
	e := newAlertEnv(t)
	s, h := e.s, e.h
	ctx := context.Background()
	s.cfg.MetricsToken = "metrics-secret-token"
	mkUser(t, s, "root@example.com", "password1", true)
	admin := mustLogin(t, h, "root@example.com", "password1")

	// Some data: a node, a running instance, metrics, a firing alert, an uptime check.
	node, err := s.store.UpsertNodeLegacy(ctx, &proto.RegisterRequest{Name: "n1", Address: "10.0.0.1", Roles: []string{"worker"}, CPUCores: 4, MemoryMB: 8192, Capabilities: map[string]string{}}, 16)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.CreateInstance(ctx, &store.Instance{ServiceID: e.svc.ID, NodeID: node.ID, ContainerID: "c1", Port: 8080, Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Q(ctx).Exec(ctx, `UPDATE instances SET cpu_usage_millis=250, memory_usage_mb=100, restarts=2`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Q(ctx).Exec(ctx, `INSERT INTO service_metrics (service_id, ts, instances, memory_mb, cpu_millis, requests, errors_5xx) VALUES ($1, now(), 1, 100, 990, 40, 2)`, e.svc.ID); err != nil {
		t.Fatal(err)
	}
	if w := do(t, h, "POST", "/api/v1/projects/"+e.proj.ID+"/alerts", e.tok["dev"], map[string]any{"name": `cpu "hot"`, "metric": "cpu_percent", "comparator": ">", "threshold": 50, "for_seconds": 0}); w.Code != 201 {
		t.Fatalf("create rule = %d %s", w.Code, w.Body)
	}
	if err := s.evaluateAlerts(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if w := do(t, h, "POST", "/api/v1/projects/"+e.proj.ID+"/uptime", e.tok["dev"], map[string]any{"url": "https://example.com/health"}); w.Code != 201 {
		t.Fatalf("create check = %d %s", w.Code, w.Body)
	}

	for _, path := range []string{"/metrics", "/api/v1/metrics/prometheus"} {
		if w := do(t, h, "GET", path, "", nil); w.Code != 401 {
			t.Fatalf("%s without credentials = %d, want 401", path, w.Code)
		}
		if w := do(t, h, "GET", path, "wrong-token", nil); w.Code != 401 {
			t.Fatalf("%s with a wrong token = %d, want 401", path, w.Code)
		}
		if w := do(t, h, "GET", path, e.tok["dev"], nil); w.Code != 403 {
			t.Fatalf("%s as a non-admin = %d, want 403", path, w.Code)
		}
		if w := do(t, h, "GET", path, admin, nil); w.Code != 200 {
			t.Fatalf("%s as admin = %d, want 200", path, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/metrics", nil)
	r.Header.Set("Authorization", "Bearer metrics-secret-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("metrics token = %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	samples := checkPromFormat(t, body)
	for _, want := range []string{
		`paas_node_up{node="n1",status="online"} 1`,
		`paas_node_memory_total_bytes{node="n1"} 8589934592`,
		`paas_service_instances{team="t",project="p",service="web",type="web"} 1`,
		`paas_service_cpu_millicores{team="t",project="p",service="web",type="web"} 250`,
		`paas_service_memory_bytes{team="t",project="p",service="web",type="web"} 104857600`,
		`paas_service_restarts{team="t",project="p",service="web",type="web"} 2`,
		`paas_service_http_requests_5m{team="t",project="p",service="web",type="web"} 40`,
		`paas_service_http_errors_5xx_5m{team="t",project="p",service="web",type="web"} 2`,
		`paas_alert_firing{project="p",service="web",rule="cpu \"hot\"",metric="cpu_percent",state="firing"} 1`,
		`paas_alert_value{project="p",service="web",rule="cpu \"hot\"",metric="cpu_percent"} 99`,
		`paas_deployments_24h 0`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
	if len(samples["paas_uptime_up"]) != 0 {
		t.Errorf("an unchecked uptime check has no up value: %v", samples["paas_uptime_up"])
	}
	if t.Failed() {
		t.Log(body)
	}
}
