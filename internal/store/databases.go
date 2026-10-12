package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type Database struct {
	ID                  string     `json:"id"`
	ProjectID           string     `json:"project_id"`
	Name                string     `json:"name"`
	Engine              string     `json:"engine"`
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
	CreatedAt           time.Time  `json:"created_at"`
}

const databaseCols = `id, project_id, name, engine, mode, server_service_id, db_name, username, password_enc, size_preset, status, error, backup_schedule_hours, last_backup_at, created_at`

func scanDatabase(row pgx.Row) (*Database, error) {
	var d Database
	err := row.Scan(&d.ID, &d.ProjectID, &d.Name, &d.Engine, &d.Mode, &d.ServerServiceID, &d.DBName, &d.Username, &d.PasswordEnc,
		&d.SizePreset, &d.Status, &d.Error, &d.BackupScheduleHours, &d.LastBackupAt, &d.CreatedAt)
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
	return scanDatabase(s.DB.QueryRow(ctx, `INSERT INTO databases (project_id, name, engine, mode, db_name, username, password_enc, size_preset, backup_schedule_hours)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING `+databaseCols,
		d.ProjectID, d.Name, d.Engine, d.Mode, d.DBName, d.Username, d.PasswordEnc, d.SizePreset, d.BackupScheduleHours))
}

func (s *Store) DatabaseByID(ctx context.Context, id string) (*Database, error) {
	return scanDatabase(s.DB.QueryRow(ctx, `SELECT `+databaseCols+` FROM databases WHERE id=$1`, id))
}

func (s *Store) DatabasesForProject(ctx context.Context, projectID string) ([]Database, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+databaseCols+` FROM databases WHERE project_id=$1 ORDER BY created_at`, projectID)
	return collectDatabases(rows, err)
}

func (s *Store) AllDatabases(ctx context.Context) ([]Database, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+databaseCols+` FROM databases ORDER BY created_at`)
	return collectDatabases(rows, err)
}

func (s *Store) DatabasesOnServer(ctx context.Context, serverServiceID string) ([]Database, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+databaseCols+` FROM databases WHERE server_service_id=$1`, serverServiceID)
	return collectDatabases(rows, err)
}

func (s *Store) SetDatabaseServer(ctx context.Context, id, serverServiceID string) error {
	_, err := s.DB.Exec(ctx, `UPDATE databases SET server_service_id=$2 WHERE id=$1`, id, serverServiceID)
	return err
}

func (s *Store) SetDatabaseStatus(ctx context.Context, id, status, errMsg string) error {
	_, err := s.DB.Exec(ctx, `UPDATE databases SET status=$2, error=$3 WHERE id=$1`, id, status, errMsg)
	return err
}

func (s *Store) SetDatabaseMode(ctx context.Context, id, mode string, serverServiceID *string) error {
	_, err := s.DB.Exec(ctx, `UPDATE databases SET mode=$2, server_service_id=$3 WHERE id=$1`, id, mode, serverServiceID)
	return err
}

func (s *Store) DeleteDatabase(ctx context.Context, id string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM databases WHERE id=$1`, id)
	return err
}

func (s *Store) TouchDatabaseBackup(ctx context.Context, id string) error {
	_, err := s.DB.Exec(ctx, `UPDATE databases SET last_backup_at=now() WHERE id=$1`, id)
	return err
}

// Links.

type DatabaseLink struct {
	ServiceID  string `json:"service_id"`
	DatabaseID string `json:"database_id"`
	EnvName    string `json:"env_name"`
}

func (s *Store) LinkDatabase(ctx context.Context, serviceID, databaseID, envName string) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO service_databases (service_id, database_id, env_name) VALUES ($1,$2,$3)
		ON CONFLICT (service_id, database_id) DO UPDATE SET env_name=EXCLUDED.env_name`, serviceID, databaseID, envName)
	return err
}

func (s *Store) UnlinkDatabase(ctx context.Context, serviceID, databaseID string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM service_databases WHERE service_id=$1 AND database_id=$2`, serviceID, databaseID)
	return err
}

func (s *Store) LinksForService(ctx context.Context, serviceID string) ([]DatabaseLink, error) {
	return s.queryLinks(ctx, `WHERE service_id=$1`, serviceID)
}

func (s *Store) LinksForDatabase(ctx context.Context, databaseID string) ([]DatabaseLink, error) {
	return s.queryLinks(ctx, `WHERE database_id=$1`, databaseID)
}

func (s *Store) queryLinks(ctx context.Context, where string, arg string) ([]DatabaseLink, error) {
	rows, err := s.DB.Query(ctx, `SELECT service_id, database_id, env_name FROM service_databases `+where, arg)
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
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

const backupCols = `id, database_id, node_id, status, path, size_bytes, error, s3_key, created_at, finished_at`

func scanBackup(row pgx.Row) (*Backup, error) {
	var b Backup
	if err := row.Scan(&b.ID, &b.DatabaseID, &b.NodeID, &b.Status, &b.Path, &b.SizeBytes, &b.Error, &b.S3Key, &b.CreatedAt, &b.FinishedAt); err != nil {
		return nil, notFound(err)
	}
	return &b, nil
}

func (s *Store) CreateBackup(ctx context.Context, databaseID, nodeID, path string) (*Backup, error) {
	return scanBackup(s.DB.QueryRow(ctx, `INSERT INTO backups (database_id, node_id, path) VALUES ($1,$2,$3) RETURNING `+backupCols, databaseID, nodeID, path))
}

func (s *Store) FinishBackup(ctx context.Context, id, status, errMsg string, size int64) error {
	_, err := s.DB.Exec(ctx, `UPDATE backups SET status=$2, error=$3, size_bytes=$4, finished_at=now() WHERE id=$1`, id, status, errMsg, size)
	return err
}

func (s *Store) BackupByID(ctx context.Context, id string) (*Backup, error) {
	return scanBackup(s.DB.QueryRow(ctx, `SELECT `+backupCols+` FROM backups WHERE id=$1`, id))
}

func (s *Store) BackupsForDatabase(ctx context.Context, databaseID string) ([]Backup, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+backupCols+` FROM backups WHERE database_id=$1 ORDER BY created_at DESC LIMIT 50`, databaseID)
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
	rows, err := s.DB.Query(ctx, `SELECT `+backupCols+` FROM backups WHERE database_id=$1 AND status<>'running' ORDER BY created_at DESC OFFSET $2`, databaseID, keep)
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
	_, err := s.DB.Exec(ctx, `DELETE FROM backups WHERE id=$1`, id)
	return err
}

func (s *Store) SetBackupS3Key(ctx context.Context, id, key string) error {
	_, err := s.DB.Exec(ctx, `UPDATE backups SET s3_key=$2 WHERE id=$1`, id, key)
	return err
}
