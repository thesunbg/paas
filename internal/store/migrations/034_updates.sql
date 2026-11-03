-- Updates of paas itself (server/update.go, internal/update). Additive only: a binary
-- rolled back to the previous release keeps working on this schema.

-- Every control plane reports itself here every few seconds: the version it runs and
-- whether it holds leadership (the highest leader_epoch is the current leader).
CREATE TABLE control_planes (
    name         TEXT PRIMARY KEY,                -- PAAS_SERVER_NAME
    version      TEXT NOT NULL DEFAULT '',
    arch         TEXT NOT NULL DEFAULT '',
    node_id      UUID REFERENCES nodes(id) ON DELETE SET NULL, -- its embedded agent's node
    leader_epoch BIGINT NOT NULL DEFAULT 0,
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Rolling upgrades, one row per attempt (history). The row is the state machine
-- (steps): whichever control plane leads continues it after a restart.
CREATE TABLE update_rollouts (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    version      TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'running', -- running | paused | failed | aborted | done
    message      TEXT NOT NULL DEFAULT '',
    steps        JSONB NOT NULL DEFAULT '[]',
    release      JSONB,
    checksums    JSONB NOT NULL DEFAULT '{}',     -- arch -> SHA-256 of paasd-linux-<arch>
    requested_by TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at  TIMESTAMPTZ
);
CREATE INDEX update_rollouts_created_idx ON update_rollouts (created_at DESC);
CREATE UNIQUE INDEX update_rollouts_active_idx ON update_rollouts ((true)) WHERE status IN ('running', 'paused');

-- A node an administrator rolled back by hand keeps its binary: it does not follow the
-- control plane's binary on its own (agent self-update) until a rollout updates it.
ALTER TABLE nodes ADD COLUMN binary_pinned BOOLEAN NOT NULL DEFAULT false;
