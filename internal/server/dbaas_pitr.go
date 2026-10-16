package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/databases"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

// Point-in-time recovery for dedicated PostgreSQL: the server archives every
// WAL segment to its "wal" volume (archive_timeout bounds the loss to 5 min),
// a base backup is taken daily, and a restore replays the archive on top of
// the newest base backup older than the target into a new database.

const pitrBaseInterval = 24 * time.Hour

func pitrCapable(db *store.Database) bool {
	return db.Engine == databases.Postgres && db.Mode == "dedicated"
}

func pitrRetention(db *store.Database) int {
	if db.Options.PITRRetention > 0 {
		return db.Options.PITRRetention
	}
	return 7
}

// takeBaseBackup streams pg_basebackup into a file on the server's node.
func (s *Server) takeBaseBackup(ctx context.Context, db *store.Database) (*store.Backup, error) {
	c, srv, err := s.dbCreds(ctx, db)
	if err != nil {
		return nil, err
	}
	in, err := s.firstInstance(ctx, srv.ID)
	if err != nil {
		return nil, err
	}
	path := filepath.Join("backups", db.ID, "base-"+time.Now().UTC().Format("20060102-150405")+".tar.gz")
	b, err := s.store.CreateBaseBackup(ctx, db.ID, in.NodeID, path)
	if err != nil {
		return nil, err
	}
	var rep proto.ExecReply
	err = s.requestAgent(ctx, in.NodeID, proto.CmdExec, proto.ExecRequest{
		ContainerID: in.ContainerID, Cmd: []string{"pg_basebackup", "-U", c.Username, "-D", "-", "-Ft", "-X", "none", "-z", "-c", "fast"},
		StdoutFile: path, TimeoutSec: 6 * 3600,
	}, &rep, 6*time.Hour)
	if err == nil && rep.Error != "" {
		err = errors.New(rep.Error)
	}
	if err == nil && rep.ExitCode != 0 {
		err = fmt.Errorf("pg_basebackup: exit code %d: %s", rep.ExitCode, strings.TrimSpace(rep.Stderr))
	}
	if err != nil {
		_ = s.store.FinishBackup(ctx, b.ID, "failed", err.Error(), 0)
		return b, err
	}
	_ = s.store.FinishBackup(ctx, b.ID, "completed", "", rep.StdoutBytes)
	b.Status, b.SizeBytes = "completed", rep.StdoutBytes
	s.prunePITR(ctx, db, in.NodeID, srv.ID)
	return b, nil
}

// prunePITR drops base backups beyond the retention (always keeping the newest)
// and the archived WAL that no kept base backup needs any more.
func (s *Server) prunePITR(ctx context.Context, db *store.Database, nodeID, serverID string) {
	bases, err := s.store.BaseBackups(ctx, db.ID)
	if err != nil || len(bases) == 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(pitrRetention(db)) * 24 * time.Hour)
	oldestKept := bases[0]
	for i, b := range bases {
		if i == 0 || b.CreatedAt.After(cutoff) {
			oldestKept = b
			continue
		}
		if b.NodeID != nil {
			_ = s.requestAgent(ctx, *b.NodeID, proto.CmdRemoveFiles, proto.RemoveFilesRequest{Paths: []string{b.Path}}, &proto.GenericReply{}, time.Minute)
		}
		_ = s.store.DeleteBackup(ctx, b.ID)
	}
	minutes := int(time.Since(oldestKept.CreatedAt).Minutes()) + 30
	cmd := databases.Command{Cmd: []string{"find", "/wal/archive", "-type", "f", "-mmin", fmt.Sprintf("+%d", minutes), "-delete"}}
	if _, err := s.execOutput(ctx, serverID, cmd, time.Minute); err != nil {
		slog.Debug("prune wal archive", "db", db.Name, "err", err)
	}
	_ = nodeID
}

// pitrTick takes due base backups (called every minute by the maintenance loop).
func (s *Server) pitrTick(ctx context.Context) {
	if !s.pitrRunning.CompareAndSwap(false, true) {
		return
	}
	defer s.pitrRunning.Store(false)
	dbs, err := s.store.AllDatabases(ctx)
	if err != nil {
		return
	}
	for i := range dbs {
		db := &dbs[i]
		if !db.Options.PITR || !pitrCapable(db) || db.Status != "ready" {
			continue
		}
		bases, _ := s.store.BaseBackups(ctx, db.ID)
		if len(bases) > 0 && time.Since(bases[0].CreatedAt) < pitrBaseInterval {
			continue
		}
		unlock := s.lock("pitr-" + db.ID)
		if _, err := s.takeBaseBackup(ctx, db); err != nil {
			slog.Warn("pitr base backup", "db", db.Name, "err", err)
		}
		unlock()
	}
}

