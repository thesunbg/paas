package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

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
	if err := s.checkQuota(r.Context(), p.TeamID, newDBUsage(in)); err != nil {
		writeLimitError(w, err)
		return
	}
	db, err := s.createDatabase(r.Context(), p, in)
	if err != nil {
		var eol *databases.ErrEOL
		if errors.As(err, &eol) {
			writeError(w, 409, err.Error())
			return
		}
		writeError(w, 400, err.Error())
		return
	}
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, &p.TeamID, "database.create", db.Name)
	out := struct {
		*store.Database
		Warning string `json:"warning,omitempty"`
	}{Database: db}
	if v, err := databases.Lookup(db.Engine, db.Version); err == nil && databases.Status(v, time.Now()) == databases.StatusEnding {
		out.Warning = fmt.Sprintf("%s %s reaches end of life on %s", db.Engine, db.Version, v.EOL.Format("2006-01-02"))
	}
	writeJSON(w, 201, out)
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
	setETag(w, db.OptionsVersion)
	out := map[string]any{"database": db, "version": db.Version, "backup_health": s.backupHealth(r.Context(), db)}
	if v, err := databases.Lookup(db.Engine, db.Version); err == nil {
		out["version_status"] = databases.Status(v, time.Now())
		if !v.EOL.IsZero() {
			out["version_eol"] = v.EOL.Format("2006-01-02")
		}
	}
	if v := s.serverVersion(r.Context(), db); v != "" {
		out["server_version"] = v
	}
	// image_digest is what the server's instance runs; image_pinned says whether a redeploy or
	// reschedule would run that same image (false: still on the tag, "not pinned yet").
	if db.ServerServiceID != nil {
		if srv, err := s.store.ServiceByID(r.Context(), *db.ServerServiceID); err == nil {
			out["image_pinned"] = isPinned(specImage(srv))
			if in, err := s.firstInstance(r.Context(), srv.ID); err == nil && in.ImageDigest != "" {
				out["image_digest"] = in.ImageDigest
			}
		}
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
			if nid := s.serviceNode(r.Context(), srv); nid != nil {
				out["server"].(map[string]any)["node_online"] = s.nodeOnline(r.Context(), nid)
			}
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
	ctx := context.WithoutCancel(r.Context())
	unlock := s.lock("db-" + db.ID) // waits for a move or update in flight
	defer unlock()
	db, err := s.store.DatabaseByID(ctx, db.ID)
	if err != nil {
		writeError(w, 404, "database not found")
		return
	}
	if db.MoveState != nil || db.Status == "upgrading" {
		writeError(w, 409, "the database is being upgraded: wait for it to finish")
		return
	}
	if err := s.deleteDatabase(ctx, db); err != nil {
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
	if db.Status == "upgrading" {
		writeError(w, 409, "the database is being upgraded")
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
	if err := s.restoreBackup(context.WithoutCancel(r.Context()), db, b, false); err != nil {
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
	}
	dedicated := *db
	dedicated.Mode, dedicated.SizePreset = "dedicated", firstNonEmpty(in.Size, db.SizePreset)
	if err := s.checkQuota(r.Context(), p.TeamID, dbUsage(&dedicated).minus(dbUsage(db))); err != nil {
		writeLimitError(w, err)
		return
	}
	if ok, err := s.store.ClaimDatabaseStatus(r.Context(), db.ID, "ready", "upgrading"); err != nil || !ok {
		writeError(w, 409, "database is not ready")
		return
	}
	if in.Size != "" {
		_ = s.store.SetDatabaseSize(r.Context(), db.ID, in.Size)
	}
	go s.moveDatabase(context.Background(), db.ID, moveTarget{Version: db.Version, Dedicated: true})
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
