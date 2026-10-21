-- Restore checks of database backups (server/backup_verify.go): when the dump was last restored into
-- a scratch database, and what came out (table count, or the error).
ALTER TABLE backups ADD COLUMN verified_at TIMESTAMPTZ;
ALTER TABLE backups ADD COLUMN verify_ok BOOLEAN;
ALTER TABLE backups ADD COLUMN verify_note TEXT NOT NULL DEFAULT '';
