package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type Deployment struct {
	ID            string     `json:"id"`
	ServiceID     string     `json:"service_id"`
	Status        string     `json:"status"`
	Trigger       string     `json:"trigger"`
	CommitSHA     string     `json:"commit_sha"`
	CommitMessage string     `json:"commit_message"`
	Branch        string     `json:"branch"`
	Image         string     `json:"image"`
	StaticPath    string     `json:"static_path"`
	BuildNodeID   *string    `json:"build_node_id"`
	Detected      string     `json:"detected"`
	PushedImage   string     `json:"pushed_image"`
	Error         string     `json:"error"`
	CreatedBy     *string    `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at"`
}

const deploymentCols = `id, service_id, status, trigger, commit_sha, commit_message, branch, image, static_path, build_node_id, detected, pushed_image, error, created_by, created_at, started_at, finished_at`

func scanDeployment(row pgx.Row) (*Deployment, error) {
	var d Deployment
	err := row.Scan(&d.ID, &d.ServiceID, &d.Status, &d.Trigger, &d.CommitSHA, &d.CommitMessage, &d.Branch, &d.Image,
		&d.StaticPath, &d.BuildNodeID, &d.Detected, &d.PushedImage, &d.Error, &d.CreatedBy, &d.CreatedAt, &d.StartedAt, &d.FinishedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return &d, nil
}

func (s *Store) CreateDeployment(ctx context.Context, d *Deployment) (*Deployment, error) {
	return scanDeployment(s.q(ctx).QueryRow(ctx, `INSERT INTO deployments (service_id, status, trigger, commit_sha, commit_message, branch, image, static_path, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING `+deploymentCols,
		d.ServiceID, d.Status, d.Trigger, d.CommitSHA, d.CommitMessage, d.Branch, d.Image, d.StaticPath, d.CreatedBy))
}

func (s *Store) DeploymentByID(ctx context.Context, id string) (*Deployment, error) {
	return scanDeployment(s.q(ctx).QueryRow(ctx, `SELECT `+deploymentCols+` FROM deployments WHERE id=$1`, id))
}

func (s *Store) DeploymentsForService(ctx context.Context, serviceID string, limit int) ([]Deployment, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT `+deploymentCols+` FROM deployments WHERE service_id=$1 ORDER BY created_at DESC LIMIT $2`, serviceID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Deployment, error) {
		d, err := scanDeployment(r)
		if err != nil {
			return Deployment{}, err
		}
		return *d, nil
	})
}

// ClaimQueuedDeployment atomically picks the oldest queued deployment and marks it building.
// Only one deployment per service is in flight at a time.
func (s *Store) ClaimQueuedDeployment(ctx context.Context) (*Deployment, error) {
	return scanDeployment(s.q(ctx).QueryRow(ctx, `UPDATE deployments SET status='building', started_at=now()
		WHERE id = (
			SELECT d.id FROM deployments d WHERE d.status='queued'
			AND NOT EXISTS (SELECT 1 FROM deployments x WHERE x.service_id=d.service_id AND x.status IN ('building','deploying'))
			ORDER BY d.created_at FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING `+deploymentCols))
}

// CancelOlderQueued cancels queued webhook deployments of a service created before the given one.
// Manual deploys and rollbacks are explicit user intent and are never cancelled by a newer commit.
func (s *Store) CancelOlderQueued(ctx context.Context, serviceID, keepID string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE deployments SET status='cancelled', finished_at=now()
		WHERE service_id=$1 AND status='queued' AND trigger='webhook' AND id<>$2 AND created_at <= (SELECT created_at FROM deployments WHERE id=$2)`, serviceID, keepID)
	return err
}

func (s *Store) UpdateDeploymentBuild(ctx context.Context, id, buildNode string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE deployments SET build_node_id=$2 WHERE id=$1`, id, buildNode)
	return err
}

func (s *Store) UpdateDeploymentBuilt(ctx context.Context, d *Deployment) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE deployments SET status='deploying', image=$2, static_path=$3, commit_sha=$4, commit_message=$5, detected=$6 WHERE id=$1`,
		d.ID, d.Image, d.StaticPath, d.CommitSHA, d.CommitMessage, d.Detected)
	return err
}

