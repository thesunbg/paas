package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type NotificationChannel struct {
	ID        string    `json:"id"`
	TeamID    string    `json:"team_id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	TargetEnc string    `json:"-"`
	Events    []string  `json:"events"`
	CreatedAt time.Time `json:"created_at"`
}

const channelCols = `id, team_id, name, kind, target_enc, events, created_at`

func (s *Store) CreateChannel(ctx context.Context, c *NotificationChannel) (*NotificationChannel, error) {
	var out NotificationChannel
	err := s.DB.QueryRow(ctx, `INSERT INTO notification_channels (team_id, name, kind, target_enc, events) VALUES ($1,$2,$3,$4,$5) RETURNING `+channelCols,
		c.TeamID, c.Name, c.Kind, c.TargetEnc, c.Events).Scan(&out.ID, &out.TeamID, &out.Name, &out.Kind, &out.TargetEnc, &out.Events, &out.CreatedAt)
	return &out, err
}

func (s *Store) Channels(ctx context.Context, teamID string) ([]NotificationChannel, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+channelCols+` FROM notification_channels WHERE team_id=$1 ORDER BY created_at`, teamID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (NotificationChannel, error) {
		var c NotificationChannel
		err := r.Scan(&c.ID, &c.TeamID, &c.Name, &c.Kind, &c.TargetEnc, &c.Events, &c.CreatedAt)
		return c, err
	})
}

func (s *Store) DeleteChannel(ctx context.Context, teamID, id string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM notification_channels WHERE team_id=$1 AND id=$2`, teamID, id)
	return err
}

// AdminTeamIDs returns the teams of administrators (for cluster-level events such as node.offline).
func (s *Store) AdminTeamIDs(ctx context.Context) ([]string, error) {
	rows, err := s.DB.Query(ctx, `SELECT DISTINCT m.team_id FROM memberships m JOIN users u ON u.id=m.user_id WHERE u.is_admin`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
