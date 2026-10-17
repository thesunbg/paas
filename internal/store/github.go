package store

import (
	"context"
	"time"
)

// GitHubInstallation links an installation of the GitHub App to a team.
type GitHubInstallation struct {
	InstallationID int64     `json:"installation_id"`
	TeamID         string    `json:"team_id"`
	AccountLogin   string    `json:"account_login"`
	AccountType    string    `json:"account_type"`
	CreatedAt      time.Time `json:"created_at"`
}

const ghInstCols = `installation_id, team_id, account_login, account_type, created_at`

func (s *Store) UpsertGitHubInstallation(ctx context.Context, in GitHubInstallation) error {
	_, err := s.q(ctx).Exec(ctx, `INSERT INTO github_installations (installation_id, team_id, account_login, account_type) VALUES ($1,$2,$3,$4)
		ON CONFLICT (installation_id) DO UPDATE SET team_id=EXCLUDED.team_id, account_login=EXCLUDED.account_login, account_type=EXCLUDED.account_type`,
		in.InstallationID, in.TeamID, in.AccountLogin, in.AccountType)
	return err
}

func (s *Store) GitHubInstallation(ctx context.Context, id int64) (*GitHubInstallation, error) {
	var g GitHubInstallation
	err := s.q(ctx).QueryRow(ctx, `SELECT `+ghInstCols+` FROM github_installations WHERE installation_id=$1`, id).
		Scan(&g.InstallationID, &g.TeamID, &g.AccountLogin, &g.AccountType, &g.CreatedAt)
	return &g, notFound(err)
}

func (s *Store) GitHubInstallationsForTeam(ctx context.Context, teamID string) ([]GitHubInstallation, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT `+ghInstCols+` FROM github_installations WHERE team_id=$1 ORDER BY account_login`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GitHubInstallation{}
	for rows.Next() {
		var g GitHubInstallation
		if err := rows.Scan(&g.InstallationID, &g.TeamID, &g.AccountLogin, &g.AccountType, &g.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) DeleteGitHubInstallation(ctx context.Context, id int64) error {
	_, err := s.q(ctx).Exec(ctx, `DELETE FROM github_installations WHERE installation_id=$1`, id)
	return err
}
