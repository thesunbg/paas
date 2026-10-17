package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

// provisionPrisma gives a Prisma app the database its schema expects when the
// connection variable is not set yet: SQLite gets a file on a persistent
// volume, PostgreSQL / MySQL / MongoDB a managed database linked under that
// variable. The generated start command applies migrations (or db push).
func (s *Server) provisionPrisma(ctx context.Context, svc *store.Service, d *store.Deployment, info *proto.Prisma) {
	envName := firstNonEmpty(info.URLEnv, "DATABASE_URL")
	env, err := s.userEnv(ctx, svc)
	if err != nil {
		return
	}
	if _, set := env[envName]; set {
		return
	}
	switch strings.ToLower(info.Provider) {
	case "sqlite":
		hasData := false
		for _, v := range svc.Spec.Volumes {
			if v.Path == "/data" {
				hasData = true
			}
		}
		if !hasData {
			fresh, err := s.store.UpdateService(ctx, svc.ID, func(f *store.Service) error {
				for _, v := range f.Spec.Volumes {
					if v.Path == "/data" {
						return nil
					}
				}
				f.Spec.Volumes = append(f.Spec.Volumes, model.Volume{Name: "data", Path: "/data"})
				return nil
			})
			if err != nil {
				s.hub.Log(d.ID, "==> prisma: could not add a volume: "+err.Error())
				return
			}
			*svc = *fresh
		}
		url := "file:/data/" + util.Slugify(svc.Slug) + ".db"
		if err := s.store.SetEnvVar(ctx, svc.ID, envName, s.box.MustEncrypt(url)); err != nil {
			return
		}
		s.hub.Log(d.ID, fmt.Sprintf("==> prisma (sqlite): %s=%s on a persistent volume mounted at /data", envName, url))
	case "postgresql", "postgres", "mysql", "mongodb":
		engine := map[string]string{"postgresql": "postgres", "postgres": "postgres", "mysql": "mysql", "mongodb": "mongo"}[strings.ToLower(info.Provider)]
		mode := "shared"
		if engine == "mongo" {
			mode = "dedicated"
		}
		proj, err := s.store.ProjectByID(ctx, svc.ProjectID)
		if err != nil {
			return
		}
		s.hub.Log(d.ID, fmt.Sprintf("==> prisma (%s): creating a %s database for %s…", info.Provider, mode, envName))
		db, err := s.createDatabase(ctx, proj, CreateDatabaseInput{Name: svc.Slug + "-db", Engine: engine, Mode: mode, Size: "micro", Link: []string{svc.ID}, EnvName: envName})
		if err != nil {
			s.hub.Log(d.ID, "==> prisma: could not create the database: "+err.Error()+" (set "+envName+" yourself)")
			return
		}
		s.waitDatabasesReady(ctx, []string{db.ID}, 10*time.Minute)
		if fresh, err := s.store.DatabaseByID(ctx, db.ID); err == nil && fresh.Status == "ready" {
			s.hub.Log(d.ID, fmt.Sprintf("==> prisma: database %s ready and linked as %s", fresh.Name, envName))
		} else {
			s.hub.Log(d.ID, "==> prisma: the database is not ready yet; the app may restart until it is")
		}
	default:
		s.hub.Log(d.ID, fmt.Sprintf("==> prisma: provider %q is not managed by the platform; set %s yourself", info.Provider, envName))
	}
	slog.Info("prisma database provisioned", "service", svc.Slug, "provider", info.Provider)
}
