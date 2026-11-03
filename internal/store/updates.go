package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/thesunbg/paas/internal/update"
)

// ErrRolloutActive: another update is running or paused.
var ErrRolloutActive = errors.New("an update is already in progress")

// ControlPlane is a control plane as it last reported itself.
type ControlPlane struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Arch        string    `json:"arch"`
	NodeID      *string   `json:"node_id"`
	LeaderEpoch int64     `json:"-"`
	Leader      bool      `json:"leader"`
	StartedAt   time.Time `json:"started_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}

// ReportControlPlane records that this control plane is alive and runs version.
func (s *Store) ReportControlPlane(ctx context.Context, name, version, arch string, nodeID *string, startedAt time.Time) error {
	_, err := s.q(ctx).Exec(ctx, `INSERT INTO control_planes (name, version, arch, node_id, started_at, last_seen_at) VALUES ($1,$2,$3,$4,$5,now())
		ON CONFLICT (name) DO UPDATE SET version=EXCLUDED.version, arch=EXCLUDED.arch, node_id=COALESCE(EXCLUDED.node_id, control_planes.node_id),
		started_at=EXCLUDED.started_at, last_seen_at=now()`, name, version, arch, nodeID, startedAt)
	return err
}

// MarkControlPlaneLeader records the epoch this control plane took leadership with.
func (s *Store) MarkControlPlaneLeader(ctx context.Context, name string, epoch int64) error {
	_, err := s.q(ctx).Exec(ctx, `INSERT INTO control_planes (name, leader_epoch) VALUES ($1,$2)
		ON CONFLICT (name) DO UPDATE SET leader_epoch=EXCLUDED.leader_epoch`, name, epoch)
	return err
}

// ControlPlanes lists the control planes by name; Leader is set on the one with the highest epoch.
func (s *Store) ControlPlanes(ctx context.Context) ([]ControlPlane, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT name, version, arch, node_id, leader_epoch, started_at, last_seen_at FROM control_planes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ControlPlane, error) {
		var c ControlPlane
		err := r.Scan(&c.Name, &c.Version, &c.Arch, &c.NodeID, &c.LeaderEpoch, &c.StartedAt, &c.LastSeenAt)
		return c, err
	})
	if err != nil {
		return nil, err
	}
	best := -1
	for i := range out {
		if out[i].LeaderEpoch > 0 && (best < 0 || out[i].LeaderEpoch > out[best].LeaderEpoch) {
			best = i
		}
	}
	if best >= 0 {
		out[best].Leader = true
	}
	return out, nil
}

// DeleteControlPlane forgets a decommissioned control plane.
func (s *Store) DeleteControlPlane(ctx context.Context, name string) error {
	_, err := s.q(ctx).Exec(ctx, `DELETE FROM control_planes WHERE name=$1`, name)
	return err
}

// SetNodeBinaryPinned pins (or releases) a node's binary: a pinned node does not follow
// the control plane's binary on its own.
func (s *Store) SetNodeBinaryPinned(ctx context.Context, id string, pinned bool) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE nodes SET binary_pinned=$2 WHERE id=$1`, id, pinned)
	return err
}

func (s *Store) NodeBinaryPinned(ctx context.Context, id string) (bool, error) {
	var p bool
	err := s.q(ctx).QueryRow(ctx, `SELECT binary_pinned FROM nodes WHERE id=$1`, id).Scan(&p)
	return p, notFound(err)
}

// PinnedNodes is the set of node ids with a pinned binary.
func (s *Store) PinnedNodes(ctx context.Context) (map[string]bool, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT id FROM nodes WHERE binary_pinned`)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out, err
}

const rolloutCols = `id, version, status, message, steps, release, checksums, requested_by, created_at, updated_at, finished_at`

func scanRollout(row pgx.Row) (*update.Rollout, error) {
	var r update.Rollout
	if err := row.Scan(&r.ID, &r.Version, &r.Status, &r.Message, &r.Steps, &r.Release, &r.Checksums, &r.RequestedBy,
		&r.CreatedAt, &r.UpdatedAt, &r.FinishedAt); err != nil {
		return nil, notFound(err)
	}
	return &r, nil
}

// CreateRollout stores a new rollout; ErrRolloutActive when one is running or paused.
func (s *Store) CreateRollout(ctx context.Context, r *update.Rollout) (*update.Rollout, error) {
	out, err := scanRollout(s.q(ctx).QueryRow(ctx, `INSERT INTO update_rollouts (version, status, message, steps, release, checksums, requested_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING `+rolloutCols, r.Version, r.Status, r.Message, r.Steps, r.Release, r.Checksums, r.RequestedBy))
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return nil, ErrRolloutActive
	}
	return out, err
}

// LatestRollout is the most recent rollout (ErrNotFound when there was none).
func (s *Store) LatestRollout(ctx context.Context) (*update.Rollout, error) {
	return scanRollout(s.q(ctx).QueryRow(ctx, `SELECT `+rolloutCols+` FROM update_rollouts ORDER BY created_at DESC LIMIT 1`))
}

// SaveRollout writes r back if its status is still expect (what it was when read):
// an administrator's pause or abort in between wins and the caller re-reads. false
// when nothing was written.
func (s *Store) SaveRollout(ctx context.Context, r *update.Rollout, expect string) (bool, error) {
	tag, err := s.q(ctx).Exec(ctx, `UPDATE update_rollouts SET status=$3, message=$4, steps=$5, finished_at=$6, updated_at=now()
		WHERE id=$1 AND status=$2`, r.ID, expect, r.Status, r.Message, r.Steps, r.FinishedAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
