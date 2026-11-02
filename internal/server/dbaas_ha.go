package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/databases"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

// High availability for dedicated PostgreSQL: a hot standby streams from the
// primary on another node. Clients use a stable alias (<server>-rw.<project>.internal)
// that the internal DNS resolves to whichever server is primary. When the
// primary's node stops responding (and the standby confirms the primary is gone,
// see dbaas_failover.go) the standby is promoted, the alias moves to it, the old
// primary is deleted (its container is stopped if the node comes back, its volume
// kept for a week) and a new standby is created as soon as another node is available.

// replicaPrelude clones the primary on first start, then runs PostgreSQL as a standby.
const replicaPrelude = `set -e
mkdir -p /docker-entrypoint-initdb.d
if [ -d /wal ]; then mkdir -p /wal/archive && chown -R postgres:postgres /wal; fi
if [ ! -s "$PGDATA/PG_VERSION" ]; then
  mkdir -p "$PGDATA" && chmod 700 "$PGDATA"
  until PGPASSWORD="$REPL_PASSWORD" pg_basebackup -h "$PRIMARY_HOST" -p 5432 -U "$REPL_USER" -D "$PGDATA" -X stream -R -c fast; do
    echo "waiting for the primary at $PRIMARY_HOST"; rm -rf "$PGDATA"/*; sleep 5
  done
  chown -R postgres:postgres "$PGDATA"
fi
grep -q "^host replication" "$PGDATA/pg_hba.conf" || echo "host replication all all scram-sha-256" >> "$PGDATA/pg_hba.conf"
exec "$@"`

func haCapable(db *store.Database) bool {
	return db.Engine == databases.Postgres && db.Mode == "dedicated"
}

// pickReplicaNode chooses an online worker node other than the primary's.
func (s *Server) pickReplicaNode(ctx context.Context, avoid string, needMB int) (*store.Node, error) {
	nodes, err := s.store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	var cands []store.Node
	for _, n := range nodes {
		if n.ID == avoid || n.Status != "online" || !hasRole(n.Roles, "worker") {
			continue
		}
		cands = append(cands, n)
	}
	if len(cands) == 0 {
		return nil, errors.New("high availability needs a second online worker node")
	}
	free := func(n store.Node) int { return n.Usage.MemoryAvailableMB }
	sort.Slice(cands, func(i, j int) bool { return free(cands[i]) > free(cands[j]) })
	if free(cands[0]) < needMB {
		slog.Warn("replica node is short on memory", "node", cands[0].Name)
	}
	return &cands[0], nil
}

func hasRole(roles []string, r string) bool {
	for _, x := range roles {
		if x == r {
			return true
		}
	}
	return false
}

// primaryNode is the node running a server's instance (or its pin).
func (s *Server) primaryNode(ctx context.Context, srv *store.Service) string {
	if srv.NodeID != nil {
		return *srv.NodeID
	}
	if in, err := s.firstInstance(ctx, srv.ID); err == nil {
		return in.NodeID
	}
	return ""
}

func (s *Server) createReplica(ctx context.Context, db *store.Database, primary *store.Service) (*store.Service, error) {
	pNode := s.primaryNode(ctx, primary)
	if pNode == "" {
		return nil, errors.New("the primary is not running")
	}
	if primary.NodeID == nil { // keep the primary where its data is
		primary.NodeID = &pNode
		_, _ = s.store.Q(ctx).Exec(ctx, `UPDATE services SET node_id=$2 WHERE id=$1`, primary.ID, pNode)
	}
	c, _, err := s.dbCreds(ctx, db)
	if err != nil {
		return nil, err
	}
	node, err := s.pickReplicaNode(ctx, pNode, primary.Spec.Resources.MemoryLimitMB)
	if err != nil {
		return nil, err
	}
	spec, err := s.dedicatedSpec(db, c)
	if err != nil {
		return nil, err
	}
	if len(spec.Command) < 4 {
		return nil, errors.New("unexpected server command")
	}
	spec.Command = append([]string{"sh", "-c", replicaPrelude, "sh"}, spec.Command[4:]...)
	for i, a := range spec.Command { // a PITR standby archives too, so WAL survives the primary's node
		if a == "archive_mode=on" {
			spec.Command[i] = "archive_mode=always"
		}
	}
	proj, err := s.store.ProjectByID(ctx, primary.ProjectID)
	if err != nil {
		return nil, err
	}
	spec.Image = keepPin(specImage(primary), spec.Image) // same binary as the primary
	spec.Env["PRIMARY_HOST"] = internalHost(primary.Slug, proj.Slug)
	spec.Env["REPL_USER"] = c.Username
	spec.Env["REPL_PASSWORD"] = c.Password
	spec.HealthCheck.TimeoutSec = 1800 // the first start copies the whole database
	spec.DNSAliases = nil
	slug := util.Slugify(db.Name + "-" + util.RandomToken(2))
	rep, err := s.store.CreateService(ctx, &store.Service{
		ProjectID: primary.ProjectID, Name: db.Name + " (standby)", Slug: slug, Type: model.TypeDatabase,
		Source: model.ServiceSource{Kind: "image", Image: spec.Image}, Spec: spec, WebhookSecret: util.RandomToken(16), NodeID: &node.ID,
	})
	if err != nil {
		return nil, err
	}
	if _, err := s.QueueDeployment(ctx, rep, "system", "", "", nil); err != nil {
		return rep, err
	}
	return rep, s.waitServiceRunning(ctx, rep.ID, 30*time.Minute)
}

