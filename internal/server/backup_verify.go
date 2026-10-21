package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thesunbg/paas/internal/databases"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

// A backup nobody ever restored is a hope, not a backup. Once a week the
// leader restores each database's newest dump into a scratch database on the
// same server, counts what came back and drops it (databases.Verify), one
// database at a time. It also warns when a database has gone two schedule
// periods without a successful backup.

const (
	verifyEvery = 7 * 24 * time.Hour
	staleAlert  = 24 * time.Hour // repeat a "no recent backup" alert at most daily
)

type backupWatch struct {
	mu        sync.Mutex
	verifying bool
	alerted   map[string]time.Time // database ID -> last stale alert
}

// backupHealth summarises a database's backups for the dashboard.
type backupHealth struct {
	State        string     `json:"state"` // ok | stale | failing | none | off
	LastSuccess  *time.Time `json:"last_success,omitempty"`
	LastVerified *time.Time `json:"last_verified,omitempty"`
	VerifyOK     *bool      `json:"verify_ok,omitempty"`
	VerifyNote   string     `json:"verify_note,omitempty"`
	Offsite      bool       `json:"offsite"`
	Message      string     `json:"message,omitempty"`
}

func (s *Server) backupHealth(ctx context.Context, db *store.Database) backupHealth {
	h := backupHealth{State: "ok"}
	latest, err := s.store.LatestBackup(ctx, db.ID)
	if err == nil {
		t := latest.CreatedAt
		h.LastSuccess = &t
		h.Offsite = latest.S3Key != ""
	}
	if v, err := s.store.LastVerified(ctx, db.ID); err == nil && v != nil {
		h.LastVerified = v
		if bs, err := s.store.BackupsForDatabase(ctx, db.ID); err == nil {
			for _, b := range bs {
				if b.VerifiedAt != nil && b.VerifiedAt.Equal(*v) {
					h.VerifyOK, h.VerifyNote = b.VerifyOK, b.VerifyNote
					break
				}
			}
		}
	}
	period := time.Duration(db.BackupScheduleHours) * time.Hour
	switch {
	case db.BackupScheduleHours <= 0:
		h.State, h.Message = "off", "Scheduled backups are off"
	case h.LastSuccess == nil && time.Since(db.CreatedAt) > period+time.Hour:
		h.State, h.Message = "none", "No successful backup yet"
	case h.LastSuccess != nil && time.Since(*h.LastSuccess) > 2*period+time.Hour:
		h.State, h.Message = "stale", fmt.Sprintf("Last successful backup %s ago", time.Since(*h.LastSuccess).Round(time.Hour))
	case h.VerifyOK != nil && !*h.VerifyOK:
		h.State, h.Message = "failing", "The last restore check failed: "+h.VerifyNote
	}
	if h.State == "ok" && !h.Offsite && h.LastSuccess != nil {
		h.Message = "Stored on the database's node only: configure off-site storage"
	}
	return h
}

// backupWatchTick runs every minute on the leader (maintenanceLoop).
func (s *Server) backupWatchTick(ctx context.Context) {
	dbs, err := s.store.AllDatabases(ctx)
	if err != nil {
		return
	}
	w := &s.bwatch
	w.mu.Lock()
	if w.alerted == nil {
		w.alerted = map[string]time.Time{}
	}
	w.mu.Unlock()
	var due *store.Database
	for i := range dbs {
		db := &dbs[i]
		if db.Status != "ready" {
			continue
		}
		h := s.backupHealth(ctx, db)
		if h.State == "stale" || h.State == "none" {
			w.mu.Lock()
			last := w.alerted[db.ID]
			if time.Since(last) > staleAlert {
				w.alerted[db.ID] = time.Now()
				w.mu.Unlock()
				s.notify(ctx, s.teamOfDatabase(ctx, db), "backup.failed", fmt.Sprintf("Database %s: %s", db.Name, strings.ToLower(h.Message)), map[string]any{"database": db.ID})
			} else {
				w.mu.Unlock()
			}
		}
		if due == nil && h.LastSuccess != nil && (h.LastVerified == nil || time.Since(*h.LastVerified) > verifyEvery) {
			due = db
		}
	}
	if due == nil {
		return
	}
	w.mu.Lock()
	if w.verifying {
		w.mu.Unlock()
		return
	}
	w.verifying = true
	w.mu.Unlock()
	go func() {
		defer func() { w.mu.Lock(); w.verifying = false; w.mu.Unlock() }()
		if _, err := s.verifyBackup(context.WithoutCancel(ctx), due); err != nil {
			slog.Warn("backup restore check", "db", due.Name, "err", err)
		}
	}()
}

