package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Alert rules and uptime checks (observability).

type AlertRule struct {
	ID              string    `json:"id"`
	ProjectID       string    `json:"project_id"`
	ServiceID       *string   `json:"service_id"` // nil: every web/worker service of the project
	Name            string    `json:"name"`
	Metric          string    `json:"metric"`
	Comparator      string    `json:"comparator"`
	Threshold       float64   `json:"threshold"`
	ForSeconds      int       `json:"for_seconds"`
	CooldownSeconds int       `json:"cooldown_seconds"`
	ChannelIDs      []string  `json:"channel_ids"`
	Enabled         bool      `json:"enabled"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

const alertRuleCols = `id, project_id, service_id, name, metric, comparator, threshold, for_seconds, cooldown_seconds, channel_ids::text[], enabled, created_at, updated_at`

func scanAlertRule(row pgx.Row) (*AlertRule, error) {
	var a AlertRule
	err := row.Scan(&a.ID, &a.ProjectID, &a.ServiceID, &a.Name, &a.Metric, &a.Comparator, &a.Threshold, &a.ForSeconds, &a.CooldownSeconds,
		&a.ChannelIDs, &a.Enabled, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	if a.ChannelIDs == nil {
		a.ChannelIDs = []string{}
	}
	return &a, nil
}

func collectAlertRules(rows pgx.Rows, err error) ([]AlertRule, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AlertRule, error) {
		a, err := scanAlertRule(r)
		if err != nil {
			return AlertRule{}, err
		}
		return *a, nil
	})
}

func (s *Store) CreateAlertRule(ctx context.Context, a *AlertRule) (*AlertRule, error) {
	return scanAlertRule(s.q(ctx).QueryRow(ctx, `INSERT INTO alert_rules (project_id, service_id, name, metric, comparator, threshold, for_seconds, cooldown_seconds, channel_ids, enabled)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::uuid[],$10) RETURNING `+alertRuleCols,
		a.ProjectID, a.ServiceID, a.Name, a.Metric, a.Comparator, a.Threshold, a.ForSeconds, a.CooldownSeconds, nonNilIDs(a.ChannelIDs), a.Enabled))
}

// UpdateAlertRule writes the editable fields (not project / service).
func (s *Store) UpdateAlertRule(ctx context.Context, a *AlertRule) (*AlertRule, error) {
	return scanAlertRule(s.q(ctx).QueryRow(ctx, `UPDATE alert_rules SET name=$2, metric=$3, comparator=$4, threshold=$5, for_seconds=$6, cooldown_seconds=$7,
		channel_ids=$8::uuid[], enabled=$9, updated_at=now() WHERE id=$1 RETURNING `+alertRuleCols,
		a.ID, a.Name, a.Metric, a.Comparator, a.Threshold, a.ForSeconds, a.CooldownSeconds, nonNilIDs(a.ChannelIDs), a.Enabled))
}

func (s *Store) AlertRuleByID(ctx context.Context, id string) (*AlertRule, error) {
	return scanAlertRule(s.q(ctx).QueryRow(ctx, `SELECT `+alertRuleCols+` FROM alert_rules WHERE id=$1`, id))
}

func (s *Store) AlertRulesForProject(ctx context.Context, projectID string) ([]AlertRule, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT `+alertRuleCols+` FROM alert_rules WHERE project_id=$1 ORDER BY created_at`, projectID)
	return collectAlertRules(rows, err)
}

