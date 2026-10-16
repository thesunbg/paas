package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/databases"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
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
	if err := s.checkCounts(r, p.TeamID, 0, len(tpl.Services), len(tpl.Databases)); err != nil {
		writeError(w, 403, err.Error())
		return
	}
	var in struct {
		Domain string `json:"domain"`
	}
	_ = decode(r, &in)
	ctx := context.WithoutCancel(r.Context())
	created, dbs, err := s.installPlan(ctx, p, tpl.Services, tpl.Databases, templateOpts{Domain: in.Domain})
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	u := currentUser(r)
	s.store.Audit(ctx, &u.ID, &p.TeamID, "template.install", tpl.ID)
	writeJSON(w, 201, map[string]any{"services": created, "databases": dbs})
}

// handleImportCompose creates services and managed databases from a pasted docker-compose.yml.
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
	plan, err := templates.ParseCompose([]byte(in.Compose), templates.ComposeOptions{})
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if err := s.checkCounts(r, p.TeamID, 0, len(plan.Services), len(plan.Databases)); err != nil {
		writeError(w, 403, err.Error())
		return
	}
	created, dbs, err := s.installPlan(context.WithoutCancel(r.Context()), p, plan.Services, plan.Databases, templateOpts{})
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"services": created, "databases": dbs, "warnings": plan.Warnings})
}