// GET /api/v1/databases/{id}/pitr
func (s *Server) handleGetPITR(w http.ResponseWriter, r *http.Request) {
	db, _ := s.databaseFor(w, r, "viewer")
	if db == nil {
		return
	}
	bases, _ := s.store.BaseBackups(r.Context(), db.ID)
	out := map[string]any{"capable": pitrCapable(db), "enabled": db.Options.PITR, "retention_days": pitrRetention(db), "base_backups": bases}
	if len(bases) > 0 {
		earliest := bases[len(bases)-1]
		if earliest.FinishedAt != nil {
			out["earliest"] = earliest.FinishedAt
		}
	}
	writeJSON(w, 200, out)
}

// POST /api/v1/databases/{id}/pitr {enabled, retention_days}
func (s *Server) handleSetPITR(w http.ResponseWriter, r *http.Request) {
	db, p := s.databaseFor(w, r, "admin")
	if db == nil {
		return
	}
	if !pitrCapable(db) {
		writeError(w, 400, "point-in-time recovery needs a dedicated PostgreSQL database (upgrade a shared one first)")
		return
	}
	if db.Status != "ready" {
		writeError(w, 409, "the database is not ready")
		return
	}
	var in struct {
		Enabled       bool `json:"enabled"`
		RetentionDays int  `json:"retention_days"`
	}
	if err := decode(r, &in); err != nil || in.RetentionDays < 0 || in.RetentionDays > 35 {
		writeError(w, 400, "retention_days must be between 1 and 35")
		return
	}
	o := db.Options
	restart := o.PITR != in.Enabled
	o.PITR = in.Enabled
	if in.RetentionDays > 0 {
		o.PITRRetention = in.RetentionDays
	}
	if err := s.store.UpdateDatabaseOptions(r.Context(), db.ID, o); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	db.Options = o
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, &p.TeamID, "database.pitr", fmt.Sprintf("%s %v", db.Name, in.Enabled))
	if !restart {
		writeJSON(w, 200, map[string]any{"enabled": o.PITR, "retention_days": pitrRetention(db)})
		return
	}
	_ = s.store.SetDatabaseStatus(r.Context(), db.ID, "upgrading", "")
	go func() {
		ctx := context.Background()
		unlock := s.lock("db-" + db.ID)
		defer unlock()
		if err := s.applyServerSpec(ctx, db); err != nil {
			_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", "point-in-time recovery: "+err.Error())
			return
		}
		_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", "")
		if db.Options.PITR {
			unlockBase := s.lock("pitr-" + db.ID)
			_, err := s.takeBaseBackup(ctx, db)
			unlockBase()
			if err != nil {
				_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", "first base backup failed: "+err.Error())
			}
		} else {
			bases, _ := s.store.BaseBackups(ctx, db.ID)
			for _, b := range bases {
				if b.NodeID != nil {
					_ = s.requestAgent(ctx, *b.NodeID, proto.CmdRemoveFiles, proto.RemoveFilesRequest{Paths: []string{b.Path}}, &proto.GenericReply{}, time.Minute)
				}
				_ = s.store.DeleteBackup(ctx, b.ID)
			}
		}
	}()
	writeJSON(w, 202, map[string]any{"enabled": o.PITR, "retention_days": pitrRetention(db)})
}

