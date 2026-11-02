package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/store"
)

// Threshold alerts. The leader evaluates every enabled rule once a minute
// against the per-minute service metrics (metrics.go), keeps a state per rule
// and service (ok -> pending -> firing -> ok) in alert_states, and notifies
// on the transition to firing and on its resolve. A cooldown suppresses a new
// firing notification shortly after the previous one (a flapping alert); the
// resolve of a firing that was not notified is not notified either.

// Alert metrics. The proxy reports request and 5xx counts but no latency, so
// there is no latency metric.
var alertMetrics = map[string]string{
	"cpu_percent":    "CPU, % of the limit (of the request without limit, of one core without either), average per instance",
	"memory_percent": "Memory, % of the limit (of the request without limit), average per instance",
	"restarts":       "Container restarts during the last 5 minutes",
	"error_rate":     "HTTP 5xx responses, % of the requests of the last 5 minutes",
	"instances":      "Running instances",
}

var alertComparators = []string{">", ">=", "<", "<="}

const (
	alertWindow       = 5 * time.Minute // rates (restarts, error rate) look this far back
	alertFreshness    = 3 * time.Minute // a sample older than this is no current value
	maxAlertRules     = 50              // per project
	defaultAlertFor   = 5 * time.Minute
	defaultAlertCool  = 30 * time.Minute
	alertEvalInterval = time.Minute
)

// metricSample is one row of service_metrics.
type metricSample struct {
	TS        time.Time
	Instances int
	MemoryMB  int
	CPUMillis int
	Requests  int
	Errors    int
	Restarts  int
}

// metricValue computes a metric from a service's recent samples (oldest first).
// ok is false when there is no data to judge (no traffic for the error rate,
// no memory limit, no recent sample).
func metricValue(metric string, samples []metricSample, res model.Resources, now time.Time) (float64, bool) {
	var recent []metricSample
	for _, s := range samples {
		if !s.TS.Before(now.Add(-alertWindow)) {
			recent = append(recent, s)
		}
	}
	var last *metricSample
	if n := len(recent); n > 0 && !recent[n-1].TS.Before(now.Add(-alertFreshness)) {
		last = &recent[n-1]
	}
	switch metric {
	case "instances":
		// No sample means the leader saw no running instance (metrics.go skips those).
		if last == nil {
			return 0, true
		}
		return float64(last.Instances), true
	case "cpu_percent":
		if last == nil || last.Instances == 0 {
			return 0, false
		}
		ref := res.CPULimitMillis
		if ref <= 0 {
			ref = res.CPURequestMillis
		}
		if ref <= 0 {
			ref = 1000
		}
		return float64(last.CPUMillis) / float64(last.Instances) * 100 / float64(ref), true
	case "memory_percent":
		ref := res.MemoryLimitMB
		if ref <= 0 {
			ref = res.MemoryRequestMB
		}
		if last == nil || last.Instances == 0 || ref <= 0 {
			return 0, false
		}
		return float64(last.MemoryMB) / float64(last.Instances) * 100 / float64(ref), true
	case "restarts":
		// The sum of the counters only grows while instances keep running; a
		// replaced instance takes its count with it, so drops are ignored.
		if len(recent) == 0 {
			return 0, true
		}
		n := 0
		for i := 1; i < len(recent); i++ {
			if d := recent[i].Restarts - recent[i-1].Restarts; d > 0 {
				n += d
			}
		}
		return float64(n), true
	case "error_rate":
		req, errs := 0, 0
		for _, s := range recent {
			req += s.Requests
			errs += s.Errors
		}
		if req == 0 {
			return 0, false
		}
		return float64(errs) * 100 / float64(req), true
	}
	return 0, false
}

func compare(v float64, comparator string, threshold float64) bool {
	switch comparator {
	case ">":
		return v > threshold
	case ">=":
		return v >= threshold
	case "<":
		return v < threshold
	case "<=":
		return v <= threshold
	}
	return false
}

