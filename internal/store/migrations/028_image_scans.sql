-- Vulnerability scanning of deployed images (Trivy on a build node) and the
-- per-project policy deciding what findings do to a deployment. A project
-- without a row uses the default: warn on CRITICAL.
CREATE TABLE project_scan_policies (
    project_id UUID PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    mode       TEXT NOT NULL DEFAULT 'warn'     CHECK (mode IN ('off', 'warn', 'block')),
    threshold  TEXT NOT NULL DEFAULT 'CRITICAL' CHECK (threshold IN ('CRITICAL', 'HIGH', 'MEDIUM', 'LOW')),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per scanned deployment. Deployments that reuse an artifact (rollback,
-- restart) are not scanned again; the service's latest scan is the newest row.
CREATE TABLE image_scans (
    deployment_id   UUID PRIMARY KEY REFERENCES deployments(id) ON DELETE CASCADE,
    service_id      UUID NOT NULL REFERENCES services(id) ON DELETE CASCADE,
    image           TEXT NOT NULL,
    status          TEXT NOT NULL,                 -- scanned | unavailable (no scanner, scan failed, no build node)
    scanner         TEXT NOT NULL DEFAULT '',
    scanner_version TEXT NOT NULL DEFAULT '',
    db_updated_at   TIMESTAMPTZ,                   -- age of the vulnerability DB used
    os              TEXT NOT NULL DEFAULT '',
    critical        INT NOT NULL DEFAULT 0,
    high            INT NOT NULL DEFAULT 0,
    medium          INT NOT NULL DEFAULT 0,
    low             INT NOT NULL DEFAULT 0,
    unknown         INT NOT NULL DEFAULT 0,
    findings        JSONB NOT NULL DEFAULT '[]',   -- most severe first, at most 200
    policy_mode     TEXT NOT NULL,                 -- the policy applied
    threshold       TEXT NOT NULL,
    blocked         BOOLEAN NOT NULL DEFAULT false,
    message         TEXT NOT NULL DEFAULT '',      -- the verdict line written to the deploy log
    scanned_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX image_scans_service ON image_scans (service_id, scanned_at DESC);
