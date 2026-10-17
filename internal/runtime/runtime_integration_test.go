//go:build integration

package runtime

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/netns"
	"github.com/containerd/errdefs"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/network"
	"github.com/thesunbg/paas/internal/proto"
)

const testNS = "paas-test"

// newTestRuntime opens a Runtime over dir. Calling it twice with the same dir
// is how the tests simulate an agent restart.
// Requires root, containerd at /run/containerd/containerd.sock and CNI plugins in /opt/cni/bin.
func newTestRuntime(t *testing.T, dir string) *Runtime {
	t.Helper()
	nm, err := network.NewManager("/opt/cni/bin", dir, "10.250")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := New(context.Background(), "/run/containerd/containerd.sock", testNS, dir, "crun", nm)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func testImage() string {
	if img := os.Getenv("TEST_IMAGE"); img != "" {
		return img
	}
	return "mirror.gcr.io/library/busybox:1.36"
}

func httpReq(id string) proto.RunRequest {
	return proto.RunRequest{
		InstanceID: "i1", ServiceID: "s1", ContainerID: id,
		NetIndex: 7, Hostname: "web", Image: testImage(), PullImage: true,
		Env:     map[string]string{"PORT": "8080"},
		Command: []string{"mkdir -p /www && echo hello-paas > /www/index.html && echo started && exec httpd -f -p 8080 -h /www"},
		Port:    8080, Resources: model.Resources{MemoryLimitMB: 64, CPULimitMillis: 500},
		QoS: model.QoSBurstable, HealthCheck: model.HealthCheck{Type: "http", Path: "/", TimeoutSec: 30},
	}
}

func uniqueID(prefix string) string { return prefix + time.Now().Format("150405.000") }

func TestRunHTTPContainer(t *testing.T) {
	dir := t.TempDir()
	rt := newTestRuntime(t, dir)
	defer rt.Close()
	req := httpReq("paas-test-" + time.Now().Format("150405"))
	ip, err := rt.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Remove(context.Background(), req.ContainerID, time.Second)
	resp, err := http.Get("http://" + ip + ":8080/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "hello-paas") {
		t.Fatalf("unexpected body %q", body)
	}
	ex := rt.Exec(context.Background(), proto.ExecRequest{ContainerID: req.ContainerID, Cmd: []string{"cat", "/www/index.html"}})
	if ex.Error != "" || !strings.Contains(ex.Stdout, "hello-paas") {
		t.Fatalf("exec: %+v", ex)
	}
	st := rt.Statuses(context.Background())
	if len(st) != 1 || !st[0].Running {
		t.Fatalf("statuses: %+v", st)
	}
	logs, _ := rt.Logs(req.ContainerID, 10)
	if len(logs) == 0 || logs[0] != "started" {
		t.Fatalf("logs: %v", logs)
	}
	t.Logf("ip=%s mem=%dMB", ip, st[0].MemoryMB)
}

// simulateReboot does what a host reboot does to a container: the task and
// the netns mount vanish, containerd's container record and the state dir stay.
// It returns a fresh Runtime over the same dataDir, already Recovered.
func simulateReboot(t *testing.T, rt *Runtime, dir, id string) *Runtime {
	t.Helper()
	ctx := context.Background()
	cctx := rt.ctx(ctx)
	rt.mu.Lock()
	m := rt.containers[id]
	cancel, nsPath := m.cancel, m.NetNS
	rt.mu.Unlock()
	if cancel != nil {
		cancel() // stop the watcher so it does not restart the task under us
	}
	ctr, err := rt.client.LoadContainer(cctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if task, err := ctr.Task(cctx, nil); err == nil {
		exit, err := task.Wait(cctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := task.Kill(cctx, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		<-exit
		if _, err := task.Delete(cctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := netns.LoadNetNS(nsPath).Remove(); err != nil {
		t.Fatal(err)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	rt2 := newTestRuntime(t, dir)
	rt2.Recover(ctx)
	return rt2
}

func waitRunning(t *testing.T, rt *Runtime, id string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !rt.isRunning(id) {
		if time.Now().After(deadline) {
			t.Fatal("container not running 30s after Recover")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func statusOf(t *testing.T, rt *Runtime, id string) proto.ContainerStatus {
	t.Helper()
	for _, s := range rt.Statuses(context.Background()) {
		if s.ContainerID == id {
			return s
		}
	}
	t.Fatalf("no status for %s", id)
	return proto.ContainerStatus{}
}

// assertServes polls the container's IP until the HTTP server answers.
func assertServes(t *testing.T, ip string) {
	t.Helper()
	if ip == "" {
		t.Fatal("empty IP")
	}
	c := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := c.Get("http://" + ip + ":8080/")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s: err=%v", ip, err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// assertSpecNetNS checks the netns path baked into the containerd spec is the
// one the runtime now tracks, and that it exists.
func assertSpecNetNS(t *testing.T, rt *Runtime, id string) {
	t.Helper()
	cctx := rt.ctx(context.Background())
	ctr, err := rt.client.LoadContainer(cctx, id)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := ctr.Spec(cctx)
	if err != nil {
		t.Fatal(err)
	}
	rt.mu.Lock()
	want := rt.containers[id].NetNS
	rt.mu.Unlock()
	got := ""
	for _, n := range spec.Linux.Namespaces {
		if n.Type == "network" {
			got = n.Path
		}
	}
	if got == "" || got != want {
		t.Fatalf("spec netns %q, runtime netns %q", got, want)
	}
	if closed, err := netns.LoadNetNS(got).Closed(); err != nil || closed {
		t.Fatalf("netns %s closed=%v err=%v", got, closed, err)
	}
}

func TestRecoverAfterReboot(t *testing.T) {
	dir := t.TempDir()
	rt := newTestRuntime(t, dir)
	req := httpReq(uniqueID("paas-test-rb-"))
	if _, err := rt.Run(context.Background(), req); err != nil {
		rt.Close()
		t.Fatal(err)
	}
	rt2 := simulateReboot(t, rt, dir, req.ContainerID)
	defer rt2.Close()
	defer rt2.Remove(context.Background(), req.ContainerID, time.Second)

	waitRunning(t, rt2, req.ContainerID)
	st := statusOf(t, rt2, req.ContainerID)
	if !st.Running || st.IP == "" {
		t.Fatalf("status: %+v", st)
	}
	assertServes(t, st.IP)
	assertSpecNetNS(t, rt2, req.ContainerID)
}

func TestUnfenceAfterReboot(t *testing.T) {
	dir := t.TempDir()
	rt := newTestRuntime(t, dir)
	req := httpReq(uniqueID("paas-test-uf-"))
	ctx := context.Background()
	if _, err := rt.Run(ctx, req); err != nil {
		rt.Close()
		t.Fatal(err)
	}
	if err := rt.Fence(ctx, req.ContainerID, time.Second); err != nil {
		rt.Close()
		t.Fatal(err)
	}
	rt2 := simulateReboot(t, rt, dir, req.ContainerID)
	defer rt2.Close()
	defer rt2.Remove(ctx, req.ContainerID, time.Second)

	// Recover is synchronous: a fenced container must stay down.
	if rt2.isRunning(req.ContainerID) {
		t.Fatal("fenced container running after Recover")
	}
	if err := rt2.Unfence(ctx, req.ContainerID); err != nil {
		t.Fatal(err)
	}
	waitRunning(t, rt2, req.ContainerID)
	st := statusOf(t, rt2, req.ContainerID)
	if !st.Running || st.Fenced {
		t.Fatalf("status: %+v", st)
	}
	assertServes(t, st.IP)
	assertSpecNetNS(t, rt2, req.ContainerID)
}

func TestRecoverDropsJob(t *testing.T) {
	dir := t.TempDir()
	rt := newTestRuntime(t, dir)
	req := httpReq(uniqueID("paas-test-job-"))
	req.Once, req.Command, req.Port, req.HealthCheck = true, []string{"sleep 60"}, 0, model.HealthCheck{}
	ctx := context.Background()
	// Run, not RunJob: nobody is waiting, like a caller that died with the agent.
	if _, err := rt.Run(ctx, req); err != nil {
		rt.Close()
		t.Fatal(err)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	rt2 := newTestRuntime(t, dir)
	defer rt2.Close()
	rt2.Recover(ctx)

	if _, err := rt2.client.LoadContainer(rt2.ctx(ctx), req.ContainerID); !errdefs.IsNotFound(err) {
		t.Fatalf("LoadContainer err = %v, want NotFound", err)
	}
	if _, err := os.Stat(rt2.containerDir(req.ContainerID)); !os.IsNotExist(err) {
		t.Fatalf("containerDir still there: %v", err)
	}
}

func TestRunLeakOnNetFiles(t *testing.T) {
	dir := t.TempDir()
	rt := newTestRuntime(t, dir)
	defer rt.Close()
	req := httpReq(uniqueID("paas-test-leak-"))
	// writeNetFiles fails when resolv.conf is a directory.
	if err := os.MkdirAll(filepath.Join(rt.containerDir(req.ContainerID), "resolv.conf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Run(context.Background(), req); err == nil {
		rt.Remove(context.Background(), req.ContainerID, time.Second)
		t.Fatal("Run succeeded, want writeNetFiles error")
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "netns")); len(ents) != 0 {
		t.Fatalf("netns leaked: %v", ents)
	}
	// host-local keeps one file per reservation, named by IP, containing the id.
	_ = filepath.WalkDir(filepath.Join(dir, "cni"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if b, _ := os.ReadFile(p); strings.Contains(string(b), req.ContainerID) {
			t.Errorf("IP reservation leaked: %s", p)
		}
		return nil
	})
	if _, err := os.Stat(rt.containerDir(req.ContainerID)); !os.IsNotExist(err) {
		t.Errorf("containerDir leaked: %v", err)
	}
}
