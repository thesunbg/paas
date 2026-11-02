package server

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/bus"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/s3"
	"github.com/thesunbg/paas/internal/store"
)

func init() {
	volumeMoveStartWait = 3 * time.Second
	volumeMovePoll = 50 * time.Millisecond
}

var goodSum = proto.VolumeSum{Bytes: 1234, Files: 5, Digest: strings.Repeat("ab", 32)}

// volAgent is a node agent for volume moves. Every command lands in a timeline shared by all
// nodes ("<cmd> <node> ..."); fail makes a command answer with that error.
type volAgent struct {
	e    *volEnv
	name string
	mu   sync.Mutex
	fail map[string]string // cmd -> error
	// what this node reports: as the sender (sent/received), and as the receiver (digest on disk)
	sent, received, digest proto.VolumeSum
	blockSend              bool // the send waits until the receive is cancelled
}

type volEnv struct {
	s      *Server
	h      func(method, path, who string, body any) (int, map[string]any)
	tok    map[string]string
	proj   *store.Project
	a, b   *store.Node
	agents map[string]*volAgent
	mu     sync.Mutex
	tl     []string
	cancel chan struct{} // closed when a receive is cancelled
	once   sync.Once
}

func (e *volEnv) log(l string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tl = append(e.tl, l)
}

func (e *volEnv) timeline() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.tl...)
}

func (v *volAgent) failure(cmd string) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.fail[cmd]
}

func (v *volAgent) setFail(cmd, msg string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.fail[cmd] = msg
}

