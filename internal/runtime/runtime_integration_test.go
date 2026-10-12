//go:build integration

package runtime

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/network"
	"github.com/thesunbg/paas/internal/proto"
)

// Requires root, containerd at /run/containerd/containerd.sock and CNI plugins in /opt/cni/bin.
func TestRunHTTPContainer(t *testing.T) {
	dir := t.TempDir()
	nm, err := network.NewManager("/opt/cni/bin", dir, "10.250")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := New(context.Background(), "/run/containerd/containerd.sock", "paas-test", dir, "crun", nm)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	img := os.Getenv("TEST_IMAGE")
	if img == "" {
		img = "mirror.gcr.io/library/busybox:1.36"
	}
	req := proto.RunRequest{
		InstanceID: "i1", ServiceID: "s1", ContainerID: "paas-test-" + time.Now().Format("150405"),
		NetIndex: 7, Hostname: "web", Image: img, PullImage: true,
		Env:     map[string]string{"PORT": "8080"},
		Command: []string{"mkdir -p /www && echo hello-paas > /www/index.html && echo started && exec httpd -f -p 8080 -h /www"},
		Port:    8080, Resources: model.Resources{MemoryLimitMB: 64, CPULimitMillis: 500},
		QoS: model.QoSBurstable, HealthCheck: model.HealthCheck{Type: "http", Path: "/", TimeoutSec: 30},
	}
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
