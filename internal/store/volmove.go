package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Phases of a volume move or restore (volume_moves.phase). Before VolumeSwitched the service is
// still pinned to from_node with its data there untouched; from VolumeStarting on it is pinned to
// to_node (the flip is one transaction with the phase change).
const (
	VolumePending   = "pending"
	VolumeStopping  = "stopping"
	VolumeCopying   = "copying"
	VolumeVerifying = "verifying"
	VolumeSwitching = "switching"
	VolumeStarting  = "starting"
	VolumeDone      = "done"
	VolumeFailed    = "failed"
	VolumeAborted   = "aborted"
)

// VolumeMoveTerminal reports whether a phase is final.
func VolumeMoveTerminal(phase string) bool {
	return phase == VolumeDone || phase == VolumeFailed || phase == VolumeAborted
}

type VolumeMove struct {
	ID             string     `json:"id"`
	ServiceID      string     `json:"service_id"`
	Kind           string     `json:"kind"` // move | restore
	FromNode       *string    `json:"from_node"`
	ToNode         *string    `json:"to_node"`
	Phase          string     `json:"phase"`
	PrevStatus     string     `json:"prev_status"`
	AbortRequested bool       `json:"abort_requested"`
	BackupID       *string    `json:"backup_id"`
	RPOAt          *time.Time `json:"rpo_at"`
	Bytes          int64      `json:"bytes"`
	Files          int64      `json:"files"`
	Digest         string     `json:"digest"`
	RetainDays     int        `json:"retain_days"`
	Error          string     `json:"error"`
	RequestedBy    string     `json:"requested_by"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	FinishedAt     *time.Time `json:"finished_at"`
}

const volumeMoveCols = `id, service_id, kind, from_node, to_node, phase, prev_status, abort_requested, backup_id, rpo_at,
	bytes, files, digest, retain_days, error, requested_by, created_at, updated_at, finished_at`

func scanVolumeMove(row pgx.Row) (*VolumeMove, error) {
	var m VolumeMove
	if err := row.Scan(&m.ID, &m.ServiceID, &m.Kind, &m.FromNode, &m.ToNode, &m.Phase, &m.PrevStatus, &m.AbortRequested, &m.BackupID, &m.RPOAt,
		&m.Bytes, &m.Files, &m.Digest, &m.RetainDays, &m.Error, &m.RequestedBy, &m.CreatedAt, &m.UpdatedAt, &m.FinishedAt); err != nil {
		return nil, notFound(err)
	}
	return &m, nil
}

func collectVolumeMoves(rows pgx.Rows, err error) ([]VolumeMove, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (VolumeMove, error) {
		m, err := scanVolumeMove(r)
		if err != nil {
			return VolumeMove{}, err
		}
		return *m, nil
	})
}

// CreateVolumeMove records a new attempt. ErrConflict: the service has an unfinished one.
func (s *Store) CreateVolumeMove(ctx context.Context, m *VolumeMove) (*VolumeMove, error) {
	if m.Kind == "" {
		m.Kind = "move"
	}
	if m.RetainDays <= 0 {
		m.RetainDays = 7
	}
	out, err := scanVolumeMove(s.q(ctx).QueryRow(ctx, `INSERT INTO volume_moves (service_id, kind, from_node, to_node, backup_id, rpo_at, retain_days, requested_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+volumeMoveCols,
		m.ServiceID, m.Kind, m.FromNode, m.ToNode, m.BackupID, m.RPOAt, m.RetainDays, m.RequestedBy))
	if IsUniqueViolation(err) {
		return nil, ErrConflict
	}
	return out, err
}

func (s *Store) VolumeMoveByID(ctx context.Context, id string) (*VolumeMove, error) {
	return scanVolumeMove(s.q(ctx).QueryRow(ctx, `SELECT `+volumeMoveCols+` FROM volume_moves WHERE id=$1`, id))
}

// ActiveVolumeMove is a service's unfinished move or restore (ErrNotFound: none).
func (s *Store) ActiveVolumeMove(ctx context.Context, serviceID string) (*VolumeMove, error) {
	return scanVolumeMove(s.q(ctx).QueryRow(ctx, `SELECT `+volumeMoveCols+` FROM volume_moves
		WHERE service_id=$1 AND phase NOT IN ('done','failed','aborted')`, serviceID))
}

// VolumeMoves are a service's attempts, newest first.
func (s *Store) VolumeMoves(ctx context.Context, serviceID string, limit int) ([]VolumeMove, error) {
	return collectVolumeMoves(s.q(ctx).Query(ctx, `SELECT `+volumeMoveCols+` FROM volume_moves WHERE service_id=$1 ORDER BY created_at DESC LIMIT $2`, serviceID, limit))
}

