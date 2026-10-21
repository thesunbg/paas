package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type VolumeBackup struct {
	ID         string     `json:"id"`
	ServiceID  string     `json:"service_id"`
	NodeID     *string    `json:"node_id"`
	Status     string     `json:"status"`
	S3Key      string     `json:"s3_key"`
	SizeBytes  int64      `json:"size_bytes"`
	Error      string     `json:"error"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

const volumeBackupCols = `id, service_id, node_id, status, s3_key, size_bytes, error, created_at, finished_at`

func scanVolumeBackup(row pgx.Row) (*VolumeBackup, error) {
	var b VolumeBackup
	if err := row.Scan(&b.ID, &b.ServiceID, &b.NodeID, &b.Status, &b.S3Key, &b.SizeBytes, &b.Error, &b.CreatedAt, &b.FinishedAt); err != nil {
		return nil, notFound(err)
	}
	return &b, nil
}

func (s *Store) CreateVolumeBackup(ctx context.Context, serviceID, nodeID string) (*VolumeBackup, error) {
	return scanVolumeBackup(s.q(ctx).QueryRow(ctx, `INSERT INTO volume_backups (service_id, node_id) VALUES ($1,$2) RETURNING `+volumeBackupCols, serviceID, nodeID))
}

func (s *Store) FinishVolumeBackup(ctx context.Context, id, status, errMsg, s3Key string, size int64) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE volume_backups SET status=$2, error=$3, s3_key=$4, size_bytes=$5, finished_at=now() WHERE id=$1`, id, status, errMsg, s3Key, size)
	return err
}

func (s *Store) VolumeBackups(ctx context.Context, serviceID string, limit int) ([]VolumeBackup, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT `+volumeBackupCols+` FROM volume_backups WHERE service_id=$1 ORDER BY created_at DESC LIMIT $2`, serviceID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (VolumeBackup, error) {
		b, err := scanVolumeBackup(r)
		if err != nil {
			return VolumeBackup{}, err
		}
		return *b, nil
	})
}

func (s *Store) VolumeBackupByID(ctx context.Context, id string) (*VolumeBackup, error) {
	return scanVolumeBackup(s.q(ctx).QueryRow(ctx, `SELECT `+volumeBackupCols+` FROM volume_backups WHERE id=$1`, id))
}

// OldVolumeBackups: what retention removes (completed beyond keep, and failures older than the oldest kept).
func (s *Store) OldVolumeBackups(ctx context.Context, serviceID string, keep int) ([]VolumeBackup, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT `+volumeBackupCols+` FROM volume_backups WHERE service_id=$1 AND status<>'running'
		AND created_at < (SELECT created_at FROM volume_backups WHERE service_id=$1 AND status='completed' ORDER BY created_at DESC OFFSET $2 - 1 LIMIT 1)`, serviceID, keep)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (VolumeBackup, error) {
		b, err := scanVolumeBackup(r)
		if err != nil {
			return VolumeBackup{}, err
		}
		return *b, nil
	})
}

func (s *Store) DeleteVolumeBackup(ctx context.Context, id string) error {
	_, err := s.q(ctx).Exec(ctx, `DELETE FROM volume_backups WHERE id=$1`, id)
	return err
}

// LastVolumeBackup is the newest backup of a service in any state (nil: none).
func (s *Store) LastVolumeBackup(ctx context.Context, serviceID string) (*VolumeBackup, error) {
	return scanVolumeBackup(s.q(ctx).QueryRow(ctx, `SELECT `+volumeBackupCols+` FROM volume_backups WHERE service_id=$1 ORDER BY created_at DESC LIMIT 1`, serviceID))
}

// RepinService moves a service with volumes to another node: its instances go
// (the reconciler starts new ones there), and its volumes on the old node are
// removed 7 days after that node is back (volume_gc).
func (s *Store) RepinService(ctx context.Context, serviceID, newNode, oldNode string) error {
	tx, err := s.q(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE services SET node_id=$2, updated_at=now() WHERE id=$1`, serviceID, newNode); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM instances WHERE service_id=$1`, serviceID); err != nil {
		return err
	}
	if oldNode != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO volume_gc (node_id, service_id, delete_after) VALUES ($1,$2, now() + interval '7 days')
			ON CONFLICT (node_id, service_id) DO NOTHING`, oldNode, serviceID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ReplaceServer makes newServer the server of a database whose old server is
// gone with its node: the database points at the new one (if it still points
// at the old one), the old service row is deleted and its volume queued for
// removal 7 days after its node is back.
func (s *Store) ReplaceServer(ctx context.Context, dbIDs []string, oldServer, newServer, oldNode string) error {
	tx, err := s.q(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, id := range dbIDs {
		tag, err := tx.Exec(ctx, `UPDATE databases SET server_service_id=$2 WHERE id=$1 AND server_service_id=$3`, id, newServer, oldServer)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrConflict
		}
	}
	if oldServer != newServer {
		if _, err := tx.Exec(ctx, `DELETE FROM services WHERE id=$1`, oldServer); err != nil {
			return err
		}
	}
	if oldNode != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO volume_gc (node_id, service_id, delete_after) VALUES ($1,$2, now() + interval '7 days')
			ON CONFLICT (node_id, service_id) DO NOTHING`, oldNode, oldServer); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
