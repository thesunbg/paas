package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Steps and statuses of an SSH install attempt (ssh_installs).
const (
	SSHStepConnect   = "connect"
	SSHStepPreflight = "preflight"
	SSHStepInstall   = "install"
	SSHStepEnroll    = "enroll"
	SSHStepDone      = "done"

	SSHRunning   = "running"
	SSHSucceeded = "succeeded"
	SSHFailed    = "failed"
)

// SSHInstall is one attempt to install a node over SSH. It never holds credentials.
type SSHInstall struct {
	ID                 string          `json:"id"`
	Host               string          `json:"host"`
	Port               int             `json:"port"`
	User               string          `json:"user"`
	AuthMethod         string          `json:"auth_method"`
	Roles              []string        `json:"roles"`
	NodeName           string          `json:"node_name"`
	HostKey            string          `json:"host_key"`
	HostKeyFingerprint string          `json:"host_key_fingerprint"`
	Reinstall          bool            `json:"reinstall"`
	PreflightOnly      bool            `json:"preflight_only"`
	Status             string          `json:"status"`
	Step               string          `json:"step"`
	Preflight          json.RawMessage `json:"preflight"` // remote.Report: checklist and what it found
	Error              string          `json:"error"`
	TokenHash          []byte          `json:"-"`
	NodeID             *string         `json:"node_id"`
	ServerName         string          `json:"server_name"`
	CreatedBy          *string         `json:"created_by"`
	CreatedByEmail     string          `json:"created_by_email"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
	FinishedAt         *time.Time      `json:"finished_at"`
}

const sshInstallCols = `id, host, port, ssh_user, auth_method, roles, node_name, host_key, host_key_fingerprint, reinstall, preflight_only,
	status, step, preflight, error, token_hash, node_id, server_name, created_by, created_by_email, created_at, updated_at, finished_at`

func scanSSHInstall(row pgx.Row) (*SSHInstall, error) {
	var i SSHInstall
	if err := row.Scan(&i.ID, &i.Host, &i.Port, &i.User, &i.AuthMethod, &i.Roles, &i.NodeName, &i.HostKey, &i.HostKeyFingerprint, &i.Reinstall,
		&i.PreflightOnly, &i.Status, &i.Step, &i.Preflight, &i.Error, &i.TokenHash, &i.NodeID, &i.ServerName, &i.CreatedBy, &i.CreatedByEmail,
		&i.CreatedAt, &i.UpdatedAt, &i.FinishedAt); err != nil {
		return nil, notFound(err)
	}
	return &i, nil
}

// CreateSSHInstall records a new attempt (status running, step connect).
func (s *Store) CreateSSHInstall(ctx context.Context, i *SSHInstall) (*SSHInstall, error) {
	return scanSSHInstall(s.q(ctx).QueryRow(ctx, `INSERT INTO ssh_installs (host, port, ssh_user, auth_method, roles, node_name, host_key,
		host_key_fingerprint, reinstall, preflight_only, server_name, created_by, created_by_email)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING `+sshInstallCols,
		i.Host, i.Port, i.User, i.AuthMethod, i.Roles, i.NodeName, i.HostKey, i.HostKeyFingerprint, i.Reinstall, i.PreflightOnly,
		i.ServerName, i.CreatedBy, i.CreatedByEmail))
}

func (s *Store) SSHInstallByID(ctx context.Context, id string) (*SSHInstall, error) {
	return scanSSHInstall(s.q(ctx).QueryRow(ctx, `SELECT `+sshInstallCols+` FROM ssh_installs WHERE id=$1`, id))
}

// SSHInstalls lists the newest attempts first.
func (s *Store) SSHInstalls(ctx context.Context, limit int) ([]SSHInstall, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT `+sshInstallCols+` FROM ssh_installs ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SSHInstall, error) {
		i, err := scanSSHInstall(r)
		if err != nil {
			return SSHInstall{}, err
		}
		return *i, nil
	})
}

// RunningSSHInstalls lists the unfinished attempts of one control plane ("" = all).
func (s *Store) RunningSSHInstalls(ctx context.Context, serverName string) ([]SSHInstall, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT `+sshInstallCols+` FROM ssh_installs WHERE status='running' AND ($1='' OR server_name=$1)`, serverName)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SSHInstall, error) {
		i, err := scanSSHInstall(r)
		if err != nil {
			return SSHInstall{}, err
		}
		return *i, nil
	})
}

