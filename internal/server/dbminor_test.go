package server

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/bus"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
)

func TestPinHelpers(t *testing.T) {
	if got := imageTag("postgres:17@sha256:abc"); got != "postgres:17" {
		t.Fatalf("imageTag = %q", got)
	}
	if keepPin("postgres:17@sha256:abc", "postgres:17") != "postgres:17@sha256:abc" {
		t.Fatal("a resize unpinned the image")
	}
	if got := keepPin("postgres:17@sha256:abc", "postgres:18"); got != "postgres:18" {
		t.Fatalf("a different tag kept the old pin: %q", got)
	}
	if got := keepPin("postgres:17", "postgres:17"); got != "postgres:17" {
		t.Fatalf("got %q", got)
	}
}

// minorAgent is a node agent for update-minor: it logs every command in order.
type minorAgent struct {
	mu         sync.Mutex
	ops        []string
	refreshErr string
	digest     string
	refreshed  []string
	tl         []string                                  // timeline of container commands: "exec <cid> ...", "stop <cid>", "run <cid> <image>"
	runs       []proto.RunRequest                        // every run request, in order
	execHook   func(*proto.ExecRequest) *proto.ExecReply // a test's answer to an exec (nil = success)
}

func (a *minorAgent) timeline(op string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tl = append(a.tl, op)
}

func (a *minorAgent) tlCopy() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.tl...)
}

func (a *minorAgent) log(op string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ops = append(a.ops, op)
}

func (a *minorAgent) opsCopy() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.ops...)
}

