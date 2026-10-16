package server

import (
	"context"
	"net/http"

	"github.com/thesunbg/paas/internal/databases"
	"github.com/thesunbg/paas/internal/store"
)

func (s *Server) handleCreateDatabase(w http.ResponseWriter, r *http.Request) {
	p := s.projectFor(w, r, r.PathValue("id"), "developer")
	if p == nil {
		return
	}
	var in CreateDatabaseInput
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if err := s.checkCounts(r, p.TeamID, 0, 0, 1); err != nil {
		writeError(w, 403, err.Error())
		return
	}
	db, err := s.createDatabase(r.Context(), p, in)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, &p.TeamID, "database.create", db.Name)
	writeJSON(w, 201, db)
}

func (s *Server) databaseFor(w http.ResponseWriter, r *http.Request, min string) (*store.Database, *store.Project) {
	db, err := s.store.DatabaseByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "database not found")
		return nil, nil
	}
	p := s.projectFor(w, r, db.ProjectID, min)
	if p == nil {
		return nil, nil
	}
	return db, p
}

func (s *Server) handleGetDatabase(w http.ResponseWriter, r *http.Request) {
	db, _ := s.databaseFor(w, r, "viewer")
	if db == nil {
		return
	}
	out := map[string]any{"database": db, "version": databases.DisplayVersion(db.Engine, db.Version)}
	if v := s.serverVersion(r.Context(), db); v != "" {
		out["server_version"] = v
	}
	if role, _ := s.teamRoleForDB(r, db); roleRank[role] >= roleRank["developer"] && (db.Status == "ready" || db.Status == "upgrading") {
		if c, srv, err := s.dbCreds(r.Context(), db); err == nil {
			out["connection"] = map[string]any{
				"host": c.Host, "port": c.Port, "database": c.DBName, "username": c.Username, "password": c.Password,
				"url": databases.URL(db.Engine, c),
			}
			if u, err := s.poolerURL(r.Context(), db); err == nil {
				out["connection"].(map[string]any)["pooled_url"] = u
			}
			if db.Options.PublicPort != 0 {
				out["external"] = map[string]any{
					"host": s.cfg.DBHost, "port": db.Options.PublicPort, "tls": s.tlsAtEdge(),
					"url": s.externalURL(db, c), "allowed_cidrs": db.Options.AllowedCIDRs,
				}
			}
			out["server"] = map[string]any{"service_id": srv.ID, "status": srv.Status, "memory_limit_mb": srv.Spec.Resources.MemoryLimitMB}
			if in, err := s.firstInstance(r.Context(), srv.ID); err == nil {
				out["server"].(map[string]any)["memory_usage_mb"] = in.MemoryUsageMB
			}
		}
	}
	links, _ := s.store.LinksForDatabase(r.Context(), db.ID)
	if links == nil {
		links = []store.DatabaseLink{}
	}
	out["links"] = links
	writeJSON(w, 200, out)
}

func (s *Server) teamRoleForDB(r *http.Request, db *store.Database) (string, error) {
	p, err := s.store.ProjectByID(r.Context(), db.ProjectID)
	if err != nil {
		return "", err
	}
	return s.teamRole(r, p.TeamID)
}

func (s *Server) handleDeleteDatabase(w http.ResponseWriter, r *http.Request) {
	db, p := s.databaseFor(w, r, "admin")
	if db == nil {
		return
	}
	if err := s.deleteDatabase(context.WithoutCancel(r.Context()), db); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, &p.TeamID, "database.delete", db.Name)
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

