package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// OIDCGroupRole maps a value of the provider's groups claim to a team role.
type OIDCGroupRole struct {
	Group  string `json:"group"`
	TeamID string `json:"team_id"`
	Role   string `json:"role"`
}

// OIDCProvider is an OpenID Connect identity provider used for single sign-on.
type OIDCProvider struct {
	ID              string          `json:"id"`
	Slug            string          `json:"slug"`
	Name            string          `json:"name"`
	Issuer          string          `json:"issuer"`
	ClientID        string          `json:"client_id"`
	ClientSecretEnc string          `json:"-"`
	Scopes          []string        `json:"scopes"`
	Enabled         bool            `json:"enabled"`
	TrustedDomains  []string        `json:"trusted_domains"`
	AllowedDomains  []string        `json:"allowed_domains"`
	EnforceDomains  []string        `json:"enforce_domains"`
	AutoProvision   bool            `json:"auto_provision"`
	DefaultTeamID   *string         `json:"default_team_id"`
	DefaultRole     string          `json:"default_role"`
	GroupsClaim     string          `json:"groups_claim"`
	GroupRoles      []OIDCGroupRole `json:"group_roles"`
	SkipTOTP        bool            `json:"skip_totp"`
	CreatedAt       time.Time       `json:"created_at"`
}

const oidcProviderCols = `id, slug, name, issuer, client_id, client_secret_enc, scopes, enabled, trusted_domains, allowed_domains, enforce_domains,
	auto_provision, default_team_id, default_role, groups_claim, group_roles, skip_totp, created_at`

func scanOIDCProvider(row pgx.Row) (*OIDCProvider, error) {
	var p OIDCProvider
	err := row.Scan(&p.ID, &p.Slug, &p.Name, &p.Issuer, &p.ClientID, &p.ClientSecretEnc, &p.Scopes, &p.Enabled, &p.TrustedDomains, &p.AllowedDomains,
		&p.EnforceDomains, &p.AutoProvision, &p.DefaultTeamID, &p.DefaultRole, &p.GroupsClaim, &p.GroupRoles, &p.SkipTOTP, &p.CreatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	if p.GroupRoles == nil {
		p.GroupRoles = []OIDCGroupRole{}
	}
	return &p, nil
}

func (s *Store) OIDCProviders(ctx context.Context) ([]OIDCProvider, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT `+oidcProviderCols+` FROM oidc_providers ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OIDCProvider{}
	for rows.Next() {
		p, err := scanOIDCProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *Store) OIDCProviderByID(ctx context.Context, id string) (*OIDCProvider, error) {
	return scanOIDCProvider(s.q(ctx).QueryRow(ctx, `SELECT `+oidcProviderCols+` FROM oidc_providers WHERE id=$1`, id))
}

func (s *Store) OIDCProviderBySlug(ctx context.Context, slug string) (*OIDCProvider, error) {
	return scanOIDCProvider(s.q(ctx).QueryRow(ctx, `SELECT `+oidcProviderCols+` FROM oidc_providers WHERE slug=$1`, slug))
}

// SaveOIDCProvider inserts p (empty ID) or updates it, and returns the stored row.
func (s *Store) SaveOIDCProvider(ctx context.Context, p *OIDCProvider) (*OIDCProvider, error) {
	if p.GroupRoles == nil {
		p.GroupRoles = []OIDCGroupRole{}
	}
	args := []any{p.Slug, p.Name, p.Issuer, p.ClientID, p.ClientSecretEnc, p.Scopes, p.Enabled, p.TrustedDomains, p.AllowedDomains, p.EnforceDomains,
		p.AutoProvision, p.DefaultTeamID, p.DefaultRole, p.GroupsClaim, p.GroupRoles, p.SkipTOTP}
	if p.ID == "" {
		return scanOIDCProvider(s.q(ctx).QueryRow(ctx, `INSERT INTO oidc_providers (slug, name, issuer, client_id, client_secret_enc, scopes, enabled,
			trusted_domains, allowed_domains, enforce_domains, auto_provision, default_team_id, default_role, groups_claim, group_roles, skip_totp)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING `+oidcProviderCols, args...))
	}
	return scanOIDCProvider(s.q(ctx).QueryRow(ctx, `UPDATE oidc_providers SET slug=$1, name=$2, issuer=$3, client_id=$4, client_secret_enc=$5, scopes=$6,
		enabled=$7, trusted_domains=$8, allowed_domains=$9, enforce_domains=$10, auto_provision=$11, default_team_id=$12, default_role=$13,
		groups_claim=$14, group_roles=$15, skip_totp=$16 WHERE id=$17 RETURNING `+oidcProviderCols, append(args, p.ID)...))
}

func (s *Store) DeleteOIDCProvider(ctx context.Context, id string) error {
	_, err := s.q(ctx).Exec(ctx, `DELETE FROM oidc_providers WHERE id=$1`, id)
	return err
}

// OIDCIdentity is an account's link to a provider.
type OIDCIdentity struct {
	ProviderID   string    `json:"provider_id"`
	ProviderSlug string    `json:"provider_slug"`
	ProviderName string    `json:"provider_name"`
	Email        string    `json:"email"`
	CreatedAt    time.Time `json:"created_at"`
}

// UserByOIDCSubject returns the account linked to (provider, subject), or ErrNotFound.
func (s *Store) UserByOIDCSubject(ctx context.Context, providerID, subject string) (*User, error) {
	return scanUser(s.q(ctx).QueryRow(ctx, `SELECT u.id, u.email, u.name, u.password_hash, u.is_admin, u.totp_enabled, u.totp_secret_enc,
		COALESCE(u.github_id, 0), COALESCE(u.github_login, ''), u.created_at
		FROM oidc_identities i JOIN users u ON u.id=i.user_id WHERE i.provider_id=$1 AND i.subject=$2`, providerID, subject))
}

// LinkOIDC attaches (provider, subject) to a user, replacing the user's previous identity at that provider.
func (s *Store) LinkOIDC(ctx context.Context, providerID, subject, userID, email string) error {
	return pgx.BeginFunc(ctx, s.q(ctx), func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM oidc_identities WHERE provider_id=$1 AND user_id=$2`, providerID, userID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO oidc_identities (provider_id, subject, user_id, email) VALUES ($1,$2,$3,$4)`, providerID, subject, userID, email)
		return err
	})
}

