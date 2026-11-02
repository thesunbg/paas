package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"time"

	"github.com/thesunbg/paas/internal/databases"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

// Losing a node. Stateless services move by themselves (reconcile). Services
// with volumes and databases are pinned to their node and wait for it; when
// it is not coming back, an administrator recovers them on another node from
// their off-site backups:
//
//   - a service: its newest volume backup is unpacked on another node, the
//     service is pinned there (POST /services/{id}/recover, or every service
//     of the node at once with POST /nodes/{id}/recover; see volmove.go);
//   - a dedicated database: a new server is created elsewhere and the newest
//     off-site dump restored into it (POST /databases/{id}/recover);
//   - a shared server: recreated, and every database it held restored from
//     its own dump (same endpoint, platform administrators only).
//
// The old volumes stay on the dead node and are removed 7 days after it comes
// back (volume_gc), in case something has to be fetched from them.

const volumeKeep = 7

func (s *Server) nodeOnline(ctx context.Context, id *string) bool {
	if id == nil {
		return false
	}
	n, err := s.store.NodeByID(ctx, *id)
	return err == nil && n.Status == "online"
}

// serviceNode is the node a pinned service lives on (its pin, else its instance's node).
func (s *Server) serviceNode(ctx context.Context, svc *store.Service) *string {
	if svc.NodeID != nil {
		return svc.NodeID
	}
	if ins, err := s.store.InstancesForService(ctx, svc.ID); err == nil && len(ins) > 0 {
		return &ins[0].NodeID
	}
	return nil
}

func volumeBackupDue(svc *store.Service, last *store.VolumeBackup) bool {
	hours := svc.Spec.VolumeBackupHours
	if hours < 0 || len(svc.Spec.Volumes) == 0 || hiddenService(svc) {
		return false
	}
	if hours == 0 {
		hours = 24
	}
	if last == nil {
		return true
	}
	if last.Status == "running" && time.Since(last.CreatedAt) < 6*time.Hour {
		return false
	}
	return time.Since(last.CreatedAt) > time.Duration(hours)*time.Hour
}

// volumeBackupTick runs every minute on the leader; one backup at a time.
func (s *Server) volumeBackupTick(ctx context.Context) {
	if !s.storage().Enabled() || !s.volBackupMu.TryLock() {
		return
	}
	svcs, err := s.store.AllServices(ctx)
	if err != nil {
		s.volBackupMu.Unlock()
		return
	}
	for i := range svcs {
		svc := &svcs[i]
		if svc.Status != model.StatusRunning {
			continue
		}
		last, _ := s.store.LastVolumeBackup(ctx, svc.ID)
		if !volumeBackupDue(svc, last) {
			continue
		}
		go func() {
			defer s.volBackupMu.Unlock()
			if _, err := s.backupVolumes(context.WithoutCancel(ctx), svc); err != nil {
				slog.Warn("volume backup", "service", svc.Slug, "err", err)
			}
		}()
		return
	}
	s.volBackupMu.Unlock()
}

