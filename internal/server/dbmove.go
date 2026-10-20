package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/databases"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/store"
)

// moveDatabase relocates a database to another server: from the shared server to its own
// (`upgrade`), or to a newer engine version (`version`, major upgrade, one way). It is the only
// function that does so.
//
// The database keeps pointing at its old server until the flip, a single compare-and-swap, so a
// failure at any step before it leaves the old server as the database: the new one is deleted and
// writes are allowed again. databases.move_state records how far a move got, so a restart of
// paasd does the same (resumeMove). The whole job holds lock("db-"+id).
//
//	1 freeze   stop the linked services, set the old database read-only
//	2 backup   after the freeze, pinned, size > 0
//	3 target   shared: ensureSharedServer + CreateLogical; dedicated: a new server (not attached)
//	4 restore  and replay the extra users
//	5 verify   same tables and row counts, the extra users can log in
//	6 flip     CAS server_service_id (+ version, mode); then the linked services start again
//	7 clean    the old logical database / server is removed at once (no way back)
type moveTarget struct {
	Version   string
	Dedicated bool // its own server (else the shared server of Version)
}

// linkedStartWait is how long each linked service gets to come up before the next one starts.
var linkedStartWait = 2 * time.Minute

func (s *Server) moveDatabase(ctx context.Context, dbID string, t moveTarget) {
	unlock := s.lock("db-" + dbID)
	defer unlock()
	db, err := s.store.DatabaseByID(ctx, dbID)
	if err != nil {
		return
	}
	label := "upgrade"
	if t.Version != db.Version {
		label = "major upgrade to " + t.Version
	}
	if db.ServerServiceID == nil {
		_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", label+" failed: database server not provisioned yet")
		return
	}
	st := &store.MoveState{Phase: store.MovePrepare, OldService: *db.ServerServiceID, TargetVersion: t.Version, TargetDedicated: t.Dedicated, OldShared: db.Mode == "shared"}
	if err := s.prepareMove(ctx, db, st, t); err != nil {
		slog.Error("database move", "db", db.Name, "err", err)
		s.abortMove(ctx, db, st, label+" failed: "+err.Error())
		return
	}
	s.finishMove(ctx, db, st)
}

