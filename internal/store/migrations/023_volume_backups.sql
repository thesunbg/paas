-- Backups of service volumes (server/volume_backup.go): a tar.gz of volumes/<service> made on the
-- service's node, kept off-site. They let a service with volumes start again on another node when
-- its own is lost.
CREATE TABLE volume_backups (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    service_id  UUID NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    node_id     UUID REFERENCES nodes(id) ON DELETE SET NULL,
    status      TEXT NOT NULL DEFAULT 'running', -- running | completed | failed
    s3_key      TEXT NOT NULL DEFAULT '',
    size_bytes  BIGINT NOT NULL DEFAULT 0,
    error       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);
CREATE INDEX volume_backups_service_idx ON volume_backups (service_id, created_at DESC);
