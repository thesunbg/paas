package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"

	"github.com/thesunbg/paas/internal/bus"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
)

// haFixture is an HA database: primary on node A (lost), standby on node B (running).
type haFixture struct {
	s            *Server
	ctx          context.Context // leader ctx (epoch)
	db           *store.Database
	old, rep     *store.Service
	nodeA, nodeB *store.Node

	mu        sync.Mutex
	inRecov   string // answer of pg_is_in_recovery on the standby
	witness   string // answer of the pg_stat_wal_receiver query on the standby
	promotes  atomic.Int32
	stops     atomic.Int32 // CmdStop sent to the old primary's node
	primaryEx atomic.Int32
	notified  atomic.Int32
}

func newHAFixture(t *testing.T) *haFixture {
	t.Helper()
	s := newTestServerBus(t)
	bg := context.Background()
	conn, err := pgx.Connect(bg, s.store.DB.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(bg) })
	epoch, err := s.store.TakeLeadership(bg, conn)
	if err != nil {
		t.Fatal(err)
	}
	f := &haFixture{s: s, ctx: store.WithEpoch(bg, epoch), inRecov: "t\n", witness: "|-1|0/1000000\n"}

	team, err := s.store.CreateTeam(bg, "t", "t", "", false)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := s.store.CreateProject(bg, team.ID, "p", "p")
	if err != nil {
		t.Fatal(err)
	}
	node := func(name string) *store.Node {
		n, err := s.store.UpsertNodeLegacy(bg, &proto.RegisterRequest{Name: name, Address: "10.0.0.1", Roles: []string{"worker"}, CPUCores: 2, MemoryMB: 2048, Capabilities: map[string]string{}}, 16)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	f.nodeA, f.nodeB = node("a"), node("b")
	// A went silent two minutes ago and is offline; B is fine.
	if _, err := s.store.DB.Exec(bg, `UPDATE nodes SET status='offline', last_seen_at=now()-interval '2 minutes' WHERE id=$1`, f.nodeA.ID); err != nil {
		t.Fatal(err)
	}
	svc := func(slug string, n *store.Node, inst string, status string, spec model.ServiceSpec) *store.Service {
		sv, err := s.store.CreateService(bg, &store.Service{ProjectID: proj.ID, Name: slug, Slug: slug, Type: model.TypeDatabase,
			Source: model.ServiceSource{Kind: "image", Image: "postgres"}, Spec: spec, NodeID: &n.ID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.store.CreateInstance(bg, &store.Instance{ServiceID: sv.ID, NodeID: n.ID, ContainerID: inst, Port: 5432, Status: status}); err != nil {
			t.Fatal(err)
		}
		return sv
	}
	f.old = svc("old", f.nodeA, "old-1", model.InstLost, model.ServiceSpec{DNSAliases: []string{"db-rw.p.internal"}})
	f.rep = svc("rep", f.nodeB, "rep-1", model.InstRunning, model.ServiceSpec{})
	db, err := s.store.CreateDatabase(bg, &store.Database{ProjectID: proj.ID, Name: "db", Engine: "postgres", Version: "17", Mode: "dedicated",
		Username: "app", PasswordEnc: s.box.MustEncrypt("pw"), SizePreset: "small"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetDatabaseServer(bg, db.ID, f.old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.UpdateDatabaseOptions(bg, db.ID, func(o *store.DBOptions) error { o.HA, o.ReplicaServiceID = true, f.rep.ID; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetDatabaseStatus(bg, db.ID, "ready", ""); err != nil {
		t.Fatal(err)
	}
	f.db, _ = s.store.DatabaseByID(bg, db.ID)

	// The standby's agent, with scripted psql.
	if _, err := bus.Handle(s.nc, proto.AgentSubject(f.nodeB.ID, proto.CmdExec), func(_ context.Context, r *proto.ExecRequest) *proto.ExecReply {
		q := r.Cmd[len(r.Cmd)-1]
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.Contains(q, "pg_is_in_recovery"):
			return &proto.ExecReply{Stdout: f.inRecov}
		case strings.Contains(q, "pg_promote"):
			f.promotes.Add(1)
			f.inRecov = "f\n"
			return &proto.ExecReply{Stdout: "t\n"}
		case strings.Contains(q, "pg_stat_wal_receiver"):
			return &proto.ExecReply{Stdout: f.witness}
		}
		return &proto.ExecReply{ExitCode: 1, Stderr: "unexpected query " + q}
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Handle(s.nc, proto.AgentSubject(f.nodeA.ID, proto.CmdStop), func(_ context.Context, r *proto.StopRequest) *proto.GenericReply {
		f.stops.Add(1)
		return &proto.GenericReply{}
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.nc.Flush(); err != nil {
		t.Fatal(err)
	}

	// Count failover notifications through a webhook channel.
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.notified.Add(1) }))
	t.Cleanup(hook.Close)
	prev := AllowPrivateNotifyTargets
	AllowPrivateNotifyTargets = true
	t.Cleanup(func() { AllowPrivateNotifyTargets = prev })
	if _, err := s.store.CreateChannel(bg, &store.NotificationChannel{TeamID: team.ID, Name: "w", Kind: "webhook", TargetEnc: s.box.MustEncrypt(hook.URL), Events: []string{"database.failover"}}); err != nil {
		t.Fatal(err)
	}
	return f
}

// waitNotified waits for n notifications and checks no more arrive.
func (f *haFixture) waitNotified(t *testing.T, n int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.notified.Load() < n && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if got := f.notified.Load(); got != n {
		t.Fatalf("notifications = %d, want %d", got, n)
	}
}

func (f *haFixture) assertPromoted(t *testing.T) {
	t.Helper()
	bg := context.Background()
	db, _ := f.s.store.DatabaseByID(bg, f.db.ID)
	if db.ServerServiceID == nil || *db.ServerServiceID != f.rep.ID || db.Options.ReplicaServiceID != "" {
		t.Errorf("database: server=%v replica=%q", db.ServerServiceID, db.Options.ReplicaServiceID)
	}
	if _, err := f.s.store.ServiceByID(bg, f.old.ID); err == nil {
		t.Error("old primary still exists")
	}
	var gc int
	_ = f.s.store.DB.QueryRow(bg, `SELECT count(*) FROM volume_gc WHERE node_id=$1 AND service_id=$2`, f.nodeA.ID, f.old.ID).Scan(&gc)
	if gc != 1 {
		t.Errorf("volume_gc rows = %d, want 1", gc)
	}
	if st, _ := f.s.store.HAState(bg, f.db.ID); st.FailoverTo != "" {
		t.Errorf("failover_to = %q after commit", st.FailoverTo)
	}
}

// A leader died after pg_promote and before the metadata commit: the next leader's haTick
// must finish without promoting again and notify once.
func TestPromoteResumesWithoutPromoting(t *testing.T) {
	f := newHAFixture(t)
	f.inRecov = "f\n" // already promoted
	if err := f.s.store.SetFailoverIntent(f.ctx, f.db.ID, f.rep.ID); err != nil {
		t.Fatal(err)
	}
	f.s.haTick(f.ctx)
	f.assertPromoted(t)
	if n := f.promotes.Load(); n != 0 {
		t.Errorf("pg_promote called %d times", n)
	}
	f.waitNotified(t, 1)
	// Finished is finished: running it again changes nothing and says nothing.
	if err := f.s.failover(f.ctx, f.db); err != nil {
		t.Errorf("repeat: %v", err)
	}
	f.waitNotified(t, 1)
}

func TestFailoverPromotesOnceAndStopsOldPrimary(t *testing.T) {
	f := newHAFixture(t)
	if err := f.s.failover(f.ctx, f.db); err != nil {
		t.Fatal(err)
	}
	f.assertPromoted(t)
	if n := f.promotes.Load(); n != 1 {
		t.Errorf("pg_promote called %d times, want 1", n)
	}
	if n := f.stops.Load(); n != 1 {
		t.Errorf("CmdStop to the old primary's node = %d, want 1", n)
	}
	f.waitNotified(t, 1)
}

// A fenced (deposed) leader cannot record the decision, so it cannot promote anything.
func TestDeposedLeaderDoesNotPromote(t *testing.T) {
	f := newHAFixture(t)
	bg := context.Background()
	conn, err := pgx.Connect(bg, f.s.store.DB.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(bg)
	if _, err := f.s.store.TakeLeadership(bg, conn); err != nil {
		t.Fatal(err)
	}
	if err := f.s.failover(f.ctx, f.db); err == nil {
		t.Fatal("failover by a deposed leader succeeded")
	}
	if n := f.promotes.Load(); n != 0 {
		t.Errorf("pg_promote called %d times", n)
	}
	if db, _ := f.s.store.DatabaseByID(bg, f.db.ID); *db.ServerServiceID != f.old.ID {
		t.Error("database moved")
	}
}

// The decision flow: wait until the standby has not streamed for 30 s, block on lag, notify
// once, and let force through (nothing else).
func TestHATickDecision(t *testing.T) {
	f := newHAFixture(t)
	bg := context.Background()
	state := func() store.HAState { st, _ := f.s.store.HAState(bg, f.db.ID); return st }

	f.s.haTick(f.ctx) // first observation of a standby that does not stream
	if st := state(); st.FailoverTo != "" || f.promotes.Load() != 0 {
		t.Fatal("promoted on the first observation")
	}

	f.witness = "streaming|0.5|0/1000000\n" // still streaming: the primary is alive
	f.s.haTick(f.ctx)
	f.s.haMu.Lock()
	_, tracking := f.s.ha.witnessDown[f.db.ID]
	f.s.haMu.Unlock()
	if tracking {
		t.Fatal("down clock kept running while the standby streams")
	}

	// Not streaming for a minute, and 64 MiB behind: blocked, one notification.
	f.witness = "|-1|0/1000000\n"
	f.s.haTick(f.ctx)
	f.s.haMu.Lock()
	f.s.ha.witnessDown[f.db.ID] = time.Now().Add(-time.Minute)
	f.s.haMu.Unlock()
	if err := f.s.store.SetPrimaryLSN(bg, f.db.ID, "0/5000000"); err != nil {
		t.Fatal(err)
	}
	f.s.haTick(f.ctx)
	f.s.haTick(f.ctx)
	if f.promotes.Load() != 0 || state().FailoverTo != "" {
		t.Fatal("promoted despite the lag")
	}
	f.waitNotified(t, 1)

	// Force accepts the loss.
	if err := f.s.store.SetFailoverForce(bg, f.db.ID, time.Minute); err != nil {
		t.Fatal(err)
	}
	f.s.haTick(f.ctx)
	f.assertPromoted(t)
	if n := f.promotes.Load(); n != 1 {
		t.Errorf("pg_promote called %d times, want 1", n)
	}
	f.waitNotified(t, 2) // blocked_lag + the failover itself
}

// Force never overrides the fence conditions: a recently seen primary is not failed over.
func TestForceDoesNotBypassFence(t *testing.T) {
	f := newHAFixture(t)
	bg := context.Background()
	if _, err := f.s.store.DB.Exec(bg, `UPDATE nodes SET last_seen_at=now() WHERE id=$1`, f.nodeA.ID); err != nil { // within haPrimaryGone
		t.Fatal(err)
	}
	f.s.haMu.Lock()
	f.s.ha.init()
	f.s.ha.witnessDown[f.db.ID] = time.Now().Add(-time.Hour)
	f.s.haMu.Unlock()
	if err := f.s.store.SetFailoverForce(bg, f.db.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	f.s.haTick(f.ctx)
	if f.promotes.Load() != 0 {
		t.Fatal("forced failover ignored the primary's last_seen")
	}
}

func TestHATickRecordsPrimaryLSN(t *testing.T) {
	f := newHAFixture(t)
	bg := context.Background()
	if _, err := f.s.store.DB.Exec(bg, `UPDATE nodes SET status='online', last_seen_at=now() WHERE id=$1`, f.nodeA.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.store.DB.Exec(bg, `UPDATE instances SET status='running' WHERE container_id='old-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Handle(f.s.nc, proto.AgentSubject(f.nodeA.ID, proto.CmdExec), func(_ context.Context, r *proto.ExecRequest) *proto.ExecReply {
		f.primaryEx.Add(1)
		return &proto.ExecReply{Stdout: "0/16B3748\n"}
	}); err != nil {
		t.Fatal(err)
	}
	_ = f.s.nc.Flush()
	f.s.haTick(f.ctx)
	if st, _ := f.s.store.HAState(bg, f.db.ID); st.PrimaryLSN != "0/16B3748" {
		t.Errorf("primary_lsn = %q (exec calls: %d)", st.PrimaryLSN, f.primaryEx.Load())
	}
}

func TestVolumeGCTick(t *testing.T) {
	f := newHAFixture(t)
	bg := context.Background()
	var mu sync.Mutex
	var removed []string
	failNext := false
	if _, err := bus.Handle(f.s.nc, proto.AgentSubject(f.nodeB.ID, proto.CmdRemoveFiles), func(_ context.Context, r *proto.RemoveFilesRequest) *proto.GenericReply {
		mu.Lock()
		defer mu.Unlock()
		if failNext {
			return &proto.GenericReply{Error: "disk error"}
		}
		removed = append(removed, r.Paths...)
		return &proto.GenericReply{}
	}); err != nil {
		t.Fatal(err)
	}
	_ = f.s.nc.Flush()
	ins := func(node, svc, after string) {
		if _, err := f.s.store.DB.Exec(bg, `INSERT INTO volume_gc VALUES ($1, $2, now() + $3::interval)`, node, svc, after); err != nil {
			t.Fatal(err)
		}
	}
	const due, later, offline, failing = "00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000002", "00000000-0000-0000-0000-000000000003", "00000000-0000-0000-0000-000000000004"
	ins(f.nodeB.ID, due, "-1 hour")
	ins(f.nodeB.ID, later, "1 hour")
	ins(f.nodeA.ID, offline, "-1 hour") // node A is offline

	f.s.volumeGCTick(f.ctx)
	if len(removed) != 1 || removed[0] != "volumes/"+due {
		t.Fatalf("removed = %v, want only the expired volume on the online node", removed)
	}
	left := func() int {
		var n int
		_ = f.s.store.DB.QueryRow(bg, `SELECT count(*) FROM volume_gc`).Scan(&n)
		return n
	}
	if n := left(); n != 2 {
		t.Errorf("rows left = %d, want 2 (not yet due, node offline)", n)
	}
	// A failed removal keeps the row for the next pass.
	mu.Lock()
	failNext = true
	mu.Unlock()
	ins(f.nodeB.ID, failing, "-1 hour")
	f.s.volumeGCTick(f.ctx)
	if n := left(); n != 3 {
		t.Errorf("rows left = %d, want 3 (removal failed)", n)
	}
}

// A heartbeat is answered only after it was recorded, and the answer lists what may be unfenced.
func TestHeartbeatAck(t *testing.T) {
	f := newHAFixture(t)
	s := f.s
	if err := s.registerBusHandlers(); err != nil {
		t.Fatal(err)
	}
	bg := context.Background()
	// rep-1 is a fenced standby with an instance and no failover pending; ghost has no instance.
	beat := func(timeout time.Duration, cs ...proto.ContainerStatus) (proto.HeartbeatAck, error) {
		ctx, cancel := context.WithTimeout(bg, timeout)
		defer cancel()
		var ack proto.HeartbeatAck
		err := bus.Request(ctx, s.nc, proto.SubjNodeHeartbeat(f.nodeB.ID), proto.Heartbeat{Containers: cs}, &ack)
		return ack, err
	}
	fenced := []proto.ContainerStatus{{ContainerID: "rep-1", Running: true, Fenced: true}, {ContainerID: "ghost", Running: true, Fenced: true}}
	ack, err := beat(3*time.Second, fenced...)
	if err != nil {
		t.Fatal(err)
	}
	if len(ack.Unfence) != 1 || ack.Unfence[0] != "rep-1" {
		t.Errorf("unfence = %v, want [rep-1]", ack.Unfence)
	}
	if n, _ := s.store.NodeByID(bg, f.nodeB.ID); time.Since(n.LastSeenAt) > 10*time.Second {
		t.Error("heartbeat not recorded")
	}

	// With a failover pending for its database, the fenced container stays fenced.
	if err := s.store.SetFailoverIntent(bg, f.db.ID, f.rep.ID); err != nil {
		t.Fatal(err)
	}
	if ack, err = beat(3*time.Second, fenced...); err != nil || len(ack.Unfence) != 0 {
		t.Errorf("pending failover: unfence = %v, err = %v", ack.Unfence, err)
	}

	// NodeHeartbeat fails: no ack (the agent's lease must not be renewed).
	if _, err := s.store.DB.Exec(bg, `ALTER TABLE nodes RENAME COLUMN usage TO usage_x`); err != nil {
		t.Fatal(err)
	}
	if _, err := beat(700 * time.Millisecond); err == nil {
		t.Fatal("heartbeat acked although it could not be recorded")
	}
}

// An instance of a service whose data lives on an offline node is kept, not replaced, and
// is running again when the node reports its container.
func TestReconcileKeepsLostPinnedInstance(t *testing.T) {
	f := newLockFixture(t)
	s := f.s
	bg := context.Background()
	var runs atomic.Int32 // CmdRun for the stateful services (the stateless one is legitimately restarted)
	var statefulIDs sync.Map
	if _, err := bus.Handle(s.nc, proto.AgentSubject(f.nodeB.ID, proto.CmdRun), func(_ context.Context, r *proto.RunRequest) *proto.RunReply {
		if _, ok := statefulIDs.Load(r.ServiceID); ok {
			runs.Add(1)
		}
		return &proto.RunReply{ContainerID: r.ContainerID, IP: "10.1.0.9"}
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.nc.Flush()

	vol := f.seedService(t, "vol", model.InstRunning)
	if _, err := s.store.UpdateService(bg, vol.ID, func(sv *store.Service) error {
		sv.Spec.Volumes = []model.Volume{{Name: "data", Path: "/data"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pinned := f.seedService(t, "pinned", model.InstRunning)
	if err := s.store.SetServiceNode(bg, pinned.ID, &f.nodeB.ID); err != nil {
		t.Fatal(err)
	}
	plain := f.seedService(t, "plain", model.InstRunning)
	statefulIDs.Store(vol.ID, true)
	statefulIDs.Store(pinned.ID, true)

	reconcileWithin(t, s, 5*time.Second) // node B has been silent for a minute: offline, its instances lost

	for _, svc := range []*store.Service{vol, pinned} {
		list, _ := s.store.InstancesForService(bg, svc.ID)
		if len(list) != 1 || list[0].Status != model.InstLost {
			t.Errorf("%s: instances = %+v, want the one lost row kept", svc.Slug, list)
		}
	}
	if c := instanceCount(t, s, plain.ID); c != 0 {
		t.Errorf("stateless service kept %d instances, want the old behavior (row dropped)", c)
	}
	if n := runs.Load(); n != 0 {
		t.Errorf("CmdRun sent %d times", n)
	}

	// The node is back and its containers are alive: the same instances are running again.
	hb, _ := json.Marshal(proto.Heartbeat{NodeID: f.nodeB.ID, Token: s.ClusterToken, Containers: []proto.ContainerStatus{
		{ContainerID: "vol-1", Running: true, IP: "10.1.0.5"}, {ContainerID: "pinned-1", Running: true, IP: "10.1.0.6"}}})
	s.onHeartbeat(&nats.Msg{Data: hb})
	for _, svc := range []*store.Service{vol, pinned} {
		list, _ := s.store.InstancesForService(bg, svc.ID)
		if len(list) != 1 || list[0].Status != model.InstRunning {
			t.Errorf("%s after the node returned: %+v", svc.Slug, list)
		}
	}
	reconcileWithin(t, s, 5*time.Second)
	if n := runs.Load(); n != 0 {
		t.Errorf("CmdRun sent %d times after the node returned", n)
	}
	if n, _ := s.store.NodeByID(bg, f.nodeB.ID); n.Status != "online" {
		t.Errorf("node status = %s", n.Status)
	}
}
