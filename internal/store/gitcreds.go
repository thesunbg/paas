package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// GitCredential is a team's access token for one git host (GitLab, Gitea /
// Forgejo, Bitbucket, or GitHub without the app).
type GitCredential struct {
	ID        string    `json:"id"`
	TeamID    string    `json:"team_id"`
	Provider  string    `json:"provider"`
	Host      string    `json:"host"`
	Username  string    `json:"username"`
	TokenEnc  string    `json:"-"`
	Name      string    `json:"name"`
	CreatedBy *string   `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const gitCredCols = `id, team_id, provider, host, username, token_enc, name, created_by, created_at, updated_at`

func scanGitCred(row pgx.Row) (*GitCredential, error) {
	var c GitCredential
	err := row.Scan(&c.ID, &c.TeamID, &c.Provider, &c.Host, &c.Username, &c.TokenEnc, &c.Name, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt)
	return &c, notFound(err)
}

// UpsertGitCredential stores the team's credential for c.Host, replacing an existing one.
func (s *Store) UpsertGitCredential(ctx context.Context, c *GitCredential) (*GitCredential, error) {
	return scanGitCred(s.q(ctx).QueryRow(ctx, `INSERT INTO git_credentials (team_id, provider, host, username, token_enc, name, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (team_id, host) DO UPDATE SET provider=EXCLUDED.provider, username=EXCLUDED.username, token_enc=EXCLUDED.token_enc,
			name=EXCLUDED.name, created_by=EXCLUDED.created_by, updated_at=now()
		RETURNING `+gitCredCols,
		c.TeamID, c.Provider, c.Host, c.Username, c.TokenEnc, c.Name, c.CreatedBy))
}

func (s *Store) GitCredentials(ctx context.Context, teamID string) ([]GitCredential, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT `+gitCredCols+` FROM git_credentials WHERE team_id=$1 ORDER BY host`, teamID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (GitCredential, error) {
		c, err := scanGitCred(r)
		return *c, err
	})
	if out == nil {
		out = []GitCredential{}
	}
	return out, err
}

// GitCredentialForHost returns the team's credential for a host (lowercase host[:port]).
func (s *Store) GitCredentialForHost(ctx context.Context, teamID, host string) (*GitCredential, error) {
	return scanGitCred(s.q(ctx).QueryRow(ctx, `SELECT `+gitCredCols+` FROM git_credentials WHERE team_id=$1 AND host=$2`, teamID, host))
}

// DeleteGitCredential removes a credential of the team; ErrNotFound when there is none.
func (s *Store) DeleteGitCredential(ctx context.Context, teamID, id string) error {
	tag, err := s.q(ctx).Exec(ctx, `DELETE FROM git_credentials WHERE team_id=$1 AND id=$2`, teamID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
