package store

import (
	"context"
	"testing"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
)

func TestFailStuckDeployments(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	proj := seedProject(t, st)
	node, err := st.UpsertNode(ctx, &proto.RegisterRequest{Name: "n", Address: "10.0.0.1", Roles: []string{"worker"}, CPUCores: 2, MemoryMB: 2048, Capabilities: map[string]string{}}, 16)
	if err != nil {
		t.Fatal(err)
	}
	mkSvc := func(slug, status string) *Service {
		svc, err := st.CreateService(ctx, &Service{ProjectID: proj.ID, Name: slug, Slug: slug, Type: model.TypeWeb,
			Source: model.ServiceSource{Kind: "image", Image: "nginx"}, Spec: model.ServiceSpec{Replicas: 1, Port: 8080}})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetServiceStatus(ctx, svc.ID, status); err != nil {
			t.Fatal(err)
		}
		return svc
	}
	mkDep := func(svc *Service, status string) *Deployment {
		d, err := st.CreateDeployment(ctx, &Deployment{ServiceID: svc.ID, Status: status, Trigger: "manual"})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	mkInst := func(svc *Service, d *Deployment, status, cid string) *Instance {
		i, err := st.CreateInstance(ctx, &Instance{ServiceID: svc.ID, DeploymentID: &d.ID, NodeID: node.ID, ContainerID: cid, Port: 8080, Status: status})
		if err != nil {
			t.Fatal(err)
		}
		return i
	}

	// (a) no current deployment: both fail.
	a := mkSvc("a", model.StatusDeploying)
	ad := mkDep(a, model.DeployDeploying)
	// (b) a different deployment is current: service goes back to running.
	b := mkSvc("b", model.StatusDeploying)
	bOld := mkDep(b, model.DeployActive)
	bd := mkDep(b, model.DeployBuilding)
	if err := st.SetServiceCurrentDeployment(ctx, b.ID, bOld.ID, model.StatusDeploying); err != nil {
		t.Fatal(err)
	}
	bStart := mkInst(b, bd, model.InstStarting, "b-new")
	bRun := mkInst(b, bOld, model.InstRunning, "b-old")
	// (c) the stuck deployment is the current one: it becomes active.
	c := mkSvc("c", model.StatusDeploying)
	cd := mkDep(c, model.DeployDeploying)
	if err := st.SetServiceCurrentDeployment(ctx, c.ID, cd.ID, model.StatusDeploying); err != nil {
		t.Fatal(err)
	}
	// (d) stopped service keeps its status.
	d := mkSvc("d", model.StatusStopped)
	dd := mkDep(d, model.DeployBuilding)

	if err := st.FailStuckDeployments(ctx); err != nil {
		t.Fatal(err)
	}
	depStatus := func(x *Deployment) string {
		got, err := st.DeploymentByID(ctx, x.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.FinishedAt == nil {
			t.Errorf("deployment %s has no finished_at", x.ID)
		}
		return got.Status
	}
	svcStatus := func(x *Service) string {
		got, err := st.ServiceByID(ctx, x.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Status
	}
	for _, tc := range []struct{ name, got, want string }{
		{"a deployment", depStatus(ad), model.DeployFailed},
		{"a service", svcStatus(a), model.StatusFailed},
		{"b deployment", depStatus(bd), model.DeployFailed},
		{"b service", svcStatus(b), model.StatusRunning},
		{"c deployment", depStatus(cd), model.DeployActive},
		{"c service", svcStatus(c), model.StatusRunning},
		{"d deployment", depStatus(dd), model.DeployFailed},
		{"d service", svcStatus(d), model.StatusStopped},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	if _, err := st.InstanceByID(ctx, bStart.ID); err != ErrNotFound {
		t.Errorf("starting instance of the failed deployment: err = %v, want ErrNotFound", err)
	}
	if _, err := st.InstanceByID(ctx, bRun.ID); err != nil {
		t.Errorf("running instance of another deployment was removed: %v", err)
	}
}