// backupVolumes archives a service's volumes on its node (the container frozen meanwhile) and uploads them.
func (s *Server) backupVolumes(ctx context.Context, svc *store.Service) (*store.VolumeBackup, error) {
	c := s.storage()
	if !c.Enabled() {
		return nil, errors.New("off-site storage is not configured")
	}
	in, err := s.firstInstance(ctx, svc.ID)
	if err != nil {
		return nil, err
	}
	b, err := s.store.CreateVolumeBackup(ctx, svc.ID, in.NodeID)
	if err != nil {
		return nil, err
	}
	name := time.Now().UTC().Format("20060102-150405") + ".tar.gz"
	file := path.Join("backups", "volumes", svc.ID, name)
	fail := func(err error) (*store.VolumeBackup, error) {
		_ = s.store.FinishVolumeBackup(ctx, b.ID, "failed", err.Error(), "", 0)
		_ = s.requestAgent(ctx, in.NodeID, proto.CmdRemoveFiles, proto.RemoveFilesRequest{Paths: []string{file}}, &proto.GenericReply{}, time.Minute)
		p, _ := s.store.ProjectByID(ctx, svc.ProjectID)
		team := ""
		if p != nil {
			team = p.TeamID
		}
		s.notify(ctx, team, "backup.failed", fmt.Sprintf("Volume backup of %s failed: %v", svc.Slug, err), map[string]any{"service": svc.ID})
		return nil, err
	}
	var rep proto.ArchiveReply
	if err := s.requestAgent(ctx, in.NodeID, proto.CmdArchive, proto.ArchiveRequest{
		Dir: path.Join("volumes", svc.ID), File: file, PauseContainer: in.ContainerID,
	}, &rep, 2*time.Hour); err != nil {
		return fail(fmt.Errorf("archive (agent too old? it updates itself within minutes): %w", err))
	}
	if rep.Error != "" {
		return fail(errors.New(rep.Error))
	}
	key := c.Key("volumes", svc.ID, name)
	if err := s.uploadLarge(ctx, in.NodeID, key, file, rep.Size); err != nil {
		return fail(fmt.Errorf("upload: %w", err))
	}
	_ = s.requestAgent(ctx, in.NodeID, proto.CmdRemoveFiles, proto.RemoveFilesRequest{Paths: []string{file}}, &proto.GenericReply{}, time.Minute)
	_ = s.store.FinishVolumeBackup(ctx, b.ID, "completed", "", key, rep.Size)
	if old, err := s.store.OldVolumeBackups(ctx, svc.ID, volumeKeep); err == nil {
		for _, o := range old {
			if o.S3Key != "" {
				_ = c.Delete(o.S3Key)
			}
			_ = s.store.DeleteVolumeBackup(ctx, o.ID)
		}
	}
	return s.store.VolumeBackupByID(ctx, b.ID)
}

// GET /api/v1/services/{id}/volume-backups
func (s *Server) handleListVolumeBackups(w http.ResponseWriter, r *http.Request) {
	svc, _ := s.serviceFor(w, r, "viewer")
	if svc == nil {
		return
	}
	bs, err := s.store.VolumeBackups(r.Context(), svc.ID, 30)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if bs == nil {
		bs = []store.VolumeBackup{}
	}
	node := s.serviceNode(r.Context(), svc)
	writeJSON(w, 200, map[string]any{
		"backups": bs, "offsite": s.storage().Enabled(), "every_hours": max(svc.Spec.VolumeBackupHours, 0),
		"enabled": svc.Spec.VolumeBackupHours >= 0, "node_online": node == nil || s.nodeAlive(r.Context(), node),
	})
}

// POST /api/v1/services/{id}/volume-backups: back up now.
func (s *Server) handleCreateVolumeBackup(w http.ResponseWriter, r *http.Request) {
	svc, p := s.serviceFor(w, r, "developer")
	if svc == nil {
		return
	}
	if len(svc.Spec.Volumes) == 0 {
		writeError(w, 400, "this service has no volumes")
		return
	}
	b, err := s.backupVolumes(context.WithoutCancel(r.Context()), svc)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, &p.TeamID, "service.volume_backup", svc.Slug)
	writeJSON(w, 201, b)
}

// latestOffsiteDump is a database's newest completed dump with an off-site copy.
func (s *Server) latestOffsiteDump(ctx context.Context, dbID string) *store.Backup {
	bs, err := s.store.BackupsForDatabase(ctx, dbID)
	if err != nil {
		return nil
	}
	for i := range bs {
		if bs[i].Kind == "dump" && bs[i].Status == "completed" && bs[i].S3Key != "" {
			return &bs[i]
		}
	}
	return nil
}

