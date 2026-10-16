ALTER TABLE databases ADD COLUMN options JSONB NOT NULL DEFAULT '{}';

CREATE TABLE db_metrics (
    database_id UUID NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
    ts          TIMESTAMPTZ NOT NULL,
    size_bytes  BIGINT NOT NULL DEFAULT 0,
    connections INT NOT NULL DEFAULT 0,
    PRIMARY KEY (database_id, ts)
);

CREATE TABLE db_users (
    database_id  UUID NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
    username     TEXT NOT NULL,
    role         TEXT NOT NULL,
    password_enc TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (database_id, username)
);

ALTER TABLE backups ADD COLUMN kind TEXT NOT NULL DEFAULT 'dump'; -- dump | base (PITR base backup)