func (s *Store) EnabledAlertRules(ctx context.Context) ([]AlertRule, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT `+alertRuleCols+` FROM alert_rules WHERE enabled ORDER BY created_at`)
	return collectAlertRules(rows, err)
}

func (s *Store) DeleteAlertRule(ctx context.Context, id string) error {
	_, err := s.q(ctx).Exec(ctx, `DELETE FROM alert_rules WHERE id=$1`, id)
	return err
}

// AlertState is the evaluation state of a rule for one service.
type AlertState struct {
	RuleID         string     `json:"rule_id"`
	ServiceID      string     `json:"service_id"`
	State          string     `json:"state"` // ok | pending | firing
	Value          *float64   `json:"value"`
	PendingSince   *time.Time `json:"pending_since"`
	FiringSince    *time.Time `json:"firing_since"`
	ResolvedAt     *time.Time `json:"resolved_at"`
	LastNotifiedAt *time.Time `json:"last_notified_at"`
	Notified       bool       `json:"notified"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

const alertStateCols = `rule_id, service_id, state, value, pending_since, firing_since, resolved_at, last_notified_at, notified, updated_at`

func collectAlertStates(rows pgx.Rows, err error) ([]AlertState, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AlertState, error) {
		var a AlertState
		err := r.Scan(&a.RuleID, &a.ServiceID, &a.State, &a.Value, &a.PendingSince, &a.FiringSince, &a.ResolvedAt, &a.LastNotifiedAt, &a.Notified, &a.UpdatedAt)
		return a, err
	})
}

// AlertStates returns the states of the given rules (all rules when ids is nil).
func (s *Store) AlertStates(ctx context.Context, ruleIDs []string) ([]AlertState, error) {
	if ruleIDs == nil {
		rows, err := s.q(ctx).Query(ctx, `SELECT `+alertStateCols+` FROM alert_states`)
		return collectAlertStates(rows, err)
	}
	rows, err := s.q(ctx).Query(ctx, `SELECT `+alertStateCols+` FROM alert_states WHERE rule_id = ANY($1::uuid[])`, ruleIDs)
	return collectAlertStates(rows, err)
}

func (s *Store) PutAlertState(ctx context.Context, a *AlertState) error {
	_, err := s.q(ctx).Exec(ctx, `INSERT INTO alert_states (rule_id, service_id, state, value, pending_since, firing_since, resolved_at, last_notified_at, notified, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,now())
		ON CONFLICT (rule_id, service_id) DO UPDATE SET state=EXCLUDED.state, value=EXCLUDED.value, pending_since=EXCLUDED.pending_since,
		firing_since=EXCLUDED.firing_since, resolved_at=EXCLUDED.resolved_at, last_notified_at=EXCLUDED.last_notified_at, notified=EXCLUDED.notified, updated_at=now()`,
		a.RuleID, a.ServiceID, a.State, a.Value, a.PendingSince, a.FiringSince, a.ResolvedAt, a.LastNotifiedAt, a.Notified)
	return err
}

// DeleteAlertStatesExcept drops the states of a rule for services it no longer covers.
func (s *Store) DeleteAlertStatesExcept(ctx context.Context, ruleID string, serviceIDs []string) error {
	_, err := s.q(ctx).Exec(ctx, `DELETE FROM alert_states WHERE rule_id=$1 AND NOT (service_id = ANY($2::uuid[]))`, ruleID, nonNilIDs(serviceIDs))
	return err
}

func (s *Store) ResetAlertStates(ctx context.Context, ruleID string) error {
	_, err := s.q(ctx).Exec(ctx, `DELETE FROM alert_states WHERE rule_id=$1`, ruleID)
	return err
}

// Uptime checks.

