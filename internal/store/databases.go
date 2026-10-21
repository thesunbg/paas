package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type Database struct {
	ID                  string     `json:"id"`
	ProjectID           string     `json:"project_id"`
	Name                string     `json:"name"`
	Engine              string     `json:"engine"`
	Version             string     `json:"version"` // engine version, a catalog name
	Mode                string     `json:"mode"`
	ServerServiceID     *string    `json:"server_service_id"`
	DBName              string     `json:"db_name"`
	Username            string     `json:"username"`
	PasswordEnc         string     `json:"-"`
	SizePreset          string     `json:"size_preset"`
	Status              string     `json:"status"`
	Error               string     `json:"error"`
	BackupScheduleHours int        `json:"backup_schedule_hours"`
	LastBackupAt        *time.Time `json:"last_backup_at"`
	Options             DBOptions  `json:"options"`
	MoveState           *MoveState `json:"-"`
	OptionsVersion      int64      `json:"options_version"` // bumped by UpdateDatabaseOptions
	CreatedAt           time.Time  `json:"created_at"`
}

// DBOptions holds the DBaaS settings of a database.
type DBOptions struct {
	// External access through the edge proxies on db.<base domain>:<port>.
	PublicPort   int      `json:"public_port,omitempty"`
	AllowedCIDRs []string `json:"allowed_cidrs,omitempty"`
	// Storage quota in MB (0 = the size preset's default); over it the database turns read-only.
	QuotaMB  int  `json:"quota_mb,omitempty"`
	ReadOnly bool `json:"read_only,omitempty"`
	// PgBouncer in front of the database (postgres).
	Pooler          bool   `json:"pooler,omitempty"`
	PoolerServiceID string `json:"pooler_service_id,omitempty"`
	// Point-in-time recovery: WAL archiving and periodic base backups (dedicated postgres).
	PITR          bool `json:"pitr,omitempty"`
	PITRRetention int  `json:"pitr_retention_days,omitempty"`
	// High availability: a streaming standby on another node (dedicated postgres).
	HA               bool   `json:"ha,omitempty"`
	ReplicaServiceID string `json:"replica_service_id,omitempty"`
}

const databaseCols = `id, project_id, name, engine, version, mode, server_service_id, db_name, username, password_enc, size_preset, status, error, backup_schedule_hours, last_backup_at, options, move_state, options_version, created_at`

func scanDatabase(row pgx.Row) (*Database, error) {
	var d Database
	err := row.Scan(&d.ID, &d.ProjectID, &d.Name, &d.Engine, &d.Version, &d.Mode, &d.ServerServiceID, &d.DBName, &d.Username, &d.PasswordEnc,
		&d.SizePreset, &d.Status, &d.Error, &d.BackupScheduleHours, &d.LastBackupAt, &d.Options, &d.MoveState, &d.OptionsVersion, &d.CreatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &d, nil
}

func collectDatabases(rows pgx.Rows, err error) ([]Database, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Database, error) {
		d, err := scanDatabase(r)
		if err != nil {
			return Database{}, err
		}
		return *d, nil
	})
}

func (s *Store) CreateDatabase(ctx context.Context, d *Database) (*Database, error) {
	return scanDatabase(s.q(ctx).QueryRow(ctx, `INSERT INTO databases (project_id, name, engine, mode, db_name, username, password_enc, size_preset, backup_schedule_hours, version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING `+databaseCols,
		d.ProjectID, d.Name, d.Engine, d.Mode, d.DBName, d.Username, d.PasswordEnc, d.SizePreset, d.BackupScheduleHours, d.Version))
}

// MarkEOLNotified records status as the last notified lifecycle status and reports whether it
// changed, so a notification is sent once per (database, status), also across restarts.
func (s *Store) MarkEOLNotified(ctx context.Context, id, status string) (bool, error) {
	tag, err := s.q(ctx).Exec(ctx, `UPDATE databases SET eol_notified=$2 WHERE id=$1 AND eol_notified<>$2`, id, status)
	return tag.RowsAffected() == 1, err
}