// handleImportRepo creates every service of a repository's docker-compose file:
// services with a build section are built from the repository, database
// containers become managed databases linked to the services that use them.
// POST /api/v1/projects/{id}/import-repo {repo, branch, installation_id, git_token, compose_path, domain}
func (s *Server) handleImportRepo(w http.ResponseWriter, r *http.Request) {
	p := s.projectFor(w, r, r.PathValue("id"), "developer")
	if p == nil {
		return
	}
	var in struct {
		Repo           string `json:"repo"`
		Branch         string `json:"branch"`
		InstallationID int64  `json:"installation_id"`
		GitToken       string `json:"git_token"`
		ComposePath    string `json:"compose_path"`
		Domain         string `json:"domain"`
		// Env holds the values the user filled in, by service: {"backend": {"OPENAI_API_KEY": "…"}}.
		Env map[string]map[string]string `json:"env"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if in.Repo == "" {
		writeError(w, 400, "repo is required")
		return
	}
	if in.Branch == "" {
		in.Branch = "main"
	}
	if in.Domain != "" {
		if err := validHostname(in.Domain); err != nil {
			writeError(w, 400, err.Error())
			return
		}
	}
	rep, status, err := s.inspectRepo(r.Context(), p, proto.DetectRequest{Repo: in.Repo, Branch: in.Branch, GitToken: in.GitToken, ComposePath: in.ComposePath}, in.InstallationID)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	if rep.Compose == nil {
		writeError(w, 422, "no docker-compose file with services built from the repository was found")
		return
	}
	dir := path.Dir(rep.Compose.Path)
	plan, err := templates.ParseCompose([]byte(rep.Compose.Content), templates.ComposeOptions{Dir: dir, Repo: true, Files: rep.Compose.Files})
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	for i := range plan.Services {
		svc := &plan.Services[i]
		for k, v := range in.Env[svc.Name] {
			if !envKeyRe.MatchString(k) {
				writeError(w, 400, "invalid environment variable name "+k)
				return
			}
			if v != "" {
				svc.Env[k] = v
			}
		}
		var unset []string
		for _, i := range svc.Inputs {
			if _, set := svc.Env[i.Key]; i.Required && !set {
				unset = append(unset, i.Key)
			}
		}
		if len(unset) > 0 {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: %s not set yet (add in the service's Environment)", svc.Name, strings.Join(unset, ", ")))
		}
	}
	if err := s.checkCounts(r, p.TeamID, 0, len(plan.Services), len(plan.Databases)); err != nil {
		writeError(w, 403, err.Error())
		return
	}
	src := &model.ServiceSource{Kind: "git", Repo: in.Repo, Branch: in.Branch, AutoDeploy: true, InstallationID: in.InstallationID}
	opts := templateOpts{Domain: in.Domain, Repo: src}
	if in.GitToken != "" {
		enc, err := s.box.Encrypt(in.GitToken)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		opts.GitTokenEnc = enc
	}
	ctx := context.WithoutCancel(r.Context())
	created, dbs, err := s.installPlan(ctx, p, plan.Services, plan.Databases, opts)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	u := currentUser(r)
	s.store.Audit(ctx, &u.ID, &p.TeamID, "repo.import", in.Repo+" "+rep.Compose.Path)
	writeJSON(w, 201, map[string]any{"services": created, "databases": dbs, "warnings": plan.Warnings, "compose_path": rep.Compose.Path})
}

type templateOpts struct {
	Domain      string               // custom domain of the first public service
	Repo        *model.ServiceSource // repository of services with a build section
	GitTokenEnc string
}

// installPlan creates databases and then services, rejecting name clashes up front.
func (s *Server) installPlan(ctx context.Context, p *store.Project, svcs []templates.Service, tdbs []templates.Database, opts templateOpts) ([]*store.Service, []*store.Database, error) {
	existing, err := s.store.ServicesForProject(ctx, p.ID)
	if err != nil {
		return nil, nil, err
	}
	taken := map[string]bool{}
	for _, e := range existing {
		taken[e.Slug] = true
	}
	if dbs, err := s.store.DatabasesForProject(ctx, p.ID); err == nil {
		for _, d := range dbs {
			taken["db:"+d.Name] = true
		}
	}
	for _, ts := range svcs {
		if taken[util.Slugify(ts.Name)] {
			return nil, nil, fmt.Errorf("a service named %q already exists in this project", util.Slugify(ts.Name))
		}
	}
	for _, tdb := range tdbs {
		if taken["db:"+util.Slugify(tdb.Name)] {
			return nil, nil, fmt.Errorf("a database named %q already exists in this project", util.Slugify(tdb.Name))
		}
	}
	dbValues := map[string]templates.DBValues{}
	var createdDBs []*store.Database
	var dbIDs []string
	for _, tdb := range tdbs {
		version := tdb.Version
		if _, err := databases.ImageFor(tdb.Engine, version); err != nil {
			version = "" // not offered: the engine's default
		}
		db, err := s.createDatabase(ctx, p, CreateDatabaseInput{Name: tdb.Name, Engine: tdb.Engine, Mode: tdb.Mode, Version: version})
		if err != nil {
			return nil, createdDBs, fmt.Errorf("database %s: %v", tdb.Name, err)
		}
		createdDBs = append(createdDBs, db)
		dbIDs = append(dbIDs, db.ID)
		dbValues[tdb.Name] = s.templateDBValues(p, db)
	}
	created, err := s.createTemplateServices(ctx, p, svcs, dbValues, opts, dbIDs...)
	return created, createdDBs, err
}

func (s *Server) templateDBValues(p *store.Project, db *store.Database) templates.DBValues {
	pass, _ := s.box.Decrypt(db.PasswordEnc)
	host := internalHost("shared-"+db.Engine, systemProjectSlug)
	if db.Mode == "dedicated" {
		host = internalHost(util.Slugify(db.Name), p.Slug)
	}
	port := databases.Engines[db.Engine].Port
	c := databases.Credentials{Host: host, Port: port, DBName: db.DBName, Username: db.Username, Password: pass}
	return templates.DBValues{ID: db.ID, Host: host, Port: strconv.Itoa(port), User: db.Username, Password: pass, Name: db.DBName, URL: databases.URL(db.Engine, c)}
}

func (s *Server) createTemplateServices(ctx context.Context, p *store.Project, list []templates.Service, dbs map[string]templates.DBValues, opts templateOpts, waitDBs ...string) ([]*store.Service, error) {
	// Hosts first: services reference each other's public URLs (${svc.<name>.url}).
	hosts := map[string]string{}
	urls := map[string]string{}
	first := true
	for _, ts := range list {
		if ts.Private || ts.Type == "worker" || ts.Port == 0 {
			continue
		}
		host := s.generatedDomain(util.Slugify(ts.Name), p.Slug)
		if first && opts.Domain != "" {
			host = opts.Domain
		}
		first = false
		hosts[ts.Name], urls[ts.Name] = host, s.urlFor(host)
	}
	var out []*store.Service
	for _, ts := range list {
		slug := util.Slugify(ts.Name)
		host := hosts[ts.Name]
		typ := model.TypeWeb
		if ts.Type == "worker" || ts.Port == 0 {
			typ = model.TypeWorker
		}
		spec := model.ServiceSpec{Port: ts.Port, Replicas: 1, Image: ts.Image, Volumes: ts.Volumes, Command: ts.Command}
		for _, d := range ts.DependsOn {
			spec.DependsOn = append(spec.DependsOn, util.Slugify(d))
		}
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
		svc := &store.Service{
			ProjectID: p.ID, Name: ts.Name, Slug: slug, Type: typ,
			Source: model.ServiceSource{Kind: "image", Image: ts.Image}, Spec: spec, WebhookSecret: util.RandomToken(20),
		}
		if ts.Build != nil {
			if opts.Repo == nil {
				return out, fmt.Errorf("%s is built from source and needs a repository", ts.Name)
			}
			svc.Source = *opts.Repo
			svc.Source.RootDir, svc.Source.Dockerfile = ts.Build.RootDir, ts.Build.Dockerfile
			svc.Build = model.BuildConfig{Builder: "dockerfile"}
			svc.GitTokenEnc = opts.GitTokenEnc
			svc.Spec.Image = ""
		}
		svc, err := s.store.CreateService(ctx, svc)
		if err != nil {
			if store.IsUniqueViolation(err) {
				return out, fmt.Errorf("a service named %q already exists in this project", slug)
			}
			return out, err
		}
		for k, v := range ts.Env {
			_ = s.store.SetEnvVar(ctx, svc.ID, k, s.box.MustEncrypt(templates.ExpandWith(v, dbs, urls, urls[ts.Name], host)))
		}
		for env, name := range ts.Links {
			if d, ok := dbs[name]; ok {
				_ = s.store.LinkDatabase(ctx, svc.ID, d.ID, env)
			}
		}
		if host != "" {
			if _, err := s.store.AddDomain(ctx, svc.ID, host, opts.Domain == "" || host != opts.Domain); err != nil {
				_, _ = s.store.AddDomain(ctx, svc.ID, s.generatedDomain(slug+"-"+util.RandomToken(2), p.Slug), true)
			}
		}
		out = append(out, svc)
		svcCopy := svc
		go func() {
			// Deploy once the databases accept connections and the services it
			// depends on run (their names must resolve when it starts).
			ctx := context.Background()
			s.waitDatabasesReady(ctx, waitDBs, 10*time.Minute)
			s.waitServicesUp(ctx, p.ID, svcCopy.Spec.DependsOn, 20*time.Minute)
			_, _ = s.QueueDeployment(ctx, svcCopy, "template", "", "", nil)
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

// inspectRepo clones a repository on a build node and analyzes it.
func (s *Server) inspectRepo(ctx context.Context, p *store.Project, req proto.DetectRequest, installationID int64) (*proto.DetectReply, int, error) {
	if installationID != 0 {
		if err := s.checkInstallation(ctx, p.TeamID, installationID); err != nil {
			return nil, 400, err
		}
		tok, err := s.gitToken(ctx, &store.Service{Source: model.ServiceSource{InstallationID: installationID}})
		if err != nil {
			return nil, 502, err
		}
		req.GitToken = tok
	}
	node, err := s.pickBuildNode(ctx)
	if err != nil {
		return nil, 503, err
	}
	var rep proto.DetectReply
	if err := s.requestAgent(ctx, node.ID, proto.CmdDetect, req, &rep, 3*time.Minute); err != nil {
		return nil, 502, err
	}
	if rep.Error != "" {
		return &rep, 422, errors.New(rep.Error)
	}
	return &rep, 200, nil
}

// handleDetect inspects a repository on a build node so the UI can preselect type and port.
func (s *Server) handleDetect(w http.ResponseWriter, r *http.Request) {
	p := s.projectFor(w, r, r.PathValue("id"), "developer")
	if p == nil {
		return
	}
	var in struct {
		proto.DetectRequest
		InstallationID int64 `json:"installation_id"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if in.Repo == "" {
		writeError(w, 400, "repo is required")
		return
	}
	if in.Branch == "" {
		in.Branch = "main"
	}
	rep, status, err := s.inspectRepo(r.Context(), p, in.DetectRequest, in.InstallationID)
	if err != nil {
		if rep != nil {
			writeJSON(w, status, rep)
			return
		}
		writeError(w, status, err.Error())
		return
	}
	out := struct {
		*proto.DetectReply
		ComposePlan *composePreview `json:"compose_plan,omitempty"`
	}{DetectReply: rep}
	if rep.Compose != nil {
		if plan, err := templates.ParseCompose([]byte(rep.Compose.Content), templates.ComposeOptions{Dir: path.Dir(rep.Compose.Path), Repo: true, Files: rep.Compose.Files}); err == nil {
			out.ComposePlan = previewOf(plan)
		}
		// The browser only needs the summary; env files can hold secrets.
		rep.Compose.Content, rep.Compose.Files = "", nil
	}
	writeJSON(w, 200, out)
}

// composePreview is what importing a compose file will create, for the UI to
// show (and collect missing variables) before anything is created.
type composePreview struct {
	Services  []previewService     `json:"services"`
	Databases []templates.Database `json:"databases"`
	Warnings  []string             `json:"warnings"`
}

type previewService struct {
	Name       string               `json:"name"`
	Public     bool                 `json:"public"`
	Port       int                  `json:"port,omitempty"`
	Dockerfile string               `json:"dockerfile,omitempty"`
	Image      string               `json:"image,omitempty"`
	Links      map[string]string    `json:"links,omitempty"`
	DependsOn  []string             `json:"depends_on,omitempty"`
	Inputs     []templates.EnvInput `json:"inputs"`
	EnvCount   int                  `json:"env_count"`
}

func previewOf(plan *templates.ComposePlan) *composePreview {
	out := &composePreview{Services: []previewService{}, Databases: plan.Databases, Warnings: plan.Warnings}
	for _, s := range plan.Services {
		ps := previewService{
			Name: s.Name, Public: !s.Private && s.Port > 0 && s.Type != "worker", Port: s.Port, Image: s.Image,
			Links: s.Links, DependsOn: s.DependsOn, Inputs: s.Inputs, EnvCount: len(s.Env),
		}
		if ps.Inputs == nil {
			ps.Inputs = []templates.EnvInput{}
		}
		if s.Build != nil {
			ps.Dockerfile = path.Join(s.Build.RootDir, firstNonEmpty(s.Build.Dockerfile, "Dockerfile"))
		}
		out.Services = append(out.Services, ps)
	}
	return out
}
