package server

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thesunbg/paas/internal/bus"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
)

// A leader deposed mid-build must not roll out, start instances or overwrite what
// the new leader wrote.
func TestDeposedLeaderWritesNothing(t *testing.T) {
	f := newLockFixture(t)
	s := f.s
	bg := context.Background()
	takeLeadership := func() int64 {
		conn, err := pgx.Connect(bg, s.store.DB.Config().ConnString())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close(bg) })
		n, err := s.store.TakeLeadership(bg, conn)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	ctx := store.WithEpoch(bg, takeLeadership())

	// Node B is online and would receive the rollout.
	if _, err := s.store.Q(bg).Exec(bg, `UPDATE nodes SET last_seen_at = now(), status='online' WHERE id=$1`, f.nodeB.ID); err != nil {
		t.Fatal(err)
	}
	var runs atomic.Int32
	if _, err := bus.Handle(s.nc, proto.AgentSubject(f.nodeB.ID, proto.CmdRun), func(_ context.Context, r *proto.RunRequest) *proto.RunReply {
		runs.Add(1)
		return &proto.RunReply{ContainerID: r.ContainerID, IP: "10.1.0.2"}
	}); err != nil {
		t.Fatal(err)
	}
	started, release := fakeBuildAgent(t, s, f.buildID)

	svc := f.seedService(t, "deposed", model.InstRunning)
	if _, err := s.store.Q(bg).Exec(bg, `DELETE FROM instances WHERE service_id=$1`, svc.ID); err != nil {
		t.Fatal(err)
	}
	before, err := s.store.ServiceByID(bg, svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.CreateDeployment(bg, &store.Deployment{ServiceID: svc.ID, Status: model.DeployQueued, Trigger: "test"}); err != nil {
		t.Fatal(err)
	}
	d, err := s.store.ClaimQueuedDeployment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.runDeployment(ctx, d)
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("build did not start")
	}

	takeLeadership() // a newer leader takes over and fails what the old one left behind
	if err := s.store.FailStuckDeployments(bg); err != nil {
		t.Fatal(err)
	}
	release()
	wg.Wait()

	got, err := s.store.DeploymentByID(bg, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.DeployFailed {
		t.Errorf("deployment status = %q, want failed", got.Status)
	}
	if c := instanceCount(t, s, svc.ID); c != 0 {
		t.Errorf("%d instances written by the deposed leader", c)
	}
	if n := runs.Load(); n != 0 {
		t.Errorf("deposed leader sent %d run commands", n)
	}
	after, err := s.store.ServiceByID(bg, svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.CurrentDeploymentID == nil || after.CurrentDeploymentID == nil || *after.CurrentDeploymentID != *before.CurrentDeploymentID {
		t.Errorf("current deployment changed: %v -> %v", before.CurrentDeploymentID, after.CurrentDeploymentID)
	}
}