type UptimeCheck struct {
	ID                  string     `json:"id"`
	ProjectID           string     `json:"project_id"`
	ServiceID           *string    `json:"service_id"`
	Name                string     `json:"name"`
	URL                 string     `json:"url"`
	Method              string     `json:"method"`
	ExpectedStatus      int        `json:"expected_status"` // 0: any 2xx or 3xx
	Keyword             string     `json:"keyword"`
	TimeoutSeconds      int        `json:"timeout_seconds"`
	IntervalSeconds     int        `json:"interval_seconds"`
	FailureThreshold    int        `json:"failure_threshold"`
	ChannelIDs          []string   `json:"channel_ids"`
	Enabled             bool       `json:"enabled"`
	State               string     `json:"state"` // unknown | up | down
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastCheckedAt       *time.Time `json:"last_checked_at"`
	LastError           string     `json:"last_error"`
	StateChangedAt      *time.Time `json:"state_changed_at"`
	CertExpiresAt       *time.Time `json:"cert_expires_at"`
	CertWarnedExpiry    *time.Time `json:"-"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

const uptimeCols = `id, project_id, service_id, name, url, method, expected_status, keyword, timeout_seconds, interval_seconds, failure_threshold,
	channel_ids::text[], enabled, state, consecutive_failures, last_checked_at, last_error, state_changed_at, cert_expires_at, cert_warned_expiry, created_at, updated_at`

func scanUptimeCheck(row pgx.Row) (*UptimeCheck, error) {
	var c UptimeCheck
	err := row.Scan(&c.ID, &c.ProjectID, &c.ServiceID, &c.Name, &c.URL, &c.Method, &c.ExpectedStatus, &c.Keyword, &c.TimeoutSeconds, &c.IntervalSeconds,
		&c.FailureThreshold, &c.ChannelIDs, &c.Enabled, &c.State, &c.ConsecutiveFailures, &c.LastCheckedAt, &c.LastError, &c.StateChangedAt,
		&c.CertExpiresAt, &c.CertWarnedExpiry, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	if c.ChannelIDs == nil {
		c.ChannelIDs = []string{}
	}
	return &c, nil
}

func collectUptimeChecks(rows pgx.Rows, err error) ([]UptimeCheck, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (UptimeCheck, error) {
		c, err := scanUptimeCheck(r)
		if err != nil {
			return UptimeCheck{}, err
		}
		return *c, nil
	})
}

func (s *Store) CreateUptimeCheck(ctx context.Context, c *UptimeCheck) (*UptimeCheck, error) {
	return scanUptimeCheck(s.q(ctx).QueryRow(ctx, `INSERT INTO uptime_checks (project_id, service_id, name, url, method, expected_status, keyword, timeout_seconds,
		interval_seconds, failure_threshold, channel_ids, enabled) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::uuid[],$12) RETURNING `+uptimeCols,
		c.ProjectID, c.ServiceID, c.Name, c.URL, c.Method, c.ExpectedStatus, c.Keyword, c.TimeoutSeconds, c.IntervalSeconds, c.FailureThreshold, nonNilIDs(c.ChannelIDs), c.Enabled))
}

// UpdateUptimeCheck writes the editable fields; a changed URL starts over (unknown state).
func (s *Store) UpdateUptimeCheck(ctx context.Context, c *UptimeCheck) (*UptimeCheck, error) {
	return scanUptimeCheck(s.q(ctx).QueryRow(ctx, `UPDATE uptime_checks SET name=$2, url=$3, method=$4, expected_status=$5, keyword=$6, timeout_seconds=$7,
		interval_seconds=$8, failure_threshold=$9, channel_ids=$10::uuid[], enabled=$11,
		state = CASE WHEN url <> $3 THEN 'unknown' ELSE state END,
		consecutive_failures = CASE WHEN url <> $3 THEN 0 ELSE consecutive_failures END,
		cert_expires_at = CASE WHEN url <> $3 THEN NULL ELSE cert_expires_at END,
		updated_at=now() WHERE id=$1 RETURNING `+uptimeCols,
		c.ID, c.Name, c.URL, c.Method, c.ExpectedStatus, c.Keyword, c.TimeoutSeconds, c.IntervalSeconds, c.FailureThreshold, nonNilIDs(c.ChannelIDs), c.Enabled))
}

func (s *Store) UptimeCheckByID(ctx context.Context, id string) (*UptimeCheck, error) {
	return scanUptimeCheck(s.q(ctx).QueryRow(ctx, `SELECT `+uptimeCols+` FROM uptime_checks WHERE id=$1`, id))
}

func (s *Store) UptimeChecksForProject(ctx context.Context, projectID string) ([]UptimeCheck, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT `+uptimeCols+` FROM uptime_checks WHERE project_id=$1 ORDER BY created_at`, projectID)
	return collectUptimeChecks(rows, err)
}