// SetDeploymentImage rewrites the artifact a deployment runs (pinning a tag to a digest), so a
// reschedule or restart of the current deployment starts that exact image.
func (s *Store) SetDeploymentImage(ctx context.Context, id, image string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE deployments SET image=$2 WHERE id=$1`, id, image)
	return err
}

func (s *Store) SetDeploymentStatus(ctx context.Context, id, status, errMsg string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE deployments SET status=$2, error=$3,
		finished_at = CASE WHEN $2 IN ('active','failed','cancelled','superseded') THEN COALESCE(finished_at, now()) ELSE finished_at END
		WHERE id=$1`, id, status, errMsg)
	return err
}

// SupersedeOthers marks every other active deployment of the service as superseded.
func (s *Store) SupersedeOthers(ctx context.Context, serviceID, activeID string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE deployments SET status='superseded' WHERE service_id=$1 AND id<>$2 AND status='active'`, serviceID, activeID)
	return err
}

// FailStuckDeployments settles deployments left mid-flight by a crashed server: the one the
// service already runs stays active, any other fails. A service still marked deploying goes
// back to running (or failed if it never had a deployment); other statuses are left alone.
// Half-started instances of failed deployments are dropped. One statement, so a crash cannot
// leave the three tables disagreeing.
func (s *Store) FailStuckDeployments(ctx context.Context) error {
	_, err := s.q(ctx).Exec(ctx, `WITH fixed AS (
		UPDATE deployments d SET
			status = CASE WHEN sv.current_deployment_id = d.id THEN 'active' ELSE 'failed' END,
			error = CASE WHEN sv.current_deployment_id = d.id THEN d.error ELSE 'interrupted by control plane restart' END,
			finished_at = now()
		FROM services sv
		WHERE sv.id = d.service_id AND d.status IN ('building','deploying')
		RETURNING d.id, d.service_id, d.status),
	svc AS (
		UPDATE services SET status = CASE WHEN current_deployment_id IS NOT NULL THEN 'running' ELSE 'failed' END, updated_at=now()
		WHERE status='deploying' AND id IN (SELECT service_id FROM fixed))
	DELETE FROM instances WHERE status='starting' AND deployment_id IN (SELECT id FROM fixed WHERE status='failed')`)
	return err
}

// RetainedImages returns images of the most recent N successful deployments per service.
func (s *Store) RetainedImages(ctx context.Context, keep int) ([]string, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT image FROM (
		SELECT image, row_number() OVER (PARTITION BY service_id ORDER BY created_at DESC) rn
		FROM deployments WHERE image<>'' AND status IN ('active','superseded','deploying','building')) x WHERE rn <= $1`, keep)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Logs.

type LogLine struct {
	ID   int64     `json:"id"`
	TS   time.Time `json:"ts"`
	Line string    `json:"line"`
}

func (s *Store) AppendDeploymentLogs(ctx context.Context, deploymentID string, lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, l := range lines {
		batch.Queue(`INSERT INTO deployment_logs (deployment_id, line) VALUES ($1,$2)`, deploymentID, l)
	}
	return s.q(ctx).SendBatch(ctx, batch).Close()
}

func (s *Store) DeploymentLogs(ctx context.Context, deploymentID string, afterID int64, limit int) ([]LogLine, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT id, ts, line FROM deployment_logs WHERE deployment_id=$1 AND id>$2 ORDER BY id LIMIT $3`, deploymentID, afterID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (LogLine, error) {
		var l LogLine
		err := r.Scan(&l.ID, &l.TS, &l.Line)
		return l, err
	})
}

// DeploymentLogTail is the last n lines of a deployment's log, oldest first.
func (s *Store) DeploymentLogTail(ctx context.Context, deploymentID string, n int) ([]string, error) {
	rows, err := s.q(ctx).Query(ctx, `SELECT line FROM (SELECT id, line FROM deployment_logs WHERE deployment_id=$1 ORDER BY id DESC LIMIT $2) t ORDER BY id`, deploymentID, n)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (s *Store) SetDeploymentPushed(ctx context.Context, id, ref string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE deployments SET pushed_image=$2 WHERE id=$1`, id, ref)
	return err
}
