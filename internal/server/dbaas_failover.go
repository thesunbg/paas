package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/databases"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
)

// Failover decision and execution for HA postgres (see dbaas_ha.go for the setup).
//
// Timing proof that the old primary P is fenced before the standby S is promoted at time T.
// Agent side (proto): P's agent fences once its lease (LeaseTTL after the SEND of the last
// acked heartbeat) has expired and no standby streams from P.
//   - The last acked send of P's agent was recorded by the control plane, so
//     last_seen_at >= that send time. The leader promotes only when last_seen_at < T - haPrimaryGone
//     (45s), hence the last acked send was before T-45 and P's lease ended before T-25
//     (haPrimaryGone >= LeaseTTL + 25s).
//   - S has not streamed since T - haWitnessDown (30s), so P's walsender saw S gone before
//     T-20 (WalSenderTimeout 10s) and P's agent (fence loop every FenceCheckEvery 5s) fenced
//     before T-15.
//
// Both clocks are monotonic and local to one machine; nothing compares two machines' clocks.
const (
	haPrimaryGone = 45 * time.Second // min silence of the primary's node (>= LeaseTTL + 25s)
	haWitnessDown = 30 * time.Second // min time the standby has not streamed (>= WalSenderTimeout + FenceCheckEvery + margin)
	haLeaderGrace = 60 * time.Second // a new leader observes this long before deciding anything (> haWitnessDown)
	haMaxLagBytes = 16 << 20         // larger standby lag blocks auto failover (needs force)
	haForceFor    = 10 * time.Minute // how long a force request stays valid
	haWitnessAge  = proto.WalSenderTimeout
)

const (
	verdictWait       = "wait"
	verdictPromote    = "promote"
	verdictBlockedLag = "blocked_lag"
)

type haWitness struct {
	ok        bool          // the standby answered
	streaming bool          // and is streaming from the primary
	downFor   time.Duration // how long it has continuously not been streaming
}

// haInput is everything failoverVerdict looks at.
type haInput struct {
	sinceLeader            time.Duration
	nodesTotal             int
	nodesOffline           int
	primaryLastSeenAge     time.Duration
	witness                haWitness
	standbyLSN, primaryLSN uint64
	force                  bool // accepts data loss: bypasses the lag check only
}

// failoverVerdict decides whether to promote. Checks run in order; force only skips the last.
func failoverVerdict(in haInput) (verdict, reason string) {
	switch {
	case in.sinceLeader < haLeaderGrace:
		return verdictWait, "recently became leader"
	case in.nodesOffline*2 > in.nodesTotal:
		return verdictWait, fmt.Sprintf("%d of %d nodes offline: this control plane may be the isolated one", in.nodesOffline, in.nodesTotal)
	case in.primaryLastSeenAge <= haPrimaryGone:
		return verdictWait, "primary node silent for less than " + haPrimaryGone.String()
	case !in.witness.ok:
		return verdictWait, "standby could not be asked"
	case in.witness.streaming:
		return verdictWait, "standby still streams from the primary"
	case in.witness.downFor < haWitnessDown:
		return verdictWait, "standby stopped streaming less than " + haWitnessDown.String() + " ago"
	case in.primaryLSN > in.standbyLSN && in.primaryLSN-in.standbyLSN > haMaxLagBytes && !in.force:
		return verdictBlockedLag, fmt.Sprintf("standby lag %d MB", (in.primaryLSN-in.standbyLSN)>>20)
	}
	return verdictPromote, "primary gone and fenced, standby caught up"
}

// parseLSN reads a pg_lsn such as 0/16B3748.
func parseLSN(s string) (uint64, bool) {
	hi, lo, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return 0, false
	}
	h, err1 := strconv.ParseUint(hi, 16, 32)
	l, err2 := strconv.ParseUint(lo, 16, 32)
	return h<<32 | l, err1 == nil && err2 == nil
}

// haMemory is what the leader has observed; it dies with the leader, which the grace period covers.
type haMemory struct {
	leaderSince time.Time
	witnessDown map[string]time.Time // db -> since when its standby has not streamed
	reason      map[string]string    // db -> last logged wait reason
	blocked     map[string]bool      // db -> blocked_lag already notified in this incident
}

func (m *haMemory) init() {
	if m.witnessDown == nil {
		m.witnessDown, m.reason, m.blocked = map[string]time.Time{}, map[string]string{}, map[string]bool{}
	}
}

// haBecameLeader starts a fresh reign: nothing observed by an earlier one counts.
func (s *Server) haBecameLeader() {
	s.haMu.Lock()
	defer s.haMu.Unlock()
	s.ha = haMemory{leaderSince: time.Now()}
	s.ha.init()
}

