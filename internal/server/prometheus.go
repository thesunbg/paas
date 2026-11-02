package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/model"
)

// Prometheus export: GET /metrics (also /api/v1/metrics/prometheus) in the text
// exposition format, built by hand (no client library in go.mod). Platform-wide
// data, so it takes PAAS_METRICS_TOKEN as a bearer token, or an administrator's
// session / API token; nobody else.

func (s *Server) handlePrometheus(w http.ResponseWriter, r *http.Request) {
	if want := s.cfg.MetricsToken; want != "" {
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") &&
			subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, "Bearer ")), []byte(want)) == 1 {
			s.writePrometheus(w, r)
			return
		}
	}
	s.authed(func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAdmin(w, r) {
			return
		}
		s.writePrometheus(w, r)
	})(w, r)
}

// promWriter writes metric families; each family's samples must be written together.
type promWriter struct {
	b bytes.Buffer
}

func (p *promWriter) family(name, typ, help string) {
	p.b.WriteString("# HELP " + name + " " + escapeHelp(help) + "\n# TYPE " + name + " " + typ + "\n")
}

// sample writes one sample; labels are name/value pairs.
func (p *promWriter) sample(name string, v float64, labels ...string) {
	p.b.WriteString(name)
	if len(labels) > 0 {
		p.b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				p.b.WriteByte(',')
			}
			p.b.WriteString(labels[i] + `="` + escapeLabel(labels[i+1]) + `"`)
		}
		p.b.WriteByte('}')
	}
	p.b.WriteByte(' ')
	p.b.WriteString(promFloat(v))
	p.b.WriteByte('\n')
}

func promFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatFloat(v, 'f', -1, 64) // integers (bytes, counts) without exponent
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }
func escapeHelp(s string) string  { return helpEscaper.Replace(s) }

const (
	mib = 1 << 20
	gib = 1 << 30
)

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

type promService struct {
	id, slug, project, team, typ, status string
	instances, cpu, memMB, restarts      int
	requests, errors                     int64
}

func (s *Server) writePrometheus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var p promWriter
	if err := s.collectPrometheus(ctx, &p); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(p.b.Bytes())
}