// stepAlert advances one rule/service state with the condition evaluated at now.
// It returns the next state and the notification to send: "firing", "resolved" or "".
func stepAlert(st store.AlertState, cond bool, now time.Time, forDur, cooldown time.Duration) (store.AlertState, string) {
	if !cond {
		switch st.State {
		case "firing":
			notify := ""
			if st.Notified {
				notify = "resolved"
			}
			st.State, st.PendingSince, st.FiringSince, st.Notified = "ok", nil, nil, false
			st.ResolvedAt = &now
			return st, notify
		case "pending":
			st.State, st.PendingSince = "ok", nil
		default:
			st.State = "ok"
		}
		return st, ""
	}
	switch st.State {
	case "firing":
		return st, ""
	case "pending":
	default:
		st.State, st.PendingSince = "pending", &now
	}
	if st.PendingSince == nil {
		st.PendingSince = &now
	}
	if now.Sub(*st.PendingSince) < forDur {
		return st, ""
	}
	st.State, st.FiringSince = "firing", &now
	if st.LastNotifiedAt != nil && now.Sub(*st.LastNotifiedAt) < cooldown {
		st.Notified = false
		return st, ""
	}
	st.Notified, st.LastNotifiedAt = true, &now
	return st, "firing"
}

// alertTargets are the services a rule watches.
func alertTargets(r *store.AlertRule, byProject map[string][]*store.Service) []*store.Service {
	var out []*store.Service
	for _, svc := range byProject[r.ProjectID] {
		if r.ServiceID != nil {
			if svc.ID == *r.ServiceID {
				out = append(out, svc)
			}
			continue
		}
		if svc.Type == model.TypeWeb || svc.Type == model.TypeWorker {
			out = append(out, svc)
		}
	}
	return out
}

func (s *Server) alertLoop(ctx context.Context) {
	t := time.NewTicker(alertEvalInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := s.evaluateAlerts(ctx, time.Now()); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("alert evaluation", "err", err)
		}
	}
}

