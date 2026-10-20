package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/databases"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/s3"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

// Point-in-time recovery for dedicated PostgreSQL: the server archives every
// WAL segment to a spool in its "wal" volume (archive_timeout bounds the loss
// to 5 min), walShipTick moves the spool of the primary and the standby to S3,
// a base backup is taken daily and uploaded to S3, and a restore downloads the
// newest base backup older than the target plus the WAL since then onto any
// node and replays it into a new database. Without S3 PITR is paused.

const (
	pitrBaseInterval = 24 * time.Hour
	walShipEvery     = 30 * time.Second
	walStaleAfter    = 10 * time.Minute // oldest spooled segment that raises a warning
	walMargin        = 30 * time.Minute // WAL kept before the oldest base backup
	pitrNoS3         = "PITR paused: cần S3"
	pitrStale        = "PITR: WAL chưa được đẩy lên S3 quá 10 phút"
)

var errPITRNeedsS3 = errors.New("point-in-time recovery cần object storage (S3)")

// pitrStatus sets or clears (msg "") the PITR warning in the database's status line.
func (s *Server) pitrStatus(ctx context.Context, db *store.Database, msg string) {
	if db.Status != "ready" || db.Error == msg || msg == "" && !strings.HasPrefix(db.Error, "PITR") {
		return
	}
	_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", msg)
}

func pitrCapable(db *store.Database) bool {
	return db.Engine == databases.Postgres && db.Mode == "dedicated"
}

func pitrRetention(db *store.Database) int {
	if db.Options.PITRRetention > 0 {
		return db.Options.PITRRetention
	}
	return 7
}

// takeBaseBackup streams pg_basebackup into a file on the server's node, uploads
// it to S3 and deletes the local file.
func (s *Server) takeBaseBackup(ctx context.Context, db *store.Database) (*store.Backup, error) {
	if !s.s3.Enabled() {
		return nil, errPITRNeedsS3
	}
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
	key := s.s3.Key("pitr", db.ID, "base", filepath.Base(path))
	if err == nil {
		err = s.uploadLarge(ctx, in.NodeID, key, path, rep.StdoutBytes)
	}
	// The base backup lives in S3 only: the local file goes either way.
	_ = s.requestAgent(ctx, in.NodeID, proto.CmdRemoveFiles, proto.RemoveFilesRequest{Paths: []string{path}}, &proto.GenericReply{}, time.Minute)
	if err != nil {
		_ = s.store.FinishBackup(ctx, b.ID, "failed", err.Error(), 0)
		return b, err
	}
	_ = s.store.SetBackupS3Key(ctx, b.ID, key)
	_ = s.store.FinishBackup(ctx, b.ID, "completed", "", rep.StdoutBytes)
	b.Status, b.SizeBytes, b.S3Key = "completed", rep.StdoutBytes, key
	s.prunePITR(ctx, db)
	return b, nil
}

// deleteBase removes a base backup everywhere (S3, and the local file of one taken before S3 was required).
func (s *Server) deleteBase(ctx context.Context, b *store.Backup) error {
	if b.S3Key != "" {
		if err := s.s3.Delete(b.S3Key); err != nil {
			return err
		}
	} else if b.NodeID != nil {
		_ = s.requestAgent(ctx, *b.NodeID, proto.CmdRemoveFiles, proto.RemoveFilesRequest{Paths: []string{b.Path}}, &proto.GenericReply{}, time.Minute)
	}
	return s.store.DeleteBackup(ctx, b.ID)
}

func (s *Server) walPrefix(db *store.Database) string { return s.s3.Key("pitr", db.ID, "wal") + "/" }