func (s *Server) collectPrometheus(ctx context.Context, p *promWriter) error {
	q := s.store.Q(ctx)

	// Nodes.
	nodes, err := s.store.Nodes(ctx)
	if err != nil {
		return err
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	p.family("paas_node_up", "gauge", "1 when the node is not offline")
	for _, n := range nodes {
		p.sample("paas_node_up", b2f(n.Status != "offline"), "node", n.Name, "status", n.Status)
	}
	p.family("paas_node_cpu_percent", "gauge", "Node CPU usage in percent")
	for _, n := range nodes {
		p.sample("paas_node_cpu_percent", n.Usage.CPUPercent, "node", n.Name)
	}
	p.family("paas_node_load1", "gauge", "Node 1-minute load average")
	for _, n := range nodes {
		p.sample("paas_node_load1", n.Usage.Load1, "node", n.Name)
	}
	p.family("paas_node_memory_used_bytes", "gauge", "Node memory in use")
	for _, n := range nodes {
		p.sample("paas_node_memory_used_bytes", float64(n.Usage.MemoryUsedMB)*mib, "node", n.Name)
	}
	p.family("paas_node_memory_total_bytes", "gauge", "Node memory")
	for _, n := range nodes {
		p.sample("paas_node_memory_total_bytes", float64(n.MemoryMB)*mib, "node", n.Name)
	}
	p.family("paas_node_disk_used_bytes", "gauge", "Node disk space in use")
	for _, n := range nodes {
		p.sample("paas_node_disk_used_bytes", n.Usage.DiskUsedGB*gib, "node", n.Name)
	}
	p.family("paas_node_disk_total_bytes", "gauge", "Node disk space")
	for _, n := range nodes {
		p.sample("paas_node_disk_total_bytes", n.Usage.DiskTotalGB*gib, "node", n.Name)
	}

	// Services: live usage of the running instances, traffic of the last 5 minutes.
	rows, err := q.Query(ctx, `SELECT s.id::text, s.slug, p.slug, t.slug, s.type, s.status FROM services s
		JOIN projects p ON p.id=s.project_id JOIN teams t ON t.id=p.team_id ORDER BY p.slug, s.slug`)
	if err != nil {
		return err
	}
	var services []*promService
	byID := map[string]*promService{}
	for rows.Next() {
		ps := &promService{}
		if err := rows.Scan(&ps.id, &ps.slug, &ps.project, &ps.team, &ps.typ, &ps.status); err != nil {
			rows.Close()
			return err
		}
		services = append(services, ps)
		byID[ps.id] = ps
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	instances, err := s.store.AllInstances(ctx)
	if err != nil {
		return err
	}
	for _, in := range instances {
		if ps := byID[in.ServiceID]; ps != nil && in.Status == model.InstRunning {
			ps.instances++
			ps.cpu += in.CPUUsageMillis
			ps.memMB += in.MemoryUsageMB
			ps.restarts += in.Restarts
		}
	}
	rows, err = q.Query(ctx, `SELECT service_id::text, sum(requests), sum(errors_5xx) FROM service_metrics WHERE ts > now() - interval '5 minutes' GROUP BY service_id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var req, errs int64
		if err := rows.Scan(&id, &req, &errs); err != nil {
			rows.Close()
			return err
		}
		if ps := byID[id]; ps != nil {
			ps.requests, ps.errors = req, errs
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	svcLabels := func(ps *promService) []string {
		return []string{"team", ps.team, "project", ps.project, "service", ps.slug, "type", ps.typ}
	}
	p.family("paas_service_running", "gauge", "1 when the service status is running")
	for _, ps := range services {
		p.sample("paas_service_running", b2f(ps.status == model.StatusRunning), append(svcLabels(ps), "status", ps.status)...)
	}
	families := []struct {
		name, help string
		http       bool // only for services the edge proxies serve
		val        func(*promService) float64
	}{
		{"paas_service_instances", "Running instances", false, func(ps *promService) float64 { return float64(ps.instances) }},
		{"paas_service_cpu_millicores", "CPU used by the running instances, millicores", false, func(ps *promService) float64 { return float64(ps.cpu) }},
		{"paas_service_memory_bytes", "Memory used by the running instances", false, func(ps *promService) float64 { return float64(ps.memMB) * mib }},
		{"paas_service_restarts", "Container restarts of the running instances (resets when an instance is replaced)", false, func(ps *promService) float64 { return float64(ps.restarts) }},
		{"paas_service_http_requests_5m", "HTTP requests served by the edge proxies during the last 5 minutes", true, func(ps *promService) float64 { return float64(ps.requests) }},
		{"paas_service_http_errors_5xx_5m", "HTTP 5xx responses during the last 5 minutes", true, func(ps *promService) float64 { return float64(ps.errors) }},
	}
	for _, f := range families {
		p.family(f.name, "gauge", f.help)
		for _, ps := range services {
			if f.http && ps.typ != model.TypeWeb && ps.typ != model.TypeStatic {
				continue
			}
			p.sample(f.name, f.val(ps), svcLabels(ps)...)
		}
	}

	// Deployments by status.
	rows, err = q.Query(ctx, `SELECT status, count(*) FROM deployments GROUP BY status ORDER BY status`)
	if err != nil {
		return err
	}
	p.family("paas_deployments", "gauge", "Deployments on record by status")
	for rows.Next() {
		var st string
		var n int64
		if err := rows.Scan(&st, &n); err != nil {
			rows.Close()
			return err
		}
		p.sample("paas_deployments", float64(n), "status", st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var recent int64
	if err := q.QueryRow(ctx, `SELECT count(*) FROM deployments WHERE created_at > now() - interval '24 hours'`).Scan(&recent); err != nil {
		return err
	}
	p.family("paas_deployments_24h", "gauge", "Deployments created during the last 24 hours")
	p.sample("paas_deployments_24h", float64(recent))

	// Alerts.
	rows, err = q.Query(ctx, `SELECT r.name, r.metric, p.slug, s.slug, st.state, st.value FROM alert_states st
		JOIN alert_rules r ON r.id=st.rule_id JOIN services s ON s.id=st.service_id JOIN projects p ON p.id=r.project_id
		WHERE r.enabled ORDER BY p.slug, r.name, s.slug`)
	if err != nil {
		return err
	}
	type alertRow struct {
		rule, metric, project, service, state string
		value                                 *float64
	}
	var alerts []alertRow
	for rows.Next() {
		var a alertRow
		if err := rows.Scan(&a.rule, &a.metric, &a.project, &a.service, &a.state, &a.value); err != nil {
			rows.Close()
			return err
		}
		alerts = append(alerts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	p.family("paas_alert_firing", "gauge", "1 when the alert rule fires for the service")
	for _, a := range alerts {
		p.sample("paas_alert_firing", b2f(a.state == "firing"), "project", a.project, "service", a.service, "rule", a.rule, "metric", a.metric, "state", a.state)
	}
	p.family("paas_alert_value", "gauge", "Last evaluated value of the alert rule's metric")
	for _, a := range alerts {
		if a.value != nil {
			p.sample("paas_alert_value", *a.value, "project", a.project, "service", a.service, "rule", a.rule, "metric", a.metric)
		}
	}

	// Uptime checks.
	checks, err := s.store.AllUptimeChecks(ctx)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(checks))
	projSlug := map[string]string{}
	for _, c := range checks {
		ids = append(ids, c.ID)
		if _, ok := projSlug[c.ProjectID]; !ok {
			if pr, err := s.store.ProjectByID(ctx, c.ProjectID); err == nil {
				projSlug[c.ProjectID] = pr.Slug
			}
		}
	}
	stats, err := s.store.UptimeStatsFor(ctx, ids)
	if err != nil {
		return err
	}
	checkLabels := func(i int) []string {
		return []string{"project", projSlug[checks[i].ProjectID], "check", checks[i].Name, "url", checks[i].URL}
	}
	p.family("paas_uptime_up", "gauge", "1 when the uptime check is up, 0 when down (unknown and disabled checks are left out)")
	for i, c := range checks {
		if c.Enabled && c.State != "unknown" {
			p.sample("paas_uptime_up", b2f(c.State == "up"), checkLabels(i)...)
		}
	}
	p.family("paas_uptime_ratio_24h", "gauge", "Share of successful checks during the last 24 hours (0..1)")
	for i, c := range checks {
		if st := stats[c.ID]; st.Uptime24h != nil {
			p.sample("paas_uptime_ratio_24h", *st.Uptime24h/100, checkLabels(i)...)
		}
	}
	p.family("paas_uptime_ratio_7d", "gauge", "Share of successful checks during the last 7 days (0..1)")
	for i, c := range checks {
		if st := stats[c.ID]; st.Uptime7d != nil {
			p.sample("paas_uptime_ratio_7d", *st.Uptime7d/100, checkLabels(i)...)
		}
	}
	p.family("paas_uptime_latency_p95_seconds", "gauge", "95th percentile response time of the successful checks of the last 24 hours")
	for i, c := range checks {
		if st := stats[c.ID]; st.P95Latency24 != nil {
			p.sample("paas_uptime_latency_p95_seconds", *st.P95Latency24/1000, checkLabels(i)...)
		}
	}
	p.family("paas_uptime_cert_expiry_timestamp_seconds", "gauge", "Expiry of the TLS certificate served (unix time)")
	for i, c := range checks {
		if c.CertExpiresAt != nil {
			p.sample("paas_uptime_cert_expiry_timestamp_seconds", float64(c.CertExpiresAt.Unix()), checkLabels(i)...)
		}
	}
	return nil
}
