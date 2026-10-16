package server

import (
	"context"
	"errors"
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
)

// Per-database usage: size and connections sampled every minute, kept 7 days,
// and a storage quota that turns the database read-only while it is exceeded.

const dbMetricsRetention = 7 * 24 * time.Hour

// execOutput runs a short command in the first instance of a service and returns stdout.
func (s *Server) execOutput(ctx context.Context, serviceID string, cmd databases.Command, timeout time.Duration) (string, error) {
	in, err := s.firstInstance(ctx, serviceID)
	if err != nil {
		return "", err
	}
	var rep proto.ExecReply
	err = s.requestAgent(ctx, in.NodeID, proto.CmdExec, proto.ExecRequest{
		ContainerID: in.ContainerID, Cmd: cmd.Cmd, Env: cmd.Env, Stdin: cmd.Stdin, TimeoutSec: int(timeout.Seconds()),
	}, &rep, timeout+10*time.Second)
	if err != nil {
		return "", err
	}
	if rep.Error != "" {
		return "", errors.New(rep.Error)
	}
	if rep.ExitCode != 0 {
		return rep.Stdout, fmt.Errorf("exit code %d: %s", rep.ExitCode, strings.TrimSpace(rep.Stderr))
	}
	return rep.Stdout, nil
}

// quotaMB is the effective storage quota of a database.
func quotaMB(db *store.Database) int {
	if db.Options.QuotaMB > 0 {
		return db.Options.QuotaMB
	}
	if db.Mode == "shared" {
		return databases.SharedQuotaMB
	}
	if p, ok := databases.PresetByName(db.SizePreset); ok {
		return p.QuotaMB
	}
	return databases.SharedQuotaMB
}

func (s *Server) dbMetricsLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.sampleDatabases(ctx)
		if i%60 == 0 {
			s.store.PruneDBMetrics(ctx, time.Now().Add(-dbMetricsRetention))
		}
	}
}

func (s *Server) sampleDatabases(ctx context.Context) {
	dbs, err := s.store.AllDatabases(ctx)
	if err != nil {
		return
	}
	now := time.Now().Truncate(time.Minute)
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i := range dbs {
		db := &dbs[i]
		if db.Status != "ready" || db.ServerServiceID == nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			s.sampleDatabase(ctx, db, now)
		}()
	}
	wg.Wait()
}

func (s *Server) sampleDatabase(ctx context.Context, db *store.Database, now time.Time) {
	c, srv, err := s.dbCreds(ctx, db)
	if err != nil {
		return
	}
	cmd, err := databases.Stats(db.Engine, db.Mode == "shared", c)
	if err != nil {
		return
	}
	out, err := s.execOutput(ctx, srv.ID, cmd, 30*time.Second)
	if err != nil {
		slog.Debug("database stats", "db", db.Name, "err", err)
		return
	}
	size, conns, err := databases.ParseStats(out)
	if err != nil {
		slog.Debug("database stats", "db", db.Name, "err", err)
		return
	}
	_ = s.store.InsertDBMetric(ctx, db.ID, store.DBMetric{TS: now, SizeBytes: size, Connections: conns})
	s.enforceQuota(ctx, db, c, srv.ID, size)
}

// enforceQuota switches the database to read-only above its quota and back once
// it is under 90% of it (PostgreSQL and MySQL; other engines are only reported).
func (s *Server) enforceQuota(ctx context.Context, db *store.Database, c databases.Credentials, serverID string, size int64) {
	if db.Engine != databases.Postgres && db.Engine != databases.MySQL {
		return
	}
	limit := int64(quotaMB(db)) << 20
	over := size > limit
	switch {
	case over && !db.Options.ReadOnly:
	case db.Options.ReadOnly && size < limit*9/10:
	default:
		return
	}
	cmd, err := databases.SetReadOnly(db.Engine, db.Mode == "shared", c, over)
	if err != nil {
		return
	}
	if _, err := s.execOutput(ctx, serverID, cmd, 30*time.Second); err != nil {
		slog.Warn("database quota", "db", db.Name, "err", err)
		return
	}
	o := db.Options
	o.ReadOnly = over
	_ = s.store.UpdateDatabaseOptions(ctx, db.ID, o)
	team := s.teamOfDatabase(ctx, db)
	if over {
		slog.Warn("database over quota: read-only", "db", db.Name, "size_mb", size>>20, "quota_mb", quotaMB(db))
		s.notify(ctx, team, "database.quota", fmt.Sprintf("Database %s uses %d MB of its %d MB quota and is now read-only. Delete data or ask for a larger quota.", db.Name, size>>20, quotaMB(db)), map[string]any{"database": db.ID})
	} else {
		s.notify(ctx, team, "database.quota", fmt.Sprintf("Database %s is back under its quota; writes are allowed again.", db.Name), map[string]any{"database": db.ID})
	}
}

func (s *Server) teamOfDatabase(ctx context.Context, db *store.Database) string {
	p, err := s.store.ProjectByID(ctx, db.ProjectID)
	if err != nil {
		return ""
	}
	return p.TeamID
}

// GET /api/v1/databases/{id}/metrics?hours=24
func (s *Server) handleDatabaseMetrics(w http.ResponseWriter, r *http.Request) {
	db, _ := s.databaseFor(w, r, "viewer")
	if db == nil {
		return
	}
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	if hours <= 0 || hours > 168 {
		hours = 24
	}
	points, err := s.store.DBMetrics(r.Context(), db.ID, time.Now().Add(-time.Duration(hours)*time.Hour))
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	maxConns := 0
	if p, ok := databases.PresetByName(db.SizePreset); ok {
		maxConns = p.MaxConns
		if db.Mode == "shared" {
			maxConns = p.MaxConns/2 + 5
		}
	}
	writeJSON(w, 200, map[string]any{"points": points, "quota_mb": quotaMB(db), "read_only": db.Options.ReadOnly, "max_connections": maxConns})
}

// PUT /api/v1/databases/{id}/quota {quota_mb} (administrators: 0 restores the default)
func (s *Server) handleDatabaseQuota(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	db, err := s.store.DatabaseByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "database not found")
		return
	}
	var in struct {
		QuotaMB int `json:"quota_mb"`
	}
	if err := decode(r, &in); err != nil || in.QuotaMB < 0 {
		writeError(w, 400, "quota_mb must be 0 or more")
		return
	}
	o := db.Options
	o.QuotaMB = in.QuotaMB
	if err := s.store.UpdateDatabaseOptions(r.Context(), db.ID, o); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	db.Options = o
	writeJSON(w, 200, map[string]int{"quota_mb": quotaMB(db)})
}