// ensureAlias gives the primary the stable read-write alias.
func (s *Server) ensureAlias(ctx context.Context, srv *store.Service) (string, error) {
	if len(srv.Spec.DNSAliases) > 0 {
		return srv.Spec.DNSAliases[0], nil
	}
	proj, err := s.store.ProjectByID(ctx, srv.ProjectID)
	if err != nil {
		return "", err
	}
	alias := internalHost(srv.Slug+"-rw", proj.Slug)
	fresh, err := s.store.UpdateService(ctx, srv.ID, func(f *store.Service) error {
		f.Spec.DNSAliases = []string{alias}
		return nil
	})
	if err != nil {
		return alias, err
	}
	*srv = *fresh
	return alias, nil
}

// POST /api/v1/databases/{id}/ha {enabled}
func (s *Server) handleSetHA(w http.ResponseWriter, r *http.Request) {
	db, p := s.databaseFor(w, r, "admin")
	if db == nil {
		return
	}
	if !haCapable(db) || db.ServerServiceID == nil {
		writeError(w, 400, "high availability needs a dedicated PostgreSQL database")
		return
	}
	if db.Status != "ready" {
		writeError(w, 409, "the database is not ready")
		return
	}
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if in.Enabled && !db.Options.HA {
		withHA := *db
		withHA.Options.HA = true
		if err := s.checkQuota(r.Context(), p.TeamID, dbUsage(&withHA).minus(dbUsage(db))); err != nil {
			writeLimitError(w, err)
			return
		}
		srv, err := s.store.ServiceByID(r.Context(), *db.ServerServiceID)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		if _, err := s.pickReplicaNode(r.Context(), s.primaryNode(r.Context(), srv), 0); err != nil {
			writeError(w, 409, err.Error())
			return
		}
	}
	if _, err := s.store.UpdateDatabaseOptions(r.Context(), db.ID, func(o *store.DBOptions) error {
		o.HA = in.Enabled
		return nil
	}); err != nil {
		writeUpdateError(w, err)
		return
	}
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, &p.TeamID, "database.ha", fmt.Sprintf("%s %v", db.Name, in.Enabled))
	go func() {
		ctx := context.Background()
		unlock := s.lock("db-" + db.ID)
		defer unlock()
		db, err := s.store.DatabaseByID(ctx, db.ID)
		if err != nil {
			return
		}
		if in.Enabled {
			// The primary needs the HA label and WAL timeouts of dedicatedSpec: restart it with them.
			if err := s.syncHASpec(ctx, db); err != nil {
				_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", "high availability spec: "+err.Error())
				return
			}
			if err := s.ensureStandby(ctx, db); err != nil {
				_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", "standby: "+err.Error())
			}
			return
		}
		s.removeStandby(ctx, db)
		// Without a standby the lease fence would only take the database down when the control plane is away.
		if err := s.syncHASpec(ctx, db); err != nil {
			_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", "high availability spec: "+err.Error())
		}
	}()
	writeJSON(w, 202, map[string]any{"ha": in.Enabled})
}

// syncHASpec redeploys the primary when its spec's HA flag differs from the database's HA option.
func (s *Server) syncHASpec(ctx context.Context, db *store.Database) error {
	srv, err := s.store.ServiceByID(ctx, *db.ServerServiceID)
	if err != nil || srv.Spec.HAPostgres == db.Options.HA {
		return err
	}
	return s.applyServerSpec(ctx, db)
}

// ensureStandby gives an HA database its alias and a running standby.
func (s *Server) ensureStandby(ctx context.Context, db *store.Database) error {
	srv, err := s.store.ServiceByID(ctx, *db.ServerServiceID)
	if err != nil {
		return err
	}
	hadAlias := len(srv.Spec.DNSAliases) > 0
	if _, err := s.ensureAlias(ctx, srv); err != nil {
		return err
	}
	if !hadAlias {
		s.publishDNS(ctx)
		go s.restartLinked(ctx, db.ID) // switch apps to the alias
	}
	if db.Options.ReplicaServiceID != "" {
		if _, err := s.store.ServiceByID(ctx, db.Options.ReplicaServiceID); err == nil {
			return nil
		}
	}
	rep, err := s.createReplica(ctx, db, srv)
	if rep != nil {
		_, _ = s.store.UpdateDatabaseOptions(ctx, db.ID, func(o *store.DBOptions) error {
			o.ReplicaServiceID = rep.ID
			return nil
		})
	}
	if err == nil {
		slog.Info("standby ready", "db", db.Name, "service", rep.Slug)
		_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", "")
	}
	return err
}

