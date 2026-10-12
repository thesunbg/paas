package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thesunbg/paas/internal/model"
)

type Project struct {
	ID        string    `json:"id"`
	TeamID    string    `json:"team_id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	NetIndex  int       `json:"net_index"`
	CreatedAt time.Time `json:"created_at"`
}

const projectCols = `id, team_id, name, slug, net_index, created_at`

func scanProject(row pgx.Row) (*Project, error) {
	var p Project
	if err := row.Scan(&p.ID, &p.TeamID, &p.Name, &p.Slug, &p.NetIndex, &p.CreatedAt); err != nil {
		return nil, notFound(err)
	}
	return &p, nil
}

func (s *Store) CreateProject(ctx context.Context, teamID, name, slug string) (*Project, error) {
	return scanProject(s.DB.QueryRow(ctx, `INSERT INTO projects (team_id, name, slug) VALUES ($1,$2,$3) RETURNING `+projectCols, teamID, name, slug))
}

func (s *Store) ProjectByID(ctx context.Context, id string) (*Project, error) {
	return scanProject(s.DB.QueryRow(ctx, `SELECT `+projectCols+` FROM projects WHERE id=$1`, id))
}

func (s *Store) ProjectBySlug(ctx context.Context, teamID, slug string) (*Project, error) {
	return scanProject(s.DB.QueryRow(ctx, `SELECT `+projectCols+` FROM projects WHERE team_id=$1 AND slug=$2`, teamID, slug))
}

func (s *Store) ProjectsForTeam(ctx context.Context, teamID string) ([]Project, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+projectCols+` FROM projects WHERE team_id=$1 ORDER BY created_at`, teamID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Project, error) {
		p, err := scanProject(r)
		if err != nil {
			return Project{}, err
		}
		return *p, nil
	})
}

func (s *Store) DeleteProject(ctx context.Context, id string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM projects WHERE id=$1`, id)
	return err
}

// Services.

type Service struct {
	ID                  string              `json:"id"`
	ProjectID           string              `json:"project_id"`
	Name                string              `json:"name"`
	Slug                string              `json:"slug"`
	Type                string              `json:"type"`
	Source              model.ServiceSource `json:"source"`
	Build               model.BuildConfig   `json:"build"`
	Spec                model.ServiceSpec   `json:"spec"`
	GitTokenEnc         string              `json:"-"`
	HasGitToken         bool                `json:"has_git_token"`
	WebhookSecret       string              `json:"-"`
	NodeID              *string             `json:"node_id"`
	CurrentDeploymentID *string             `json:"current_deployment_id"`
	Status              string              `json:"status"`
	LastRequestAt       *time.Time          `json:"last_request_at"`
	CreatedAt           time.Time           `json:"created_at"`
	UpdatedAt           time.Time           `json:"updated_at"`
}

const serviceCols = `id, project_id, name, slug, type, source, build, spec, git_token_enc, webhook_secret, node_id, current_deployment_id, status, last_request_at, created_at, updated_at`

func scanService(row pgx.Row) (*Service, error) {
	var s Service
	err := row.Scan(&s.ID, &s.ProjectID, &s.Name, &s.Slug, &s.Type, &s.Source, &s.Build, &s.Spec, &s.GitTokenEnc,
		&s.WebhookSecret, &s.NodeID, &s.CurrentDeploymentID, &s.Status, &s.LastRequestAt, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	s.HasGitToken = s.GitTokenEnc != ""
	return &s, nil
}

func collectServices(rows pgx.Rows, err error) ([]Service, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Service, error) {
		s, err := scanService(r)
		if err != nil {
			return Service{}, err
		}
		return *s, nil
	})
}

func (s *Store) CreateService(ctx context.Context, svc *Service) (*Service, error) {
	return scanService(s.DB.QueryRow(ctx, `INSERT INTO services (project_id, name, slug, type, source, build, spec, git_token_enc, webhook_secret, node_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING `+serviceCols,
		svc.ProjectID, svc.Name, svc.Slug, svc.Type, svc.Source, svc.Build, svc.Spec, svc.GitTokenEnc, svc.WebhookSecret, svc.NodeID))
}

func (s *Store) UpdateServiceConfig(ctx context.Context, svc *Service) error {
	_, err := s.DB.Exec(ctx, `UPDATE services SET name=$2, source=$3, build=$4, spec=$5, git_token_enc=$6, updated_at=now() WHERE id=$1`,
		svc.ID, svc.Name, svc.Source, svc.Build, svc.Spec, svc.GitTokenEnc)
	return err
}

func (s *Store) ServiceByID(ctx context.Context, id string) (*Service, error) {
	return scanService(s.DB.QueryRow(ctx, `SELECT `+serviceCols+` FROM services WHERE id=$1`, id))
}

