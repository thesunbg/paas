CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email         TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL,
    is_admin      BOOLEAN NOT NULL DEFAULT false,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    token_hash TEXT PRIMARY KEY,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE api_tokens (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    last_used_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE teams (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT NOT NULL,
    slug       TEXT NOT NULL UNIQUE,
    is_system  BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE memberships (
    team_id    UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       TEXT NOT NULL DEFAULT 'developer', -- owner | admin | developer | viewer
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, user_id)
);

CREATE TABLE projects (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id    UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    slug       TEXT NOT NULL,
    net_index  SERIAL UNIQUE, -- used to derive the project's private subnet / bridge
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (team_id, slug)
);

CREATE TABLE nodes (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name         TEXT NOT NULL UNIQUE,
    address      TEXT NOT NULL DEFAULT '',
    roles        TEXT[] NOT NULL DEFAULT '{}',
    labels       JSONB NOT NULL DEFAULT '{}',
    status       TEXT NOT NULL DEFAULT 'online', -- online | offline | cordoned
    cpu_cores    INT NOT NULL DEFAULT 0,
    memory_mb    INT NOT NULL DEFAULT 0,
    reserved_mb  INT NOT NULL DEFAULT 1024,
    overcommit_memory DOUBLE PRECISION NOT NULL DEFAULT 1.5,
    overcommit_cpu    DOUBLE PRECISION NOT NULL DEFAULT 4.0,
    capabilities JSONB NOT NULL DEFAULT '{}',
    usage        JSONB NOT NULL DEFAULT '{}',
    version      TEXT NOT NULL DEFAULT '',
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE services (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id     UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    slug           TEXT NOT NULL,
    type           TEXT NOT NULL, -- web | worker | static | database
    source         JSONB NOT NULL DEFAULT '{}',
    build          JSONB NOT NULL DEFAULT '{}',
    spec           JSONB NOT NULL DEFAULT '{}',
    git_token_enc  TEXT NOT NULL DEFAULT '',
    webhook_secret TEXT NOT NULL,
    node_id        UUID REFERENCES nodes(id) ON DELETE SET NULL, -- pinned node (volumes)
    current_deployment_id UUID,
    status         TEXT NOT NULL DEFAULT 'created', -- created | deploying | running | sleeping | failed | stopped
    last_request_at TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, slug)
);

CREATE TABLE env_vars (
    service_id UUID NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    key        TEXT NOT NULL,
    value_enc  TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (service_id, key)
);

CREATE TABLE domains (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id UUID NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    hostname   TEXT NOT NULL UNIQUE,
    is_generated BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE deployments (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id     UUID NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    status         TEXT NOT NULL DEFAULT 'queued', -- queued | building | deploying | active | superseded | failed | cancelled
    trigger        TEXT NOT NULL DEFAULT 'manual', -- manual | webhook | rollback | api
    commit_sha     TEXT NOT NULL DEFAULT '',
    commit_message TEXT NOT NULL DEFAULT '',
    branch         TEXT NOT NULL DEFAULT '',
    image          TEXT NOT NULL DEFAULT '',
    static_path    TEXT NOT NULL DEFAULT '',
    build_node_id  UUID REFERENCES nodes(id) ON DELETE SET NULL,
    detected       TEXT NOT NULL DEFAULT '',
    error          TEXT NOT NULL DEFAULT '',
    created_by     UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at     TIMESTAMPTZ,
    finished_at    TIMESTAMPTZ
);
CREATE INDEX deployments_service_idx ON deployments (service_id, created_at DESC);
CREATE INDEX deployments_queued_idx ON deployments (created_at) WHERE status = 'queued';

CREATE TABLE deployment_logs (
    id            BIGSERIAL PRIMARY KEY,
    deployment_id UUID NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    ts            TIMESTAMPTZ NOT NULL DEFAULT now(),
    line          TEXT NOT NULL
);
CREATE INDEX deployment_logs_dep_idx ON deployment_logs (deployment_id, id);

CREATE TABLE instances (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id    UUID NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    deployment_id UUID REFERENCES deployments(id) ON DELETE SET NULL,
    node_id       UUID NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    container_id  TEXT NOT NULL UNIQUE,
    ip            TEXT NOT NULL DEFAULT '',
    port          INT NOT NULL DEFAULT 0,
    status        TEXT NOT NULL DEFAULT 'starting', -- starting | running | stopping | stopped | failed | lost
    memory_request_mb INT NOT NULL DEFAULT 0,
    cpu_request_millis INT NOT NULL DEFAULT 0,
    memory_usage_mb INT NOT NULL DEFAULT 0,
    memory_peak_mb  INT NOT NULL DEFAULT 0,
    cpu_usage_millis INT NOT NULL DEFAULT 0,
    restarts      INT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX instances_service_idx ON instances (service_id);
CREATE INDEX instances_node_idx ON instances (node_id);

CREATE TABLE databases (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    engine       TEXT NOT NULL, -- postgres | mysql | redis | mongo
    mode         TEXT NOT NULL, -- shared | dedicated
    server_service_id UUID REFERENCES services(id) ON DELETE SET NULL, -- the service running the DB server
    db_name      TEXT NOT NULL DEFAULT '',
    username     TEXT NOT NULL DEFAULT '',
    password_enc TEXT NOT NULL,
    size_preset  TEXT NOT NULL DEFAULT 'small',
    status       TEXT NOT NULL DEFAULT 'provisioning', -- provisioning | ready | failed | deleting
    error        TEXT NOT NULL DEFAULT '',
    backup_schedule_hours INT NOT NULL DEFAULT 24,
    last_backup_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (project_id, name)
);

CREATE TABLE service_databases (
    service_id  UUID NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    database_id UUID NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
    env_name    TEXT NOT NULL,
    PRIMARY KEY (service_id, database_id)
);

CREATE TABLE backups (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    database_id UUID NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
    node_id     UUID REFERENCES nodes(id) ON DELETE SET NULL,
    status      TEXT NOT NULL DEFAULT 'running', -- running | completed | failed
    path        TEXT NOT NULL DEFAULT '',
    size_bytes  BIGINT NOT NULL DEFAULT 0,
    error       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);

CREATE TABLE audit_logs (
    id         BIGSERIAL PRIMARY KEY,
    user_id    UUID REFERENCES users(id) ON DELETE SET NULL,
    team_id    UUID REFERENCES teams(id) ON DELETE CASCADE,
    action     TEXT NOT NULL,
    target     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
