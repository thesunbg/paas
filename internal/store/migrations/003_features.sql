-- Pull request previews
ALTER TABLE services ADD COLUMN parent_id UUID REFERENCES services(id) ON DELETE CASCADE;
ALTER TABLE services ADD COLUMN preview_pr INT;
CREATE UNIQUE INDEX services_preview_idx ON services (parent_id, preview_pr) WHERE parent_id IS NOT NULL;

-- Cron jobs
CREATE TABLE job_runs (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id   UUID NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    node_id      UUID REFERENCES nodes(id) ON DELETE SET NULL,
    container_id TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'running', -- running | succeeded | failed
    exit_code    INT,
    output       TEXT NOT NULL DEFAULT '',
    trigger      TEXT NOT NULL DEFAULT 'schedule',
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at  TIMESTAMPTZ
);
CREATE INDEX job_runs_service_idx ON job_runs (service_id, started_at DESC);

-- Notifications
CREATE TABLE notification_channels (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id    UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    kind       TEXT NOT NULL, -- webhook | slack | discord | telegram
    target_enc TEXT NOT NULL, -- URL (or bot token + chat id for telegram), encrypted
    events     TEXT[] NOT NULL DEFAULT '{deploy.failed,deploy.succeeded,instance.crashloop,node.offline,backup.failed}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Metrics history (lightweight, one sample per minute per service)
CREATE TABLE service_metrics (
    service_id UUID NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    ts         TIMESTAMPTZ NOT NULL,
    instances  INT NOT NULL,
    memory_mb  INT NOT NULL,
    cpu_millis INT NOT NULL,
    requests   INT NOT NULL DEFAULT 0,
    errors_5xx INT NOT NULL DEFAULT 0,
    PRIMARY KEY (service_id, ts)
);
CREATE TABLE node_metrics (
    node_id     UUID NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    ts          TIMESTAMPTZ NOT NULL,
    cpu_percent REAL NOT NULL,
    memory_used_mb INT NOT NULL,
    memory_committed_mb INT NOT NULL,
    PRIMARY KEY (node_id, ts)
);

-- Two-factor authentication
ALTER TABLE users ADD COLUMN totp_secret_enc TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN totp_enabled BOOLEAN NOT NULL DEFAULT false;
