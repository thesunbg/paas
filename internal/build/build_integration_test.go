//go:build integration

package build

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
)

func gitRepoFrom(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("cp", "-a", src+"/.", dir).CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatal(string(out))
		}
	}
	return "file://" + dir
}

func examples() string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(f), "..", "..", "examples")
}

// Requires buildkitd with the containerd worker on /run/buildkit/buildkitd.sock.
func TestBuildGoAndStatic(t *testing.T) {
	b := New("unix:///run/buildkit/buildkitd.sock", t.TempDir(), 2)
	b.AllowLocalGit = true
	log := func(l string) { t.Log(l) }

	rep := b.Build(context.Background(), proto.BuildRequest{
		DeploymentID: "d1", ServiceID: "s1", ServiceType: model.TypeWeb,
		Repo: gitRepoFrom(t, filepath.Join(examples(), "go-http")), Branch: "main",
		ImageName: "paas.local/test-go:d1",
	}, log)
	if rep.Error != "" {
		t.Fatal(rep.Error)
	}
	if rep.Image == "" || rep.CommitSHA == "" || rep.Detected != "go" {
		t.Fatalf("unexpected reply %+v", rep)
	}

	rep = b.Build(context.Background(), proto.BuildRequest{
		DeploymentID: "d2", ServiceID: "s2", ServiceType: model.TypeStatic,
		Repo: gitRepoFrom(t, filepath.Join(examples(), "static-site")), Branch: "main",
	}, log)
	if rep.Error != "" {
		t.Fatal(rep.Error)
	}
	if _, err := os.Stat(filepath.Join(rep.StaticPath, "assets", "style.css")); err != nil {
		t.Fatalf("static output missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rep.StaticPath, ".git")); err == nil {
		t.Fatal(".git must not be exported")
	}
}