func (s *Server) removeStandby(ctx context.Context, db *store.Database) {
	if db.Options.ReplicaServiceID == "" {
		return
	}
	if rep, err := s.store.ServiceByID(ctx, db.Options.ReplicaServiceID); err == nil {
		node := rep.NodeID
		_ = s.deleteService(ctx, rep)
		if node != nil {
			s.removeVolumes(ctx, *node, rep.ID)
		}
	}
	_, _ = s.store.UpdateDatabaseOptions(ctx, db.ID, func(o *store.DBOptions) error {
		o.ReplicaServiceID = ""
		return nil
	})
}

func (s *Server) haBusy(id string) bool {
	s.haMu.Lock()
	defer s.haMu.Unlock()
	if s.haRunning == nil {
		s.haRunning = map[string]time.Time{}
	}
	if t, ok := s.haRunning[id]; ok && time.Since(t) < 35*time.Minute {
		return true
	}
	s.haRunning[id] = time.Now()
	return false
}

func (s *Server) haDone(id string) {
	s.haMu.Lock()
	delete(s.haRunning, id)
	s.haMu.Unlock()
}

// POST /api/v1/databases/{id}/ha/switchover promotes the standby on purpose (node maintenance).
func (s *Server) handleSwitchover(w http.ResponseWriter, r *http.Request) {
	db, p := s.databaseFor(w, r, "admin")
	if db == nil {
		return
	}
	if !db.Options.HA || db.Options.ReplicaServiceID == "" || db.ServerServiceID == nil {
		writeError(w, 409, "this database has no standby")
		return
	}
	ctx := context.WithoutCancel(r.Context())
	unlock := s.lock("db-" + db.ID)
	defer unlock()
	srv, err := s.store.ServiceByID(ctx, *db.ServerServiceID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	c, _, _ := s.dbCreds(ctx, db)
	out, err := s.execOutput(ctx, srv.ID, databases.Command{Cmd: []string{"psql", "-U", c.Username, "-d", "postgres", "-Atc",
		"SELECT count(*) FROM pg_stat_replication WHERE state = 'streaming'"}}, 15*time.Second)
	if err != nil || strings.TrimSpace(out) == "0" {
		writeError(w, 409, "the standby is not streaming yet")
		return
	}
	if err := s.promoteStandby(ctx, db, srv, true); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	u := currentUser(r)
	s.store.Audit(ctx, &u.ID, &p.TeamID, "database.switchover", db.Name)
	writeJSON(w, 200, map[string]string{"status": "switched"})
}

// GET /api/v1/databases/{id}/ha
func (s *Server) handleGetHA(w http.ResponseWriter, r *http.Request) {
	db, _ := s.databaseFor(w, r, "viewer")
	if db == nil {
		return
	}
	out := map[string]any{"capable": haCapable(db), "enabled": db.Options.HA}
	if db.ServerServiceID != nil {
		if srv, err := s.store.ServiceByID(r.Context(), *db.ServerServiceID); err == nil {
			if n, err := s.store.NodeByID(r.Context(), s.primaryNode(r.Context(), srv)); err == nil {
				out["primary"] = map[string]any{"service": srv.Slug, "node": n.Name, "status": srv.Status}
			}
			if len(srv.Spec.DNSAliases) > 0 {
				out["alias"] = srv.Spec.DNSAliases[0]
			}
			if db.Options.ReplicaServiceID != "" {
				c, _, _ := s.dbCreds(r.Context(), db)
				q := "SELECT coalesce(string_agg(state || ' ' || coalesce(extract(epoch FROM replay_lag)::text, '0'), ','), '') FROM pg_stat_replication"
				if lag, err := s.execOutput(r.Context(), srv.ID, databases.Command{Cmd: []string{"psql", "-U", c.Username, "-d", "postgres", "-Atc", q}}, 15*time.Second); err == nil {
					out["replication"] = strings.TrimSpace(lag)
				}
			}
		}
	}
	if db.Options.ReplicaServiceID != "" {
		if rep, err := s.store.ServiceByID(r.Context(), db.Options.ReplicaServiceID); err == nil {
			st := map[string]any{"service": rep.Slug, "status": rep.Status}
			if rep.NodeID != nil {
				if n, err := s.store.NodeByID(r.Context(), *rep.NodeID); err == nil {
					st["node"] = n.Name
				}
			}
			out["standby"] = st
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) removeVolumes(ctx context.Context, nodeID, serviceID string) error {
	var rep proto.GenericReply
	if err := s.requestAgent(ctx, nodeID, proto.CmdRemoveFiles, proto.RemoveFilesRequest{Paths: []string{filepath.Join("volumes", serviceID)}}, &rep, time.Minute); err != nil {
		return err
	}
	if rep.Error != "" {
		return errors.New(rep.Error)
	}
	return nil
}

func (s *Server) haLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.haTick(ctx)
	}
}