// haObserve updates the witness clock and reports how long the standby has not streamed.
func (s *Server) haObserve(dbID string, w haWitness) haWitness {
	s.haMu.Lock()
	defer s.haMu.Unlock()
	s.ha.init()
	if !w.ok || w.streaming {
		delete(s.ha.witnessDown, dbID) // unknown is not "down": the clock restarts
		return w
	}
	since, ok := s.ha.witnessDown[dbID]
	if !ok {
		since = time.Now()
		s.ha.witnessDown[dbID] = since
	}
	w.downFor = time.Since(since)
	return w
}

// haReset forgets the incident of a database whose primary is fine (or was replaced).
func (s *Server) haReset(dbID string) {
	s.haMu.Lock()
	defer s.haMu.Unlock()
	s.ha.init()
	delete(s.ha.witnessDown, dbID)
	delete(s.ha.reason, dbID)
	delete(s.ha.blocked, dbID)
}

// haReasonChanged is true when reason differs from the last one logged for the database.
func (s *Server) haReasonChanged(dbID, reason string) bool {
	s.haMu.Lock()
	defer s.haMu.Unlock()
	s.ha.init()
	if s.ha.reason[dbID] == reason {
		return false
	}
	s.ha.reason[dbID] = reason
	return true
}

// markBlocked is true once per incident (haReset starts a new one).
func (s *Server) markBlocked(dbID string) bool {
	s.haMu.Lock()
	defer s.haMu.Unlock()
	s.ha.init()
	if s.ha.blocked[dbID] {
		return false
	}
	s.ha.blocked[dbID] = true
	return true
}

func (s *Server) sinceLeader() time.Duration {
	s.haMu.Lock()
	defer s.haMu.Unlock()
	if s.ha.leaderSince.IsZero() {
		return time.Duration(1<<63 - 1) // not elected through leaderLoop (tests)
	}
	return time.Since(s.ha.leaderSince)
}

// haProbe runs a psql query on a server, returning trimmed output.
func (s *Server) haProbe(ctx context.Context, db *store.Database, serviceID, query string) (string, error) {
	c, _, _ := s.dbCreds(ctx, db)
	out, err := s.execOutput(ctx, serviceID, databases.Command{Cmd: []string{"psql", "-U", c.Username, "-d", "postgres", "-Atc", query}}, 10*time.Second)
	return strings.TrimSpace(out), err
}

// haWitnessOf asks the standby (a path independent of the control plane <-> primary link)
// whether it still streams from the primary, and how far it got.
func (s *Server) haWitnessOf(ctx context.Context, db *store.Database) (w haWitness, lsn uint64) {
	const q = `SELECT coalesce((SELECT status FROM pg_stat_wal_receiver),''), coalesce((SELECT extract(epoch FROM now()-last_msg_receipt_time) FROM pg_stat_wal_receiver),-1), coalesce(pg_last_wal_receive_lsn()::text,'')`
	out, err := s.haProbe(ctx, db, db.Options.ReplicaServiceID, q)
	if err != nil {
		return haWitness{}, 0
	}
	f := strings.Split(out, "|")
	if len(f) != 3 {
		return haWitness{}, 0
	}
	age, _ := strconv.ParseFloat(f[1], 64)
	lsn, _ = parseLSN(f[2])
	w.ok = true
	w.streaming = f[0] == "streaming" && age >= 0 && time.Duration(age*float64(time.Second)) < haWitnessAge
	return w, lsn
}

