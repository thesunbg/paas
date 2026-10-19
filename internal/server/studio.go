package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/thesunbg/paas/internal/databases"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proxy"
	"github.com/thesunbg/paas/internal/resources"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

// The DB studio is dbweb (github.com/thesunbg/dbweb, embedded mode) run for a
// project: one hidden web service next to the project's databases, asleep
// when unused. dbweb has no login, so its route is gated (proxy/gate.go): the
// dashboard hands developers a short-lived ticket, and the edge adds the
// token dbweb requires to every request it forwards. Other containers on the
// project network do not have it.

const (
	roleStudio        = "studio"
	studioSlug        = "dbweb-studio"
	studioPort        = 4317
	studioTokenHeader = "X-Dbweb-Token"
	settingStudioKey  = "studio_key"
)

func hiddenService(sv *store.Service) bool {
	return sv.Type == model.TypeDatabase || sv.Spec.Role != ""
}

func (s *Server) studioKey(ctx context.Context) (string, error) {
	enc, err := s.store.GetSetting(ctx, settingStudioKey)
	if errors.Is(err, store.ErrNotFound) {
		if err := s.store.SetSetting(ctx, settingStudioKey, s.box.MustEncrypt(util.RandomToken(32))); err != nil {
			return "", err
		}
		enc, err = s.store.GetSetting(ctx, settingStudioKey)
	}
	if err != nil {
		return "", err
	}
	return s.box.Decrypt(enc)
}

// studioSecret derives a per-studio secret (gate key or dbweb token).
func studioSecret(key, purpose, serviceID string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(purpose + "|" + serviceID))
	return hex.EncodeToString(mac.Sum(nil))
}

// studioGate is the route gate of a studio service.
func studioGate(key, serviceID string) *model.RouteGate {
	return &model.RouteGate{Key: studioSecret(key, "gate", serviceID), Header: studioTokenHeader, Value: studioSecret(key, "embed", serviceID)}
}

// studioHost is dbweb--<project>.<base>: no generated service domain
// (<service>-<project>) can take that form, since slugs never start with "-",
// and the "--" sits past the 4th character, which IDNA reserves.
func (s *Server) studioHost(p *store.Project) string {
	return fmt.Sprintf("dbweb--%s.%s", p.Slug, s.cfg.BaseDomain)
}

// studioConnection is one entry of dbweb's DBWEB_CONNECTIONS.
type studioConnection struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Kind     string         `json:"kind"`
	Host     string         `json:"host"`
	Port     int            `json:"port"`
	Username string         `json:"username,omitempty"`
	Password string         `json:"password,omitempty"`
	Database string         `json:"database,omitempty"`
	Group    string         `json:"group,omitempty"`
	Options  map[string]any `json:"options,omitempty"`
}

var studioKinds = map[string]string{databases.Postgres: "postgres", databases.MySQL: "mysql", databases.Mongo: "mongodb", databases.Redis: "redis"}

// studioConnections lists the project's databases for dbweb, with the
// credentials linked services use.
func (s *Server) studioConnections(ctx context.Context, p *store.Project) (string, error) {
	dbs, err := s.store.DatabasesForProject(ctx, p.ID)
	if err != nil {
		return "", err
	}
	out := []studioConnection{}
	for i := range dbs {
		db := &dbs[i]
		kind, ok := studioKinds[db.Engine]
		if !ok || db.ServerServiceID == nil {
			continue
		}
		c, _, err := s.dbCreds(ctx, db)
		if err != nil {
			continue
		}
		sc := studioConnection{ID: "paas-" + db.ID, Name: db.Name, Kind: kind, Host: c.Host, Port: c.Port, Username: c.Username, Password: c.Password, Database: c.DBName, Group: p.Name}
		switch db.Engine {
		case databases.Mongo:
			sc.Options = map[string]any{"authSource": "admin"}
		case databases.Redis:
			sc.Username, sc.Database = "", ""
		}
		out = append(out, sc)
	}
	b, err := json.Marshal(out)
	return string(b), err
}