func (s *Store) DatabaseByID(ctx context.Context, id string) (*Database, error) {
	return scanDatabase(s.q(ctx).QueryRow(ctx, `SELECT `+databaseCols+` FROM databases WHERE id=$1`, id))
}

func (s *Store) DatabasesForProject(ctx context.Context, projectID string) ([]Database, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT `+databaseCols+` FROM databases WHERE project_id=$1 ORDER BY created_at`, projectID)
	return collectDatabases(rows, err)
}

func (s *Store) AllDatabases(ctx context.Context) ([]Database, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT `+databaseCols+` FROM databases ORDER BY created_at`)
	return collectDatabases(rows, err)
}

func (s *Store) DatabasesOnServer(ctx context.Context, serverServiceID string) ([]Database, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT `+databaseCols+` FROM databases WHERE server_service_id=$1`, serverServiceID)
	return collectDatabases(rows, err)
}

func (s *Store) SetDatabaseServer(ctx context.Context, id, serverServiceID string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE databases SET server_service_id=$2 WHERE id=$1`, id, serverServiceID)
	return err
}

func (s *Store) SetDatabaseStatus(ctx context.Context, id, status, errMsg string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE databases SET status=$2, error=$3 WHERE id=$1`, id, status, errMsg)
	return err
}

// Phases of a MoveState: before the flip the old server is still the database; from the flip on the
// new one is.
const (
	MovePrepare = "prepare"
	MoveFlip    = "flip"
)

// MoveState is the progress record of a database move (see migration 021).
type MoveState struct {
	Phase           string   `json:"phase"`
	OldService      string   `json:"old_service"`
	NewService      string   `json:"new_service,omitempty"`
	TargetVersion   string   `json:"target_version"`
	TargetDedicated bool     `json:"target_dedicated"`
	OldShared       bool     `json:"old_shared"`
	Stopped         []string `json:"stopped,omitempty"` // linked services stopped for the move, to start again at the end
}

// SetMoveState records (nil clears) the progress of a move.
func (s *Store) SetMoveState(ctx context.Context, id string, m *MoveState) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE databases SET move_state=$2 WHERE id=$1`, id, m)
	return err
}

// FlipDatabase points the database at its new server in one compare-and-swap on the old one, and
// records the flip phase in the same statement. It reports whether the swap happened.
func (s *Store) FlipDatabase(ctx context.Context, id, oldService, newService, version, mode string, m MoveState) (bool, error) {
	m.Phase = MoveFlip
	tag, err := s.q(ctx).Exec(ctx, `UPDATE databases SET server_service_id=$3, version=$4, mode=$5, move_state=$6 WHERE id=$1 AND server_service_id=$2`,
		id, oldService, newService, version, mode, m)
	return tag.RowsAffected() == 1, err
}

// ClaimDatabaseStatus moves a database from `from` to `to` and reports whether it was in `from`.
func (s *Store) ClaimDatabaseStatus(ctx context.Context, id, from, to string) (bool, error) {
	tag, err := s.q(ctx).Exec(ctx, `UPDATE databases SET status=$3, error='' WHERE id=$1 AND status=$2`, id, from, to)
	return tag.RowsAffected() == 1, err
}

func (s *Store) DeleteDatabase(ctx context.Context, id string) error {
	_, err := s.q(ctx).Exec(ctx, `DELETE FROM databases WHERE id=$1`, id)
	return err
}

