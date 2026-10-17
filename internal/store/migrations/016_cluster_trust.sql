-- Per-node NATS identity. NULL nkey = legacy node still using the shared cluster token.
ALTER TABLE nodes ADD COLUMN nkey TEXT UNIQUE;

-- One-time join tokens: hash is sha256 of the token.
CREATE TABLE join_tokens (
    hash       BYTEA PRIMARY KEY,
    roles      TEXT[] NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    created_by UUID
);