// SetSSHInstallStep moves a running attempt to a step.
func (s *Store) SetSSHInstallStep(ctx context.Context, id, step string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE ssh_installs SET step=$2, updated_at=now() WHERE id=$1 AND status='running'`, id, step)
	return err
}

// SetSSHInstallPreflight stores the preflight checklist and what it learned about the machine.
func (s *Store) SetSSHInstallPreflight(ctx context.Context, id string, report any) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE ssh_installs SET preflight=$2, updated_at=now() WHERE id=$1`, id, report)
	return err
}

// SetSSHInstallHostKey stores the host key the connection was verified against.
func (s *Store) SetSSHInstallHostKey(ctx context.Context, id, key string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE ssh_installs SET host_key=$2, updated_at=now() WHERE id=$1`, id, key)
	return err
}

// RevokeJoinToken deletes a join token that has not been used yet.
func (s *Store) RevokeJoinToken(ctx context.Context, hash []byte) error {
	_, err := s.q(ctx).Exec(ctx, `DELETE FROM join_tokens WHERE hash=$1 AND used_at IS NULL`, hash)
	return err
}

// SetSSHInstallToken records the join token issued for the attempt.
func (s *Store) SetSSHInstallToken(ctx context.Context, id string, hash []byte) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE ssh_installs SET token_hash=$2, updated_at=now() WHERE id=$1`, id, hash)
	return err
}

// FinishSSHInstall ends a running attempt. A succeeded attempt with a node also stores the
// accepted host key on the node.
func (s *Store) FinishSSHInstall(ctx context.Context, id, status, errMsg string, nodeID *string) error {
	return pgx.BeginFunc(ctx, s.q(ctx), func(tx pgx.Tx) error {
		var hostKey string
		err := tx.QueryRow(ctx, `UPDATE ssh_installs SET status=$2, error=$3, node_id=COALESCE($4, node_id),
			step=CASE WHEN $2='succeeded' AND NOT preflight_only THEN 'done' ELSE step END, updated_at=now(), finished_at=now()
			WHERE id=$1 AND status='running' RETURNING host_key`, id, status, errMsg, nodeID).Scan(&hostKey)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if nodeID != nil && status == SSHSucceeded && hostKey != "" {
			_, err = tx.Exec(ctx, `UPDATE nodes SET ssh_host_key=$2 WHERE id=$1`, *nodeID, hostKey)
		}
		return err
	})
}

// AppendSSHInstallLog stores output lines of an attempt.
func (s *Store) AppendSSHInstallLog(ctx context.Context, id string, lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	_, err := s.q(ctx).Exec(ctx, `INSERT INTO ssh_install_logs (install_id, line) SELECT $1, unnest($2::text[])`, id, lines)
	if err == nil {
		_, err = s.q(ctx).Exec(ctx, `UPDATE ssh_installs SET updated_at=now() WHERE id=$1`, id)
	}
	return err
}

// SSHInstallLogs returns lines after the given id, oldest first.
func (s *Store) SSHInstallLogs(ctx context.Context, id string, after int64, limit int) ([]LogLine, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT id, created_at, line FROM ssh_install_logs WHERE install_id=$1 AND id>$2 ORDER BY id LIMIT $3`, id, after, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[LogLine])
}

// SSHInstallLogTail returns the last n lines, oldest first.
func (s *Store) SSHInstallLogTail(ctx context.Context, id string, n int) ([]string, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT line FROM (SELECT id, line FROM ssh_install_logs WHERE install_id=$1 ORDER BY id DESC LIMIT $2) t ORDER BY id`, id, n)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// JoinTokenNode returns the node a join token enrolled, nil while unused.
func (s *Store) JoinTokenNode(ctx context.Context, hash []byte) (*string, error) {
	var id *string
	err := s.q(ctx).QueryRow(ctx, `SELECT node_id FROM join_tokens WHERE hash=$1`, hash).Scan(&id)
	return id, notFound(err)
}

// NodeSSHHostKeys maps node id to the SSH host key accepted when it was installed over SSH.
func (s *Store) NodeSSHHostKeys(ctx context.Context) (map[string]string, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT id, ssh_host_key FROM nodes WHERE ssh_host_key<>''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, k string
		if err := rows.Scan(&id, &k); err != nil {
			return nil, err
		}
		out[id] = k
	}
	return out, rows.Err()
}