func (s *Server) handleLinkDatabase(w http.ResponseWriter, r *http.Request) {
	db, p := s.databaseFor(w, r, "developer")
	if db == nil {
		return
	}
	var in struct {
		ServiceID string `json:"service_id"`
		EnvName   string `json:"env_name"`
		Restart   *bool  `json:"restart"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	svc, err := s.store.ServiceByID(r.Context(), in.ServiceID)
	if err != nil || svc.ProjectID != p.ID {
		writeError(w, 400, "service must belong to the same project")
		return
	}
	if in.EnvName == "" {
		in.EnvName = databases.Engines[db.Engine].DefaultEnv
	}
	if !envKeyRe.MatchString(in.EnvName) {
		writeError(w, 400, "invalid env name")
		return
	}
	if err := s.store.LinkDatabase(r.Context(), svc.ID, db.ID, in.EnvName); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if (in.Restart == nil || *in.Restart) && db.Status == "ready" && svc.CurrentDeploymentID != nil {
		u := currentUser(r)
		_, _ = s.restartService(r.Context(), svc, &u.ID)
	}
	writeJSON(w, 200, map[string]string{"status": "linked", "env_name": in.EnvName})
}

func (s *Server) handleUnlinkDatabase(w http.ResponseWriter, r *http.Request) {
	db, p := s.databaseFor(w, r, "developer")
	if db == nil {
		return
	}
	if svc, err := s.store.ServiceByID(r.Context(), r.PathValue("service")); err != nil || svc.ProjectID != p.ID {
		writeError(w, 404, "service not found in this project")
		return
	}
	if err := s.store.UnlinkDatabase(r.Context(), r.PathValue("service"), db.ID); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if svc, err := s.store.ServiceByID(r.Context(), r.PathValue("service")); err == nil && svc.CurrentDeploymentID != nil && svc.Status == "running" {
		u := currentUser(r)
		_, _ = s.restartService(r.Context(), svc, &u.ID)
	}
	writeJSON(w, 200, map[string]string{"status": "unlinked"})
}

func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	db, _ := s.databaseFor(w, r, "viewer")
	if db == nil {
		return
	}
	b, err := s.store.BackupsForDatabase(r.Context(), db.ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if b == nil {
		b = []store.Backup{}
	}
	writeJSON(w, 200, b)
}

func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	db, _ := s.databaseFor(w, r, "developer")
	if db == nil {
		return
	}
	if db.Status != "ready" {
		writeError(w, 400, "database is not ready")
		return
	}
	b, err := s.backupDatabase(context.WithoutCancel(r.Context()), db)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, b)
}

func (s *Server) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	db, p := s.databaseFor(w, r, "admin")
	if db == nil {
		return
	}
	b, err := s.store.BackupByID(r.Context(), r.PathValue("backup"))
	if err != nil || b.DatabaseID != db.ID {
		writeError(w, 404, "backup not found")
		return
	}
	if b.Kind == "base" {
		writeError(w, 400, "base backups are restored with point-in-time recovery")
		return
	}
	if err := s.restoreBackup(context.WithoutCancel(r.Context()), db, b); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, &p.TeamID, "database.restore", db.Name)
	writeJSON(w, 200, map[string]string{"status": "restored"})
}

func (s *Server) handleUpgradeDatabase(w http.ResponseWriter, r *http.Request) {
	db, p := s.databaseFor(w, r, "admin")
	if db == nil {
		return
	}
	if db.Mode != "shared" || db.Status != "ready" {
		writeError(w, 400, "only ready shared databases can be upgraded")
		return
	}
	var in struct {
		Size string `json:"size"`
	}
	_ = decode(r, &in)
	if in.Size != "" {
		if _, ok := databases.PresetByName(in.Size); !ok {
			writeError(w, 400, "unknown size")
			return
		}
		db.SizePreset = in.Size
		_, _ = s.store.DB.Exec(r.Context(), `UPDATE databases SET size_preset=$2 WHERE id=$1`, db.ID, in.Size)
	}
	_ = s.store.SetDatabaseStatus(r.Context(), db.ID, "upgrading", "")
	go func() {
		ctx := context.Background()
		if err := s.upgradeDatabase(ctx, db); err != nil {
			_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", "upgrade failed: "+err.Error())
			return
		}
		_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", "")
	}()
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, &p.TeamID, "database.upgrade", db.Name)
	writeJSON(w, 202, map[string]string{"status": "upgrading"})
}

func (s *Server) handleRetryDatabase(w http.ResponseWriter, r *http.Request) {
	db, _ := s.databaseFor(w, r, "developer")
	if db == nil {
		return
	}
	if db.Status != "failed" {
		writeError(w, 400, "only failed databases can be retried")
		return
	}
	_ = s.store.SetDatabaseStatus(r.Context(), db.ID, "provisioning", "")
	go s.provisionDatabase(context.Background(), db.ID)
	writeJSON(w, 202, map[string]string{"status": "provisioning"})
}