func (s *Store) UnlinkOIDC(ctx context.Context, providerID, userID string) error {
	_, err := s.q(ctx).Exec(ctx, `DELETE FROM oidc_identities WHERE provider_id=$1 AND user_id=$2`, providerID, userID)
	return err
}

// TouchOIDCEmail records the email the provider reported at the last sign-in.
func (s *Store) TouchOIDCEmail(ctx context.Context, providerID, subject, email string) {
	_, _ = s.q(ctx).Exec(ctx, `UPDATE oidc_identities SET email=$3 WHERE provider_id=$1 AND subject=$2`, providerID, subject, email)
}

func (s *Store) OIDCIdentitiesForUser(ctx context.Context, userID string) ([]OIDCIdentity, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT p.id, p.slug, p.name, i.email, i.created_at FROM oidc_identities i
		JOIN oidc_providers p ON p.id=i.provider_id WHERE i.user_id=$1 ORDER BY i.created_at`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (OIDCIdentity, error) {
		var i OIDCIdentity
		err := r.Scan(&i.ProviderID, &i.ProviderSlug, &i.ProviderName, &i.Email, &i.CreatedAt)
		return i, err
	})
}

// OIDCLogin is a sign-in in flight.
type OIDCLogin struct {
	ProviderID  string
	Nonce       string
	Verifier    string
	RedirectURI string
	LinkUserID  *string
}

func (s *Store) CreateOIDCLogin(ctx context.Context, stateHash string, l *OIDCLogin, expires time.Time) error {
	_, _ = s.q(ctx).Exec(ctx, `DELETE FROM oidc_logins WHERE expires_at < now()`)
	_, err := s.q(ctx).Exec(ctx, `INSERT INTO oidc_logins (state_hash, provider_id, nonce, verifier, redirect_uri, link_user_id, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, stateHash, l.ProviderID, l.Nonce, l.Verifier, l.RedirectURI, l.LinkUserID, expires)
	return err
}

// TakeOIDCLogin deletes and returns an unexpired sign-in: each state works once.
func (s *Store) TakeOIDCLogin(ctx context.Context, stateHash string) (*OIDCLogin, error) {
	var l OIDCLogin
	err := s.q(ctx).QueryRow(ctx, `DELETE FROM oidc_logins WHERE state_hash=$1 AND expires_at > now()
		RETURNING provider_id, nonce, verifier, redirect_uri, link_user_id`, stateHash).Scan(&l.ProviderID, &l.Nonce, &l.Verifier, &l.RedirectURI, &l.LinkUserID)
	if err != nil {
		return nil, notFound(err)
	}
	return &l, nil
}

// InstanceOwnerID is the first administrator account (the one created at setup).
// It keeps password sign-in when SSO is enforced, as a break-glass account.
func (s *Store) InstanceOwnerID(ctx context.Context) (string, error) {
	var id string
	err := s.q(ctx).QueryRow(ctx, `SELECT id FROM users WHERE is_admin ORDER BY created_at, id LIMIT 1`).Scan(&id)
	return id, notFound(err)
}