// ensureStudio creates the project's studio or brings its configuration up to
// date, and deploys or restarts it when needed.
func (s *Server) ensureStudio(ctx context.Context, p *store.Project) (*store.Service, error) {
	unlock := s.lock("studio-" + p.ID)
	defer unlock()
	key, err := s.studioKey(ctx)
	if err != nil {
		return nil, err
	}
	svcs, err := s.store.ServicesForProject(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	var svc *store.Service
	for i := range svcs {
		if svcs[i].Spec.Role == roleStudio {
			svc = &svcs[i]
		}
	}
	created := false
	if svc == nil {
		spec := model.ServiceSpec{
			Port: studioPort, Replicas: 1, Role: roleStudio,
			HealthCheck: model.HealthCheck{Type: "http", Path: "/api/health", TimeoutSec: 180},
			Sleep:       model.SleepPolicy{Enabled: true, IdleMinutes: 20},
			Volumes:     []model.Volume{{Name: "data", Path: "/data"}},
		}
		size, _ := resources.SizeByName("small")
		resources.Apply(&spec, size, model.QoSBurstable)
		if err := resources.Normalize(model.TypeWeb, &spec); err != nil {
			return nil, err
		}
		svc, err = s.store.CreateService(ctx, &store.Service{
			ProjectID: p.ID, Name: "DB studio", Slug: studioSlug, Type: model.TypeWeb,
			Source:        model.ServiceSource{Kind: "git", Repo: s.cfg.StudioRepo, Branch: s.cfg.StudioBranch},
			Build:         model.BuildConfig{Builder: "dockerfile"},
			Spec:          spec,
			WebhookSecret: util.RandomToken(20),
		})
		if err != nil {
			if store.IsUniqueViolation(err) {
				return nil, fmt.Errorf("a service named %q already exists in this project; rename it to use the DB studio", studioSlug)
			}
			return nil, err
		}
		if _, err := s.store.AddDomain(ctx, svc.ID, s.studioHost(p), true); err != nil {
			return nil, err
		}
		created = true
	}
	conns, err := s.studioConnections(ctx, p)
	if err != nil {
		return nil, err
	}
	want := map[string]string{"DBWEB_EMBED_TOKEN": studioSecret(key, "embed", svc.ID), "DBWEB_CONNECTIONS": conns}
	have := map[string]string{}
	if vars, err := s.store.EnvVars(ctx, svc.ID); err == nil {
		for _, v := range vars {
			if plain, err := s.box.Decrypt(v.ValueEnc); err == nil {
				have[v.Key] = plain
			}
		}
	}
	changed := false
	for k, v := range want {
		if have[k] != v {
			if err := s.store.SetEnvVar(ctx, svc.ID, k, s.box.MustEncrypt(v)); err != nil {
				return nil, err
			}
			changed = true
		}
	}
	if created || svc.CurrentDeploymentID == nil {
		if !s.deployInFlight(ctx, svc.ID) {
			if _, err := s.QueueDeployment(ctx, svc, "studio", "", "", nil); err != nil {
				return nil, err
			}
		}
	} else if changed && svc.Status == model.StatusRunning {
		// Sleeping studios pick the new list up when they wake.
		if _, err := s.restartService(ctx, svc, nil); err != nil {
			return nil, err
		}
	}
	s.publishRoutes(context.WithoutCancel(ctx))
	return s.store.ServiceByID(ctx, svc.ID)
}

func (s *Server) deployInFlight(ctx context.Context, serviceID string) bool {
	deps, err := s.store.DeploymentsForService(ctx, serviceID, 3)
	if err != nil {
		return false
	}
	for _, d := range deps {
		switch d.Status {
		case model.DeployQueued, model.DeployBuilding, model.DeployDeploying:
			return true
		}
	}
	return false
}

// POST /api/v1/projects/{id}/studio {database_id}
// Opens the DB studio: returns its URL with a ticket once it runs, or its
// state while it builds or starts (the dashboard polls).
func (s *Server) handleStudio(w http.ResponseWriter, r *http.Request) {
	p := s.projectFor(w, r, r.PathValue("id"), "developer")
	if p == nil {
		return
	}
	var in struct {
		DatabaseID string `json:"database_id"`
	}
	_ = decode(r, &in)
	ctx := r.Context()
	svc, err := s.ensureStudio(ctx, p)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	out := map[string]any{"service_id": svc.ID, "status": "starting"}
	deps, _ := s.store.DeploymentsForService(ctx, svc.ID, 1)
	if len(deps) > 0 {
		d := deps[0]
		out["deployment"] = map[string]any{"id": d.ID, "status": d.Status, "error": d.Error}
		if d.Status == model.DeployQueued || d.Status == model.DeployBuilding {
			out["status"] = "building"
		}
		if d.Status == model.DeployFailed && svc.CurrentDeploymentID == nil {
			out["status"] = "failed"
		}
	}
	if svc.CurrentDeploymentID != nil && (svc.Status == model.StatusRunning || svc.Status == model.StatusSleeping) {
		key, err := s.studioKey(ctx)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		host := s.studioHost(p)
		u := url.URL{Scheme: "http", Host: host, Path: "/"}
		if s.tlsAtEdge() {
			u.Scheme = "https"
		}
		u.RawQuery = url.Values{proxy.GateTicketParam: {proxy.GateTicket(studioGate(key, svc.ID).Key, host, time.Now())}}.Encode()
		if in.DatabaseID != "" {
			u.Fragment = "connection=paas-" + in.DatabaseID
		}
		out["status"], out["url"] = "ready", u.String()
		u2 := currentUser(r)
		s.store.Audit(ctx, &u2.ID, &p.TeamID, "studio.open", p.Slug)
	}
	writeJSON(w, 200, out)
}
