package agent

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Overridden by tests.
var (
	buildkitDir     = "/etc/buildkit"
	restartBuildkit = func() error {
		if out, err := exec.Command("systemctl", "restart", "buildkit.service").CombinedOutput(); err != nil {
			return fmt.Errorf("restart buildkit: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
)

// setupBuildNetwork puts BuildKit's RUN steps on the firewalled build bridge.
// The installer points buildkitd at cni.conflist; the subnet depends on the
// node index, so the file is written here, after registration.
func (a *Agent) setupBuildNetwork() error {
	toml, err := os.ReadFile(filepath.Join(buildkitDir, "buildkitd.toml"))
	if err != nil {
		return err
	}
	if !bytes.Contains(toml, []byte(`networkMode = "cni"`)) {
		return fmt.Errorf("buildkitd.toml has no networkMode = \"cni\": build steps would run on the host network; re-run the installer on this node")
	}
	conf, err := a.net.BuildConfList()
	if err != nil {
		return err
	}
	path := filepath.Join(buildkitDir, "cni.conflist")
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, conf) {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, conf, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return restartBuildkit()
}
