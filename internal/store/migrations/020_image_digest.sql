-- Digest of the platform manifest (linux/<arch>) of the image an instance actually runs, as the
-- agent reports it ('' = a node that does not report it). The server pins database services to it.
ALTER TABLE instances ADD COLUMN image_digest TEXT NOT NULL DEFAULT '';

-- A pinned backup is kept by retention until its database is deleted (the restore point taken
-- before a minor update or a major upgrade).
ALTER TABLE backups ADD COLUMN pinned BOOLEAN NOT NULL DEFAULT false;