func (s *Store) ServicesForProject(ctx context.Context, projectID string) ([]Service, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+serviceCols+` FROM services WHERE project_id=$1 ORDER BY created_at`, projectID)
	return collectServices(rows, err)
}

func (s *Store) AllServices(ctx context.Context) ([]Service, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+serviceCols+` FROM services ORDER BY created_at`)
	return collectServices(rows, err)
}

func (s *Store) SetServiceStatus(ctx context.Context, id, status string) error {
	_, err := s.DB.Exec(ctx, `UPDATE services SET status=$2, updated_at=now() WHERE id=$1`, id, status)
	return err
}

func (s *Store) SetServiceCurrentDeployment(ctx context.Context, id, deploymentID, status string) error {
	_, err := s.DB.Exec(ctx, `UPDATE services SET current_deployment_id=$2, status=$3, updated_at=now() WHERE id=$1`, id, deploymentID, status)
	return err
}

func (s *Store) SetServiceNode(ctx context.Context, id string, nodeID *string) error {
	_, err := s.DB.Exec(ctx, `UPDATE services SET node_id=$2 WHERE id=$1`, id, nodeID)
	return err
}

func (s *Store) SetServiceSpec(ctx context.Context, id string, spec model.ServiceSpec) error {
	_, err := s.DB.Exec(ctx, `UPDATE services SET spec=$2, updated_at=now() WHERE id=$1`, id, spec)
	return err
}

func (s *Store) TouchServiceRequest(ctx context.Context, id string, at time.Time) error {
	_, err := s.DB.Exec(ctx, `UPDATE services SET last_request_at=GREATEST(COALESCE(last_request_at, $2), $2) WHERE id=$1`, id, at)
	return err
}

func (s *Store) DeleteService(ctx context.Context, id string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM services WHERE id=$1`, id)
	return err
}

// Env vars.

type EnvVar struct {
	Key      string `json:"key"`
	ValueEnc string `json:"-"`
}

func (s *Store) EnvVars(ctx context.Context, serviceID string) ([]EnvVar, error) {
	rows, err := s.DB.Query(ctx, `SELECT key, value_enc FROM env_vars WHERE service_id=$1 ORDER BY key`, serviceID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EnvVar, error) {
		var e EnvVar
		err := r.Scan(&e.Key, &e.ValueEnc)
		return e, err
	})
}

func (s *Store) SetEnvVar(ctx context.Context, serviceID, key, valueEnc string) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO env_vars (service_id, key, value_enc) VALUES ($1,$2,$3)
		ON CONFLICT (service_id, key) DO UPDATE SET value_enc=EXCLUDED.value_enc`, serviceID, key, valueEnc)
	return err
}

func (s *Store) DeleteEnvVar(ctx context.Context, serviceID, key string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM env_vars WHERE service_id=$1 AND key=$2`, serviceID, key)
	return err
}

// Domains.

type Domain struct {
	ID          string    `json:"id"`
	ServiceID   string    `json:"service_id"`
	Hostname    string    `json:"hostname"`
	IsGenerated bool      `json:"is_generated"`
	CreatedAt   time.Time `json:"created_at"`
}

func (s *Store) AddDomain(ctx context.Context, serviceID, host string, generated bool) (*Domain, error) {
	var d Domain
	err := s.DB.QueryRow(ctx, `INSERT INTO domains (service_id, hostname, is_generated) VALUES ($1, lower($2), $3)
		RETURNING id, service_id, hostname, is_generated, created_at`, serviceID, host, generated).
		Scan(&d.ID, &d.ServiceID, &d.Hostname, &d.IsGenerated, &d.CreatedAt)
	return &d, err
}

func (s *Store) DomainsForService(ctx context.Context, serviceID string) ([]Domain, error) {
	return s.queryDomains(ctx, `WHERE service_id=$1`, serviceID)
}

func (s *Store) AllDomains(ctx context.Context) ([]Domain, error) {
	return s.queryDomains(ctx, ``)
}

func (s *Store) queryDomains(ctx context.Context, where string, args ...any) ([]Domain, error) {
	rows, err := s.DB.Query(ctx, `SELECT id, service_id, hostname, is_generated, created_at FROM domains `+where+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Domain, error) {
		var d Domain
		err := r.Scan(&d.ID, &d.ServiceID, &d.Hostname, &d.IsGenerated, &d.CreatedAt)
		return d, err
	})
}

func (s *Store) DeleteDomain(ctx context.Context, serviceID, id string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM domains WHERE service_id=$1 AND id=$2`, serviceID, id)
	return err
}
