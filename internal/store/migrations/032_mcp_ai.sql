-- AI failure diagnosis: one cached answer per deployment (the provider
-- settings live encrypted in settings under 'ai_provider').
CREATE TABLE deployment_diagnoses (
    deployment_id UUID PRIMARY KEY REFERENCES deployments(id) ON DELETE CASCADE,
    result        JSONB NOT NULL,
    provider      TEXT NOT NULL DEFAULT '',
    model         TEXT NOT NULL DEFAULT '',
    created_by    UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
