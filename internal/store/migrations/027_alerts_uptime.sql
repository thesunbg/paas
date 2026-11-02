-- Observability: threshold alerts on the stored metrics, HTTP uptime checks.

-- Sum of the container restart counters of a service's instances, per minute
-- (alerts on restarts look at its growth).
ALTER TABLE service_metrics ADD COLUMN restarts INT NOT NULL DEFAULT 0;

-- A rule watches one metric of one service, or of every web/worker service of
-- the project when service_id is NULL.
CREATE TABLE alert_rules (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id       UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    service_id       UUID REFERENCES services(id) ON DELETE CASCADE,
    name             TEXT NOT NULL,
    metric           TEXT NOT NULL,               -- cpu_percent | memory_percent | restarts | error_rate | instances
    comparator       TEXT NOT NULL,               -- > | >= | < | <=
    threshold        DOUBLE PRECISION NOT NULL,
    for_seconds      INT NOT NULL DEFAULT 300,    -- the condition must hold this long before firing
    cooldown_seconds INT NOT NULL DEFAULT 1800,   -- no new firing notification within this after the previous one
    channel_ids      UUID[] NOT NULL DEFAULT '{}', -- empty: the team's channels subscribed to alert.* events
    enabled          BOOLEAN NOT NULL DEFAULT true,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX alert_rules_project ON alert_rules (project_id);

-- Evaluation state per rule and service (persisted so a new leader neither
-- forgets a firing alert nor notifies it again).
CREATE TABLE alert_states (
    rule_id          UUID NOT NULL REFERENCES alert_rules(id) ON DELETE CASCADE,
    service_id       UUID NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    state            TEXT NOT NULL DEFAULT 'ok',  -- ok | pending | firing
    value            DOUBLE PRECISION,            -- last evaluated value; NULL = no data
    pending_since    TIMESTAMPTZ,
    firing_since     TIMESTAMPTZ,
    resolved_at      TIMESTAMPTZ,
    last_notified_at TIMESTAMPTZ,                 -- last firing notification (cooldown)
    notified         BOOLEAN NOT NULL DEFAULT false, -- the current firing was notified: its resolve is too
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (rule_id, service_id)
);

CREATE TABLE uptime_checks (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id           UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    service_id           UUID REFERENCES services(id) ON DELETE CASCADE,
    name                 TEXT NOT NULL,
    url                  TEXT NOT NULL,
    method               TEXT NOT NULL DEFAULT 'GET',
    expected_status      INT NOT NULL DEFAULT 0,  -- 0: any 2xx or 3xx
    keyword              TEXT NOT NULL DEFAULT '',
    timeout_seconds      INT NOT NULL DEFAULT 10,
    interval_seconds     INT NOT NULL DEFAULT 60 CHECK (interval_seconds >= 30),
    failure_threshold    INT NOT NULL DEFAULT 3,  -- consecutive failures before "down"
    channel_ids          UUID[] NOT NULL DEFAULT '{}',
    enabled              BOOLEAN NOT NULL DEFAULT true,
    state                TEXT NOT NULL DEFAULT 'unknown', -- unknown | up | down
    consecutive_failures INT NOT NULL DEFAULT 0,
    last_checked_at      TIMESTAMPTZ,
    last_error           TEXT NOT NULL DEFAULT '',
    state_changed_at     TIMESTAMPTZ,
    cert_expires_at      TIMESTAMPTZ,             -- https: the served certificate's expiry
    cert_warned_expiry   TIMESTAMPTZ,             -- expiry already warned about (once per certificate)
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX uptime_checks_project ON uptime_checks (project_id);

-- Recent results, kept 7 days (enough for the 7-day uptime).
CREATE TABLE uptime_results (
    check_id    UUID NOT NULL REFERENCES uptime_checks(id) ON DELETE CASCADE,
    ts          TIMESTAMPTZ NOT NULL,
    ok          BOOLEAN NOT NULL,
    status_code INT NOT NULL DEFAULT 0,
    latency_ms  INT NOT NULL DEFAULT 0,
    error       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (check_id, ts)
);
