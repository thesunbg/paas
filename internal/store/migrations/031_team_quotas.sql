-- Per-team quota overrides set by instance administrators: a JSON object with
-- any of the TeamQuota keys (max_projects, max_memory_mb…). Keys left out use
-- the instance default (setting team_quota_default); 0 means unlimited.
ALTER TABLE teams ADD COLUMN quota JSONB NOT NULL DEFAULT '{}';
