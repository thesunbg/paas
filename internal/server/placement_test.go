package server

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/scheduler"
	"github.com/thesunbg/paas/internal/store"
)

// seedDeployService creates a team, project and image service for deploy tests.
func seedDeployService(t *testing.T, s *Server) *store.Service {
	t.Helper()
	ctx := context.Background()
	team, err := s.store.CreateTeam(ctx, "t", "t", "", false)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := s.store.CreateProject(ctx, team.ID, "p", "p")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := s.store.CreateService(ctx, &store.Service{
		ProjectID: proj.ID, Name: "web", Slug: "web", Type: model.TypeWeb,
		Source: model.ServiceSource{Kind: "git", Repo: "https://example.invalid/r.git", Branch: "main"},
		Spec:   model.ServiceSpec{Replicas: 1, Port: 8080},
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestWebhookCancelsOnlyWebhooks(t *testing.T) {
	s := newTestServerBus(t)
	ctx := context.Background()
	svc := seedDeployService(t, s)
	status := func(d *store.Deployment) string {
		got, err := s.store.DeploymentByID(ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Status
	}
	src, err := s.store.CreateDeployment(ctx, &store.Deployment{ServiceID: svc.ID, Status: model.DeployActive, Trigger: "manual", Image: "img:1"})
	if err != nil {
		t.Fatal(err)
	}
	rb, err := s.redeploy(ctx, svc, src, "rollback", nil)
	if err != nil {
		t.Fatal(err)
	}
	w1, err := s.QueueDeployment(ctx, svc, "webhook", "c1", "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := status(rb); got != model.DeployQueued {
		t.Fatalf("rollback after webhook = %q, want queued", got)
	}
	w2, err := s.QueueDeployment(ctx, svc, "webhook", "c2", "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := status(w1); got != "cancelled" {
		t.Fatalf("first webhook = %q, want cancelled", got)
	}
	if _, err := s.QueueDeployment(ctx, svc, "manual", "c3", "m", nil); err != nil {
		t.Fatal(err)
	}
	if got := status(w2); got != model.DeployQueued {
		t.Fatalf("webhook after manual = %q, want queued", got)
	}
}

func TestPlaceInstanceSerializesCapacity(t *testing.T) {
	s := newTestServerBus(t)
	ctx := context.Background()
	svc := seedDeployService(t, s)
	n, err := s.store.UpsertNodeLegacy(ctx, &proto.RegisterRequest{Name: "n", Address: "10.0.0.1", Roles: []string{"worker"}, CPUCores: 4, MemoryMB: 1000, Capabilities: map[string]string{}}, 16)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpdateNodeSettings(ctx, n.ID, "online", 1, 1, 0, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	req := scheduler.Request{Resources: model.Resources{MemoryRequestMB: 200}}
	const total = 10
	errs := make([]error, total)
	var wg sync.WaitGroup
	for i := range total {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = s.store.PlaceInstance(ctx, &store.Instance{
				ServiceID: svc.ID, ContainerID: "c" + string(rune('a'+i)), Port: 8080, Status: model.InstStarting, MemoryRequestMB: 200,
			}, func(nodes []store.Node, committed map[string][2]int) (*store.Node, error) {
				return scheduler.Pick(nodes, committed, req)
			})
		}()
	}
	wg.Wait()
	ok, full := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, scheduler.ErrNoCapacity):
			full++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	insts, err := s.store.InstancesForService(ctx, svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok != 5 || full != 5 || len(insts) != 5 {
		t.Fatalf("placed %d, rejected %d, rows %d; want 5, 5, 5", ok, full, len(insts))
	}
}
