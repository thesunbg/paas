package server

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/databases"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

// pgPrelude runs before the PostgreSQL entrypoint of dedicated servers: it
// prepares the WAL archive directory and allows replication connections
// (password-authenticated) for standbys.
const pgPrelude = `set -e
mkdir -p /docker-entrypoint-initdb.d
echo 'grep -q "^host replication" "$PGDATA/pg_hba.conf" || echo "host replication all all scram-sha-256" >> "$PGDATA/pg_hba.conf"' > /docker-entrypoint-initdb.d/10-replication.sh
if [ -s "$PGDATA/PG_VERSION" ]; then grep -q "^host replication" "$PGDATA/pg_hba.conf" || echo "host replication all all scram-sha-256" >> "$PGDATA/pg_hba.conf"; fi
if [ -d /wal ]; then mkdir -p /wal/archive && chown -R postgres:postgres /wal; fi
exec "$@"`

const archiveCommand = "test ! -f /wal/archive/%f && cp %p /wal/archive/.%f.tmp && mv /wal/archive/.%f.tmp /wal/archive/%f"

// dedicatedSpec is the server spec of a dedicated database, including the
// DBaaS options (WAL archiving for point-in-time recovery, standby support).
func (s *Server) dedicatedSpec(db *store.Database, c databases.Credentials) (model.ServiceSpec, error) {
	preset, ok := databases.PresetByName(db.SizePreset)
	if !ok {
		return model.ServiceSpec{}, fmt.Errorf("unknown size %q", db.SizePreset)
	}
	spec, err := databases.ServerSpec(db.Engine, db.Version, preset, false, c)
	if err != nil {
		return spec, err
	}
	if db.Engine == databases.Postgres {
		args := append([]string{}, spec.Command...)
		args = append(args, "-c", "wal_level=replica", "-c", "wal_keep_size=1024", "-c", "max_wal_senders=5", "-c", "hot_standby=on")
		if db.Options.PITR {
			spec.Volumes = append(spec.Volumes, model.Volume{Name: "wal", Path: "/wal"})
			// The spool is drained to S3 (walShipTick); the dot-prefixed temp file keeps it from shipping a partial segment.
			args = append(args, "-c", "archive_mode=on",
				"-c", "archive_command="+archiveCommand,
				"-c", "archive_timeout=300")
		}
		if db.Options.HA {
			// The agent fences this container when it loses the control plane. wal_sender_timeout is the
			// "walsender notices a lost standby" step of the failover timing proof (dbaas_failover.go).
			spec.HAPostgres = true
			args = append(args, "-c", fmt.Sprintf("wal_sender_timeout=%ds", int(proto.WalSenderTimeout.Seconds())),
				"-c", fmt.Sprintf("wal_receiver_timeout=%ds", int(proto.WalSenderTimeout.Seconds())))
		}
		spec.Command = append([]string{"sh", "-c", pgPrelude, "sh"}, args...)
	}
	return spec, nil
}

// applyServerSpec rewrites a dedicated server's spec and redeploys it.
func (s *Server) applyServerSpec(ctx context.Context, db *store.Database) error {
	if db.ServerServiceID == nil {
		return fmt.Errorf("the database server is not provisioned")
	}
	srv, err := s.store.ServiceByID(ctx, *db.ServerServiceID)
	if err != nil {
		return err
	}
	c, _, err := s.dbCreds(ctx, db)
	if err != nil {
		return err
	}
	spec, err := s.dedicatedSpec(db, c)
	if err != nil {
		return err
	}
	fresh, err := s.store.UpdateService(ctx, srv.ID, func(f *store.Service) error {
		spec.DNSAliases = f.Spec.DNSAliases
		spec.Image = keepPin(specImage(f), spec.Image) // a resize must not unpin the binary
		f.Spec = spec
		f.Source.Image = spec.Image
		return nil
	})
	if err != nil {
		return err
	}
	srv = fresh
	if _, err := s.restartService(ctx, srv, nil); err != nil {
		return err
	}
	return s.waitServiceRunning(ctx, srv.ID, 10*time.Minute)
}