// prunePITR drops base backups beyond the retention (always keeping the newest)
// and the WAL in S3 older than the oldest kept base backup minus walMargin.
// Timeline history files are kept: they are tiny and every restore needs them.
func (s *Server) prunePITR(ctx context.Context, db *store.Database) {
	bases, err := s.store.BaseBackups(ctx, db.ID)
	if err != nil || len(bases) == 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(pitrRetention(db)) * 24 * time.Hour)
	oldestKept := bases[0]
	for i := range bases {
		b := &bases[i]
		if i == 0 || b.CreatedAt.After(cutoff) {
			oldestKept = *b
			continue
		}
		if err := s.deleteBase(ctx, b); err != nil {
			slog.Warn("prune base backup", "db", db.Name, "err", err)
			oldestKept = *b // still in S3: keep the WAL it needs
		}
	}
	objs, err := s.s3.List(s.walPrefix(db))
	if err != nil {
		slog.Warn("prune wal archive", "db", db.Name, "err", err)
		return
	}
	before := oldestKept.CreatedAt.Add(-walMargin)
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, ".history") && o.LastModified.Before(before) {
			if err := s.s3.Delete(o.Key); err != nil {
				slog.Warn("prune wal archive", "db", db.Name, "err", err)
				return
			}
		}
	}
}

// neededWAL selects the archived files a restore from a base backup created at
// since replays: every .history file and the segments written after since-walMargin.
func neededWAL(objs []s3.Object, since time.Time) []string {
	var names []string
	for _, o := range objs {
		name := path.Base(o.Key)
		if name == "." || name == "/" || strings.HasPrefix(name, ".") {
			continue
		}
		if strings.HasSuffix(name, ".history") || !o.LastModified.Before(since.Add(-walMargin)) {
			names = append(names, name)
		}
	}
	return names
}

// walShipLoop drains the WAL spools to S3 (runs on the leader).
func (s *Server) walShipLoop(ctx context.Context) {
	t := time.NewTicker(walShipEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.walShipTick(ctx)
	}
}

func (s *Server) walShipTick(ctx context.Context) {
	if !s.s3.Enabled() {
		return
	}
	dbs, err := s.store.AllDatabases(ctx)
	if err != nil {
		return
	}
	for i := range dbs {
		db := &dbs[i]
		if !db.Options.PITR || !pitrCapable(db) || db.ServerServiceID == nil || db.Status != "ready" {
			continue
		}
		go func() {
			unlock, ok := s.tryLock("pitr-ship-" + db.ID)
			if !ok {
				return
			}
			defer unlock()
			if s.shipWAL(ctx, db) {
				s.pitrStatus(ctx, db, pitrStale)
			} else {
				s.pitrStatus(ctx, db, "")
			}
		}()
	}
}

// shipWAL uploads the spooled WAL of the primary and of the standby, deleting
// each file once it is in S3. It reports whether a segment has waited too long.
func (s *Server) shipWAL(ctx context.Context, db *store.Database) (stale bool) {
	for _, id := range []string{*db.ServerServiceID, db.Options.ReplicaServiceID} {
		if id != "" && s.shipSpool(ctx, db, id) {
			stale = true
		}
	}
	return stale
}

