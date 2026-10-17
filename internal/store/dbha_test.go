package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
)

type haFixture struct {
	st           *Store
	db           *Database
	old, rep     *Service
	nodeA, nodeB *Node
}

func newHAFixture(t *testing.T) *haFixture {
	t.Helper()
	st := testStore(t)
	ctx := context.Background()
	proj := seedProject(t, st)
	node := func(name string) *Node {
		n, err := st.UpsertNodeLegacy(ctx, &proto.RegisterRequest{Name: name, Address: "10.0.0.1", Roles: []string{"worker"}, CPUCores: 2, MemoryMB: 2048, Capabilities: map[string]string{}}, 16)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	f := &haFixture{st: st, nodeA: node("a"), nodeB: node("b")}
	svc := func(slug string, n *Node, spec model.ServiceSpec) *Service {
		s, err := st.CreateService(ctx, &Service{ProjectID: proj.ID, Name: slug, Slug: slug, Type: model.TypeDatabase,
			Source: model.ServiceSource{Kind: "image", Image: "postgres"}, Spec: spec, NodeID: &n.ID})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	f.old = svc("old", f.nodeA, model.ServiceSpec{DNSAliases: []string{"db-rw.p.internal"}, HealthCheck: model.HealthCheck{TimeoutSec: 180}})
	f.rep = svc("rep", f.nodeB, model.ServiceSpec{HealthCheck: model.HealthCheck{TimeoutSec: 1800}})
	db, err := st.CreateDatabase(ctx, &Database{ProjectID: proj.ID, Name: "db", Engine: "postgres", Mode: "dedicated", PasswordEnc: "x", SizePreset: "small"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDatabaseServer(ctx, db.ID, f.old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateDatabaseOptions(ctx, db.ID, func(o *DBOptions) error { o.HA, o.ReplicaServiceID = true, f.rep.ID; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := st.SetFailoverIntent(ctx, db.ID, f.rep.ID); err != nil {
		t.Fatal(err)
	}
	f.db, _ = st.DatabaseByID(ctx, db.ID)
	return f
}

func (f *haFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.st.DB.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A deposed leader's CommitFailover changes nothing; the current one's changes everything at once.
func TestCommitFailoverAtomic(t *testing.T) {
	f := newHAFixture(t)
	bg := context.Background()
	stale := WithEpoch(bg, takeLeadership(t, f.st))
	takeLeadership(t, f.st) // a newer leader

	err := f.st.CommitFailover(stale, f.db.ID, f.rep.ID, f.old.ID, f.old.Spec.DNSAliases, f.nodeA.ID)
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("stale epoch: err = %v, want ErrFenced", err)
	}
	db, _ := f.st.DatabaseByID(bg, f.db.ID)
	if db.ServerServiceID == nil || *db.ServerServiceID != f.old.ID || db.Options.ReplicaServiceID != f.rep.ID {
		t.Errorf("database changed by a fenced commit: server=%v replica=%q", db.ServerServiceID, db.Options.ReplicaServiceID)
	}
	if _, err := f.st.ServiceByID(bg, f.old.ID); err != nil {
		t.Errorf("old service removed by a fenced commit: %v", err)
	}
	if rep, _ := f.st.ServiceByID(bg, f.rep.ID); len(rep.Spec.DNSAliases) != 0 || rep.Version != f.rep.Version {
		t.Errorf("standby changed by a fenced commit: %+v v%d", rep.Spec.DNSAliases, rep.Version)
	}
	if n := f.count(t, `SELECT count(*) FROM volume_gc`); n != 0 {
		t.Errorf("%d volume_gc rows from a fenced commit", n)
	}
	if h, _ := f.st.HAState(bg, f.db.ID); h.FailoverTo != f.rep.ID {
		t.Errorf("intent = %q after a fenced commit", h.FailoverTo)
	}

	fresh := WithEpoch(bg, takeLeadership(t, f.st))
	if err := f.st.CommitFailover(fresh, f.db.ID, f.rep.ID, f.old.ID, f.old.Spec.DNSAliases, f.nodeA.ID); err != nil {
		t.Fatalf("commit: %v", err)
	}
	db, _ = f.st.DatabaseByID(bg, f.db.ID)
	if db.ServerServiceID == nil || *db.ServerServiceID != f.rep.ID || db.Options.ReplicaServiceID != "" || !db.Options.HA {
		t.Errorf("database after commit: server=%v replica=%q ha=%v", db.ServerServiceID, db.Options.ReplicaServiceID, db.Options.HA)
	}
	if _, err := f.st.ServiceByID(bg, f.old.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("old service still there: %v", err)
	}
	rep, _ := f.st.ServiceByID(bg, f.rep.ID)
	if len(rep.Spec.DNSAliases) != 1 || rep.Spec.DNSAliases[0] != "db-rw.p.internal" || rep.Spec.HealthCheck.TimeoutSec != 180 || rep.Version != f.rep.Version+1 {
		t.Errorf("standby after commit: aliases=%v timeout=%d version=%d", rep.Spec.DNSAliases, rep.Spec.HealthCheck.TimeoutSec, rep.Version)
	}
	if n := f.count(t, `SELECT count(*) FROM volume_gc WHERE node_id=$1 AND service_id=$2 AND delete_after > now() + interval '6 days'`, f.nodeA.ID, f.old.ID); n != 1 {
		t.Errorf("volume_gc rows = %d, want 1 (7 days)", n)
	}
	if h, _ := f.st.HAState(bg, f.db.ID); h.FailoverTo != "" {
		t.Errorf("intent not cleared: %q", h.FailoverTo)
	}
	// Repeating it is refused (the database no longer points at the old server).
	if err := f.st.CommitFailover(fresh, f.db.ID, f.rep.ID, f.old.ID, nil, f.nodeA.ID); err == nil {
		t.Error("second commit succeeded")
	}
}

func TestHAStateAndPending(t *testing.T) {
	f := newHAFixture(t)
	ctx := context.Background()
	for _, id := range []string{f.old.ID, f.rep.ID} {
		if p, err := f.st.FailoverPending(ctx, id); err != nil || !p {
			t.Errorf("FailoverPending(%s) = %v, %v; want true", id, p, err)
		}
	}
	if err := f.st.SetFailoverIntent(ctx, f.db.ID, ""); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.st.FailoverPending(ctx, f.old.ID); p {
		t.Error("pending after the intent was cleared")
	}
	if err := f.st.SetPrimaryLSN(ctx, f.db.ID, "0/16B3748"); err != nil {
		t.Fatal(err)
	}
	if h, _ := f.st.HAState(ctx, f.db.ID); h.PrimaryLSN != "0/16B3748" || h.Forced {
		t.Errorf("state = %+v", h)
	}
	if err := f.st.SetFailoverForce(ctx, f.db.ID, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if h, _ := f.st.HAState(ctx, f.db.ID); !h.Forced || h.PrimaryLSN != "0/16B3748" {
		t.Errorf("state after force = %+v", h)
	}
	if err := f.st.SetFailoverForce(ctx, f.db.ID, 0); err != nil {
		t.Fatal(err)
	}
	if h, _ := f.st.HAState(ctx, f.db.ID); h.Forced {
		t.Error("still forced after withdrawing")
	}
}

func TestApplyContainerStatusRevivesLostInstance(t *testing.T) {
	f := newHAFixture(t)
	ctx := context.Background()
	in, err := f.st.CreateInstance(ctx, &Instance{ServiceID: f.old.ID, NodeID: f.nodeA.ID, ContainerID: "c1", Port: 5432, Status: model.InstLost})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.ApplyContainerStatus(ctx, f.nodeA.ID, proto.ContainerStatus{ContainerID: "c1", Running: true, IP: "10.1.0.2"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.st.InstanceByID(ctx, in.ID); got.Status != model.InstRunning || got.IP != "10.1.0.2" {
		t.Errorf("after a running report: %s %s", got.Status, got.IP)
	}
	// Lost does not count against the node's capacity (NodeCommitted), running does again.
	if _, err := f.st.DB.Exec(ctx, `UPDATE instances SET status='lost', memory_request_mb=100 WHERE id=$1`, in.ID); err != nil {
		t.Fatal(err)
	}
	if c, _ := f.st.NodeCommitted(ctx); c[f.nodeA.ID][0] != 0 {
		t.Errorf("lost instance committed %v", c[f.nodeA.ID])
	}
	_ = f.st.ApplyContainerStatus(ctx, f.nodeA.ID, proto.ContainerStatus{ContainerID: "c1", Running: true})
	if c, _ := f.st.NodeCommitted(ctx); c[f.nodeA.ID][0] != 100 {
		t.Errorf("revived instance committed %v, want 100", c[f.nodeA.ID])
	}
	if _, err := f.st.DB.Exec(ctx, `UPDATE instances SET status='lost' WHERE id=$1`, in.ID); err != nil {
		t.Fatal(err)
	}
	_ = f.st.ApplyContainerStatus(ctx, f.nodeA.ID, proto.ContainerStatus{ContainerID: "c1", Running: false})
	if got, _ := f.st.InstanceByID(ctx, in.ID); got.Status != model.InstFailed {
		t.Errorf("after a stopped report: %s, want failed", got.Status)
	}
}

func TestDueVolumeGC(t *testing.T) {
	f := newHAFixture(t)
	ctx := context.Background()
	exec := func(q string, a ...any) {
		if _, err := f.st.DB.Exec(ctx, q, a...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO volume_gc VALUES ($1, gen_random_uuid(), now() - interval '1 hour')`, f.nodeA.ID) // due, online
	exec(`INSERT INTO volume_gc VALUES ($1, gen_random_uuid(), now() + interval '1 hour')`, f.nodeA.ID) // not due
	exec(`INSERT INTO volume_gc VALUES ($1, gen_random_uuid(), now() - interval '1 hour')`, f.nodeB.ID) // due, node offline
	exec(`UPDATE nodes SET status='offline' WHERE id=$1`, f.nodeB.ID)
	due, err := f.st.DueVolumeGC(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].NodeID != f.nodeA.ID {
		t.Fatalf("due = %+v, want the one expired volume on the online node", due)
	}
	if err := f.st.DeleteVolumeGC(ctx, due[0].NodeID, due[0].ServiceID); err != nil {
		t.Fatal(err)
	}
	if due, _ = f.st.DueVolumeGC(ctx); len(due) != 0 {
		t.Errorf("still due: %+v", due)
	}
}