// PUT /api/v1/databases/{id}/size {size}
func (s *Server) handleDatabaseResize(w http.ResponseWriter, r *http.Request) {
	db, p := s.databaseFor(w, r, "admin")
	if db == nil {
		return
	}
	var in struct {
		Size string `json:"size"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	preset, ok := databases.PresetByName(in.Size)
	if !ok {
		writeError(w, 400, "unknown size")
		return
	}
	if db.Mode != "dedicated" {
		writeError(w, 400, "shared databases are resized by upgrading them to a dedicated server")
		return
	}
	if db.Status != "ready" {
		writeError(w, 409, "the database is not ready")
		return
	}
	if l := s.teamLimits(r.Context()); limited(r) && l.MaxMemoryMB > 0 && preset.MemoryMB > l.MaxMemoryMB {
		writeError(w, 403, fmt.Sprintf("memory is limited to %d MB per instance", l.MaxMemoryMB))
		return
	}
	if err := s.store.SetDatabaseSize(r.Context(), db.ID, in.Size); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	db.SizePreset = in.Size
	_ = s.store.SetDatabaseStatus(r.Context(), db.ID, "upgrading", "")
	go func() {
		ctx := context.Background()
		unlock := s.lock("db-" + db.ID)
		defer unlock()
		if err := s.applyServerSpec(ctx, db); err != nil {
			_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", "resize failed: "+err.Error())
			return
		}
		_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", "")
	}()
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, &p.TeamID, "database.resize", db.Name+" "+in.Size)
	writeJSON(w, 202, map[string]string{"status": "upgrading"})
}

var dbUserRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,30}$`)

// GET /api/v1/databases/{id}/users
func (s *Server) handleListDBUsers(w http.ResponseWriter, r *http.Request) {
	db, _ := s.databaseFor(w, r, "developer")
	if db == nil {
		return
	}
	users, err := s.store.DBUsers(r.Context(), db.ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	type out struct {
		store.DBUser
		Password string `json:"password"`
		URL      string `json:"url"`
	}
	c, _, _ := s.dbCreds(r.Context(), db)
	res := []out{}
	for _, u := range users {
		pass, _ := s.box.Decrypt(u.PasswordEnc)
		uc := c
		uc.Username, uc.Password = u.Username, pass
		res = append(res, out{DBUser: u, Password: pass, URL: databases.URL(db.Engine, uc)})
	}
	writeJSON(w, 200, res)
}

// POST /api/v1/databases/{id}/users {name, role}
func (s *Server) handleCreateDBUser(w http.ResponseWriter, r *http.Request) {
	db, p := s.databaseFor(w, r, "admin")
	if db == nil {
		return
	}
	var in struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	in.Name = strings.ToLower(strings.TrimSpace(in.Name))
	if !dbUserRe.MatchString(in.Name) {
		writeError(w, 400, "name: 2-31 lowercase letters, digits or _, starting with a letter")
		return
	}
	if db.Status != "ready" {
		writeError(w, 409, "the database is not ready")
		return
	}
	// Roles are global on a server: prefix with the owner so names never clash.
	username := db.Username + "_" + in.Name
	if len(username) > 60 {
		username = username[:60]
	}
	c, srv, err := s.dbCreds(r.Context(), db)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	pass := util.RandomPassword(24)
	cmd, err := databases.CreateUser(db.Engine, db.Mode == "shared", c, username, pass, in.Role)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if _, err := s.execOutput(r.Context(), srv.ID, cmd, 30*time.Second); err != nil {
		writeError(w, 500, "create user: "+err.Error())
		return
	}
	if err := s.store.CreateDBUser(r.Context(), store.DBUser{DatabaseID: db.ID, Username: username, Role: in.Role, PasswordEnc: s.box.MustEncrypt(pass)}); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, &p.TeamID, "database.user.create", db.Name+" "+username)
	uc := c
	uc.Username, uc.Password = username, pass
	writeJSON(w, 201, map[string]string{"username": username, "role": in.Role, "password": pass, "url": databases.URL(db.Engine, uc)})
}

// DELETE /api/v1/databases/{id}/users/{user}
func (s *Server) handleDeleteDBUser(w http.ResponseWriter, r *http.Request) {
	db, p := s.databaseFor(w, r, "admin")
	if db == nil {
		return
	}
	du, err := s.store.DBUser(r.Context(), db.ID, r.PathValue("user"))
	if err != nil {
		writeError(w, 404, "user not found")
		return
	}
	c, srv, err := s.dbCreds(r.Context(), db)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	cmd, err := databases.DropUser(db.Engine, db.Mode == "shared", c, du.Username)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if _, err := s.execOutput(r.Context(), srv.ID, cmd, 30*time.Second); err != nil {
		writeError(w, 500, "drop user: "+err.Error())
		return
	}
	_ = s.store.DeleteDBUser(r.Context(), db.ID, du.Username)
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, &p.TeamID, "database.user.delete", db.Name+" "+du.Username)
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// POST /api/v1/databases/{id}/users/{user}/password rotates an extra user's password.
func (s *Server) handleRotateDBUser(w http.ResponseWriter, r *http.Request) {
	db, _ := s.databaseFor(w, r, "admin")
	if db == nil {
		return
	}
	du, err := s.store.DBUser(r.Context(), db.ID, r.PathValue("user"))
	if err != nil {
		writeError(w, 404, "user not found")
		return
	}
	c, srv, err := s.dbCreds(r.Context(), db)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	pass := util.RandomPassword(24)
	cmd, err := databases.SetPassword(db.Engine, db.Mode == "shared", c, du.Username, pass)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if _, err := s.execOutput(r.Context(), srv.ID, cmd, 30*time.Second); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	_ = s.store.SetDBUserPassword(r.Context(), db.ID, du.Username, s.box.MustEncrypt(pass))
	writeJSON(w, 200, map[string]string{"username": du.Username, "password": pass})
}