func newMinorAgent(t *testing.T, s *Server, nodeID string) *minorAgent {
	a := &minorAgent{digest: "sha256:new"}
	reg := func(_ any, err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	reg(bus.Handle(s.nc, proto.AgentSubject(nodeID, proto.CmdExec), func(_ context.Context, r *proto.ExecRequest) *proto.ExecReply {
		a.log("exec")
		a.mu.Lock()
		hook := a.execHook
		a.mu.Unlock()
		a.timeline("exec " + r.ContainerID + " " + strings.Join(r.Cmd, " ") + " | " + r.Stdin + " | " + strings.Join(r.Env, " "))
		if hook != nil {
			if rep := hook(r); rep != nil {
				return rep
			}
		}
		return &proto.ExecReply{StdoutBytes: 42}
	}))
	reg(bus.Handle(s.nc, proto.AgentSubject(nodeID, proto.CmdRefreshImage), func(_ context.Context, r *proto.RefreshImageRequest) *proto.RefreshImageReply {
		a.log("refresh")
		a.mu.Lock()
		defer a.mu.Unlock()
		a.refreshed = append(a.refreshed, r.Image)
		if a.refreshErr != "" {
			return &proto.RefreshImageReply{Error: a.refreshErr}
		}
		return &proto.RefreshImageReply{Digest: a.digest}
	}))
	reg(bus.Handle(s.nc, proto.AgentSubject(nodeID, proto.CmdStop), func(_ context.Context, r *proto.StopRequest) *proto.GenericReply {
		a.log("stop")
		a.timeline("stop " + r.ContainerID)
		return &proto.GenericReply{}
	}))
	reg(bus.Handle(s.nc, proto.AgentSubject(nodeID, proto.CmdRun), func(_ context.Context, r *proto.RunRequest) *proto.RunReply {
		a.log("run " + r.Image)
		a.timeline("run " + r.ContainerID + " " + r.Image)
		a.mu.Lock()
		a.runs = append(a.runs, *r)
		a.mu.Unlock()
		return &proto.RunReply{ContainerID: r.ContainerID, IP: "10.0.0.9"}
	}))
	reg(bus.Handle(s.nc, proto.AgentSubject(nodeID, proto.CmdRemoveFiles), func(_ context.Context, r *proto.RemoveFilesRequest) *proto.GenericReply {
		return &proto.GenericReply{}
	}))
	if err := s.nc.Flush(); err != nil {
		t.Fatal(err)
	}
	return a
}

type minorEnv struct {
	s     *Server
	h     http.Handler
	proj  *store.Project
	node  *store.Node
	team  *store.Team
	agent *minorAgent
	tok   map[string]string // root (platform admin), tadmin, dev, viewer
}

// newMinorEnv: a node with a fake agent, a team with an admin, a developer and a viewer, a
// platform administrator and a running deploy worker.
func newMinorEnv(t *testing.T) *minorEnv {
	t.Helper()
	s := newTestServerBus(t)
	h := s.routes()
	ctx := context.Background()
	e := &minorEnv{s: s, h: h, tok: map[string]string{}}
	var err error
	if e.team, err = s.store.CreateTeam(ctx, "t", "t", "", false); err != nil {
		t.Fatal(err)
	}
	for _, u := range []struct {
		name, role string
		admin      bool
	}{{"root", "", true}, {"tadmin", "admin", false}, {"dev", "developer", false}, {"viewer", "viewer", false}} {
		usr := mkUser(t, s, u.name+"@example.com", "password1", u.admin)
		if u.role != "" {
			if err := s.store.AddMember(ctx, e.team.ID, usr.ID, u.role); err != nil {
				t.Fatal(err)
			}
		}
		e.tok[u.name] = mustLogin(t, h, u.name+"@example.com", "password1")
	}
	if e.proj, err = s.store.CreateProject(ctx, e.team.ID, "p", "p"); err != nil {
		t.Fatal(err)
	}
	if e.node, err = s.store.UpsertNodeLegacy(ctx, &proto.RegisterRequest{Name: "n", Address: "10.0.0.1", Roles: []string{"worker"}, CPUCores: 4, MemoryMB: 8192, Capabilities: map[string]string{}}, 16); err != nil {
		t.Fatal(err)
	}
	e.agent = newMinorAgent(t, s, e.node.ID)
	wctx, cancel := context.WithCancel(ctx)
	go s.deployWorker(wctx)
	t.Cleanup(cancel)
	return e
}

// database adds a ready postgres 17 on a server that runs a pinned image on the node.
func (e *minorEnv) database(t *testing.T, name, mode string, srv *store.Service) (*store.Database, *store.Service) {
	t.Helper()
	ctx := context.Background()
	s := e.s
	db, err := s.store.CreateDatabase(ctx, &store.Database{ProjectID: e.proj.ID, Name: name, Engine: "postgres", Version: "17", Mode: mode,
		DBName: name, Username: name, PasswordEnc: s.box.MustEncrypt("pw"), SizePreset: "nano"})
	if err != nil {
		t.Fatal(err)
	}
	if srv == nil {
		pinned := "postgres:17@sha256:old"
		srv, err = s.store.CreateService(ctx, &store.Service{
			ProjectID: e.proj.ID, Name: name + " server", Slug: name + "-srv", Type: model.TypeDatabase,
			Source: model.ServiceSource{Kind: "image", Image: pinned}, NodeID: &e.node.ID,
			Spec: model.ServiceSpec{Image: pinned, Language: "postgres", Replicas: 1, Port: 5432, QoS: model.QoSBurstable,
				Volumes:     []model.Volume{{Name: "data", Path: "/var/lib/postgresql/data"}},
				Resources:   model.Resources{MemoryLimitMB: 256, MemoryRequestMB: 128},
				HealthCheck: model.HealthCheck{Type: "tcp", TimeoutSec: 5}},
		})
		if err != nil {
			t.Fatal(err)
		}
		d, err := s.store.CreateDeployment(ctx, &store.Deployment{ServiceID: srv.ID, Status: model.DeployActive, Trigger: "system", Image: "docker.io/library/" + pinned})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.store.SetServiceCurrentDeployment(ctx, srv.ID, d.ID, model.StatusRunning); err != nil {
			t.Fatal(err)
		}
		if _, err := s.store.CreateInstance(ctx, &store.Instance{ServiceID: srv.ID, DeploymentID: &d.ID, NodeID: e.node.ID, ContainerID: name + "-c", Port: 5432, Status: model.InstRunning}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.store.SetDatabaseServer(ctx, db.ID, srv.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetDatabaseStatus(ctx, db.ID, "ready", ""); err != nil {
		t.Fatal(err)
	}
	db, _ = s.store.DatabaseByID(ctx, db.ID)
	return db, srv
}

func (e *minorEnv) post(t *testing.T, who string, db *store.Database, body any) int {
	t.Helper()
	return do(t, e.h, "POST", "/api/v1/databases/"+db.ID+"/update-minor", e.tok[who], body).Code
}

// settle waits until the update job has put the database back to ready and, for a
// major upgrade, finished the move: abortMove/finishMove set "ready" first (so linked
// services get their DATABASE_URL), then restart those services and only then clear
// move_state.
func (e *minorEnv) settle(t *testing.T, db *store.Database) *store.Database {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		got, err := e.s.store.DatabaseByID(context.Background(), db.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "upgrading" && got.MoveState == nil {
			return got
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("update-minor did not finish")
	return nil
}

func TestUpdateMinorDedicated(t *testing.T) {
	e := newMinorEnv(t)
	ctx := context.Background()
	db, srv := e.database(t, "d1", "dedicated", nil)
	versionCache.Store(db.ID, cachedVersion{v: "17.1", at: time.Now()})

	if code := e.post(t, "tadmin", db, nil); code != 202 {
		t.Fatalf("update-minor = %d", code)
	}
	got := e.settle(t, db)
	if got.Status != "ready" || got.Error != "" {
		t.Fatalf("status %q error %q", got.Status, got.Error)
	}
	ops := e.agent.opsCopy()
	// backup (exec), pull, then stop the old instance and start the new one on the refreshed tag.
	want := []string{"exec", "refresh", "stop", "run docker.io/library/postgres:17"}
	if !slices.Equal(ops, want) {
		t.Fatalf("ops = %v, want %v", ops, want)
	}
	if len(e.agent.refreshed) != 1 || e.agent.refreshed[0] != "docker.io/library/postgres:17" {
		t.Fatalf("refreshed %v: only the selected tag may be pulled", e.agent.refreshed)
	}
	bs, _ := e.s.store.BackupsForDatabase(ctx, db.ID)
	if len(bs) != 1 || !bs[0].Pinned || bs[0].Status != "completed" {
		t.Fatalf("backups = %+v, want one pinned completed backup", bs)
	}
	fresh, _ := e.s.store.ServiceByID(ctx, srv.ID)
	if fresh.Spec.Image != "postgres:17@sha256:new" || fresh.Source.Image != "postgres:17@sha256:new" {
		t.Fatalf("spec image = %q, source image = %q", fresh.Spec.Image, fresh.Source.Image)
	}
	cur, _ := e.s.store.DeploymentByID(ctx, *fresh.CurrentDeploymentID)
	if cur.Image != "docker.io/library/postgres:17@sha256:new" {
		t.Fatalf("current deployment image = %q: a reschedule would not run the pinned digest", cur.Image)
	}
	if _, ok := versionCache.Load(db.ID); ok {
		t.Fatal("versionCache not cleared")
	}
}

func TestUpdateMinorPullFailureLeavesDatabaseRunning(t *testing.T) {
	e := newMinorEnv(t)
	ctx := context.Background()
	db, srv := e.database(t, "d1", "dedicated", nil)
	e.agent.refreshErr = "registry unreachable"

	if code := e.post(t, "tadmin", db, nil); code != 202 {
		t.Fatalf("update-minor = %d", code)
	}
	got := e.settle(t, db)
	if got.Status != "ready" || !strings.Contains(got.Error, "registry unreachable") {
		t.Fatalf("status %q error %q", got.Status, got.Error)
	}
	if ops := e.agent.opsCopy(); !slices.Equal(ops, []string{"exec", "refresh"}) {
		t.Fatalf("ops = %v: the old instance must not be stopped when the pull fails", ops)
	}
	ins, _ := e.s.store.InstancesForNode(ctx, e.node.ID)
	if len(ins) != 1 || ins[0].Status != model.InstRunning {
		t.Fatalf("instances = %+v", ins)
	}
	if fresh, _ := e.s.store.ServiceByID(ctx, srv.ID); fresh.Spec.Image != "postgres:17@sha256:old" {
		t.Fatalf("spec image moved to %q", fresh.Spec.Image)
	}
	if bs, _ := e.s.store.BackupsForDatabase(ctx, db.ID); len(bs) != 1 || !bs[0].Pinned {
		t.Fatalf("the restore point must exist and be pinned: %+v", bs)
	}
}

func TestUpdateMinorPermissionsAndGates(t *testing.T) {
	e := newMinorEnv(t)
	ctx := context.Background()

	ded, _ := e.database(t, "d1", "dedicated", nil)
	for _, who := range []string{"dev", "viewer"} {
		if code := e.post(t, who, ded, nil); code != 403 {
			t.Errorf("%s on dedicated = %d, want 403", who, code)
		}
	}

	sh, srv := e.database(t, "s1", "shared", nil)
	sibling, _ := e.database(t, "s2", "shared", srv)
	for _, who := range []string{"tadmin", "dev", "viewer"} {
		if code := e.post(t, who, sh, map[string]bool{"confirm_shared": true}); code != 403 {
			t.Errorf("%s on shared = %d, want 403 (platform administrator only)", who, code)
		}
	}
	if code := e.post(t, "root", sh, nil); code != 400 {
		t.Errorf("shared without confirm_shared = %d, want 400", code)
	}

	// A sibling on the same server is being upgraded: refused, nothing started.
	if err := e.s.store.SetDatabaseStatus(ctx, sibling.ID, "upgrading", ""); err != nil {
		t.Fatal(err)
	}
	if code := e.post(t, "root", sh, map[string]bool{"confirm_shared": true}); code != 409 {
		t.Errorf("sibling upgrading = %d, want 409", code)
	}
	if got, _ := e.s.store.DatabaseByID(ctx, sh.ID); got.Status != "ready" {
		t.Errorf("a refused update changed the status to %q", got.Status)
	}

	// HA is not supported yet.
	if _, err := e.s.store.UpdateDatabaseOptions(ctx, ded.ID, func(o *store.DBOptions) error { o.HA = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if code := e.post(t, "tadmin", ded, nil); code != 409 {
		t.Errorf("HA = %d, want 409", code)
	}
	if ops := e.agent.opsCopy(); len(ops) != 0 {
		t.Errorf("refused requests reached the agent: %v", ops)
	}

	// The platform administrator can update the shared server once the sibling is done.
	if err := e.s.store.SetDatabaseStatus(ctx, sibling.ID, "ready", ""); err != nil {
		t.Fatal(err)
	}
	if code := e.post(t, "root", sh, map[string]bool{"confirm_shared": true}); code != 202 {
		t.Fatalf("root on shared = %d, want 202", code)
	}
	if got := e.settle(t, sh); got.Status != "ready" || got.Error != "" {
		t.Fatalf("status %q error %q", got.Status, got.Error)
	}
}

func TestPinFirstDigest(t *testing.T) {
	e := newMinorEnv(t)
	ctx := context.Background()
	s := e.s
	srv, err := s.store.CreateService(ctx, &store.Service{
		ProjectID: e.proj.ID, Name: "pg", Slug: "pg", Type: model.TypeDatabase,
		Source: model.ServiceSource{Kind: "image", Image: "postgres:17"},
		Spec:   model.ServiceSpec{Image: "postgres:17", Language: "postgres", Replicas: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := s.store.CreateDeployment(ctx, &store.Deployment{ServiceID: srv.ID, Status: model.DeployActive, Trigger: "system", Image: "docker.io/library/postgres:17"})
	_ = s.store.SetServiceCurrentDeployment(ctx, srv.ID, d.ID, model.StatusRunning)
	in, _ := s.store.CreateInstance(ctx, &store.Instance{ServiceID: srv.ID, DeploymentID: &d.ID, NodeID: e.node.ID, ContainerID: "c", Status: model.InstRunning})

	// The agent reports a digest through the heartbeat path.
	if err := s.store.ApplyContainerStatus(ctx, e.node.ID, proto.ContainerStatus{ContainerID: "c", Running: true, ImageDigest: "sha256:aaa"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.store.InstanceByID(ctx, in.ID); got.ImageDigest != "sha256:aaa" {
		t.Fatalf("instance digest = %q", got.ImageDigest)
	}
	s.pinFirstDigest(ctx, in, "sha256:aaa")
	got, _ := s.store.ServiceByID(ctx, srv.ID)
	if got.Spec.Image != "postgres:17@sha256:aaa" {
		t.Fatalf("spec image = %q", got.Spec.Image)
	}
	if cur, _ := s.store.DeploymentByID(ctx, d.ID); cur.Image != "docker.io/library/postgres:17@sha256:aaa" {
		t.Fatalf("deployment image = %q", cur.Image)
	}
	// Pinned: a later digest (another node, a newer pull) never moves it; only update-minor does.
	s.pinFirstDigest(ctx, in, "sha256:bbb")
	if got, _ := s.store.ServiceByID(ctx, srv.ID); got.Spec.Image != "postgres:17@sha256:aaa" {
		t.Fatalf("pin moved to %q", got.Spec.Image)
	}
	// A node that reports no digest never pins (ApplyContainerStatus keeps what it had).
	if err := s.store.ApplyContainerStatus(ctx, e.node.ID, proto.ContainerStatus{ContainerID: "c", Running: true}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.store.InstanceByID(ctx, in.ID); got.ImageDigest != "sha256:aaa" {
		t.Fatalf("an empty digest wiped %q", got.ImageDigest)
	}
}

func TestOldBackupsSkipsPinned(t *testing.T) {
	e := newMinorEnv(t)
	ctx := context.Background()
	db, _ := e.database(t, "d1", "dedicated", nil)
	var ids []string
	for i := 0; i < 4; i++ {
		b, err := e.s.store.CreateBackup(ctx, db.ID, e.node.ID, "backups/x/"+string(rune('a'+i))+".sql")
		if err != nil {
			t.Fatal(err)
		}
		_ = e.s.store.FinishBackup(ctx, b.ID, "completed", "", 1)
		ids = append(ids, b.ID)
		time.Sleep(5 * time.Millisecond)
	}
	if err := e.s.store.PinBackup(ctx, ids[0]); err != nil { // the oldest one is the pinned restore point
		t.Fatal(err)
	}
	old, err := e.s.store.OldBackups(ctx, db.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != 1 || old[0].ID != ids[1] {
		t.Fatalf("old = %+v, want only backup %s (the pinned one is kept and does not count)", old, ids[1])
	}
}
