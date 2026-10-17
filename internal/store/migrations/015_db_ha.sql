-- Control-plane state of DB high availability.
-- primary_lsn: last pg_current_wal_lsn() of the primary (lag check at failover, not in
-- databases.options: that is CAS-versioned and this is written every 10 s).
-- failover_to: the standby service chosen for promotion; set before pg_promote so a new
-- leader finishes the failover. '' = none.
-- force_until: an admin accepted data loss (standby lag over the limit) until then.
CREATE TABLE db_ha_state (
    database_id UUID PRIMARY KEY REFERENCES databases(id) ON DELETE CASCADE,
    primary_lsn pg_lsn,
    lsn_at      TIMESTAMPTZ,
    failover_to TEXT NOT NULL DEFAULT '',
    force_until TIMESTAMPTZ,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Volumes of removed servers, kept until delete_after (data of a failed-over primary).
CREATE TABLE volume_gc (
    node_id      UUID NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    service_id   UUID NOT NULL,
    delete_after TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (node_id, service_id)
);