// haTick checks every HA database (every 10 s, leader only): records the primary's WAL
// position, finishes a failover that was started, fails over when the verdict says so, and
// recreates a missing standby.
func (s *Server) haTick(ctx context.Context) {
	dbs, err := s.store.AllDatabases(ctx)
	if err != nil {
		return
	}
	nodes, _ := s.store.Nodes(ctx)
	nodeByID := map[string]store.Node{}
	offline := 0
	for _, n := range nodes {
		nodeByID[n.ID] = n
		if n.Status == "offline" {
			offline++
		}
	}
	for i := range dbs {
		db := &dbs[i]
		// Not while `upgrading`: a move or resize owns the server, a failover would race it.
		if !db.Options.HA || db.ServerServiceID == nil || db.Status != "ready" {
			continue
		}
		srv, err := s.store.ServiceByID(ctx, *db.ServerServiceID)
		if err != nil {
			continue
		}
		st, err := s.store.HAState(ctx, db.ID)
		if err != nil {
			continue
		}
		if st.FailoverTo != "" {
			// Decided earlier (maybe by a leader that died): finish it, the verdict was already given.
			if err := s.failover(ctx, db); err != nil {
				slog.Error("database failover (resume)", "db", db.Name, "err", err)
			}
			continue
		}
		pn, ok := nodeByID[s.primaryNode(ctx, srv)]
		if !ok || pn.Status != "offline" {
			s.haReset(db.ID)
			if ok && db.Options.ReplicaServiceID != "" && pn.Status == "online" {
				s.recordPrimaryLSN(ctx, db, srv)
			}
			s.haEnsureStandby(ctx, db)
			continue
		}
		if db.Options.ReplicaServiceID == "" {
			continue
		}
		w, standbyLSN := s.haWitnessOf(ctx, db)
		primaryLSN, _ := parseLSN(st.PrimaryLSN)
		verdict, reason := failoverVerdict(haInput{
			sinceLeader: s.sinceLeader(), nodesTotal: len(nodes), nodesOffline: offline,
			primaryLastSeenAge: time.Since(pn.LastSeenAt), witness: s.haObserve(db.ID, w),
			standbyLSN: standbyLSN, primaryLSN: primaryLSN, force: st.Forced,
		})
		switch verdict {
		case verdictPromote:
			if err := s.failover(ctx, db); err != nil {
				slog.Error("database failover", "db", db.Name, "err", err)
			}
		case verdictBlockedLag:
			if s.haReasonChanged(db.ID, reason) {
				slog.Warn("database failover blocked", "db", db.Name, "reason", reason)
			}
			if s.markBlocked(db.ID) {
				s.notify(ctx, s.teamOfDatabase(ctx, db), "database.failover",
					fmt.Sprintf("Database %s has lost its primary but its standby is behind (%s). Automatic failover is blocked: POST /api/v1/databases/%s/ha/failover {\"force\": true} accepts the data loss.", db.Name, reason, db.ID),
					map[string]any{"database": db.ID})
			}
		default:
			if s.haReasonChanged(db.ID, reason) {
				slog.Info("database failover waiting", "db", db.Name, "reason", reason)
			}
		}
	}
}

func (s *Server) recordPrimaryLSN(ctx context.Context, db *store.Database, srv *store.Service) {
	out, err := s.haProbe(ctx, db, srv.ID, "SELECT pg_current_wal_lsn()")
	if _, ok := parseLSN(out); err != nil || !ok {
		return
	}
	if err := s.store.SetPrimaryLSN(ctx, db.ID, out); err != nil {
		slog.Debug("record primary lsn", "db", db.Name, "err", err)
	}
}

// haEnsureStandby recreates a missing standby in the background.
func (s *Server) haEnsureStandby(ctx context.Context, db *store.Database) {
	if db.Options.ReplicaServiceID != "" || db.Status != "ready" || s.haBusy(db.ID) {
		return
	}
	go func(id string) {
		defer s.haDone(id)
		unlock := s.lock("db-" + id)
		defer unlock()
		fresh, err := s.store.DatabaseByID(ctx, id)
		if err != nil || !fresh.Options.HA || fresh.Options.ReplicaServiceID != "" {
			return
		}
		if err := s.ensureStandby(ctx, fresh); err != nil {
			slog.Debug("recreate standby", "db", fresh.Name, "err", err)
		}
	}(db.ID)
}

// failover promotes the standby (re-reading the database under its lock: the failover may
// have finished meanwhile).
func (s *Server) failover(ctx context.Context, db *store.Database) error {
	unlock := s.lock("db-" + db.ID)
	defer unlock()
	fresh, err := s.store.DatabaseByID(ctx, db.ID)
	if err != nil || fresh.Options.ReplicaServiceID == "" || fresh.ServerServiceID == nil {
		return err
	}
	old, err := s.store.ServiceByID(ctx, *fresh.ServerServiceID)
	if err != nil {
		return err
	}
	return s.promoteStandby(ctx, fresh, old, false)
}

