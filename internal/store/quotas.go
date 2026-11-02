package store

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/thesunbg/paas/internal/model"
)

// TeamServiceSpec is what quota accounting needs from one service of a team.
type TeamServiceSpec struct {
	Type      string
	Spec      model.ServiceSpec
	IsPreview bool
}

// TeamServiceSpecs returns every service of a team's projects (database servers,
// previews and platform-run services included).
func (s *Store) TeamServiceSpecs(ctx context.Context, teamID string) ([]TeamServiceSpec, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT sv.type, sv.spec, sv.parent_id IS NOT NULL FROM services sv
		JOIN projects p ON p.id=sv.project_id WHERE p.team_id=$1`, teamID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (TeamServiceSpec, error) {
		var t TeamServiceSpec
		err := r.Scan(&t.Type, &t.Spec, &t.IsPreview)
		return t, err
	})
}

// TeamDatabases returns the databases of a team's projects.
func (s *Store) TeamDatabases(ctx context.Context, teamID string) ([]Database, error) {
	return collectDatabases(s.q(ctx).Query(ctx, `SELECT `+databaseCols+` FROM databases
		WHERE project_id IN (SELECT id FROM projects WHERE team_id=$1)`, teamID))
}

// TeamCustomDomains counts the domains of a team's services that users added (not generated ones).
func (s *Store) TeamCustomDomains(ctx context.Context, teamID string) (int, error) {
	var n int
	err := s.q(ctx).QueryRow(ctx, `SELECT count(*) FROM domains d JOIN services sv ON sv.id=d.service_id
		JOIN projects p ON p.id=sv.project_id WHERE p.team_id=$1 AND NOT d.is_generated`, teamID).Scan(&n)
	return n, err
}

// TeamQuota returns a team's quota override (a JSON object, {} when none).
func (s *Store) TeamQuota(ctx context.Context, teamID string) (json.RawMessage, error) {
	var raw json.RawMessage
	err := s.q(ctx).QueryRow(ctx, `SELECT quota FROM teams WHERE id=$1`, teamID).Scan(&raw)
	return raw, notFound(err)
}

func (s *Store) SetTeamQuota(ctx context.Context, teamID string, raw json.RawMessage) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE teams SET quota=$2 WHERE id=$1`, teamID, raw)
	return err
}
