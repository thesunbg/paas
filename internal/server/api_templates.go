package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/thesunbg/paas/internal/databases"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/resources"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/templates"
	"github.com/thesunbg/paas/internal/util"
)

func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, templates.All)
}

// handleInstallTemplate creates a template's databases and services in a project.
func (s *Server) handleInstallTemplate(w http.ResponseWriter, r *http.Request) {
	p := s.projectFor(w, r, r.PathValue("id"), "developer")
	if p == nil {
		return
	}
	tpl, ok := templates.ByID(r.PathValue("template"))
	if !ok {
		writeError(w, 404, "unknown template")
		return
	}
	var in struct {
		Domain string `json:"domain"`
	}
	_ = decode(r, &in)
	ctx := context.WithoutCancel(r.Context())
	dbValues := map[string]templates.DBValues{}
	var createdDBs []*store.Database
	for _, tdb := range tpl.Databases {
		db, err := s.createDatabase(ctx, p, CreateDatabaseInput{Name: tdb.Name, Engine: tdb.Engine, Mode: tdb.Mode})
		if err != nil {
			writeError(w, 400, fmt.Sprintf("database %s: %v", tdb.Name, err))
			return
		}
		createdDBs = append(createdDBs, db)
		pass, _ := s.box.Decrypt(db.PasswordEnc)
		host := internalHost("shared-"+db.Engine, systemProjectSlug)
		if db.Mode == "dedicated" {
			host = internalHost(util.Slugify(db.Name), p.Slug)
		}
		port := databases.Engines[db.Engine].Port
		c := databases.Credentials{Host: host, Port: port, DBName: db.DBName, Username: db.Username, Password: pass}
		dbValues[tdb.Name] = templates.DBValues{Host: host, Port: strconv.Itoa(port), User: db.Username, Password: pass, Name: db.DBName, URL: databases.URL(db.Engine, c)}
	}
	var dbIDs []string
	for _, d := range createdDBs {
		dbIDs = append(dbIDs, d.ID)
	}
	created, err := s.createTemplateServices(ctx, p, tpl.Services, dbValues, in.Domain, dbIDs...)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	u := currentUser(r)
	s.store.Audit(ctx, &u.ID, &p.TeamID, "template.install", tpl.ID)
	writeJSON(w, 201, map[string]any{"services": created, "databases": createdDBs})
}

// handleImportCompose creates image services from a docker-compose.yml.
func (s *Server) handleImportCompose(w http.ResponseWriter, r *http.Request) {
	p := s.projectFor(w, r, r.PathValue("id"), "developer")
	if p == nil {
		return
	}
	var in struct {
		Compose string `json:"compose"`
	}
	if err := decode(r, &in); err != nil || in.Compose == "" {
		writeError(w, 400, "compose (the YAML content) is required")
		return
	}
	svcs, warnings, err := templates.ParseCompose([]byte(in.Compose))
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	created, err := s.createTemplateServices(context.WithoutCancel(r.Context()), p, svcs, nil, "")
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if warnings == nil {
		warnings = []string{}
	}
	writeJSON(w, 201, map[string]any{"services": created, "warnings": warnings})
}

func (s *Server) createTemplateServices(ctx context.Context, p *store.Project, list []templates.Service, dbs map[string]templates.DBValues, customDomain string, waitDBs ...string) ([]*store.Service, error) {
	var out []*store.Service
	for i, ts := range list {
		slug := util.Slugify(ts.Name)
		host := s.generatedDomain(slug, p.Slug)
		if i == 0 && customDomain != "" {
			host = customDomain
		}
		publicURL := s.urlFor(host)
		typ := model.TypeWeb
		if ts.Type == "worker" || ts.Port == 0 {
			typ = model.TypeWorker
		}
		spec := model.ServiceSpec{Port: ts.Port, Replicas: 1, Image: ts.Image, Volumes: ts.Volumes, Command: ts.Command}
		if ts.Health != "" {
			spec.HealthCheck = model.HealthCheck{Type: "http", Path: ts.Health, TimeoutSec: 180}
		} else {
			spec.HealthCheck = model.HealthCheck{TimeoutSec: 180}
		}
		size, ok := resources.SizeByName(firstNonEmpty(ts.Size, "small"))
		if !ok {
			size, _ = resources.SizeByName("small")
		}
		resources.Apply(&spec, size, model.QoSBurstable)
		if err := resources.Normalize(typ, &spec); err != nil {
			return out, fmt.Errorf("%s: %w", ts.Name, err)
		}
		svc, err := s.store.CreateService(ctx, &store.Service{
			ProjectID: p.ID, Name: ts.Name, Slug: slug, Type: typ,
			Source: model.ServiceSource{Kind: "image", Image: ts.Image}, Spec: spec, WebhookSecret: util.RandomToken(20),
		})
		if err != nil {
			if store.IsUniqueViolation(err) {
				return out, fmt.Errorf("a service named %q already exists in this project", slug)
			}
			return out, err
		}
		for k, v := range ts.Env {
			_ = s.store.SetEnvVar(ctx, svc.ID, k, s.box.MustEncrypt(templates.Expand(v, dbs, publicURL, host)))
		}
		if typ == model.TypeWeb {
			if _, err := s.store.AddDomain(ctx, svc.ID, host, customDomain == "" || i > 0); err != nil {
				_, _ = s.store.AddDomain(ctx, svc.ID, s.generatedDomain(slug+"-"+util.RandomToken(2), p.Slug), true)
			}
		}
		out = append(out, svc)
		svcCopy := svc
		go func() {
			// Deploy once the template's databases accept connections.
			s.waitDatabasesReady(context.Background(), waitDBs, 10*time.Minute)
			_, _ = s.QueueDeployment(context.Background(), svcCopy, "template", "", "", nil)
		}()
	}
	return out, nil
}

func (s *Server) waitDatabasesReady(ctx context.Context, ids []string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for _, id := range ids {
		for time.Now().Before(deadline) {
			db, err := s.store.DatabaseByID(ctx, id)
			if err != nil || db.Status == "ready" || db.Status == "failed" {
				break
			}
			time.Sleep(2 * time.Second)
		}
	}
}
