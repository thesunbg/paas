-- Bulk installs of a template (a class of learners, a set of customers): one
-- item per copy, in one shared project or in one account + project each.
CREATE TABLE batches (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT NOT NULL,
    template   TEXT NOT NULL,
    mode       TEXT NOT NULL,                 -- instances | accounts
    project_id UUID REFERENCES projects(id) ON DELETE SET NULL, -- instances mode
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    status     TEXT NOT NULL DEFAULT 'creating', -- creating | ready | deleting
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE batch_items (
    batch_id     UUID NOT NULL REFERENCES batches(id) ON DELETE CASCADE,
    idx          INT NOT NULL,
    label        TEXT NOT NULL,
    email        TEXT NOT NULL DEFAULT '',
    user_id      UUID REFERENCES users(id) ON DELETE SET NULL,
    project_id   UUID REFERENCES projects(id) ON DELETE SET NULL,
    service_id   UUID REFERENCES services(id) ON DELETE SET NULL,
    database_ids UUID[] NOT NULL DEFAULT '{}',
    password_enc TEXT NOT NULL DEFAULT '', -- the account's first password (accounts mode), encrypted
    error        TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (batch_id, idx)
);