func (s *Store) TouchDatabaseBackup(ctx context.Context, id string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE databases SET last_backup_at=now() WHERE id=$1`, id)
	return err
}

// Links.

type DatabaseLink struct {
	ServiceID  string `json:"service_id"`
	DatabaseID string `json:"database_id"`
	EnvName    string `json:"env_name"`
}

func (s *Store) LinkDatabase(ctx context.Context, serviceID, databaseID, envName string) error {
	_, err := s.q(ctx).Exec(ctx, `INSERT INTO service_databases (service_id, database_id, env_name) VALUES ($1,$2,$3)
		ON CONFLICT (service_id, database_id) DO UPDATE SET env_name=EXCLUDED.env_name`, serviceID, databaseID, envName)
	return err
}

func (s *Store) UnlinkDatabase(ctx context.Context, serviceID, databaseID string) error {
	_, err := s.q(ctx).Exec(ctx, `DELETE FROM service_databases WHERE service_id=$1 AND database_id=$2`, serviceID, databaseID)
	return err
}

func (s *Store) LinksForService(ctx context.Context, serviceID string) ([]DatabaseLink, error) {
	return s.queryLinks(ctx, `WHERE service_id=$1`, serviceID)
}

func (s *Store) LinksForDatabase(ctx context.Context, databaseID string) ([]DatabaseLink, error) {
	return s.queryLinks(ctx, `WHERE database_id=$1`, databaseID)
}

func (s *Store) queryLinks(ctx context.Context, where string, arg string) ([]DatabaseLink, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT service_id, database_id, env_name FROM service_databases `+where, arg)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (DatabaseLink, error) {
		var l DatabaseLink
		err := r.Scan(&l.ServiceID, &l.DatabaseID, &l.EnvName)
		return l, err
	})
}

// Backups.

type Backup struct {
	ID         string     `json:"id"`
	DatabaseID string     `json:"database_id"`
	NodeID     *string    `json:"node_id"`
	Status     string     `json:"status"`
	Path       string     `json:"path"`
	SizeBytes  int64      `json:"size_bytes"`
	Error      string     `json:"error"`
	S3Key      string     `json:"s3_key"`
	Kind       string     `json:"kind"`   // dump | base
	Pinned     bool       `json:"pinned"` // kept by retention until the database is deleted
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at"`
	// Restore check (server/backup_verify.go).
	VerifiedAt *time.Time `json:"verified_at"`
	VerifyOK   *bool      `json:"verify_ok"`
	VerifyNote string     `json:"verify_note"`
}

const backupCols = `id, database_id, node_id, status, path, size_bytes, error, s3_key, kind, pinned, created_at, finished_at, verified_at, verify_ok, verify_note`

func scanBackup(row pgx.Row) (*Backup, error) {
	var b Backup
	if err := row.Scan(&b.ID, &b.DatabaseID, &b.NodeID, &b.Status, &b.Path, &b.SizeBytes, &b.Error, &b.S3Key, &b.Kind, &b.Pinned, &b.CreatedAt, &b.FinishedAt, &b.VerifiedAt, &b.VerifyOK, &b.VerifyNote); err != nil {
		return nil, notFound(err)
	}
	return &b, nil
}

func (s *Store) CreateBackup(ctx context.Context, databaseID, nodeID, path string) (*Backup, error) {
	return scanBackup(s.q(ctx).QueryRow(ctx, `INSERT INTO backups (database_id, node_id, path) VALUES ($1,$2,$3) RETURNING `+backupCols, databaseID, nodeID, path))
}

func (s *Store) FinishBackup(ctx context.Context, id, status, errMsg string, size int64) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE backups SET status=$2, error=$3, size_bytes=$4, finished_at=now() WHERE id=$1`, id, status, errMsg, size)
	return err
}

func (s *Store) BackupByID(ctx context.Context, id string) (*Backup, error) {
	return scanBackup(s.q(ctx).QueryRow(ctx, `SELECT `+backupCols+` FROM backups WHERE id=$1`, id))
}

func (s *Store) BackupsForDatabase(ctx context.Context, databaseID string) ([]Backup, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT `+backupCols+` FROM backups WHERE database_id=$1 ORDER BY created_at DESC LIMIT 50`, databaseID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Backup, error) {
		b, err := scanBackup(r)
		if err != nil {
			return Backup{}, err
		}
		return *b, nil
	})
}

