CREATE TABLE github_installations (
    installation_id BIGINT PRIMARY KEY,
    team_id UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    account_login TEXT NOT NULL,
    account_type TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX github_installations_team_idx ON github_installations (team_id);
