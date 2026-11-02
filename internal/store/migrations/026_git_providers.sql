-- Per-team credentials for git hosts other than the GitHub App (GitLab, Gitea /
-- Forgejo, Bitbucket Cloud, or a GitHub personal token). A credential is only
-- ever sent to its own host: one per team and host.
CREATE TABLE git_credentials (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id    UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    provider   TEXT NOT NULL,              -- github | gitlab | gitea | bitbucket
    host       TEXT NOT NULL,              -- lowercase host[:port], e.g. gitlab.com, git.example.com:8443
    username   TEXT NOT NULL DEFAULT '',   -- HTTP basic user for clones ('' = provider default)
    token_enc  TEXT NOT NULL,              -- access token or app password, encrypted
    name       TEXT NOT NULL DEFAULT '',
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (team_id, host)
);
