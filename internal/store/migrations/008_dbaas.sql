ALTER TABLE databases ADD COLUMN options JSONB NOT NULL DEFAULT '{}';

CREATE TABLE db_metrics (
    database_id UUID NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
    ts          TIMESTAMPTZ NOT NULL,
    size_bytes  BIGINT NOT NULL DEFAULT 0,
    connections INT NOT NULL DEFAULT 0,
    PRIMARY KEY (database_id, ts)
);
