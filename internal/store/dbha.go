package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thesunbg/paas/internal/model"
)

// HAState is the control-plane bookkeeping of one HA database (db_ha_state).
type HAState struct {
	PrimaryLSN string // text form, e.g. 0/16B3748; "" = unknown
	FailoverTo string // standby service chosen for promotion; "" = none
	Forced     bool   // an admin accepted data loss and the window is still open
}

func (s *Store) HAState(ctx context.Context, dbID string) (HAState, error) {
	var h HAState
	err := s.q(ctx).QueryRow(ctx, `SELECT coalesce(primary_lsn::text,''), failover_to, coalesce(force_until > now(), false) FROM db_ha_state WHERE database_id=$1`, dbID).Scan(&h.PrimaryLSN, &h.FailoverTo, &h.Forced)
	if errors.Is(err, pgx.ErrNoRows) {
		return HAState{}, nil
	}
	return h, err
}

func (s *Store) SetPrimaryLSN(ctx context.Context, dbID, lsn string) error {
	_, err := s.q(ctx).Exec(ctx, `INSERT INTO db_ha_state (database_id, primary_lsn, lsn_at) VALUES ($1, $2::pg_lsn, now())
		ON CONFLICT (database_id) DO UPDATE SET primary_lsn=EXCLUDED.primary_lsn, lsn_at=now(), updated_at=now()`, dbID, lsn)
	return err
}

// SetFailoverIntent records (repID) or clears ("") the standby being promoted.
func (s *Store) SetFailoverIntent(ctx context.Context, dbID, repID string) error {
	_, err := s.q(ctx).Exec(ctx, `INSERT INTO db_ha_state (database_id, failover_to) VALUES ($1, $2)
		ON CONFLICT (database_id) DO UPDATE SET failover_to=EXCLUDED.failover_to, updated_at=now()`, dbID, repID)
	return err
}

// SetFailoverForce lets failover proceed despite standby lag for d (d <= 0 withdraws it).
func (s *Store) SetFailoverForce(ctx context.Context, dbID string, d time.Duration) error {
	_, err := s.q(ctx).Exec(ctx, `INSERT INTO db_ha_state (database_id, force_until) VALUES ($1, CASE WHEN $2::interval > interval '0' THEN now() + $2::interval END)
		ON CONFLICT (database_id) DO UPDATE SET force_until=EXCLUDED.force_until, updated_at=now()`, dbID, d.String())
	return err
}

func (s *Store) FailoverIntent(ctx context.Context, dbID string) (string, error) {
	h, err := s.HAState(ctx, dbID)
	return h.FailoverTo, err
}

// FailoverPending: a failover involving this server is under way (it must not be unfenced).
func (s *Store) FailoverPending(ctx context.Context, serviceID string) (bool, error) {
	var b bool
	err := s.q(ctx).QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM db_ha_state h JOIN databases d ON d.id=h.database_id
		WHERE h.failover_to <> '' AND (d.server_service_id=$1::uuid OR h.failover_to=$1::text))`, serviceID).Scan(&b)
	return b, err
}

// CommitFailover makes the promoted standby the database's server in ONE transaction: it
// takes over the alias, replaces the old server in databases, and the old server (with its
// instances) is deleted, its volume queued for removal in 7 days on gcNode ("" = none) and
// the intent cleared. Either all of it happens or none, so a leader that dies at any point
// leaves a state the next leader can finish (failover_to still set) or sees as done.
func (s *Store) CommitFailover(ctx context.Context, dbID, newServerID, oldServerID string, aliases []string, gcNode string) error {
	tx, err := s.q(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var spec model.ServiceSpec
	if err := tx.QueryRow(ctx, `SELECT spec FROM services WHERE id=$1 FOR UPDATE`, newServerID).Scan(&spec); err != nil {
		return notFound(err)
	}
	spec.DNSAliases = aliases
	spec.HealthCheck.TimeoutSec = 180
	if _, err := tx.Exec(ctx, `UPDATE services SET spec=$2, version=version+1, updated_at=now() WHERE id=$1`, newServerID, spec); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE databases SET server_service_id=$2, options = options - 'replica_service_id', options_version=options_version+1
		WHERE id=$1 AND server_service_id=$3`, dbID, newServerID, oldServerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `DELETE FROM services WHERE id=$1`, oldServerID); err != nil {
		return err
	}
	if gcNode != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO volume_gc (node_id, service_id, delete_after) VALUES ($1,$2, now() + interval '7 days')
			ON CONFLICT (node_id, service_id) DO NOTHING`, gcNode, oldServerID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE db_ha_state SET failover_to='', force_until=NULL, primary_lsn=NULL, lsn_at=NULL, updated_at=now() WHERE database_id=$1`, dbID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type VolumeGC struct{ NodeID, ServiceID string }

// DueVolumeGC lists volumes past their retention whose node is online (to delete them).
func (s *Store) DueVolumeGC(ctx context.Context) ([]VolumeGC, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT g.node_id, g.service_id FROM volume_gc g JOIN nodes n ON n.id=g.node_id
		WHERE g.delete_after < now() AND n.status='online'
		AND NOT EXISTS (SELECT 1 FROM services s WHERE s.id=g.service_id AND s.node_id=g.node_id)`) // never the live copy
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (VolumeGC, error) {
		var v VolumeGC
		return v, r.Scan(&v.NodeID, &v.ServiceID)
	})
}

func (s *Store) DeleteVolumeGC(ctx context.Context, nodeID, serviceID string) error {
	_, err := s.q(ctx).Exec(ctx, `DELETE FROM volume_gc WHERE node_id=$1 AND service_id=$2`, nodeID, serviceID)
	return err
}