// UnfinishedVolumeMoves are the attempts a restarted control plane must finish or undo.
func (s *Store) UnfinishedVolumeMoves(ctx context.Context) ([]VolumeMove, error) {
	return collectVolumeMoves(s.q(ctx).Query(ctx, `SELECT `+volumeMoveCols+` FROM volume_moves WHERE phase NOT IN ('done','failed','aborted') ORDER BY created_at`))
}

// AdvanceVolumeMove moves an attempt from one phase to the next; false when it is not in from
// any more (another runner moved it, or it finished).
func (s *Store) AdvanceVolumeMove(ctx context.Context, id, from, to string) (bool, error) {
	tag, err := s.q(ctx).Exec(ctx, `UPDATE volume_moves SET phase=$3, updated_at=now() WHERE id=$1 AND phase=$2`, id, from, to)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) SetVolumeMovePrevStatus(ctx context.Context, id, status string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE volume_moves SET prev_status=$2, updated_at=now() WHERE id=$1`, id, status)
	return err
}

func (s *Store) SetVolumeMoveSum(ctx context.Context, id string, bytes, files int64, digest string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE volume_moves SET bytes=$2, files=$3, digest=$4, updated_at=now() WHERE id=$1`, id, bytes, files, digest)
	return err
}

// RequestVolumeMoveAbort flags an unfinished attempt for its runner to abort; false when it
// already switched over (or finished), where there is nothing left to abort.
func (s *Store) RequestVolumeMoveAbort(ctx context.Context, id string) (bool, error) {
	tag, err := s.q(ctx).Exec(ctx, `UPDATE volume_moves SET abort_requested=true, updated_at=now()
		WHERE id=$1 AND phase IN ('pending','stopping','copying','verifying')`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// EndVolumeMove records the outcome of an attempt (done, failed, aborted).
func (s *Store) EndVolumeMove(ctx context.Context, id, phase, errMsg string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE volume_moves SET phase=$2, error=$3, updated_at=now(), finished_at=now() WHERE id=$1`, id, phase, errMsg)
	return err
}

// FlipVolumeMove pins the service to the move's target node, in one transaction with the phase
// change switching -> starting: the old instances' rows go (they are stopped), a pending removal
// of an old copy on the target is cancelled (it is the service's data now). False: the service
// is no longer on the source node, or the move is not switching.
func (s *Store) FlipVolumeMove(ctx context.Context, m *VolumeMove) (bool, error) {
	tx, err := s.q(ctx).Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE volume_moves SET phase='starting', updated_at=now() WHERE id=$1 AND phase='switching'`, m.ID)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	tag, err = tx.Exec(ctx, `UPDATE services SET node_id=$2, updated_at=now() WHERE id=$1 AND (node_id=$3 OR node_id IS NULL)`, m.ServiceID, m.ToNode, m.FromNode)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM instances WHERE service_id=$1`, m.ServiceID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM volume_gc WHERE node_id=$1 AND service_id=$2`, m.ToNode, m.ServiceID); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// FinishVolumeMove marks a switched move done and queues the old copy on the source node for
// removal after the retention (volume_gc: once that node is online).
func (s *Store) FinishVolumeMove(ctx context.Context, m *VolumeMove, errMsg string) error {
	tx, err := s.q(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE volume_moves SET phase='done', error=$2, updated_at=now(), finished_at=now() WHERE id=$1`, m.ID, errMsg); err != nil {
		return err
	}
	if m.FromNode != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO volume_gc (node_id, service_id, delete_after) VALUES ($1,$2, now() + make_interval(days => $3))
			ON CONFLICT (node_id, service_id) DO UPDATE SET delete_after=EXCLUDED.delete_after`, *m.FromNode, m.ServiceID, m.RetainDays); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// RollbackVolumeMove pins a switched service back to the source node (its copy there was never
// written to after the stop) and queues the copy on the target for removal. False: the service
// is no longer on the target node.
func (s *Store) RollbackVolumeMove(ctx context.Context, m *VolumeMove, errMsg string) (bool, error) {
	tx, err := s.q(ctx).Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE services SET node_id=$2, updated_at=now() WHERE id=$1 AND node_id=$3`, m.ServiceID, m.FromNode, m.ToNode)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM instances WHERE service_id=$1`, m.ServiceID); err != nil {
		return false, err
	}
	if m.ToNode != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO volume_gc (node_id, service_id, delete_after) VALUES ($1,$2, now())
			ON CONFLICT (node_id, service_id) DO UPDATE SET delete_after=EXCLUDED.delete_after`, *m.ToNode, m.ServiceID); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE volume_moves SET phase='failed', error=$2, updated_at=now(), finished_at=now() WHERE id=$1`, m.ID, errMsg); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