// prepareMove runs steps 1-6 up to and including the flip. db is the database as it is before; on
// success it is updated to what the flip made it.
func (s *Server) prepareMove(ctx context.Context, db *store.Database, st *store.MoveState, t moveTarget) error {
	oc, oldSrv, err := s.dbCreds(ctx, db)
	if err != nil {
		return err
	}
	save := func() error { return s.store.SetMoveState(ctx, db.ID, st) }
	tdb := *db // the database as it will be on the target
	tdb.Version, tdb.Mode = t.Version, "shared"
	if t.Dedicated {
		tdb.Mode = "dedicated"
	}

	// Extensions: a dump that uses one the new image lacks cannot be restored. Checked before the
	// freeze when the target server already exists; otherwise right after it is created (the
	// freeze is already on, but nothing is restored yet).
	versionChanged := t.Version != db.Version
	checked := false
	if versionChanged && !t.Dedicated {
		if target := s.runningSharedServer(ctx, db.Engine, t.Version); target != nil {
			tdb.ServerServiceID = &target.ID
			tc, _, err := s.dbCreds(ctx, &tdb)
			if err != nil {
				return err
			}
			if err := s.checkExtensions(ctx, db, oc, oldSrv.ID, &tdb, tc, target.ID); err != nil {
				return err
			}
			checked = true
		}
	}

	if err := save(); err != nil { // before the old server is touched
		return err
	}
	if err := s.freezeWrites(ctx, db, st, oc, oldSrv.ID, save); err != nil {
		return err
	}
	b, err := s.backupDatabase(ctx, db)
	if err != nil {
		return fmt.Errorf("backup before the move: %w", err)
	}
	if b.SizeBytes <= 0 {
		return errors.New("the backup taken before the move is empty")
	}
	if err := s.store.PinBackup(ctx, b.ID); err != nil {
		return err
	}

	var target *store.Service
	if t.Dedicated {
		slug := db.Name + "-v" + strings.ReplaceAll(t.Version, ".", "-")
		if db.Mode == "shared" {
			slug = db.Name + "-db" // avoid a clash with an app of the same name
		}
		target, err = s.createDedicatedServer(ctx, &tdb, dedicatedOpts{pinNode: b.NodeID, slug: slug,
			onCreated: func(id string) error { st.NewService = id; return save() }})
		if err != nil {
			return fmt.Errorf("create the new database server: %w", err)
		}
		tdb.ServerServiceID = &target.ID
	} else {
		if target, err = s.ensureSharedServer(ctx, db.Engine, t.Version); err != nil {
			return fmt.Errorf("shared server for %s %s: %w", db.Engine, t.Version, err)
		}
		st.NewService = target.ID
		if err := save(); err != nil {
			return err
		}
		tdb.ServerServiceID = &target.ID
	}
	tc, _, err := s.dbCreds(ctx, &tdb)
	if err != nil {
		return err
	}
	// "running" is not "accepting connections": wait until the engine answers a server-level query.
	probe, err := databases.Stats(db.Engine, !t.Dedicated, tc)
	if err != nil {
		return err
	}
	if err := s.execOnService(ctx, target.ID, probe, "", "", 8); err != nil {
		return fmt.Errorf("the new database server does not answer: %w", err)
	}
	if !t.Dedicated {
		preset, _ := databases.PresetByName(db.SizePreset)
		cmd, err := databases.CreateLogical(db.Engine, tc, preset.MaxConns/2+5)
		if err != nil {
			return err
		}
		if err := s.execOnService(ctx, target.ID, cmd, "", "", 5); err != nil {
			return fmt.Errorf("create database on the shared server: %w", err)
		}
	}
	if versionChanged && !checked {
		if err := s.checkExtensions(ctx, db, oc, oldSrv.ID, &tdb, tc, target.ID); err != nil {
			return err
		}
	}

	if err := s.restoreBackup(ctx, &tdb, b, true); err != nil {
		return fmt.Errorf("restore into the new server: %w", err)
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
		cmd, err := databases.CreateUser(db.Engine, tdb.Mode == "shared", tc, u.Username, pass, u.Role)
		if err != nil {
			return err
		}
		if err := s.execOnService(ctx, target.ID, cmd, "", "", 1); err != nil {
			return fmt.Errorf("create user %s on the new server: %w", u.Username, err)
		}
	}
	if err := s.verifyMove(ctx, db, oc, oldSrv.ID, &tdb, tc, users); err != nil {
		return fmt.Errorf("verification failed (the old database is untouched): %w", err)
	}

	ok, err := s.store.FlipDatabase(ctx, db.ID, st.OldService, target.ID, t.Version, tdb.Mode, *st)
	if err != nil {
		// The statement may have committed before the connection broke: the row says which.
		if fresh, rerr := s.store.DatabaseByID(ctx, db.ID); rerr != nil || fresh.MoveState == nil || fresh.MoveState.Phase != store.MoveFlip {
			return err
		}
	} else if !ok {
		return errors.New("the database moved while it was being upgraded")
	}
	st.Phase = store.MoveFlip
	db.ServerServiceID, db.Version, db.Mode = &target.ID, t.Version, tdb.Mode
	return nil
}

// freezeWrites stops the linked services (remembered in st, to start them again) and sets the
// database read-only as a second net. Engines without a read-only switch (Mongo) rely on the first.
func (s *Server) freezeWrites(ctx context.Context, db *store.Database, st *store.MoveState, oc databases.Credentials, serverID string, save func() error) error {
	links, err := s.store.LinksForDatabase(ctx, db.ID)
	if err != nil {
		return err
	}
	for _, l := range links {
		svc, err := s.store.ServiceByID(ctx, l.ServiceID)
		if err != nil || svc.Status != model.StatusRunning {
			continue
		}
		st.Stopped = append(st.Stopped, svc.ID)
		if err := save(); err != nil { // before the stop: a restart must know to start it again
			return err
		}
		s.stopService(ctx, svc)
	}
	if cmd, err := databases.SetReadOnly(db.Engine, st.OldShared, oc, true); err == nil {
		if _, err := s.execOutput(ctx, serverID, cmd, time.Minute); err != nil {
			return fmt.Errorf("freeze writes: %w", err)
		}
	}
	return nil
}