// evaluateAlerts runs one evaluation of every enabled rule.
func (s *Server) evaluateAlerts(ctx context.Context, now time.Time) error {
	rules, err := s.store.EnabledAlertRules(ctx)
	if err != nil || len(rules) == 0 {
		return err
	}
	services, err := s.store.AllServices(ctx)
	if err != nil {
		return err
	}
	byProject := map[string][]*store.Service{}
	for i := range services {
		byProject[services[i].ProjectID] = append(byProject[services[i].ProjectID], &services[i])
	}
	states, err := s.store.AlertStates(ctx, nil)
	if err != nil {
		return err
	}
	stateOf := map[[2]string]store.AlertState{}
	for _, st := range states {
		stateOf[[2]string{st.RuleID, st.ServiceID}] = st
	}
	samples := map[string][]metricSample{}
	projects := map[string]*store.Project{}
	for i := range rules {
		r := &rules[i]
		p := projects[r.ProjectID]
		if p == nil {
			if p, err = s.store.ProjectByID(ctx, r.ProjectID); err != nil {
				continue
			}
			projects[r.ProjectID] = p
		}
		targets := alertTargets(r, byProject)
		ids := make([]string, 0, len(targets))
		for _, svc := range targets {
			ids = append(ids, svc.ID)
			sm, ok := samples[svc.ID]
			if !ok {
				if sm, err = s.recentSamples(ctx, svc.ID, now); err != nil {
					return err
				}
				samples[svc.ID] = sm
			}
			st, ok := stateOf[[2]string{r.ID, svc.ID}]
			if !ok {
				st = store.AlertState{RuleID: r.ID, ServiceID: svc.ID, State: "ok"}
			}
			// Only running services are judged: a stopped or sleeping one resolves.
			v, has := 0.0, false
			if svc.Status == model.StatusRunning {
				v, has = metricValue(r.Metric, sm, svc.Spec.Resources, now)
			}
			st.Value = nil
			if has {
				st.Value = &v
			}
			next, notify := stepAlert(st, has && compare(v, r.Comparator, r.Threshold), now, time.Duration(r.ForSeconds)*time.Second, time.Duration(r.CooldownSeconds)*time.Second)
			if err := s.store.PutAlertState(ctx, &next); err != nil {
				return err
			}
			if notify != "" {
				s.notifyAlert(ctx, p, svc, r, notify, v)
			}
		}
		if err := s.store.DeleteAlertStatesExcept(ctx, r.ID, ids); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) recentSamples(ctx context.Context, serviceID string, now time.Time) ([]metricSample, error) {
	rows, err := s.store.Q(ctx).Query(ctx, `SELECT ts, instances, memory_mb, cpu_millis, requests, errors_5xx, restarts FROM service_metrics
		WHERE service_id=$1 AND ts >= $2 ORDER BY ts`, serviceID, now.Add(-alertWindow-time.Minute))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []metricSample
	for rows.Next() {
		var m metricSample
		if err := rows.Scan(&m.TS, &m.Instances, &m.MemoryMB, &m.CPUMillis, &m.Requests, &m.Errors, &m.Restarts); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// formatMetric renders a value with its unit.
func formatMetric(metric string, v float64) string {
	switch metric {
	case "cpu_percent", "memory_percent", "error_rate":
		return fmt.Sprintf("%.1f%%", v)
	}
	return fmt.Sprintf("%g", math.Round(v*100)/100)
}

func (s *Server) notifyAlert(ctx context.Context, p *store.Project, svc *store.Service, r *store.AlertRule, kind string, v float64) {
	data := map[string]any{"rule_id": r.ID, "rule": r.Name, "project": p.Slug, "service": svc.Slug, "service_id": svc.ID,
		"metric": r.Metric, "comparator": r.Comparator, "threshold": r.Threshold, "value": v}
	var text string
	if kind == "firing" {
		text = fmt.Sprintf("Alert %q firing on %s/%s: %s is %s (%s %g for %s)", r.Name, p.Slug, svc.Slug, r.Metric, formatMetric(r.Metric, v),
			r.Comparator, r.Threshold, time.Duration(r.ForSeconds)*time.Second)
	} else {
		text = fmt.Sprintf("Alert %q resolved on %s/%s: %s is back to %s", r.Name, p.Slug, svc.Slug, r.Metric, formatMetric(r.Metric, v))
	}
	slog.Info("alert "+kind, "rule", r.Name, "service", svc.Slug, "value", v)
	s.notifyVia(ctx, p.TeamID, r.ChannelIDs, "alert."+kind, text, data)
}

// API

type alertStateView struct {
	store.AlertState
	ServiceName string `json:"service_name"`
}

type alertRuleView struct {
	store.AlertRule
	States []alertStateView `json:"states"`
}

type alertInput struct {
	Name            *string   `json:"name"`
	ServiceID       *string   `json:"service_id"`
	Metric          *string   `json:"metric"`
	Comparator      *string   `json:"comparator"`
	Threshold       *float64  `json:"threshold"`
	ForSeconds      *int      `json:"for_seconds"`
	CooldownSeconds *int      `json:"cooldown_seconds"`
	ChannelIDs      *[]string `json:"channel_ids"`
	Enabled         *bool     `json:"enabled"`
}

// apply copies the given fields onto r.
func (in *alertInput) apply(r *store.AlertRule) {
	if in.Name != nil {
		r.Name = strings.TrimSpace(*in.Name)
	}
	if in.Metric != nil {
		r.Metric = *in.Metric
	}
	if in.Comparator != nil {
		r.Comparator = *in.Comparator
	}
	if in.Threshold != nil {
		r.Threshold = *in.Threshold
	}
	if in.ForSeconds != nil {
		r.ForSeconds = *in.ForSeconds
	}
	if in.CooldownSeconds != nil {
		r.CooldownSeconds = *in.CooldownSeconds
	}
	if in.ChannelIDs != nil {
		r.ChannelIDs = *in.ChannelIDs
	}
	if in.Enabled != nil {
		r.Enabled = *in.Enabled
	}
}

func validateAlertRule(r *store.AlertRule) error {
	if _, ok := alertMetrics[r.Metric]; !ok {
		return fmt.Errorf("metric must be one of cpu_percent, memory_percent, restarts, error_rate, instances")
	}
	if !slices.Contains(alertComparators, r.Comparator) {
		return errors.New("comparator must be >, >=, < or <=")
	}
	if math.IsNaN(r.Threshold) || math.IsInf(r.Threshold, 0) || r.Threshold < 0 {
		return errors.New("threshold must be a non-negative number")
	}
	if r.ForSeconds < 0 || r.ForSeconds > 86400 {
		return errors.New("for_seconds must be between 0 and 86400")
	}
	if r.CooldownSeconds < 0 || r.CooldownSeconds > 7*86400 {
		return errors.New("cooldown_seconds must be between 0 and 604800")
	}
	if r.Name == "" || len(r.Name) > 100 {
		return errors.New("name is required (at most 100 characters)")
	}
	return nil
}

// checkChannels verifies that the chosen channels belong to the team.
func (s *Server) checkChannels(ctx context.Context, teamID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	chs, err := s.store.Channels(ctx, teamID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if !slices.ContainsFunc(chs, func(c store.NotificationChannel) bool { return c.ID == id }) {
			return fmt.Errorf("unknown notification channel %s", id)
		}
	}
	return nil
}

// projectServiceRef resolves an optional service id within a project.
func (s *Server) projectServiceRef(ctx context.Context, projectID string, id *string) (*string, error) {
	if id == nil || *id == "" {
		return nil, nil
	}
	svc, err := s.store.ServiceByID(ctx, *id)
	if err != nil || svc.ProjectID != projectID {
		return nil, errors.New("service not found in this project")
	}
	return &svc.ID, nil
}

func (s *Server) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	p := s.projectFor(w, r, r.PathValue("id"), "viewer")
	if p == nil {
		return
	}
	ctx := r.Context()
	rules, err := s.store.AlertRulesForProject(ctx, p.ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if f := r.URL.Query().Get("service"); f != "" {
		rules = slices.DeleteFunc(rules, func(a store.AlertRule) bool { return a.ServiceID != nil && *a.ServiceID != f })
	}
	ids := make([]string, 0, len(rules))
	for _, a := range rules {
		ids = append(ids, a.ID)
	}
	states, err := s.store.AlertStates(ctx, ids)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	svcs, _ := s.store.ServicesForProject(ctx, p.ID)
	names := map[string]string{}
	for _, svc := range svcs {
		names[svc.ID] = svc.Name
	}
	out := make([]alertRuleView, 0, len(rules))
	for _, a := range rules {
		v := alertRuleView{AlertRule: a, States: []alertStateView{}}
		for _, st := range states {
			if st.RuleID == a.ID {
				v.States = append(v.States, alertStateView{AlertState: st, ServiceName: names[st.ServiceID]})
			}
		}
		out = append(out, v)
	}
	writeJSON(w, 200, map[string]any{"rules": out, "metrics": alertMetrics, "comparators": alertComparators})
}

func (s *Server) handleCreateAlert(w http.ResponseWriter, r *http.Request) {
	p := s.projectFor(w, r, r.PathValue("id"), "developer")
	if p == nil {
		return
	}
	var in alertInput
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	ctx := r.Context()
	svcID, err := s.projectServiceRef(ctx, p.ID, in.ServiceID)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	a := &store.AlertRule{ProjectID: p.ID, ServiceID: svcID, ForSeconds: int(defaultAlertFor.Seconds()), CooldownSeconds: int(defaultAlertCool.Seconds()), Enabled: true}
	in.apply(a)
	if a.Name == "" {
		a.Name = fmt.Sprintf("%s %s %g", a.Metric, a.Comparator, a.Threshold)
	}
	if err := validateAlertRule(a); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if err := s.checkChannels(ctx, p.TeamID, a.ChannelIDs); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if existing, err := s.store.AlertRulesForProject(ctx, p.ID); err == nil && len(existing) >= maxAlertRules {
		writeError(w, 400, fmt.Sprintf("a project has at most %d alert rules", maxAlertRules))
		return
	}
	out, err := s.store.CreateAlertRule(ctx, a)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, out)
}

// alertFor loads the rule of the request and checks the caller's role in its project.
func (s *Server) alertFor(w http.ResponseWriter, r *http.Request, min string) (*store.AlertRule, *store.Project) {
	a, err := s.store.AlertRuleByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "alert rule not found")
		return nil, nil
	}
	p := s.projectFor(w, r, a.ProjectID, min)
	if p == nil {
		return nil, nil
	}
	return a, p
}

func (s *Server) handleUpdateAlert(w http.ResponseWriter, r *http.Request) {
	a, p := s.alertFor(w, r, "developer")
	if a == nil {
		return
	}
	var in alertInput
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if in.ServiceID != nil {
		writeError(w, 400, "service_id cannot be changed: create another rule")
		return
	}
	before := *a
	in.apply(a)
	if err := validateAlertRule(a); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	ctx := r.Context()
	if err := s.checkChannels(ctx, p.TeamID, a.ChannelIDs); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	out, err := s.store.UpdateAlertRule(ctx, a)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	// A different condition (or a disabled rule) starts from a clean state.
	if !out.Enabled || before.Metric != out.Metric || before.Comparator != out.Comparator || before.Threshold != out.Threshold {
		_ = s.store.ResetAlertStates(ctx, out.ID)
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleDeleteAlert(w http.ResponseWriter, r *http.Request) {
	a, _ := s.alertFor(w, r, "developer")
	if a == nil {
		return
	}
	if err := s.store.DeleteAlertRule(r.Context(), a.ID); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}