// promoteStandby makes the standby the database's server. Every step can be repeated, so a
// leader that dies anywhere in it is followed by one that finishes the job (failover_to
// records the decision): stop the old primary (planned: cleanly; unplanned: best effort) ->
// record the intent -> promote unless already promoted -> one transaction that moves the
// alias and the database to the standby and removes the old server -> side effects.
func (s *Server) promoteStandby(ctx context.Context, db *store.Database, oldPrimary *store.Service, planned bool) error {
	rep, err := s.store.ServiceByID(ctx, db.Options.ReplicaServiceID)
	if errors.Is(err, store.ErrNotFound) {
		_ = s.store.SetFailoverIntent(ctx, db.ID, "") // the standby is gone: the decision is void
	}
	if err != nil {
		return err
	}
	in, err := s.firstInstance(ctx, rep.ID)
	if err != nil {
		return fmt.Errorf("standby is not running: %w", err)
	}
	intent, err := s.store.FailoverIntent(ctx, db.ID)
	if err != nil {
		return err
	}
	c, _, _ := s.dbCreds(ctx, db)
	oldNode := s.primaryNode(ctx, oldPrimary) // before its instances go
	if intent == "" {
		if planned {
			s.stopService(ctx, oldPrimary)
			time.Sleep(3 * time.Second) // let the standby replay the shutdown checkpoint
		} else {
			s.stopOldPrimary(ctx, oldPrimary)
		}
		if err := s.store.SetFailoverIntent(ctx, db.ID, rep.ID); err != nil {
			return err
		}
	}
	slog.Warn("failing over database", "db", db.Name, "to", rep.Slug, "node", in.NodeID, "planned", planned)
	rec, err := s.haProbe(ctx, db, rep.ID, "SELECT pg_is_in_recovery()")
	if err != nil {
		return fmt.Errorf("standby state: %w", err)
	}
	switch rec {
	case "t":
		out, err := s.execOutput(ctx, rep.ID, databases.Command{Cmd: []string{"psql", "-U", c.Username, "-d", "postgres", "-Atc", "SELECT pg_promote(true, 60)"}}, 90*time.Second)
		if err != nil || strings.TrimSpace(out) != "t" {
			return fmt.Errorf("promote: %v %s", err, strings.TrimSpace(out))
		}
	case "f": // promoted by an earlier attempt
	default:
		return fmt.Errorf("standby state: unexpected %q", rec)
	}
	if err := s.store.CommitFailover(ctx, db.ID, rep.ID, oldPrimary.ID, oldPrimary.Spec.DNSAliases, oldNode); err != nil {
		return err
	}
	// Committed. Side effects: at most once, they are not part of the decision.
	s.haReset(db.ID)
	s.publishAll(ctx)
	if db.Options.PITR {
		// The WAL archive continues on the new primary's node: start a fresh base backup there.
		go func() {
			if fresh, err := s.store.DatabaseByID(ctx, db.ID); err == nil {
				unlock := s.lock("pitr-" + db.ID)
				defer unlock()
				_, _ = s.takeBaseBackup(ctx, fresh)
			}
		}()
	}
	msg := fmt.Sprintf("Database %s failed over to its standby (the primary's node stopped responding). A new standby is created when another node is available.", db.Name)
	if planned {
		msg = fmt.Sprintf("Database %s switched over to its standby; a new standby is being created.", db.Name)
	}
	s.notify(ctx, s.teamOfDatabase(ctx, db), "database.failover", msg, map[string]any{"database": db.ID})
	return nil
}

// stopOldPrimary tells the old primary's node to stop it (best effort: the node is usually
// unreachable, and then its agent has fenced it on its own).
func (s *Server) stopOldPrimary(ctx context.Context, old *store.Service) {
	for _, in := range s.instancesOf(ctx, old.ID) {
		_ = s.requestAgent(ctx, in.NodeID, proto.CmdStop, proto.StopRequest{ContainerID: in.ContainerID, GraceSec: 2}, &proto.GenericReply{}, 5*time.Second)
	}
}

// POST /api/v1/databases/{id}/ha/failover {force}
// force accepts losing the writes the standby has not received (lag over haMaxLagBytes). It
// changes nothing else: the primary must still be confirmed gone and fenced, so the failover
// happens when haTick next finds it so, within haForceFor.
func (s *Server) handleForceFailover(w http.ResponseWriter, r *http.Request) {
	db, p := s.databaseFor(w, r, "admin")
	if db == nil {
		return
	}
	if !db.Options.HA || db.Options.ReplicaServiceID == "" || db.ServerServiceID == nil {
		writeError(w, 409, "this database has no standby")
		return
	}
	var in struct {
		Force bool `json:"force"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	d := time.Duration(0)
	if in.Force {
		d = haForceFor
	}
	if err := s.store.SetFailoverForce(r.Context(), db.ID, d); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, &p.TeamID, "database.failover.force", fmt.Sprintf("%s %v", db.Name, in.Force))
	writeJSON(w, 202, map[string]any{"force": in.Force, "valid_for": d.String()})
}

// volumeGCTick removes the volumes of deleted servers once their retention is over and their
// node is online. The queue is in the database, so it survives leader changes.
func (s *Server) volumeGCTick(ctx context.Context) {
	due, err := s.store.DueVolumeGC(ctx)
	if err != nil {
		return
	}
	for _, v := range due {
		if err := s.removeVolumes(ctx, v.NodeID, v.ServiceID); err != nil {
			slog.Warn("volume gc", "node", v.NodeID, "service", v.ServiceID, "err", err)
			continue
		}
		_ = s.store.DeleteVolumeGC(ctx, v.NodeID, v.ServiceID)
	}
}