// runningSharedServer is the shared server of an engine version when it already exists and runs.
func (s *Server) runningSharedServer(ctx context.Context, engine, version string) *store.Service {
	svcs, err := s.store.ServicesForProject(ctx, s.systemProjectID)
	if err != nil {
		return nil
	}
	slug := sharedSlug(engine, version)
	for i := range svcs {
		if svcs[i].Slug == slug && svcs[i].Status == model.StatusRunning {
			if _, err := s.firstInstance(ctx, svcs[i].ID); err == nil {
				return &svcs[i]
			}
		}
	}
	return nil
}

// checkExtensions fails when the database uses a PostgreSQL extension that the target lacks.
func (s *Server) checkExtensions(ctx context.Context, db *store.Database, oc databases.Credentials, oldID string, tdb *store.Database, tc databases.Credentials, targetID string) error {
	cmd, ok := databases.Extensions(db.Engine, db.Mode == "shared", oc, false)
	if !ok {
		return nil
	}
	installed, err := s.execOutput(ctx, oldID, cmd, time.Minute)
	if err != nil {
		return fmt.Errorf("list extensions: %w", err)
	}
	if strings.TrimSpace(installed) == "" {
		return nil
	}
	cmd, _ = databases.Extensions(db.Engine, tdb.Mode == "shared", tc, true)
	available, err := s.execOutput(ctx, targetID, cmd, time.Minute)
	if err != nil {
		return fmt.Errorf("list extensions of the new server: %w", err)
	}
	if missing := databases.MissingExtensions(installed, available); len(missing) > 0 {
		return fmt.Errorf("this database uses the extension(s) %s, which %s %s does not provide", strings.Join(missing, ", "), db.Engine, tdb.Version)
	}
	return nil
}

// verifyMove compares what the database holds on the old and the new server, and that every extra
// user can log in on the new one. Sizes say nothing; row counts do.
func (s *Server) verifyMove(ctx context.Context, db *store.Database, oc databases.Credentials, oldID string, tdb *store.Database, tc databases.Credentials, users []store.DBUser) error {
	srcCmd, err := databases.Inventory(db.Engine, db.Mode == "shared", oc)
	if err != nil {
		return err
	}
	dstCmd, err := databases.Inventory(db.Engine, tdb.Mode == "shared", tc)
	if err != nil {
		return err
	}
	src, err := s.execOutput(ctx, oldID, srcCmd, 15*time.Minute)
	if err != nil {
		return fmt.Errorf("inventory of the old database: %w", err)
	}
	dst, err := s.execOutput(ctx, *tdb.ServerServiceID, dstCmd, 15*time.Minute)
	if err != nil {
		return fmt.Errorf("inventory of the new database: %w", err)
	}
	if err := databases.CompareInventory(src, dst); err != nil {
		return err
	}
	for _, u := range users {
		pass, err := s.box.Decrypt(u.PasswordEnc)
		if err != nil {
			return err
		}
		cmd, err := databases.CheckLogin(db.Engine, tc, u.Username, pass)
		if err != nil {
			return err
		}
		if err := s.execOnService(ctx, *tdb.ServerServiceID, cmd, "", "", 1); err != nil {
			return fmt.Errorf("user %s cannot log in on the new server: %w", u.Username, err)
		}
	}
	return nil
}

// abortMove undoes a move that did not reach the flip: the new server (or logical database) goes,
// writes are allowed again, the linked services start again, the status is `ready` with msg.
func (s *Server) abortMove(ctx context.Context, db *store.Database, st *store.MoveState, msg string) {
	if st.NewService != "" && st.NewService != st.OldService {
		if err := s.dropPlacement(ctx, db, st.NewService, !st.TargetDedicated); err != nil {
			slog.Warn("remove the new server of an aborted move", "db", db.Name, "err", err)
			msg += " (the new server could not be removed: " + err.Error() + ")"
		}
	}
	if !db.Options.ReadOnly { // a quota freeze that was on before the move stays on
		if oc, _, err := s.dbCreds(ctx, db); err == nil {
			if cmd, err := databases.SetReadOnly(db.Engine, st.OldShared, oc, false); err == nil {
				if _, err := s.execOutput(ctx, st.OldService, cmd, time.Minute); err != nil {
					slog.Warn("allow writes again", "db", db.Name, "err", err)
					msg += " (writes could not be re-enabled: " + err.Error() + ")"
				}
			}
		}
	}
	// ready first: a linked service that starts gets its DATABASE_URL only from a ready database.
	_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", msg)
	s.startStopped(ctx, st.Stopped)
	_ = s.store.SetMoveState(ctx, db.ID, nil)
}