// POST /api/v1/databases/{id}/recover {allow_empty}
func (s *Server) handleRecoverDatabase(w http.ResponseWriter, r *http.Request) {
	db, p := s.databaseFor(w, r, "admin")
	if db == nil {
		return
	}
	var in struct {
		AllowEmpty bool `json:"allow_empty"`
	}
	_ = decode(r, &in)
	ctx := r.Context()
	if db.ServerServiceID == nil {
		writeError(w, 409, "the database has no server")
		return
	}
	srv, err := s.store.ServiceByID(ctx, *db.ServerServiceID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	old := s.serviceNode(ctx, srv)
	if old == nil || s.nodeOnline(ctx, old) {
		writeError(w, 409, "the database server's node is online: use a backup restore instead")
		return
	}
	if db.Options.HA {
		writeError(w, 409, "this database has high availability: its standby takes over by itself")
		return
	}
	if !s.storage().Enabled() && !in.AllowEmpty {
		writeError(w, 409, "off-site storage is not configured, so there is no backup to recover from")
		return
	}
	u := currentUser(r)
	if db.Mode == "shared" {
		if !u.IsAdmin {
			writeError(w, 403, "this database lives on a shared server: an administrator recovers the whole server")
			return
		}
		go s.recoverSharedServer(context.Background(), srv, *old, in.AllowEmpty)
	} else {
		b := s.latestOffsiteDump(ctx, db.ID)
		if b == nil && !in.AllowEmpty {
			writeError(w, 409, "no off-site backup of this database: pass allow_empty to start it empty")
			return
		}
		go s.recoverDedicated(context.Background(), db, srv, *old, b)
	}
	s.store.Audit(ctx, &u.ID, &p.TeamID, "database.recover", db.Name)
	writeJSON(w, 202, map[string]string{"status": "recovering"})
}

func (s *Server) recoverDedicated(ctx context.Context, db *store.Database, old *store.Service, oldNode string, b *store.Backup) {
	unlock := s.lock("db-" + db.ID)
	defer unlock()
	_ = s.store.SetDatabaseStatus(ctx, db.ID, "recovering", "")
	target, err := s.createDedicatedServer(ctx, db, dedicatedOpts{slug: db.Name + "-r" + util.RandomToken(3)})
	fail := func(err error) {
		if target != nil {
			_ = s.deleteService(ctx, target)
		}
		_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", "recovery failed: "+err.Error())
		s.notify(ctx, s.teamOfDatabase(ctx, db), "database.recover", fmt.Sprintf("Recovering database %s failed: %v", db.Name, err), map[string]any{"database": db.ID})
	}
	if err != nil {
		fail(fmt.Errorf("create the new server: %w", err))
		return
	}
	tdb := *db
	tdb.ServerServiceID = &target.ID
	if err := s.waitServiceRunning(ctx, target.ID, 15*time.Minute); err != nil {
		fail(fmt.Errorf("the new server did not start: %w", err))
		return
	}
	if err := s.loadDatabase(ctx, &tdb, target, b, false); err != nil {
		fail(err)
		return
	}
	if err := s.store.ReplaceServer(ctx, []string{db.ID}, old.ID, target.ID, oldNode); err != nil {
		fail(err)
		return
	}
	msg := "recovered on another node"
	if b != nil {
		msg += " from the backup of " + b.CreatedAt.UTC().Format("2006-01-02 15:04 UTC")
	} else {
		msg += ", empty"
	}
	_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", "")
	s.restartLinked(ctx, db.ID)
	s.publishAll(ctx)
	s.notify(ctx, s.teamOfDatabase(ctx, db), "database.recover", fmt.Sprintf("Database %s %s.", db.Name, msg), map[string]any{"database": db.ID})
}

// loadDatabase fills a database on a fresh, running server: the logical
// database (shared), the dump b (nil: none), and its extra users.
func (s *Server) loadDatabase(ctx context.Context, db *store.Database, target *store.Service, b *store.Backup, shared bool) error {
	tc, _, err := s.dbCreds(ctx, db)
	if err != nil {
		return err
	}
	probe, err := databases.Stats(db.Engine, shared, tc)
	if err != nil {
		return err
	}
	if err := s.execOnService(ctx, target.ID, probe, "", "", 8); err != nil {
		return fmt.Errorf("the new server does not answer: %w", err)
	}
	if shared {
		preset, _ := databases.PresetByName(db.SizePreset)
		cmd, err := databases.CreateLogical(db.Engine, tc, preset.MaxConns/2+5)
		if err != nil {
			return err
		}
		if err := s.execOnService(ctx, target.ID, cmd, "", "", 5); err != nil {
			return fmt.Errorf("create the database: %w", err)
		}
	}
	if b != nil {
		if err := s.restoreBackup(ctx, db, b, true); err != nil {
			return fmt.Errorf("restore the backup: %w", err)
		}
	}
	users, err := s.store.DBUsers(ctx, db.ID)
	if err != nil {
		return err
	}
	for _, u := range users {
		pass, err := s.box.Decrypt(u.PasswordEnc)
		if err != nil {
			return err
		}
		cmd, err := databases.CreateUser(db.Engine, shared, tc, u.Username, pass, u.Role)
		if err != nil {
			return err
		}
		if err := s.execOnService(ctx, target.ID, cmd, "", "", 1); err != nil {
			return fmt.Errorf("create user %s: %w", u.Username, err)
		}
	}
	return nil
}

// recoverSharedServer recreates a shared server lost with its node and
// restores every database it held from that database's newest off-site dump.
func (s *Server) recoverSharedServer(ctx context.Context, old *store.Service, oldNode string, allowEmpty bool) {
	unlock := s.lock("recover-shared-" + old.ID)
	defer unlock()
	all, err := s.store.AllDatabases(ctx)
	if err != nil {
		return
	}
	var dbs []*store.Database
	for i := range all {
		if all[i].ServerServiceID != nil && *all[i].ServerServiceID == old.ID {
			dbs = append(dbs, &all[i])
		}
	}
	if len(dbs) == 0 {
		_ = s.store.ReplaceServer(ctx, nil, old.ID, "", oldNode)
		return
	}
	engine, version := dbs[0].Engine, dbs[0].Version
	for _, db := range dbs {
		_ = s.store.SetDatabaseStatus(ctx, db.ID, "recovering", "")
	}
	report := func(text string) {
		s.notify(ctx, "", "database.recover", text, map[string]any{"service": old.ID})
		slog.Warn("shared server recovery", "server", old.Slug, "msg", text)
	}
	// The old row goes first: ensureSharedServer creates the server by its slug.
	if err := s.store.ReplaceServer(ctx, nil, old.ID, "", oldNode); err != nil {
		report(fmt.Sprintf("Recovering %s failed: %v", old.Slug, err))
		return
	}
	target, err := s.ensureSharedServer(ctx, engine, version)
	if err != nil {
		report(fmt.Sprintf("Recovering %s failed: the new server did not start: %v", old.Slug, err))
		for _, db := range dbs {
			_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", "recovery failed: the shared server did not start")
		}
		return
	}
	restored, empty, failed := 0, 0, 0
	for _, db := range dbs {
		// server_service_id became NULL with the old row (ON DELETE SET NULL).
		_, _ = s.store.Q(ctx).Exec(ctx, `UPDATE databases SET server_service_id=$2 WHERE id=$1`, db.ID, target.ID)
		db.ServerServiceID = &target.ID
		b := s.latestOffsiteDump(ctx, db.ID)
		// Without a backup the database is still created (empty), so its apps can connect.
		msg := ""
		switch err := s.loadDatabase(ctx, db, target, b, true); {
		case err != nil:
			msg = "recovery failed: " + err.Error()
			failed++
		case b == nil:
			empty++
			if !allowEmpty {
				msg = "recovered empty: there was no off-site backup"
			}
		default:
			restored++
		}
		_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", msg)
		s.restartLinked(ctx, db.ID)
	}
	s.publishAll(ctx)
	report(fmt.Sprintf("Shared server %s recovered on another node: of %d databases, %d restored from backups, %d started empty, %d failed.", old.Slug, len(dbs), restored, empty, failed))
}
