-- Moves of a service with volumes to another node (server/volmove.go), and restores of one from
-- its newest volume backup when its node is lost. One row per attempt, kept as history; at most
-- one unfinished attempt per service. The row is the state machine: a restart of paasd finishes
-- or undoes an attempt from its phase.
CREATE TABLE volume_moves (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id      UUID NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    kind            TEXT NOT NULL DEFAULT 'move',    -- move (live copy node to node) | restore (from a backup)
    from_node       UUID REFERENCES nodes(id) ON DELETE SET NULL,
    to_node         UUID REFERENCES nodes(id) ON DELETE SET NULL,
    -- pending | stopping | copying | verifying | switching | starting | done | failed | aborted
    phase           TEXT NOT NULL DEFAULT 'pending',
    prev_status     TEXT NOT NULL DEFAULT '',        -- the service's status before the move (restored on failure)
    abort_requested BOOLEAN NOT NULL DEFAULT false,
    backup_id       UUID REFERENCES volume_backups(id) ON DELETE SET NULL,
    rpo_at          TIMESTAMPTZ,                     -- restore: when the backup used was taken (later writes are lost)
    bytes           BIGINT NOT NULL DEFAULT 0,
    files           BIGINT NOT NULL DEFAULT 0,
    digest          TEXT NOT NULL DEFAULT '',
    retain_days     INT NOT NULL DEFAULT 7,          -- the old copy stays on from_node this long after success
    error           TEXT NOT NULL DEFAULT '',
    requested_by    TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at     TIMESTAMPTZ
);
CREATE INDEX volume_moves_service_idx ON volume_moves (service_id, created_at DESC);
CREATE UNIQUE INDEX volume_moves_active_idx ON volume_moves (service_id) WHERE phase NOT IN ('done', 'failed', 'aborted');