// POST /api/v1/databases/{id}/pitr/restore {target_time?, name?}
// Restores into a new database next to the original, which is left untouched.
func (s *Server) handlePITRRestore(w http.ResponseWriter, r *http.Request) {
	src, p := s.databaseFor(w, r, "admin")
	if src == nil {
		return
	}
	if !src.Options.PITR || !pitrCapable(src) || src.ServerServiceID == nil {
		writeError(w, 400, "point-in-time recovery is not enabled for this database")
		return
	}
	var in struct {
		TargetTime string `json:"target_time"`
		Name       string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	target := time.Now()
	if in.TargetTime != "" {
		t, err := time.Parse(time.RFC3339, in.TargetTime)
		if err != nil {
			writeError(w, 400, "target_time must be RFC 3339, e.g. 2026-10-16T09:30:00+07:00")
			return
		}
		if t.After(time.Now()) {
			writeError(w, 400, "target time is in the future")
			return
		}
		target = t
	}
	bases, _ := s.store.BaseBackups(r.Context(), src.ID)
	var base *store.Backup
	for i := range bases {
		if bases[i].FinishedAt != nil && !bases[i].FinishedAt.After(target) && bases[i].NodeID != nil {
			base = &bases[i]
			break
		}
	}
	if base == nil {
		writeError(w, 400, "no base backup older than the target time; the earliest restore point is shown on the database page")
		return
	}
	if err := s.checkCounts(r, p.TeamID, 0, 0, 1); err != nil {
		writeError(w, 403, err.Error())
		return
	}
	name := util.Slugify(src.Name + "-restored-" + target.UTC().Format("0102-1504"))
	if strings.TrimSpace(in.Name) != "" {
		name = util.Slugify(in.Name)
	}
	db, err := s.store.CreateDatabase(r.Context(), &store.Database{
		ProjectID: src.ProjectID, Name: name, Engine: src.Engine, Version: src.Version, Mode: "dedicated", DBName: src.DBName, Username: src.Username,
		PasswordEnc: src.PasswordEnc, SizePreset: src.SizePreset, BackupScheduleHours: src.BackupScheduleHours,
	})
	if err != nil {
		if store.IsUniqueViolation(err) {
			writeError(w, 409, fmt.Sprintf("a database named %q already exists in this project", name))
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	db.Options = store.DBOptions{PITR: true, PITRRetention: src.Options.PITRRetention}
	_ = s.store.UpdateDatabaseOptions(r.Context(), db.ID, db.Options)
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, &p.TeamID, "database.pitr.restore", fmt.Sprintf("%s -> %s at %s", src.Name, db.Name, target.UTC().Format(time.RFC3339)))
	go func() {
		ctx := context.Background()
		if err := s.restorePITR(ctx, src, db, base, in.TargetTime != "", target); err != nil {
			slog.Error("pitr restore", "db", db.Name, "err", err)
			_ = s.store.SetDatabaseStatus(ctx, db.ID, "failed", "restore: "+err.Error())
			return
		}
		_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", "")
	}()
	writeJSON(w, 202, db)
}

func (s *Server) restorePITR(ctx context.Context, src, db *store.Database, base *store.Backup, hasTarget bool, target time.Time) error {
	srcSrv, err := s.store.ServiceByID(ctx, *src.ServerServiceID)
	if err != nil {
		return err
	}
	node := *base.NodeID
	// The WAL archive lives on the source server's node, next to the base backup.
	if srcSrv.NodeID != nil && *srcSrv.NodeID != node {
		return errors.New("the WAL archive and the base backup are on different nodes")
	}
	c, _, _ := s.dbCreds(ctx, db)
	spec, err := s.dedicatedSpec(db, c)
	if err != nil {
		return err
	}
	srv, err := s.store.CreateService(ctx, &store.Service{
		ProjectID: db.ProjectID, Name: db.Name + " (postgres)", Slug: util.Slugify(db.Name), Type: model.TypeDatabase,
		Source: model.ServiceSource{Kind: "image", Image: spec.Image}, Spec: spec, WebhookSecret: util.RandomToken(16), NodeID: &node,
	})
	if err != nil {
		return err
	}
	if err := s.store.SetDatabaseServer(ctx, db.ID, srv.ID); err != nil {
		return err
	}
	req := proto.PITRPrepareRequest{
		BaseBackup: base.Path,
		WALSource:  filepath.Join("volumes", srcSrv.ID, "wal", "archive"),
		DataDir:    filepath.Join("volumes", srv.ID, "data", "pgdata"),
		WALDest:    filepath.Join("volumes", srv.ID, "wal"),
	}
	if hasTarget {
		req.TargetTime = target.UTC().Format(time.RFC3339)
	}
	// Make the latest WAL segment available before preparing.
	_, _ = s.execOutput(ctx, srcSrv.ID, databases.Command{Cmd: []string{"psql", "-U", src.Username, "-d", "postgres", "-Atc", "SELECT pg_switch_wal()"}}, 30*time.Second)
	time.Sleep(2 * time.Second)
	var rep proto.GenericReply
	if err := s.requestAgent(ctx, node, proto.CmdPITRPrepare, req, &rep, 30*time.Minute); err != nil {
		return err
	}
	if rep.Error != "" {
		return errors.New(rep.Error)
	}
	if _, err := s.QueueDeployment(ctx, srv, "system", "", "", nil); err != nil {
		return err
	}
	if err := s.waitServiceRunning(ctx, srv.ID, 30*time.Minute); err != nil {
		return err
	}
	// Wait for the recovery to reach its target and promote.
	deadline := time.Now().Add(time.Hour)
	for time.Now().Before(deadline) {
		out, err := s.execOutput(ctx, srv.ID, databases.Command{Cmd: []string{"psql", "-U", db.Username, "-d", "postgres", "-Atc", "SELECT pg_is_in_recovery()"}}, 30*time.Second)
		if err == nil && strings.TrimSpace(out) == "f" {
			slog.Info("pitr restore complete", "db", db.Name, "target", target)
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return errors.New("recovery did not finish within an hour")
}
