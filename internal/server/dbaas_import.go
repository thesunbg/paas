package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/databases"
)

// POST /api/v1/databases/{id}/import {source_url}
// Copies a database from another server (the old host of an application) into
// this one. The dump runs inside the database server, so the source must be
// reachable from the platform. The source URL is used once and never stored.
func (s *Server) handleDatabaseImport(w http.ResponseWriter, r *http.Request) {
	db, p := s.databaseFor(w, r, "admin")
	if db == nil {
		return
	}
	var in struct {
		SourceURL string `json:"source_url"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	in.SourceURL = strings.TrimSpace(in.SourceURL)
	if db.Status != "ready" {
		writeError(w, 409, "the database is not ready")
		return
	}
	c, srv, err := s.dbCreds(r.Context(), db)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	cmd, err := databases.ImportFrom(db.Engine, db.Mode == "shared", c, in.SourceURL)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	u, _ := url.Parse(in.SourceURL)
	_ = s.store.SetDatabaseStatus(r.Context(), db.ID, "importing", "")
	go func() {
		ctx := context.Background()
		unlock := s.lock("db-" + db.ID)
		defer unlock()
		// A safety copy first: an import replaces what the database holds.
		if _, err := s.backupDatabase(ctx, db); err != nil {
			_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", "import cancelled: the backup taken before it failed: "+err.Error())
			return
		}
		out, err := s.execOutput(ctx, srv.ID, cmd, 3*time.Hour)
		msg := ""
		if err != nil {
			msg = importError(err, out)
		}
		_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", msg)
	}()
	user := currentUser(r)
	s.store.Audit(r.Context(), &user.ID, &p.TeamID, "database.import", db.Name+" from "+u.Hostname())
	writeJSON(w, 202, map[string]string{"status": "importing"})
}

func importError(err error, _ string) string {
	text := err.Error()
	if strings.Contains(text, fmt.Sprintf("exit code %d", databases.ImportExitDump)) {
		hint := ""
		if strings.Contains(text, "server version mismatch") || strings.Contains(text, "aborting because of server version mismatch") {
			hint = " The source server is newer than this one: create the database with the same or a newer version."
		}
		return "import failed: could not read the source database: " + trimTail(text, 600) + hint
	}
	// pg_restore and mongorestore exit non-zero for single objects that could
	// not be recreated (extensions, comments); the rest is in place.
	return "import finished with errors: " + trimTail(text, 600)
}

func trimTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
