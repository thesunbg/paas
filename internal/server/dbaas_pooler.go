package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/thesunbg/paas/internal/databases"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

// PgBouncer in front of a PostgreSQL database. It runs next to the database
// server (same project network) and listens on 5432, so tenants reach a pooler
// of a shared database through the same firewall rule as the server itself.
// Linked services get <ENV>_POOLED in addition to their direct URL.

const poolerImage = "docker.io/edoburu/pgbouncer:latest"

func (s *Server) poolerURL(ctx context.Context, db *store.Database) (string, error) {
	if !db.Options.Pooler || db.Options.PoolerServiceID == "" {
		return "", errors.New("no pooler")
	}
	pool, err := s.store.ServiceByID(ctx, db.Options.PoolerServiceID)
	if err != nil {
		return "", err
	}
	proj, err := s.store.ProjectByID(ctx, pool.ProjectID)
	if err != nil {
		return "", err
	}
	c, _, err := s.dbCreds(ctx, db)
	if err != nil {
		return "", err
	}
	c.Host, c.Port = internalHost(pool.Slug, proj.Slug), 5432
	return databases.URL(db.Engine, c), nil
}

func (s *Server) createPooler(ctx context.Context, db *store.Database) (*store.Service, error) {
	c, srv, err := s.dbCreds(ctx, db)
	if err != nil {
		return nil, err
	}
	preset, _ := databases.PresetByName(db.SizePreset)
	serverConns := preset.MaxConns
	if db.Mode == "shared" {
		serverConns = preset.MaxConns/2 + 5
	}
	poolSize := max(2, min(20, serverConns-5))
	spec := model.ServiceSpec{
		Port: 5432, Replicas: 1, Image: poolerImage,
		Resources: model.Resources{MemoryLimitMB: 64, MemoryRequestMB: 32, CPULimitMillis: 500, CPURequestMillis: 50},
		QoS:       model.QoSBurstable, HealthCheck: model.HealthCheck{Type: "tcp", TimeoutSec: 60},
		Env: map[string]string{
			"DB_HOST": c.Host, "DB_PORT": strconv.Itoa(c.Port), "DB_USER": c.Username, "DB_PASSWORD": c.Password, "DB_NAME": c.DBName,
			"LISTEN_PORT": "5432", "AUTH_TYPE": "scram-sha-256", "POOL_MODE": "transaction",
			"MAX_CLIENT_CONN": "1000", "DEFAULT_POOL_SIZE": strconv.Itoa(poolSize), "MAX_PREPARED_STATEMENTS": "200",
			"SERVER_RESET_QUERY": "DISCARD ALL", "IGNORE_STARTUP_PARAMETERS": "extra_float_digits,options",
		},
		Language: "pgbouncer",
	}
	slug := "pgbouncer-" + util.Slugify(db.DBName)
	if len(slug) > 50 {
		slug = slug[:50]
	}
	pool, err := s.store.CreateService(ctx, &store.Service{
		ProjectID: srv.ProjectID, Name: db.Name + " (pgbouncer)", Slug: slug, Type: model.TypeDatabase,
		Source: model.ServiceSource{Kind: "image", Image: poolerImage}, Spec: spec, WebhookSecret: util.RandomToken(16),
	})
	if err != nil {
		return nil, err
	}
	if _, err := s.QueueDeployment(ctx, pool, "system", "", "", nil); err != nil {
		return nil, err
	}
	return pool, s.waitServiceRunning(ctx, pool.ID, 5*time.Minute)
}

func (s *Server) removePooler(ctx context.Context, db *store.Database) {
	if db.Options.PoolerServiceID == "" {
		return
	}
	if pool, err := s.store.ServiceByID(ctx, db.Options.PoolerServiceID); err == nil {
		if err := s.deleteService(ctx, pool); err != nil {
			slog.Warn("delete pooler", "db", db.Name, "err", err)
		}
	}
}

// POST /api/v1/databases/{id}/pooler {enabled}
func (s *Server) handleDatabasePooler(w http.ResponseWriter, r *http.Request) {
	db, p := s.databaseFor(w, r, "admin")
	if db == nil {
		return
	}
	if db.Engine != databases.Postgres {
		writeError(w, 400, "connection pooling is available for PostgreSQL")
		return
	}
	if db.Status != "ready" {
		writeError(w, 409, "the database is not ready")
		return
	}
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	ctx := context.WithoutCancel(r.Context())
	unlock := s.lock("db-" + db.ID)
	defer unlock()
	db, _ = s.store.DatabaseByID(ctx, db.ID)
	o := db.Options
	if in.Enabled && !o.Pooler {
		pool, err := s.createPooler(ctx, db)
		if err != nil {
			if pool != nil {
				_ = s.deleteService(ctx, pool)
			}
			writeError(w, 500, fmt.Sprintf("start pgbouncer: %v", err))
			return
		}
		o.Pooler, o.PoolerServiceID = true, pool.ID
	} else if !in.Enabled && o.Pooler {
		s.removePooler(ctx, db)
		o.Pooler, o.PoolerServiceID = false, ""
	}
	if err := s.store.UpdateDatabaseOptions(ctx, db.ID, o); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	u := currentUser(r)
	s.store.Audit(ctx, &u.ID, &p.TeamID, "database.pooler", fmt.Sprintf("%s %v", db.Name, in.Enabled))
	go s.restartLinked(context.Background(), db.ID)
	writeJSON(w, 200, map[string]any{"pooler": o.Pooler})
}
