-- Bumped by every write to a service's config (name/source/build/spec/git token) or a
-- database's options. Writers use it as a compare-and-swap so a stale read-modify-write
-- (e.g. a build finishing minutes later) cannot overwrite a concurrent change.
ALTER TABLE services ADD COLUMN version BIGINT NOT NULL DEFAULT 1;
-- (databases.version is already the engine version, hence the different name)
ALTER TABLE databases ADD COLUMN options_version BIGINT NOT NULL DEFAULT 1;
