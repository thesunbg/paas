package server

import (
	"context"
	"log/slog"
	"strings"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/store"
)

// Templates write a database's connection details into plain env vars (${db.<name>.host} …,
// expanded once when the template is installed), and such services are not linked to the
// database. A move (shared → dedicated, major version) or a recovery that puts the database on a
// server with another host would leave them pointing at the old one. These helpers find them and
// point them at the new host.

// serverHost is the internal host name clients use to reach a database server.
func (s *Server) serverHost(ctx context.Context, serverID string) (string, error) {
	srv, err := s.store.ServiceByID(ctx, serverID)
	if err != nil {
		return "", err
	}
	proj, err := s.store.ProjectByID(ctx, srv.ProjectID)
	if err != nil {
		return "", err
	}
	if len(srv.Spec.DNSAliases) > 0 {
		return srv.Spec.DNSAliases[0], nil
	}
	return internalHost(srv.Slug, proj.Slug), nil
}

// envDependents returns the services of db's project whose env names both host (the server db
// lives on) and db's user. A service that also names the user of another database on the same
// server (oldServerID) is left out: its host vars cannot be told apart.
func (s *Server) envDependents(ctx context.Context, db *store.Database, host, oldServerID string) []string {
	if host == "" || db.Username == "" {
		return nil
	}
	svcs, err := s.store.ServicesForProject(ctx, db.ProjectID)
	if err != nil {
		return nil
	}
	var others []string
	if oldServerID != "" {
		if dbs, err := s.store.DatabasesOnServer(ctx, oldServerID); err == nil {
			for _, o := range dbs {
				if o.ID != db.ID && o.Username != "" {
					others = append(others, o.Username)
				}
			}
		}
	}
	var ids []string
	for _, svc := range svcs {
		if db.ServerServiceID != nil && svc.ID == *db.ServerServiceID || svc.ID == oldServerID {
			continue
		}
		vars, err := s.store.EnvVars(ctx, svc.ID)
		if err != nil {
			continue
		}
		hasHost, hasUser, ambiguous := false, false, false
		for _, v := range vars {
			plain, err := s.box.Decrypt(v.ValueEnc)
			if err != nil {
				continue
			}
			hasHost = hasHost || strings.Contains(plain, host)
			hasUser = hasUser || strings.Contains(plain, db.Username)
			for _, u := range others {
				ambiguous = ambiguous || strings.Contains(plain, u)
			}
		}
		if hasHost && hasUser {
			if ambiguous {
				slog.Warn("service uses several databases of one server in its env: update its host vars by hand",
					"service", svc.Slug, "db", db.Name, "host", host)
				continue
			}
			ids = append(ids, svc.ID)
		}
	}
	return ids
}

// retargetEnv replaces oldHost with newHost in the env vars of the services ids, and restarts
// those that run unless they are in skip (the caller starts them itself). It returns the
// services whose env changed.
func (s *Server) retargetEnv(ctx context.Context, ids []string, oldHost, newHost string, skip map[string]bool) []string {
	if oldHost == "" || newHost == "" || oldHost == newHost {
		return nil
	}
	var changed []string
	for _, id := range ids {
		vars, err := s.store.EnvVars(ctx, id)
		if err != nil {
			continue
		}
		n := 0
		for _, v := range vars {
			plain, err := s.box.Decrypt(v.ValueEnc)
			if err != nil || !strings.Contains(plain, oldHost) {
				continue
			}
			if err := s.store.SetEnvVar(ctx, id, v.Key, s.box.MustEncrypt(strings.ReplaceAll(plain, oldHost, newHost))); err != nil {
				slog.Warn("retarget env var", "service", id, "key", v.Key, "err", err)
				continue
			}
			n++
		}
		if n == 0 {
			continue
		}
		changed = append(changed, id)
		svc, err := s.store.ServiceByID(ctx, id)
		if err != nil {
			continue
		}
		slog.Info("database host changed: env updated", "service", svc.Slug, "from", oldHost, "to", newHost, "vars", n)
		if skip[id] || svc.CurrentDeploymentID == nil || svc.Status != model.StatusRunning {
			continue
		}
		if _, err := s.restartService(ctx, svc, nil); err != nil {
			slog.Warn("restart after database host change", "service", svc.Slug, "err", err)
		}
	}
	return changed
}
