package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type JobRun struct {
	ID          string     `json:"id"`
	ServiceID   string     `json:"service_id"`
	NodeID      *string    `json:"node_id"`
	ContainerID string     `json:"container_id"`
	Status      string     `json:"status"`
	ExitCode    *int       `json:"exit_code"`
	Output      string     `json:"output"`
	Trigger     string     `json:"trigger"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at"`
}

const jobCols = `id, service_id, node_id, container_id, status, exit_code, output, trigger, started_at, finished_at`

func scanJob(row pgx.Row) (*JobRun, error) {
	var j JobRun
	if err := row.Scan(&j.ID, &j.ServiceID, &j.NodeID, &j.ContainerID, &j.Status, &j.ExitCode, &j.Output, &j.Trigger, &j.StartedAt, &j.FinishedAt); err != nil {
		return nil, notFound(err)
	}
	return &j, nil
}

func (s *Store) CreateJobRun(ctx context.Context, serviceID, nodeID, containerID, trigger string) (*JobRun, error) {
	return scanJob(s.q(ctx).QueryRow(ctx, `INSERT INTO job_runs (service_id, node_id, container_id, trigger) VALUES ($1,$2,$3,$4) RETURNING `+jobCols,
		serviceID, nodeID, containerID, trigger))
}

func (s *Store) FinishJobRun(ctx context.Context, id, status string, exitCode int, output string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE job_runs SET status=$2, exit_code=$3, output=$4, finished_at=now() WHERE id=$1`, id, status, exitCode, output)
	return err
}

func (s *Store) JobRuns(ctx context.Context, serviceID string, limit int) ([]JobRun, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT `+jobCols+` FROM job_runs WHERE service_id=$1 ORDER BY started_at DESC LIMIT $2`, serviceID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (JobRun, error) {
		j, err := scanJob(r)
		if err != nil {
			return JobRun{}, err
		}
		return *j, nil
	})
}

func (s *Store) RunningJob(ctx context.Context, serviceID string) (bool, error) {
	var n int
	err := s.q(ctx).QueryRow(ctx, `SELECT count(*) FROM job_runs WHERE service_id=$1 AND status='running' AND started_at > now() - interval '1 day'`, serviceID).Scan(&n)
	return n > 0, err
}

// FailInterruptedJobs marks jobs left running by a control plane restart.
func (s *Store) FailInterruptedJobs(ctx context.Context) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE job_runs SET status='failed', output=output || E'\n(interrupted by control plane restart)', finished_at=now() WHERE status='running'`)
	return err
}

// FailLostJobs fails the running jobs of a node that the node no longer reports (alive): the
// agent restarted and dropped them. The 30s grace covers the time between CreateJobRun and the
// agent receiving the request. FinishJobRun still overwrites if the real reply ever arrives.
func (s *Store) FailLostJobs(ctx context.Context, nodeID string, alive []string) (int64, error) {
	tag, err := s.q(ctx).Exec(ctx, `UPDATE job_runs SET status='failed', exit_code=-1, output=output || E'\n(lost: the node no longer runs this job)', finished_at=now()
		WHERE node_id=$1 AND status='running' AND started_at < now() - interval '30 seconds' AND NOT (id::text = ANY($2))`, nodeID, alive)
	return tag.RowsAffected(), err
}

func (s *Store) PurgeJobRuns(ctx context.Context, keep int) error {
	_, err := s.q(ctx).Exec(ctx, `DELETE FROM job_runs j WHERE id IN (
		SELECT id FROM (SELECT id, row_number() OVER (PARTITION BY service_id ORDER BY started_at DESC) rn FROM job_runs) x WHERE rn > $1)`, keep)
	return err
}

func (s *Store) JobRunByID(ctx context.Context, id string) (*JobRun, error) {
	return scanJob(s.q(ctx).QueryRow(ctx, `SELECT `+jobCols+` FROM job_runs WHERE id=$1`, id))
}