func (s *Store) AllUptimeChecks(ctx context.Context) ([]UptimeCheck, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT `+uptimeCols+` FROM uptime_checks ORDER BY created_at`)
	return collectUptimeChecks(rows, err)
}

func (s *Store) DeleteUptimeCheck(ctx context.Context, id string) error {
	_, err := s.q(ctx).Exec(ctx, `DELETE FROM uptime_checks WHERE id=$1`, id)
	return err
}

type UptimeResult struct {
	TS         time.Time `json:"ts"`
	OK         bool      `json:"ok"`
	StatusCode int       `json:"status_code"`
	LatencyMS  int       `json:"latency_ms"`
	Error      string    `json:"error,omitempty"`
}

// RecordUptimeResult stores a result and the check's new state in one transaction.
func (s *Store) RecordUptimeResult(ctx context.Context, c *UptimeCheck, r *UptimeResult) error {
	return pgx.BeginFunc(ctx, s.q(ctx), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO uptime_results (check_id, ts, ok, status_code, latency_ms, error) VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`,
			c.ID, r.TS, r.OK, r.StatusCode, r.LatencyMS, r.Error); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE uptime_checks SET state=$2, consecutive_failures=$3, last_checked_at=$4, last_error=$5, state_changed_at=$6,
			cert_expires_at=$7, cert_warned_expiry=$8 WHERE id=$1`,
			c.ID, c.State, c.ConsecutiveFailures, c.LastCheckedAt, c.LastError, c.StateChangedAt, c.CertExpiresAt, c.CertWarnedExpiry)
		return err
	})
}

func (s *Store) UptimeResults(ctx context.Context, checkID string, limit int) ([]UptimeResult, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT ts, ok, status_code, latency_ms, error FROM uptime_results WHERE check_id=$1 ORDER BY ts DESC LIMIT $2`, checkID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (UptimeResult, error) {
		var u UptimeResult
		err := r.Scan(&u.TS, &u.OK, &u.StatusCode, &u.LatencyMS, &u.Error)
		return u, err
	})
}

// UptimeStats summarizes a check's results; the percentages are nil without results.
type UptimeStats struct {
	Uptime24h    *float64 `json:"uptime_24h"`
	Uptime7d     *float64 `json:"uptime_7d"`
	AvgLatency24 *float64 `json:"avg_latency_ms_24h"`
	P95Latency24 *float64 `json:"p95_latency_ms_24h"`
	Checks24h    int      `json:"checks_24h"`
}

// UptimeStatsFor computes the stats of the given checks.
func (s *Store) UptimeStatsFor(ctx context.Context, checkIDs []string) (map[string]UptimeStats, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT check_id::text,
		100.0 * count(*) FILTER (WHERE ok AND ts > now() - interval '24 hours') / NULLIF(count(*) FILTER (WHERE ts > now() - interval '24 hours'), 0),
		100.0 * count(*) FILTER (WHERE ok) / NULLIF(count(*), 0),
		avg(latency_ms) FILTER (WHERE ok AND ts > now() - interval '24 hours'),
		percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE ok AND ts > now() - interval '24 hours'),
		count(*) FILTER (WHERE ts > now() - interval '24 hours')
		FROM uptime_results WHERE check_id = ANY($1::uuid[]) AND ts > now() - interval '7 days' GROUP BY check_id`, nonNilIDs(checkIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]UptimeStats{}
	for rows.Next() {
		var id string
		var st UptimeStats
		if err := rows.Scan(&id, &st.Uptime24h, &st.Uptime7d, &st.AvgLatency24, &st.P95Latency24, &st.Checks24h); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
}

// PruneUptimeResults keeps 7 days of results.
func (s *Store) PruneUptimeResults(ctx context.Context) error {
	_, err := s.q(ctx).Exec(ctx, `DELETE FROM uptime_results WHERE ts < now() - interval '7 days'`)
	return err
}

func nonNilIDs(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