func (s *Server) shipSpool(ctx context.Context, db *store.Database, serviceID string) (stale bool) {
	in, err := s.firstInstance(ctx, serviceID)
	if err != nil {
		return false
	}
	out, err := s.execOutput(ctx, serviceID, databases.Command{Cmd: []string{"ls", "-1", "/wal/archive"}}, 30*time.Second)
	if err != nil {
		return false
	}
	var shipped []string
	failed := false
	for _, name := range strings.Split(out, "\n") {
		name = strings.TrimSpace(name)
		if name == "" || strings.HasPrefix(name, ".") { // .tmp: still being written by archive_command
			continue
		}
		file := filepath.Join("volumes", serviceID, "wal", "archive", name)
		if err := s.transfer(ctx, in.NodeID, http.MethodPut, s.s3.Key("pitr", db.ID, "wal", name), file); err != nil {
			slog.Warn("ship wal", "db", db.Name, "file", name, "err", err)
			failed = true
			break
		}
		shipped = append(shipped, file)
	}
	if len(shipped) > 0 {
		_ = s.requestAgent(ctx, in.NodeID, proto.CmdRemoveFiles, proto.RemoveFilesRequest{Paths: shipped}, &proto.GenericReply{}, time.Minute)
	}
	if !failed {
		return false
	}
	old, err := s.execOutput(ctx, serviceID, databases.Command{Cmd: []string{"find", "/wal/archive", "-maxdepth", "1", "-type", "f", "!", "-name", ".*", "-mmin", "+" + fmt.Sprint(int(walStaleAfter.Minutes())), "-print"}}, 30*time.Second)
	return err == nil && strings.TrimSpace(old) != ""
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
		if !s.s3.Enabled() { // nothing is taken or deleted until S3 is back
			s.pitrStatus(ctx, db, pitrNoS3)
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
	if in.Enabled && !s.s3.Enabled() {
		writeError(w, 400, "point-in-time recovery cần object storage (S3)")
		return
	}
	restart := db.Options.PITR != in.Enabled
	o, err := s.store.UpdateDatabaseOptions(r.Context(), db.ID, func(o *store.DBOptions) error {
		o.PITR = in.Enabled
		if in.RetentionDays > 0 {
			o.PITRRetention = in.RetentionDays
		}
		return nil
	})
	if err != nil {
		writeUpdateError(w, err)
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
			for i := range bases {
				_ = s.deleteBase(ctx, &bases[i])
			}
			if s.s3.Enabled() { // the WAL of a database without PITR is useless
				if objs, err := s.s3.List(s.walPrefix(db)); err == nil {
					for _, o := range objs {
						_ = s.s3.Delete(o.Key)
					}
				}
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
	if !s.s3.Enabled() {
		writeError(w, 400, errPITRNeedsS3.Error())
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
		if bases[i].FinishedAt != nil && !bases[i].FinishedAt.After(target) && bases[i].S3Key != "" {
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
	if _, err := databases.CheckCreate(src.Engine, src.Version, time.Now()); err != nil {
		code := 400
		var eol *databases.ErrEOL
		if errors.As(err, &eol) {
			code = 409
		}
		writeError(w, code, err.Error())
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
	db.Options, _ = s.store.UpdateDatabaseOptions(r.Context(), db.ID, func(o *store.DBOptions) error {
		*o = store.DBOptions{PITR: true, PITRRetention: src.Options.PITRRetention}
		return nil
	})
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
	c, _, _ := s.dbCreds(ctx, db)
	spec, err := s.dedicatedSpec(db, c)
	if err != nil {
		return err
	}
	// Base backup and WAL are in S3: any node can take the restore.
	n, err := s.pickReplicaNode(ctx, "", spec.Resources.MemoryLimitMB)
	if err != nil {
		return err
	}
	node := n.ID
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
		BaseBackup: filepath.Join("volumes", srv.ID, "restore-base.tar.gz"),
		WALSource:  filepath.Join("volumes", srv.ID, "restore-wal"),
		DataDir:    filepath.Join("volumes", srv.ID, "data", "pgdata"),
		WALDest:    filepath.Join("volumes", srv.ID, "wal"),
	}
	if hasTarget {
		req.TargetTime = target.UTC().Format(time.RFC3339)
	}
	// Make the latest WAL segment available in S3 before downloading.
	_, _ = s.execOutput(ctx, srcSrv.ID, databases.Command{Cmd: []string{"psql", "-U", src.Username, "-d", "postgres", "-Atc", "SELECT pg_switch_wal()"}}, 30*time.Second)
	time.Sleep(2 * time.Second)
	func() {
		unlock := s.lock("pitr-ship-" + src.ID)
		defer unlock()
		s.shipWAL(ctx, src)
	}()
	// The temporary files go whether the restore worked or not.
	defer s.requestAgent(ctx, node, proto.CmdRemoveFiles, proto.RemoveFilesRequest{Paths: []string{req.BaseBackup, req.WALSource}}, &proto.GenericReply{}, time.Minute)
	if err := s.transfer(ctx, node, http.MethodGet, base.S3Key, req.BaseBackup); err != nil {
		return fmt.Errorf("download base backup: %w", err)
	}
	objs, err := s.s3.List(s.walPrefix(src))
	if err != nil {
		return fmt.Errorf("list WAL: %w", err)
	}
	for _, name := range neededWAL(objs, base.CreatedAt) {
		if err := s.transfer(ctx, node, http.MethodGet, s.s3.Key("pitr", src.ID, "wal", name), filepath.Join(req.WALSource, name)); err != nil {
			return fmt.Errorf("download WAL %s: %w", name, err)
		}
	}
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
