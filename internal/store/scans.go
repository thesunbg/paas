package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/scan"
)

// Image scan statuses.
const (
	ScanScanned     = "scanned"
	ScanUnavailable = "unavailable" // no scanner on the node, the scan failed, or no build node
)

// ImageScan is the vulnerability scan of one deployment's image.
type ImageScan struct {
	DeploymentID   string              `json:"deployment_id"`
	ServiceID      string              `json:"service_id"`
	Image          string              `json:"image"`
	Status         string              `json:"status"`
	Scanner        string              `json:"scanner"`
	ScannerVersion string              `json:"scanner_version"`
	DBUpdatedAt    *time.Time          `json:"db_updated_at"`
	OS             string              `json:"os"`
	Critical       int                 `json:"critical"`
	High           int                 `json:"high"`
	Medium         int                 `json:"medium"`
	Low            int                 `json:"low"`
	Unknown        int                 `json:"unknown"`
	Findings       []proto.ScanFinding `json:"findings"`
	PolicyMode     string              `json:"policy_mode"`
	Threshold      string              `json:"threshold"`
	Blocked        bool                `json:"blocked"`
	Message        string              `json:"message"`
	ScannedAt      time.Time           `json:"scanned_at"`
}

const imageScanCols = `deployment_id, service_id, image, status, scanner, scanner_version, db_updated_at, os,
	critical, high, medium, low, unknown, findings, policy_mode, threshold, blocked, message, scanned_at`

func scanImageScan(row pgx.Row) (*ImageScan, error) {
	var x ImageScan
	err := row.Scan(&x.DeploymentID, &x.ServiceID, &x.Image, &x.Status, &x.Scanner, &x.ScannerVersion, &x.DBUpdatedAt, &x.OS,
		&x.Critical, &x.High, &x.Medium, &x.Low, &x.Unknown, &x.Findings, &x.PolicyMode, &x.Threshold, &x.Blocked, &x.Message, &x.ScannedAt)
	if err != nil {
		return nil, notFound(err)
	}
	if x.Findings == nil {
		x.Findings = []proto.ScanFinding{}
	}
	return &x, nil
}

// SaveImageScan records (or replaces) the scan of a deployment.
func (s *Store) SaveImageScan(ctx context.Context, x *ImageScan) error {
	if x.Findings == nil {
		x.Findings = []proto.ScanFinding{}
	}
	if len(x.Findings) > proto.MaxScanFindings {
		x.Findings = x.Findings[:proto.MaxScanFindings]
	}
	if x.ScannedAt.IsZero() {
		x.ScannedAt = time.Now().UTC()
	}
	_, err := s.q(ctx).Exec(ctx, `INSERT INTO image_scans (`+imageScanCols+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
		ON CONFLICT (deployment_id) DO UPDATE SET image=EXCLUDED.image, status=EXCLUDED.status, scanner=EXCLUDED.scanner,
			scanner_version=EXCLUDED.scanner_version, db_updated_at=EXCLUDED.db_updated_at, os=EXCLUDED.os,
			critical=EXCLUDED.critical, high=EXCLUDED.high, medium=EXCLUDED.medium, low=EXCLUDED.low, unknown=EXCLUDED.unknown,
			findings=EXCLUDED.findings, policy_mode=EXCLUDED.policy_mode, threshold=EXCLUDED.threshold,
			blocked=EXCLUDED.blocked, message=EXCLUDED.message, scanned_at=EXCLUDED.scanned_at`,
		x.DeploymentID, x.ServiceID, x.Image, x.Status, x.Scanner, x.ScannerVersion, x.DBUpdatedAt, x.OS,
		x.Critical, x.High, x.Medium, x.Low, x.Unknown, x.Findings, x.PolicyMode, x.Threshold, x.Blocked, x.Message, x.ScannedAt)
	return err
}

// ImageScanForDeployment returns the scan of one deployment.
func (s *Store) ImageScanForDeployment(ctx context.Context, deploymentID string) (*ImageScan, error) {
	return scanImageScan(s.q(ctx).QueryRow(ctx, `SELECT `+imageScanCols+` FROM image_scans WHERE deployment_id=$1`, deploymentID))
}

// LatestImageScan returns the newest scan of a service.
func (s *Store) LatestImageScan(ctx context.Context, serviceID string) (*ImageScan, error) {
	return scanImageScan(s.q(ctx).QueryRow(ctx, `SELECT `+imageScanCols+` FROM image_scans WHERE service_id=$1
		ORDER BY scanned_at DESC LIMIT 1`, serviceID))
}

// ProjectScanPolicy returns a project's scan policy, or the default when it never set one.
func (s *Store) ProjectScanPolicy(ctx context.Context, projectID string) (scan.Policy, error) {
	var p scan.Policy
	err := s.q(ctx).QueryRow(ctx, `SELECT mode, threshold FROM project_scan_policies WHERE project_id=$1`, projectID).Scan(&p.Mode, &p.Threshold)
	if errors.Is(err, pgx.ErrNoRows) {
		return scan.DefaultPolicy(), nil
	}
	return p, err
}

// SetProjectScanPolicy stores a (normalized) policy.
func (s *Store) SetProjectScanPolicy(ctx context.Context, projectID string, p scan.Policy) error {
	_, err := s.q(ctx).Exec(ctx, `INSERT INTO project_scan_policies (project_id, mode, threshold) VALUES ($1,$2,$3)
		ON CONFLICT (project_id) DO UPDATE SET mode=EXCLUDED.mode, threshold=EXCLUDED.threshold, updated_at=now()`,
		projectID, p.Mode, p.Threshold)
	return err
}

// ClearDeploymentImage forgets the image of a deployment the scan policy blocked,
// so a rollback to it cannot put that image live after all.
func (s *Store) ClearDeploymentImage(ctx context.Context, deploymentID string) error {
	_, err := s.q(ctx).Exec(ctx, `UPDATE deployments SET image='', pushed_image='' WHERE id=$1`, deploymentID)
	return err
}
