-- Single sign-on with OpenID Connect providers (Google Workspace, Entra ID,
-- Keycloak, Authentik…) configured by the instance administrators.
CREATE TABLE oidc_providers (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug              TEXT NOT NULL UNIQUE,              -- used in URLs: /api/v1/auth/oidc/<slug>
    name              TEXT NOT NULL,                     -- "Sign in with <name>"
    issuer            TEXT NOT NULL,
    client_id         TEXT NOT NULL,
    client_secret_enc TEXT NOT NULL DEFAULT '',          -- sealed with the master key
    scopes            TEXT[] NOT NULL DEFAULT '{openid,email,profile}',
    enabled           BOOLEAN NOT NULL DEFAULT true,
    -- Email domains this provider is authoritative for: a verified email there
    -- may sign in to an existing account with that address (auto-link).
    trusted_domains   TEXT[] NOT NULL DEFAULT '{}',
    -- When not empty, only verified emails in these domains may sign in.
    allowed_domains   TEXT[] NOT NULL DEFAULT '{}',
    -- Password (and GitHub) sign-in is disabled for these domains (except for the instance owner).
    enforce_domains   TEXT[] NOT NULL DEFAULT '{}',
    auto_provision    BOOLEAN NOT NULL DEFAULT false,    -- create unknown users on first sign-in
    default_team_id   UUID REFERENCES teams(id) ON DELETE SET NULL,
    default_role      TEXT NOT NULL DEFAULT 'developer',
    groups_claim      TEXT NOT NULL DEFAULT 'groups',
    group_roles       JSONB NOT NULL DEFAULT '[]',       -- [{group, team_id, role}]
    skip_totp         BOOLEAN NOT NULL DEFAULT true,     -- the IdP enforces MFA
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- An account's identity at a provider: (provider, sub) is stable, the email is not.
CREATE TABLE oidc_identities (
    provider_id UUID NOT NULL REFERENCES oidc_providers(id) ON DELETE CASCADE,
    subject     TEXT NOT NULL,
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    email       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (provider_id, subject),
    UNIQUE (provider_id, user_id)
);

-- Sign-ins in flight (between the redirect to the provider and its callback):
-- shared by every control plane, single use, minutes long.
CREATE TABLE oidc_logins (
    state_hash   TEXT PRIMARY KEY,
    provider_id  UUID NOT NULL REFERENCES oidc_providers(id) ON DELETE CASCADE,
    nonce        TEXT NOT NULL,
    verifier     TEXT NOT NULL,                          -- PKCE code verifier
    redirect_uri TEXT NOT NULL,
    link_user_id UUID REFERENCES users(id) ON DELETE CASCADE, -- set when a signed-in user links the provider
    expires_at   TIMESTAMPTZ NOT NULL
);