// OldBackups returns completed backups beyond the newest `keep` for a database.
func (s *Store) OldBackups(ctx context.Context, databaseID string, keep int) ([]Backup, error) {
	// Only completed backups count towards keep: failures older than the oldest
	// kept one go too, but never push a good backup out.
	rows, err := s.q(ctx).Query(ctx, `SELECT `+backupCols+` FROM backups WHERE database_id=$1 AND kind='dump' AND status<>'running' AND NOT pinned
		AND created_at < (SELECT created_at FROM backups WHERE database_id=$1 AND kind='dump' AND status='completed' AND NOT pinned
			ORDER BY created_at DESC OFFSET $2 - 1 LIMIT 1)
		ORDER BY created_at DESC`, databaseID, keep)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Backup, error) {
		b, err := scanBackup(r)
		if err != nil {
			return Backup{}, err
		}
		return *b, nil
	})
}

func (s *Store) DeleteBackup(ctx context.Context, id string) error {
	_, err := s.q(ctx).Exec(ctx, `DELETE FROM backups WHERE id=$1`, id)
	return err
}

// PinBackup exempts a backup from retention.
func (s *Store) PinBackup(ctx context.Context, id string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE backups SET pinned=true WHERE id=$1`, id)
	return err
}

func (s *Store) SetBackupS3Key(ctx context.Context, id, key string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE backups SET s3_key=$2 WHERE id=$1`, id, key)
	return err
}

// UpdateDatabaseOptions is UpdateService for databases.options: fn edits the fresh
// options, the write is a compare-and-swap on options_version, and fn may run again.
func (s *Store) UpdateDatabaseOptions(ctx context.Context, id string, fn func(*DBOptions) error) (DBOptions, error) {
	for range casAttempts {
		db, err := s.DatabaseByID(ctx, id)
		if err != nil {
			return DBOptions{}, err
		}
		if err := fn(&db.Options); err != nil {
			return DBOptions{}, err
		}
		var next DBOptions
		err = s.q(ctx).QueryRow(ctx, `UPDATE databases SET options=$2, options_version=options_version+1 WHERE id=$1 AND options_version=$3 RETURNING options`,
			id, db.Options, db.OptionsVersion).Scan(&next)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		return next, err
	}
	return DBOptions{}, ErrConflict
}

// UsedPublicPorts lists the external ports already given to databases.
func (s *Store) UsedPublicPorts(ctx context.Context) (map[int]bool, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT (options->>'public_port')::int FROM databases WHERE (options->>'public_port') IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = true
	}
	return out, rows.Err()
}

// DBMetric is one sample of a database's size and connections.
type DBMetric struct {
	TS          time.Time `json:"ts"`
	SizeBytes   int64     `json:"size_bytes"`
	Connections int       `json:"connections"`
}