func newVolAgent(t *testing.T, e *volEnv, n *store.Node) *volAgent {
	v := &volAgent{e: e, name: n.Name, fail: map[string]string{}, sent: goodSum, received: goodSum, digest: goodSum}
	s := e.s
	reg := func(_ any, err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	reg(bus.Handle(s.nc, proto.AgentSubject(n.ID, proto.CmdRun), func(_ context.Context, r *proto.RunRequest) *proto.RunReply {
		e.log("run " + v.name + " " + r.ContainerID)
		if f := v.failure("run"); f != "" {
			return &proto.RunReply{Error: f}
		}
		return &proto.RunReply{ContainerID: r.ContainerID, IP: "10.0.0.9"}
	}))
	reg(bus.Handle(s.nc, proto.AgentSubject(n.ID, proto.CmdStop), func(_ context.Context, r *proto.StopRequest) *proto.GenericReply {
		e.log("stop " + v.name + " " + r.ContainerID)
		return &proto.GenericReply{Error: v.failure("stop")}
	}))
	reg(bus.Handle(s.nc, proto.AgentSubject(n.ID, proto.CmdRemoveFiles), func(_ context.Context, r *proto.RemoveFilesRequest) *proto.GenericReply {
		e.log("rm " + v.name + " " + strings.Join(r.Paths, ","))
		return &proto.GenericReply{}
	}))
	reg(bus.Handle(s.nc, proto.AgentSubject(n.ID, proto.CmdVolumeRecv), func(_ context.Context, r *proto.VolumeRecvRequest) *proto.VolumeRecvReply {
		if r.Cancel {
			e.log("recv-cancel " + v.name + " " + r.Dir)
			e.once.Do(func() { close(e.cancel) })
			return &proto.VolumeRecvReply{}
		}
		e.log("recv " + v.name + " " + r.Dir + " from=" + r.From)
		if len(r.Token) < 32 {
			return &proto.VolumeRecvReply{Error: "short token"}
		}
		if f := v.failure("recv"); f != "" {
			return &proto.VolumeRecvReply{Error: f}
		}
		return &proto.VolumeRecvReply{Port: 40000}
	}))
	reg(bus.Handle(s.nc, proto.AgentSubject(n.ID, proto.CmdVolumeSend), func(_ context.Context, r *proto.VolumeSendRequest) *proto.VolumeSendReply {
		e.log("send " + v.name + " " + r.Dir + " to=" + r.Addr)
		v.mu.Lock()
		block := v.blockSend
		v.mu.Unlock()
		if block {
			<-e.cancel
			return &proto.VolumeSendReply{Error: "connection reset by peer"}
		}
		if f := v.failure("send"); f != "" {
			return &proto.VolumeSendReply{Error: f}
		}
		return &proto.VolumeSendReply{Sent: v.sent, Received: v.received}
	}))
	reg(bus.Handle(s.nc, proto.AgentSubject(n.ID, proto.CmdVolumeDigest), func(_ context.Context, r *proto.VolumeDigestRequest) *proto.VolumeDigestReply {
		e.log("digest " + v.name + " " + r.Dir)
		if f := v.failure("digest"); f != "" {
			return &proto.VolumeDigestReply{Error: f}
		}
		return &proto.VolumeDigestReply{Sum: v.digest}
	}))
	reg(bus.Handle(s.nc, proto.AgentSubject(n.ID, proto.CmdVolumeCommit), func(_ context.Context, r *proto.VolumeCommitRequest) *proto.GenericReply {
		e.log("commit " + v.name + " " + r.From + " -> " + r.To)
		return &proto.GenericReply{Error: v.failure("commit")}
	}))
	reg(bus.Handle(s.nc, proto.AgentSubject(n.ID, proto.CmdTransfer), func(_ context.Context, r *proto.TransferRequest) *proto.TransferReply {
		e.log("transfer " + v.name + " " + r.Method + " " + r.Path)
		return &proto.TransferReply{Error: v.failure("transfer")}
	}))
	reg(bus.Handle(s.nc, proto.AgentSubject(n.ID, proto.CmdExtract), func(_ context.Context, r *proto.ExtractRequest) *proto.GenericReply {
		e.log("extract " + v.name + " " + r.File + " -> " + r.Dir)
		return &proto.GenericReply{Error: v.failure("extract")}
	}))
	if err := s.nc.Flush(); err != nil {
		t.Fatal(err)
	}
	return v
}

// newVolEnv: two worker nodes a and b with fake agents, a team (admin, developer, viewer), a
// platform administrator (root).
func newVolEnv(t *testing.T) *volEnv {
	t.Helper()
	s := newTestServerBus(t)
	h := s.routes()
	ctx := context.Background()
	e := &volEnv{s: s, tok: map[string]string{}, agents: map[string]*volAgent{}, cancel: make(chan struct{})}
	team, err := s.store.CreateTeam(ctx, "t", "t", "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []struct {
		name, role string
		admin      bool
	}{{"root", "", true}, {"tadmin", "admin", false}, {"dev", "developer", false}, {"viewer", "viewer", false}} {
		usr := mkUser(t, s, u.name+"@example.com", "password1", u.admin)
		if u.role != "" {
			if err := s.store.AddMember(ctx, team.ID, usr.ID, u.role); err != nil {
				t.Fatal(err)
			}
		}
		e.tok[u.name] = mustLogin(t, h, u.name+"@example.com", "password1")
	}
	if e.proj, err = s.store.CreateProject(ctx, team.ID, "p", "p"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		n, err := s.store.UpsertNodeLegacy(ctx, &proto.RegisterRequest{Name: name, Address: "10.0.0." + name, Roles: []string{"worker"}, CPUCores: 4, MemoryMB: 8192, Capabilities: map[string]string{}}, 16)
		if err != nil {
			t.Fatal(err)
		}
		if name == "a" {
			e.a = n
		} else {
			e.b = n
		}
		e.agents[name] = newVolAgent(t, e, n)
	}
	e.h = func(method, path, who string, body any) (int, map[string]any) {
		w := do(t, h, method, path, e.tok[who], body)
		out := map[string]any{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	return e
}

// stateful adds a running web service with a volume, pinned to node a with one instance there.
func (e *volEnv) stateful(t *testing.T, slug string) *store.Service {
	t.Helper()
	ctx := context.Background()
	s := e.s
	svc, err := s.store.CreateService(ctx, &store.Service{
		ProjectID: e.proj.ID, Name: slug, Slug: slug, Type: model.TypeWeb,
		Source: model.ServiceSource{Kind: "image", Image: "app:1"}, NodeID: &e.a.ID,
		Spec: model.ServiceSpec{Image: "app:1", Replicas: 1, Port: 8080, QoS: model.QoSBurstable, Resources: model.Resources{MemoryLimitMB: 64, MemoryRequestMB: 32},
			Volumes: []model.Volume{{Name: "data", Path: "/data"}}, HealthCheck: model.HealthCheck{Type: "tcp", TimeoutSec: 5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.store.CreateDeployment(ctx, &store.Deployment{ServiceID: svc.ID, Status: model.DeployActive, Trigger: "system", Image: "app:1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetServiceCurrentDeployment(ctx, svc.ID, d.ID, model.StatusRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.CreateInstance(ctx, &store.Instance{ServiceID: svc.ID, DeploymentID: &d.ID, NodeID: e.a.ID, ContainerID: slug + "-c", Port: 8080, Status: model.InstRunning}); err != nil {
		t.Fatal(err)
	}
	return svc
}

func (e *volEnv) move(t *testing.T, svc *store.Service, body map[string]any) string {
	t.Helper()
	code, out := e.h("POST", "/api/v1/services/"+svc.ID+"/move", "tadmin", body)
	if code != 202 {
		t.Fatalf("POST move = %d %v", code, out)
	}
	return out["id"].(string)
}

func (e *volEnv) wait(t *testing.T, id string) *store.VolumeMove {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		m, err := e.s.store.VolumeMoveByID(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if store.VolumeMoveTerminal(m.Phase) {
			return m
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the move did not finish")
	return nil
}

// where returns the service's pin, status, and the nodes of its running instances.
func (e *volEnv) where(t *testing.T, svc *store.Service) (pin, status string, running []string) {
	t.Helper()
	ctx := context.Background()
	fresh, err := e.s.store.ServiceByID(ctx, svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.NodeID != nil {
		pin = *fresh.NodeID
	}
	for _, in := range e.s.instancesOf(ctx, svc.ID) {
		if in.Status == model.InstRunning {
			running = append(running, in.NodeID)
		}
	}
	return pin, fresh.Status, running
}

// requireOnA: the service is pinned to a and runs there (and only there).
func (e *volEnv) requireOnA(t *testing.T, svc *store.Service) {
	t.Helper()
	// A rollback after the flip records its end with the pin, then starts the service again.
	pin, status, running := e.where(t, svc)
	for deadline := time.Now().Add(10 * time.Second); len(running) == 0 && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
		pin, status, running = e.where(t, svc)
	}
	if pin != e.a.ID || status != model.StatusRunning || len(running) != 1 || running[0] != e.a.ID {
		t.Fatalf("want running on a: pin %s status %s running on %v (a=%s b=%s)\n%s", pin, status, running, e.a.ID, e.b.ID, strings.Join(e.timeline(), "\n"))
	}
}

func (e *volEnv) gcRows(t *testing.T, svc *store.Service) map[string]time.Duration {
	t.Helper()
	rows, err := e.s.store.Q(context.Background()).Query(context.Background(), `SELECT node_id, delete_after - now() FROM volume_gc WHERE service_id=$1`, svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]time.Duration{}
	for rows.Next() {
		var n string
		var d time.Duration
		if err := rows.Scan(&n, &d); err != nil {
			t.Fatal(err)
		}
		out[n] = d
	}
	return out
}

func TestVolumeMoveSuccess(t *testing.T) {
	e := newVolEnv(t)
	svc := e.stateful(t, "web")
	id := e.move(t, svc, map[string]any{"node": "b", "confirm_downtime": true})
	m := e.wait(t, id)
	if m.Phase != store.VolumeDone || m.Error != "" || m.Bytes != goodSum.Bytes || m.Digest != goodSum.Digest || m.Kind != "move" {
		t.Fatalf("move = %+v", m)
	}
	pin, status, running := e.where(t, svc)
	if pin != e.b.ID || status != model.StatusRunning || len(running) != 1 || running[0] != e.b.ID {
		t.Fatalf("pin %s status %s running %v", pin, status, running)
	}
	tl := e.timeline()
	requireBefore(t, tl, []string{"stop a web-c"}, []string{"recv b moves/" + id})
	requireBefore(t, tl, []string{"recv b"}, []string{"send a volumes/" + svc.ID})
	requireBefore(t, tl, []string{"send a"}, []string{"digest b moves/" + id})
	requireBefore(t, tl, []string{"digest b"}, []string{"commit b moves/" + id + " -> volumes/" + svc.ID})
	requireBefore(t, tl, []string{"commit b"}, []string{"run b web-"})
	if at(tl, "send a", ":40000") < 0 || at(tl, "recv b", "from=0.1.255.254") < 0 && at(tl, "recv b", "from=") < 0 {
		t.Fatalf("the transfer must use the mesh addresses:\n%s", strings.Join(tl, "\n"))
	}
	// The copy on a stays for the retention, removed later by volume_gc (never b's: it is live).
	gc := e.gcRows(t, svc)
	if d, ok := gc[e.a.ID]; !ok || d < 6*24*time.Hour || d > 7*24*time.Hour+time.Minute {
		t.Fatalf("volume_gc = %v", gc)
	}
	if _, ok := gc[e.b.ID]; ok {
		t.Fatal("the live copy is queued for removal")
	}
	if at(tl, "rm a") >= 0 {
		t.Fatal("the old copy was removed at once")
	}
	// Status API: history, no active move.
	code, out := e.h("GET", "/api/v1/services/"+svc.ID+"/move", "viewer", nil)
	if code != 200 || out["active"] != nil || len(out["history"].([]any)) != 1 {
		t.Fatalf("GET move = %d %v", code, out)
	}
	if len(out["targets"].([]any)) != 0 {
		t.Fatal("a viewer sees move targets")
	}
}

// A failure at any step before the flip leaves the service running on a, its pin untouched, and
// the copy on b removed.
func TestVolumeMoveFailureLeavesServiceOnSource(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *volEnv)
		want  string
	}{
		{"stop fails", nil, "could not be stopped"},
		{"receiver fails", func(e *volEnv) { e.agents["b"].setFail("recv", "the WireGuard mesh is not up") }, "mesh is not up"},
		{"send fails", func(e *volEnv) { e.agents["a"].setFail("send", "connection reset") }, "connection reset"},
		{"receiver unpacked less", func(e *volEnv) { e.agents["a"].received = proto.VolumeSum{Bytes: 10, Files: 5, Digest: "x"} }, "unpacked 10 bytes"},
		{"digest fails", func(e *volEnv) { e.agents["b"].setFail("digest", "i/o error") }, "i/o error"},
		{"checksum differs", func(e *volEnv) { e.agents["b"].digest = proto.VolumeSum{Bytes: 1234, Files: 5, Digest: "cd"} }, "verification failed"},
		{"commit fails", func(e *volEnv) { e.agents["b"].setFail("commit", "rename: cross-device link") }, "cross-device"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newVolEnv(t)
			svc := e.stateful(t, "web")
			if c.name == "stop fails" {
				// An agent that does not answer on an online node keeps the instance (lost): the
				// container may still write, so the move must not copy.
				ctx := context.Background()
				silent, err := e.s.store.UpsertNodeLegacy(ctx, &proto.RegisterRequest{Name: "c", Address: "10.0.0.3", Roles: []string{"worker"}, CPUCores: 4, MemoryMB: 8192, Capabilities: map[string]string{}}, 16)
				if err != nil {
					t.Fatal(err)
				}
				fresh, _ := e.s.store.ServiceByID(ctx, svc.ID)
				if _, err := e.s.store.CreateInstance(ctx, &store.Instance{ServiceID: svc.ID, DeploymentID: fresh.CurrentDeploymentID, NodeID: silent.ID, ContainerID: "web-silent", Port: 8080, Status: model.InstRunning}); err != nil {
					t.Fatal(err)
				}
			} else {
				c.setup(e)
			}
			m := e.wait(t, e.move(t, svc, map[string]any{"node": "b", "confirm_downtime": true}))
			if m.Phase != store.VolumeFailed || !strings.Contains(m.Error, c.want) {
				t.Fatalf("move = %s %q, want failed with %q", m.Phase, m.Error, c.want)
			}
			tl := e.timeline()
			if c.name == "stop fails" {
				if at(tl, "send a") >= 0 {
					t.Fatal("copied while the container may still run")
				}
				pin, _, _ := e.where(t, svc)
				if pin != e.a.ID {
					t.Fatal("pin moved")
				}
				return
			}
			e.agents["a"].setFail("run", "")
			e.requireOnA(t, svc)
			if at(tl, "run b") >= 0 {
				t.Fatalf("started on b:\n%s", strings.Join(tl, "\n"))
			}
			if at(tl, "recv-cancel b moves/"+m.ID) < 0 {
				t.Fatalf("the copy on b was not removed:\n%s", strings.Join(tl, "\n"))
			}
			if c.name == "commit fails" && at(tl, "rm b volumes/"+svc.ID) < 0 {
				t.Fatalf("a possibly committed copy on b was not removed:\n%s", strings.Join(tl, "\n"))
			}
			if _, ok := e.gcRows(t, svc)[e.a.ID]; ok {
				t.Fatal("the live copy on a is queued for removal")
			}
		})
	}
}

// After the flip, a service that does not start on b goes back to a.
func TestVolumeMoveStartFailureRollsBack(t *testing.T) {
	e := newVolEnv(t)
	svc := e.stateful(t, "web")
	e.agents["b"].setFail("run", "image pull failed")
	m := e.wait(t, e.move(t, svc, map[string]any{"node": "b", "confirm_downtime": true}))
	if m.Phase != store.VolumeFailed || !strings.Contains(m.Error, "back on its previous node") {
		t.Fatalf("move = %s %q", m.Phase, m.Error)
	}
	e.requireOnA(t, svc)
	tl := e.timeline()
	requireBefore(t, tl, []string{"run b"}, []string{"run a web-"})
	gc := e.gcRows(t, svc)
	if _, ok := gc[e.b.ID]; !ok {
		t.Fatalf("the copy on b is not queued for removal: %v", gc)
	}
	if _, ok := gc[e.a.ID]; ok {
		t.Fatal("the live copy on a is queued for removal")
	}
}

func TestVolumeMoveAbort(t *testing.T) {
	e := newVolEnv(t)
	svc := e.stateful(t, "web")
	e.agents["a"].blockSend = true
	id := e.move(t, svc, map[string]any{"node": "b", "confirm_downtime": true})
	deadline := time.Now().Add(10 * time.Second)
	for at(e.timeline(), "send a") < 0 {
		if time.Now().After(deadline) {
			t.Fatal("the copy did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code, out := e.h("GET", "/api/v1/services/"+svc.ID+"/move", "viewer", nil); code != 200 || out["active"] == nil {
		t.Fatalf("GET move during the copy = %d %v", code, out)
	}
	if code, _ := e.h("DELETE", "/api/v1/services/"+svc.ID+"/move", "dev", nil); code != 403 {
		t.Fatalf("developer abort = %d", code)
	}
	if code, out := e.h("DELETE", "/api/v1/services/"+svc.ID+"/move", "tadmin", nil); code != 202 {
		t.Fatalf("abort = %d %v", code, out)
	}
	m := e.wait(t, id)
	if m.Phase != store.VolumeAborted {
		t.Fatalf("move = %s %q", m.Phase, m.Error)
	}
	e.requireOnA(t, svc)
	if code, _ := e.h("DELETE", "/api/v1/services/"+svc.ID+"/move", "tadmin", nil); code != 404 {
		t.Fatalf("abort with nothing running = %d", code)
	}
}

// Past the flip there is nothing to abort.
func TestVolumeMoveAbortAfterFlip(t *testing.T) {
	e := newVolEnv(t)
	svc := e.stateful(t, "web")
	ctx := context.Background()
	m, err := e.s.store.CreateVolumeMove(ctx, &store.VolumeMove{ServiceID: svc.ID, FromNode: &e.a.ID, ToNode: &e.b.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{store.VolumeStopping, store.VolumeCopying, store.VolumeVerifying, store.VolumeSwitching} {
		prev := m.Phase
		if ok, err := e.s.store.AdvanceVolumeMove(ctx, m.ID, prev, p); err != nil || !ok {
			t.Fatal(ok, err)
		}
		m.Phase = p
	}
	if code, _ := e.h("DELETE", "/api/v1/services/"+svc.ID+"/move", "tadmin", nil); code != 409 {
		t.Fatalf("abort while switching = %d", code)
	}
}

// A restarted control plane undoes a move that had not flipped, and finishes one that had.
func TestResumeVolumeMove(t *testing.T) {
	setup := func(t *testing.T, phase string) (*volEnv, *store.Service, *store.VolumeMove) {
		e := newVolEnv(t)
		svc := e.stateful(t, "web")
		ctx := context.Background()
		m, err := e.s.store.CreateVolumeMove(ctx, &store.VolumeMove{ServiceID: svc.ID, FromNode: &e.a.ID, ToNode: &e.b.ID})
		if err != nil {
			t.Fatal(err)
		}
		// What the dead runner did: the service stopped, the phase recorded.
		_ = e.s.store.SetVolumeMovePrevStatus(ctx, m.ID, model.StatusRunning)
		for _, in := range e.s.instancesOf(ctx, svc.ID) {
			_ = e.s.store.DeleteInstance(ctx, in.ID)
		}
		_ = e.s.store.SetServiceStatus(ctx, svc.ID, model.StatusStopped)
		for _, p := range []string{store.VolumeStopping, store.VolumeCopying, store.VolumeVerifying, store.VolumeSwitching} {
			if ok, err := e.s.store.AdvanceVolumeMove(ctx, m.ID, m.Phase, p); err != nil || !ok {
				t.Fatal(ok, err)
			}
			m.Phase = p
			if p == phase {
				break
			}
		}
		if phase == store.VolumeStarting {
			if ok, err := e.s.store.FlipVolumeMove(ctx, m); err != nil || !ok {
				t.Fatalf("flip: %v %v", ok, err)
			}
			// The flip is one-shot.
			if ok, _ := e.s.store.FlipVolumeMove(ctx, m); ok {
				t.Fatal("a second flip applied")
			}
		}
		return e, svc, m
	}
	for _, phase := range []string{store.VolumeCopying, store.VolumeVerifying, store.VolumeSwitching} {
		t.Run("in "+phase+": undone", func(t *testing.T) {
			e, svc, m := setup(t, phase)
			e.s.resumeVolumeMoves(context.Background())
			got := e.wait(t, m.ID)
			if got.Phase != store.VolumeFailed || !strings.Contains(got.Error, "interrupted") {
				t.Fatalf("move = %s %q", got.Phase, got.Error)
			}
			e.requireOnA(t, svc)
			if at(e.timeline(), "recv-cancel b") < 0 {
				t.Fatal("the copy on b was not removed")
			}
		})
	}
	t.Run("after the flip: finished", func(t *testing.T) {
		e, svc, m := setup(t, store.VolumeStarting)
		e.s.resumeVolumeMoves(context.Background())
		got := e.wait(t, m.ID)
		if got.Phase != store.VolumeDone {
			t.Fatalf("move = %s %q", got.Phase, got.Error)
		}
		pin, status, running := e.where(t, svc)
		if pin != e.b.ID || status != model.StatusRunning || len(running) != 1 || running[0] != e.b.ID {
			t.Fatalf("pin %s status %s running %v", pin, status, running)
		}
	})
}

func TestVolumeMoveAPIChecks(t *testing.T) {
	e := newVolEnv(t)
	svc := e.stateful(t, "web")
	ctx := context.Background()
	path := "/api/v1/services/" + svc.ID + "/move"
	for _, c := range []struct {
		who  string
		body map[string]any
		code int
		want string
	}{
		{"viewer", map[string]any{"node": "b", "confirm_downtime": true}, 403, ""},
		{"dev", map[string]any{"node": "b", "confirm_downtime": true}, 403, ""},
		{"tadmin", map[string]any{"node": "b"}, 400, "confirm_downtime"},
		{"tadmin", map[string]any{"node": "a", "confirm_downtime": true}, 409, "already on"},
		{"tadmin", map[string]any{"node": "nope", "confirm_downtime": true}, 409, "not found"},
		{"tadmin", map[string]any{"node": "b", "retain_days": 365, "confirm_downtime": true}, 400, "retain_days"},
	} {
		code, out := e.h("POST", path, c.who, c.body)
		if code != c.code || !strings.Contains(strings.ToLower(asString(out["error"])), strings.ToLower(c.want)) {
			t.Fatalf("%s %v = %d %v, want %d %q", c.who, c.body, code, out, c.code, c.want)
		}
	}
	// A cordoned target is refused.
	if _, err := e.s.store.Q(ctx).Exec(ctx, `UPDATE nodes SET status='cordoned' WHERE id=$1`, e.b.ID); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.h("POST", path, "tadmin", map[string]any{"node": "b", "confirm_downtime": true}); code != 409 {
		t.Fatalf("cordoned target = %d", code)
	}
	if _, err := e.s.store.Q(ctx).Exec(ctx, `UPDATE nodes SET status='online' WHERE id=$1`, e.b.ID); err != nil {
		t.Fatal(err)
	}
	// Team admins see where it can go.
	code, out := e.h("GET", path, "tadmin", nil)
	if code != 200 || len(out["targets"].([]any)) != 1 || out["targets"].([]any)[0].(map[string]any)["name"] != "b" {
		t.Fatalf("GET move = %d %v", code, out)
	}
	// One move at a time.
	e.agents["a"].blockSend = true
	id := e.move(t, svc, map[string]any{"confirm_downtime": true}) // target picked: b
	if code, out := e.h("POST", path, "tadmin", map[string]any{"node": "b", "confirm_downtime": true}); code != 409 || !strings.Contains(asString(out["error"]), "in progress") {
		t.Fatalf("second move = %d %v", code, out)
	}
	if code, _ := e.h("DELETE", path, "tadmin", nil); code != 202 {
		t.Fatal("abort")
	}
	e.wait(t, id)
	// No volumes, a database server: refused.
	plain, _ := e.s.store.UpdateService(ctx, svc.ID, func(s *store.Service) error { s.Spec.Volumes = nil; return nil })
	if code, _ := e.h("POST", "/api/v1/services/"+plain.ID+"/move", "tadmin", map[string]any{"node": "b", "confirm_downtime": true}); code != 409 {
		t.Fatalf("no volumes = %d", code)
	}
	// The source node offline: a move is refused (recover instead).
	_, _ = e.s.store.UpdateService(ctx, svc.ID, func(s *store.Service) error { s.Spec.Volumes = []model.Volume{{Name: "d", Path: "/d"}}; return nil })
	if _, err := e.s.store.Q(ctx).Exec(ctx, `UPDATE nodes SET status='offline' WHERE id=$1`, e.a.ID); err != nil {
		t.Fatal(err)
	}
	if code, out := e.h("POST", path, "tadmin", map[string]any{"node": "b", "confirm_downtime": true}); code != 409 || !strings.Contains(asString(out["error"]), "recover") {
		t.Fatalf("offline source = %d %v", code, out)
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// Node loss: the service is restored on b from its newest off-site volume backup, with the
// backup's time recorded as the RPO.
func TestVolumeRestoreFromBackup(t *testing.T) {
	e := newVolEnv(t)
	ctx := context.Background()
	e.s.s3.Store(&s3.Config{Endpoint: "http://127.0.0.1:9", Bucket: "b", AccessKey: "k", SecretKey: "s"})
	svc := e.stateful(t, "web")
	old, _ := e.s.store.CreateVolumeBackup(ctx, svc.ID, e.a.ID)
	_ = e.s.store.FinishVolumeBackup(ctx, old.ID, "completed", "", "volumes/"+svc.ID+"/old.tar.gz", 10)
	time.Sleep(10 * time.Millisecond)
	b, _ := e.s.store.CreateVolumeBackup(ctx, svc.ID, e.a.ID)
	_ = e.s.store.FinishVolumeBackup(ctx, b.ID, "completed", "", "volumes/"+svc.ID+"/new.tar.gz", 10)

	path := "/api/v1/services/" + svc.ID + "/recover"
	if code, _ := e.h("POST", path, "tadmin", map[string]any{}); code != 409 {
		t.Fatalf("recover with the node online = %d", code)
	}
	// Cordoned but still reporting: its data is there, a restore would throw it away.
	if _, err := e.s.store.Q(ctx).Exec(ctx, `UPDATE nodes SET status='cordoned' WHERE id=$1`, e.a.ID); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.h("POST", path, "tadmin", map[string]any{}); code != 409 {
		t.Fatalf("recover with the node cordoned and alive = %d", code)
	}
	if _, err := e.s.store.Q(ctx).Exec(ctx, `UPDATE nodes SET status='offline' WHERE id=$1`, e.a.ID); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.h("POST", path, "dev", map[string]any{}); code != 403 {
		t.Fatalf("developer recover = %d", code)
	}
	code, out := e.h("POST", path, "tadmin", map[string]any{"node": "b"})
	if code != 202 || out["rpo_at"] == nil {
		t.Fatalf("recover = %d %v", code, out)
	}
	mv := out["move"].(map[string]any)
	m := e.wait(t, mv["id"].(string))
	if m.Phase != store.VolumeDone || m.Kind != "restore" || m.BackupID == nil || *m.BackupID != b.ID || m.RPOAt == nil || !m.RPOAt.Equal(b.CreatedAt) {
		t.Fatalf("restore = %+v", m)
	}
	pin, status, running := e.where(t, svc)
	if pin != e.b.ID || status != model.StatusRunning || len(running) != 1 || running[0] != e.b.ID {
		t.Fatalf("pin %s status %s running %v", pin, status, running)
	}
	tl := e.timeline()
	requireBefore(t, tl, []string{"transfer b GET"}, []string{"extract b", "-> moves/" + m.ID})
	requireBefore(t, tl, []string{"extract b"}, []string{"commit b moves/" + m.ID})
	requireBefore(t, tl, []string{"commit b"}, []string{"run b web-"})
	if _, ok := e.gcRows(t, svc)[e.a.ID]; !ok {
		t.Fatal("the copy on the lost node is not queued for removal")
	}
}

func TestRecoverNode(t *testing.T) {
	e := newVolEnv(t)
	ctx := context.Background()
	e.s.s3.Store(&s3.Config{Endpoint: "http://127.0.0.1:9", Bucket: "b", AccessKey: "k", SecretKey: "s"})
	withBackup := e.stateful(t, "web")
	b, _ := e.s.store.CreateVolumeBackup(ctx, withBackup.ID, e.a.ID)
	_ = e.s.store.FinishVolumeBackup(ctx, b.ID, "completed", "", "volumes/x.tar.gz", 10)
	noBackup := e.stateful(t, "api")
	path := "/api/v1/nodes/" + e.a.ID + "/recover"
	if code, _ := e.h("POST", path, "root", map[string]any{"confirm_data_loss": true}); code != 409 {
		t.Fatalf("recover an online node = %d", code)
	}
	if _, err := e.s.store.Q(ctx).Exec(ctx, `UPDATE nodes SET status='offline' WHERE id=$1`, e.a.ID); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.h("POST", path, "tadmin", map[string]any{"confirm_data_loss": true}); code != 403 {
		t.Fatalf("team admin = %d", code)
	}
	if code, _ := e.h("POST", path, "root", map[string]any{}); code != 400 {
		t.Fatalf("without confirm_data_loss = %d", code)
	}
	code, out := e.h("POST", path, "root", map[string]any{"confirm_data_loss": true})
	if code != 202 {
		t.Fatalf("recover node = %d %v", code, out)
	}
	started, skipped := out["started"].([]any), out["skipped"].([]any)
	if len(started) != 1 || started[0].(map[string]any)["service"] != "web" || started[0].(map[string]any)["rpo_at"] == nil {
		t.Fatalf("started %v", started)
	}
	if len(skipped) != 1 || skipped[0].(map[string]any)["name"] != "api" {
		t.Fatalf("skipped %v", skipped)
	}
	m := e.wait(t, started[0].(map[string]any)["move"].(string))
	if m.Phase != store.VolumeDone {
		t.Fatalf("restore = %s %q", m.Phase, m.Error)
	}
	if pin, _, _ := e.where(t, noBackup); pin != e.a.ID {
		t.Fatal("a service without a backup was moved")
	}
}

func TestDrainMovesVolumes(t *testing.T) {
	e := newVolEnv(t)
	svc := e.stateful(t, "web")
	// Already cordoned (an earlier plain drain): still alive, so its volumes can move.
	if _, err := e.s.store.Q(context.Background()).Exec(context.Background(), `UPDATE nodes SET status='cordoned' WHERE id=$1`, e.a.ID); err != nil {
		t.Fatal(err)
	}
	code, out := e.h("POST", "/api/v1/nodes/"+e.a.ID+"/drain", "root", map[string]any{"move_volumes": true})
	if code != 202 {
		t.Fatalf("drain = %d %v", code, out)
	}
	if ms := out["moving_stateful"].([]any); len(ms) != 1 || ms[0] != "web" || len(out["pinned_stateful"].([]any)) != 0 {
		t.Fatalf("drain = %v", out)
	}
	ms, _ := e.s.store.VolumeMoves(context.Background(), svc.ID, 1)
	if len(ms) != 1 {
		t.Fatal("no move recorded")
	}
	if m := e.wait(t, ms[0].ID); m.Phase != store.VolumeDone {
		t.Fatalf("move = %s %q", m.Phase, m.Error)
	}
	if pin, _, _ := e.where(t, svc); pin != e.b.ID {
		t.Fatal("not moved")
	}

	// Without the flag the service stays.
	e2 := newVolEnv(t)
	svc2 := e2.stateful(t, "web")
	code, out = e2.h("POST", "/api/v1/nodes/"+e2.a.ID+"/drain", "root", nil)
	if code != 202 || len(out["pinned_stateful"].([]any)) != 1 || len(out["moving_stateful"].([]any)) != 0 {
		t.Fatalf("drain = %d %v", code, out)
	}
	if ms, _ := e2.s.store.VolumeMoves(context.Background(), svc2.ID, 1); len(ms) != 0 {
		t.Fatal("moved without move_volumes")
	}
}