// finishMove runs steps 6-7 after the flip (db already points at the new server): the linked
// services start again one by one, the old server is cleaned, move_state goes.
func (s *Server) finishMove(ctx context.Context, db *store.Database, st *store.MoveState) {
	if db.Options.ReadOnly { // over quota before the move: the new database is not frozen yet
		if oc, _, err := s.dbCreds(ctx, db); err == nil {
			if cmd, err := databases.SetReadOnly(db.Engine, db.Mode == "shared", oc, true); err == nil {
				_, _ = s.execOutput(ctx, st.NewService, cmd, time.Minute)
			}
		}
	}
	_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", "")
	versionCache.Delete(db.ID)
	s.startStopped(ctx, st.Stopped)
	// No way back: the old database goes now, with its users.
	if err := s.dropPlacement(ctx, db, st.OldService, st.OldShared); err != nil {
		slog.Warn("remove the old database server", "db", db.Name, "err", err)
		_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", "upgraded, but the old database server could not be removed: "+err.Error())
	}
	_ = s.store.SetMoveState(ctx, db.ID, nil)
}

// startStopped starts the services a move stopped, one at a time, each after the previous came up.
func (s *Server) startStopped(ctx context.Context, ids []string) {
	for _, id := range ids {
		svc, err := s.store.ServiceByID(ctx, id)
		if err != nil {
			continue
		}
		if err := s.startService(ctx, svc); err != nil {
			slog.Warn("start linked service", "service", svc.Slug, "err", err)
			continue
		}
		if err := s.waitServiceRunning(ctx, id, linkedStartWait); err != nil {
			slog.Warn("linked service did not come up", "service", svc.Slug, "err", err)
		}
	}
}

// resumeMove finishes or undoes a move that paasd was restarted in the middle of.
func (s *Server) resumeMove(ctx context.Context, dbID string) {
	unlock := s.lock("db-" + dbID)
	defer unlock()
	db, err := s.store.DatabaseByID(ctx, dbID)
	if err != nil || db.MoveState == nil {
		return
	}
	if db.MoveState.Phase == store.MoveFlip {
		s.finishMove(ctx, db, db.MoveState)
		return
	}
	s.abortMove(ctx, db, db.MoveState, "operation interrupted by a control plane restart")
}

// POST /api/v1/databases/{id}/version {version, confirm_downtime}: a one-way major upgrade.
func (s *Server) handleDatabaseVersion(w http.ResponseWriter, r *http.Request) {
	db, p := s.databaseFor(w, r, "admin")
	if db == nil {
		return
	}
	var in struct {
		Version         string `json:"version"`
		ConfirmDowntime bool   `json:"confirm_downtime"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	switch {
	case db.Engine == databases.Redis:
		writeError(w, 409, "redis cannot be upgraded: it has no restore, and a newer RDB file cannot be loaded by an older server")
		return
	case db.Options.HA || db.Options.PITR:
		writeError(w, 409, "turn high availability and point-in-time recovery off first, then enable them again after the upgrade")
		return
	case db.Options.Pooler:
		writeError(w, 409, "turn the connection pooler off first, then enable it again after the upgrade")
		return
	}
	if _, err := databases.CheckUpgrade(db.Engine, db.Version, in.Version, time.Now()); err != nil {
		var eol *databases.ErrEOL
		if errors.As(err, &eol) {
			writeError(w, 409, err.Error())
			return
		}
		writeError(w, 400, err.Error())
		return
	}
	if !in.ConfirmDowntime {
		writeError(w, 400, "confirm_downtime is required: the services using this database stop until the upgrade is done")
		return
	}
	if db.ServerServiceID == nil {
		writeError(w, 409, "database is not ready")
		return
	}
	if ok, err := s.store.ClaimDatabaseStatus(r.Context(), db.ID, "ready", "upgrading"); err != nil || !ok {
		writeError(w, 409, "database is not ready")
		return
	}
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, &p.TeamID, "database.version", fmt.Sprintf("%s %s -> %s", db.Name, db.Version, in.Version))
	go s.moveDatabase(context.Background(), db.ID, moveTarget{Version: in.Version, Dedicated: db.Mode != "shared"})
	writeJSON(w, 202, map[string]string{"status": "upgrading"})
}
