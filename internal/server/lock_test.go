package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
)

type lockFixture struct {
	s       *Server
	project *store.Project
	buildID string // online node with role build
	nodeB   *store.Node
}

// newLockFixture seeds a build node and a worker node B whose heartbeat is stale.
func newLockFixture(t *testing.T) *lockFixture {
	t.Helper()
	s := newTestServerBus(t)
	ctx := context.Background()
	team, err := s.store.CreateTeam(ctx, "t", "t", "", false)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := s.store.CreateProject(ctx, team.ID, "p", "p")
	if err != nil {
		t.Fatal(err)
	}
	bn, err := s.store.UpsertNodeLegacy(ctx, &proto.RegisterRequest{Name: "build", Address: "10.0.0.1", Roles: []string{"build"}, CPUCores: 2, MemoryMB: 2048, Capabilities: map[string]string{}}, 16)
	if err != nil {
		t.Fatal(err)
	}
	nb, err := s.store.UpsertNodeLegacy(ctx, &proto.RegisterRequest{Name: "b", Address: "10.0.0.2", Roles: []string{"worker"}, CPUCores: 2, MemoryMB: 2048, Capabilities: map[string]string{}}, 16)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Q(ctx).Exec(ctx, `UPDATE nodes SET last_seen_at = now() - interval '1 minute' WHERE id=$1`, nb.ID); err != nil {
		t.Fatal(err)
	}
	return &lockFixture{s: s, project: proj, buildID: bn.ID, nodeB: nb}
}

// seedService makes a running git service with an old active deployment and one
// instance (given status) on node B.
func (f *lockFixture) seedService(t *testing.T, name, instStatus string) *store.Service {
	t.Helper()
	ctx := context.Background()
	svc, err := f.s.store.CreateService(ctx, &store.Service{
		ProjectID: f.project.ID, Name: name, Slug: name, Type: model.TypeWeb,
		Source: model.ServiceSource{Kind: "git", Repo: "https://example.invalid/r.git", Branch: "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	old, err := f.s.store.CreateDeployment(ctx, &store.Deployment{ServiceID: svc.ID, Status: model.DeployActive, Trigger: "test", Image: "img:old"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.store.SetServiceCurrentDeployment(ctx, svc.ID, old.ID, model.StatusRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.store.CreateInstance(ctx, &store.Instance{
		ServiceID: svc.ID, DeploymentID: &old.ID, NodeID: f.nodeB.ID, ContainerID: name + "-1", Port: 8080, Status: instStatus,
	}); err != nil {
		t.Fatal(err)
	}
	return svc
}

// startBuild queues a deployment for svc, claims it and runs it in a goroutine tracked by wg.
func (f *lockFixture) startBuild(t *testing.T, wg *sync.WaitGroup, svc *store.Service) *store.Deployment {
	t.Helper()
	ctx := context.Background()
	if _, err := f.s.store.CreateDeployment(ctx, &store.Deployment{ServiceID: svc.ID, Status: model.DeployQueued, Trigger: "test"}); err != nil {
		t.Fatal(err)
	}
	d, err := f.s.store.ClaimQueuedDeployment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("runDeployment panicked: %v", r)
			}
		}()
		f.s.runDeployment(ctx, d)
	}()
	return d
}

func waitStarted(t *testing.T, started <-chan string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d builds started", i, n)
		}
	}
}

func reconcileWithin(t *testing.T, s *Server, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	done := make(chan struct{})
	go func() { s.reconcileOnce(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(d + time.Second):
		t.Fatal("reconcile blocked by builds")
	}
}

func instanceCount(t *testing.T, s *Server, svcID string) int {
	t.Helper()
	list, err := s.store.InstancesForService(context.Background(), svcID)
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}

// Builds run without the service lock, so a reconcile pass must still converge
// the services being built (here: drop instances lost with their node).
func TestReconcileRunsDuringBuilds(t *testing.T) {
	f := newLockFixture(t)
	s := f.s
	started, release := fakeBuildAgent(t, s, f.buildID)
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait) // runs after release (registered later runs first)
	t.Cleanup(release)

	var svcs []*store.Service
	for i := 0; i < 4; i++ {
		svc := f.seedService(t, fmt.Sprintf("svc%d", i), model.InstRunning)
		svcs = append(svcs, svc)
		f.startBuild(t, &wg, svc)
	}
	waitStarted(t, started, 4)

	reconcileWithin(t, s, 5*time.Second)

	n, err := s.store.NodeByID(context.Background(), f.nodeB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n.Status != "offline" {
		t.Errorf("node B status = %q, want offline", n.Status)
	}
	for _, svc := range svcs {
		if c := instanceCount(t, s, svc.ID); c != 0 {
			t.Errorf("%s: %d instances left; reconcile did not run on a service being built", svc.Slug, c)
		}
	}
}

func TestReconcileSkipsLockedService(t *testing.T) {
	f := newLockFixture(t)
	s := f.s
	if _, err := s.store.Q(context.Background()).Exec(context.Background(), `UPDATE nodes SET status='offline' WHERE id=$1`, f.nodeB.ID); err != nil {
		t.Fatal(err)
	}
	svc := f.seedService(t, "locked", model.InstLost)

	unlock := s.lock(svc.ID)
	reconcileWithin(t, s, 5*time.Second)
	if c := instanceCount(t, s, svc.ID); c != 1 {
		t.Fatalf("locked service was reconciled: %d instances, want 1", c)
	}
	unlock()

	reconcileWithin(t, s, 5*time.Second)
	if c := instanceCount(t, s, svc.ID); c != 0 {
		t.Fatalf("unlocked service not reconciled: %d instances, want 0", c)
	}
}

func TestDeployServiceChangedDuringBuild(t *testing.T) {
	// run blocks a build, applies change, lets the build finish and returns the deployment.
	run := func(t *testing.T, change func(f *lockFixture, svc *store.Service)) (*lockFixture, *store.Service, *store.Deployment) {
		f := newLockFixture(t)
		started, release := fakeBuildAgent(t, f.s, f.buildID)
		var wg sync.WaitGroup
		t.Cleanup(wg.Wait)
		t.Cleanup(release)
		svc := f.seedService(t, "chg", model.InstRunning)
		d := f.startBuild(t, &wg, svc)
		waitStarted(t, started, 1)
		change(f, svc)
		release()
		wg.Wait()
		return f, svc, d
	}

	t.Run("stopped", func(t *testing.T) {
		f, svc, d := run(t, func(f *lockFixture, svc *store.Service) {
			if err := f.s.store.SetServiceStatus(context.Background(), svc.ID, model.StatusStopped); err != nil {
				t.Fatal(err)
			}
		})
		got, err := f.s.store.DeploymentByID(context.Background(), d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != model.DeployCancelled {
			t.Errorf("deployment status = %q, want cancelled", got.Status)
		}
		cur, err := f.s.store.ServiceByID(context.Background(), svc.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cur.Status != model.StatusStopped {
			t.Errorf("service status = %q, want stopped", cur.Status)
		}
	})

	t.Run("deleted", func(t *testing.T) {
		f, _, d := run(t, func(f *lockFixture, svc *store.Service) {
			if err := f.s.store.DeleteService(context.Background(), svc.ID); err != nil {
				t.Fatal(err)
			}
		})
		got, err := f.s.store.DeploymentByID(context.Background(), d.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		if err == nil && got.Status == model.DeployActive {
			t.Errorf("deployment of deleted service is active")
		}
	})
}