func (s *Store) InsertDBMetric(ctx context.Context, dbID string, m DBMetric) error {
	_, err := s.q(ctx).Exec(ctx, `INSERT INTO db_metrics (database_id, ts, size_bytes, connections) VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
		dbID, m.TS, m.SizeBytes, m.Connections)
	return err
}

func (s *Store) DBMetrics(ctx context.Context, dbID string, since time.Time) ([]DBMetric, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT ts, size_bytes, connections FROM db_metrics WHERE database_id=$1 AND ts >= $2 ORDER BY ts`, dbID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DBMetric{}
	for rows.Next() {
		var m DBMetric
		if err := rows.Scan(&m.TS, &m.SizeBytes, &m.Connections); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) PruneDBMetrics(ctx context.Context, before time.Time) {
	_, _ = s.q(ctx).Exec(ctx, `DELETE FROM db_metrics WHERE ts < $1`, before)
}

// DBUser is an extra login on a database.
type DBUser struct {
	DatabaseID  string    `json:"database_id"`
	Username    string    `json:"username"`
	Role        string    `json:"role"`
	PasswordEnc string    `json:"-"`
	CreatedAt   time.Time `json:"created_at"`
}

func (s *Store) DBUsers(ctx context.Context, dbID string) ([]DBUser, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT database_id, username, role, password_enc, created_at FROM db_users WHERE database_id=$1 ORDER BY created_at`, dbID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DBUser{}
	for rows.Next() {
		var u DBUser
		if err := rows.Scan(&u.DatabaseID, &u.Username, &u.Role, &u.PasswordEnc, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) DBUser(ctx context.Context, dbID, username string) (*DBUser, error) {
	var u DBUser
	err := s.q(ctx).QueryRow(ctx, `SELECT database_id, username, role, password_enc, created_at FROM db_users WHERE database_id=$1 AND username=$2`, dbID, username).
		Scan(&u.DatabaseID, &u.Username, &u.Role, &u.PasswordEnc, &u.CreatedAt)
	return &u, notFound(err)
}

func (s *Store) CreateDBUser(ctx context.Context, u DBUser) error {
	_, err := s.q(ctx).Exec(ctx, `INSERT INTO db_users (database_id, username, role, password_enc) VALUES ($1,$2,$3,$4)`, u.DatabaseID, u.Username, u.Role, u.PasswordEnc)
	return err
}

func (s *Store) SetDBUserPassword(ctx context.Context, dbID, username, enc string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE db_users SET password_enc=$3 WHERE database_id=$1 AND username=$2`, dbID, username, enc)
	return err
}

func (s *Store) DeleteDBUser(ctx context.Context, dbID, username string) error {
	_, err := s.q(ctx).Exec(ctx, `DELETE FROM db_users WHERE database_id=$1 AND username=$2`, dbID, username)
	return err
}

func (s *Store) SetDatabaseSize(ctx context.Context, id, size string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE databases SET size_preset=$2 WHERE id=$1`, id, size)
	return err
}

// CreateBaseBackup records a PITR base backup.
func (s *Store) CreateBaseBackup(ctx context.Context, databaseID, nodeID, path string) (*Backup, error) {
	return scanBackup(s.q(ctx).QueryRow(ctx, `INSERT INTO backups (database_id, node_id, path, kind) VALUES ($1,$2,$3,'base') RETURNING `+backupCols, databaseID, nodeID, path))
}

// BaseBackups lists completed PITR base backups, newest first.
func (s *Store) BaseBackups(ctx context.Context, databaseID string) ([]Backup, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT `+backupCols+` FROM backups WHERE database_id=$1 AND kind='base' AND status='completed' ORDER BY created_at DESC`, databaseID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Backup, error) {
		b, err := scanBackup(r)
		if err != nil {
			return Backup{}, err
		}
		return *b, nil
	})
}

// SetBackupVerified records the outcome of a restore check.
func (s *Store) SetBackupVerified(ctx context.Context, id string, ok bool, note string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE backups SET verified_at=now(), verify_ok=$2, verify_note=$3 WHERE id=$1`, id, ok, note)
	return err
}

// LatestBackup is the newest completed dump of a database.
func (s *Store) LatestBackup(ctx context.Context, databaseID string) (*Backup, error) {
	return scanBackup(s.q(ctx).QueryRow(ctx, `SELECT `+backupCols+` FROM backups WHERE database_id=$1 AND kind='dump' AND status='completed'
		ORDER BY created_at DESC LIMIT 1`, databaseID))
}

// LastVerified is when a database's backups were last checked by a restore (nil: never).
func (s *Store) LastVerified(ctx context.Context, databaseID string) (*time.Time, error) {
	var t *time.Time
	err := s.q(ctx).QueryRow(ctx, `SELECT max(verified_at) FROM backups WHERE database_id=$1`, databaseID).Scan(&t)
	return t, err
}