// verifyBackup restores the newest dump of db into a scratch database and records the result.
func (s *Server) verifyBackup(ctx context.Context, db *store.Database) (*store.Backup, error) {
	b, err := s.store.LatestBackup(ctx, db.ID)
	if err != nil {
		return nil, fmt.Errorf("no completed backup to check")
	}
	unlock := s.lock("db-" + db.ID)
	defer unlock()
	ok, note := true, ""
	n, err := s.runVerify(ctx, db, b)
	if err != nil {
		ok, note = false, err.Error()
		s.notify(ctx, s.teamOfDatabase(ctx, db), "backup.failed", fmt.Sprintf("Database %s: restoring its latest backup failed: %v", db.Name, err), map[string]any{"database": db.ID})
	} else {
		unit := "tables"
		switch db.Engine {
		case databases.Mongo:
			unit = "collections"
		case databases.Redis:
			unit = "keys"
		}
		note = fmt.Sprintf("restored %d %s", n, unit)
	}
	_ = s.store.SetBackupVerified(ctx, b.ID, ok, note)
	b.VerifyOK, b.VerifyNote = &ok, note
	return b, err
}

func (s *Server) runVerify(ctx context.Context, db *store.Database, b *store.Backup) (int, error) {
	c, srv, err := s.dbCreds(ctx, db)
	if err != nil {
		return 0, err
	}
	in, err := s.firstInstance(ctx, srv.ID)
	if err != nil {
		return 0, err
	}
	if b.NodeID == nil || *b.NodeID != in.NodeID {
		if b.S3Key == "" || !s.storage().Enabled() {
			return 0, fmt.Errorf("the backup file is on another node and there is no off-site copy")
		}
		if err := s.transfer(ctx, in.NodeID, http.MethodGet, b.S3Key, b.Path); err != nil {
			return 0, fmt.Errorf("download the off-site copy: %w", err)
		}
	}
	scratch := "paas_verify_" + strings.ToLower(util.RandomToken(6))
	cmd, err := databases.Verify(db.Engine, db.Mode == "shared", c, scratch)
	if err != nil {
		return 0, err
	}
	var rep proto.ExecReply
	err = s.requestAgent(ctx, in.NodeID, proto.CmdExec, proto.ExecRequest{
		ContainerID: in.ContainerID, Cmd: cmd.Cmd, Env: cmd.Env, StdinFile: b.Path, TimeoutSec: 3600,
	}, &rep, time.Hour+time.Minute)
	if err == nil && rep.Error != "" {
		err = fmt.Errorf("%s", rep.Error)
	}
	if err == nil && rep.ExitCode != 0 {
		err = fmt.Errorf("exit code %d: %s", rep.ExitCode, strings.TrimSpace(rep.Stderr))
	}
	if err != nil {
		return 0, err
	}
	lines := strings.Fields(strings.TrimSpace(rep.Stdout))
	if len(lines) == 0 {
		return 0, fmt.Errorf("the check printed nothing")
	}
	n, err := strconv.Atoi(lines[len(lines)-1])
	if err != nil {
		return 0, fmt.Errorf("unexpected output %q", lines[len(lines)-1])
	}
	if n == 0 && db.Engine != databases.Redis {
		return 0, fmt.Errorf("the restored database is empty")
	}
	return n, nil
}

// POST /api/v1/databases/{id}/backups/verify: run the restore check now.
func (s *Server) handleVerifyBackup(w http.ResponseWriter, r *http.Request) {
	db, p := s.databaseFor(w, r, "developer")
	if db == nil {
		return
	}
	b, err := s.verifyBackup(context.WithoutCancel(r.Context()), db)
	if b == nil {
		writeError(w, 400, err.Error())
		return
	}
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, &p.TeamID, "database.backup.verify", db.Name)
	writeJSON(w, 200, b)
}
