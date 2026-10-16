package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thesunbg/paas/internal/network"
)

func TestSetupBuildNetwork(t *testing.T) {
	dir := t.TempDir()
	buildkitDir = dir
	restarts := 0
	restartBuildkit = func() error { restarts++; return nil }
	nm, err := network.NewManager(t.TempDir(), t.TempDir(), "10.100")
	if err != nil {
		t.Fatal(err)
	}
	nm.Configure([2]int{10, 100}, 3, 0)
	a := &Agent{net: nm}

	toml := filepath.Join(dir, "buildkitd.toml")
	os.WriteFile(toml, []byte("[worker.containerd]\n  enabled = true\n"), 0o644)
	if err := a.setupBuildNetwork(); err == nil || restarts != 0 {
		t.Fatalf("host-network buildkit must be refused: err=%v restarts=%d", err, restarts)
	}

	os.WriteFile(toml, []byte("[worker.containerd]\n  networkMode = \"cni\"\n"), 0o644)
	if err := a.setupBuildNetwork(); err != nil || restarts != 1 {
		t.Fatalf("err=%v restarts=%d", err, restarts)
	}
	conf, _ := os.ReadFile(filepath.Join(dir, "cni.conflist"))
	for _, want := range []string{`"bridge": "pn1022"`, `"subnet": "10.103.255.128/26"`} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("conflist missing %s:\n%s", want, conf)
		}
	}
	if err := a.setupBuildNetwork(); err != nil || restarts != 1 {
		t.Fatalf("unchanged config must not restart buildkit: err=%v restarts=%d", err, restarts)
	}
}
