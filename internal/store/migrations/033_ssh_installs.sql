-- Nodes added over SSH from the dashboard or the CLI (server/ssh_install.go). One row per
-- attempt, kept as history. The SSH credentials (password, private key) are never stored:
-- they live in the memory of the control plane running the attempt, for its duration only.
CREATE TABLE ssh_installs (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    host                 TEXT NOT NULL,
    port                 INT NOT NULL DEFAULT 22,
    ssh_user             TEXT NOT NULL,
    auth_method          TEXT NOT NULL,                  -- key | password (the method only)
    roles                TEXT[] NOT NULL,
    node_name            TEXT NOT NULL DEFAULT '',       -- requested name; '' = the machine's hostname
    host_key             TEXT NOT NULL DEFAULT '',       -- accepted host key, authorized_keys format
    host_key_fingerprint TEXT NOT NULL DEFAULT '',       -- SHA256:...
    reinstall            BOOLEAN NOT NULL DEFAULT false,
    preflight_only       BOOLEAN NOT NULL DEFAULT false,
    status               TEXT NOT NULL DEFAULT 'running', -- running | succeeded | failed
    step                 TEXT NOT NULL DEFAULT 'connect', -- connect | preflight | install | enroll | done
    preflight            JSONB NOT NULL DEFAULT '{}',     -- preflight report: checklist, OS, memory, disk
    error                TEXT NOT NULL DEFAULT '',
    token_hash           BYTEA,                           -- join_tokens.hash of the token issued for it
    node_id              UUID REFERENCES nodes(id) ON DELETE SET NULL,
    server_name          TEXT NOT NULL DEFAULT '',        -- control plane running the attempt
    created_by           UUID REFERENCES users(id) ON DELETE SET NULL,
    created_by_email     TEXT NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at          TIMESTAMPTZ
);
CREATE INDEX ssh_installs_created_idx ON ssh_installs (created_at DESC);

-- Installer output of an attempt, credentials masked.
CREATE TABLE ssh_install_logs (
    id         BIGSERIAL PRIMARY KEY,
    install_id UUID NOT NULL REFERENCES ssh_installs(id) ON DELETE CASCADE,
    line       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ssh_install_logs_idx ON ssh_install_logs (install_id, id);

-- The node a join token enrolled, so an install can tell when its node has joined.
ALTER TABLE join_tokens ADD COLUMN node_id UUID REFERENCES nodes(id) ON DELETE SET NULL;

-- SSH host key accepted when the node was installed over SSH (authorized_keys format).
ALTER TABLE nodes ADD COLUMN ssh_host_key TEXT NOT NULL DEFAULT '';
