package build

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thesunbg/paas/internal/proto"
)

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestSanitizeSymlinks(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "b", "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, "/", filepath.Join(src, "x"))
	mustSymlink(t, "../../etc", filepath.Join(src, "y"))
	mustSymlink(t, "/nonexistent/zz", filepath.Join(src, ".dockerignore"))
	mustSymlink(t, "b", filepath.Join(src, "a"))
	mustSymlink(t, "b/file", filepath.Join(src, "c"))
	mustSymlink(t, "x", filepath.Join(src, "viaX")) // chained through an escaping link

	var logs []string
	if err := sanitizeSymlinks(src, func(l string) { logs = append(logs, l) }); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"x", "y", ".dockerignore", "viaX"} {
		if exists(filepath.Join(src, n)) {
			t.Errorf("%s should have been removed", n)
		}
	}
	for _, n := range []string{"a", "c"} {
		if !exists(filepath.Join(src, n)) {
			t.Errorf("%s should have been kept", n)
		}
	}
	if len(logs) != 4 {
		t.Errorf("want 4 log lines, got %v", logs)
	}

	// A dangling .dockerignore link no longer makes ensureDockerignore write
	// through it to a file outside the repo.
	outside := filepath.Join(t.TempDir(), "zz")
	mustSymlink(t, outside, filepath.Join(src, ".dockerignore"))
	if err := sanitizeSymlinks(src, func(string) {}); err != nil {
		t.Fatal(err)
	}
	ensureDockerignore(src)
	if exists(outside) {
		t.Fatal("file created outside the repository")
	}
	if fi, err := os.Lstat(filepath.Join(src, ".dockerignore")); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf(".dockerignore should be a regular file in the repo: %v", err)
	}
}

func TestSanitizeSymlinksUnderSymlinkedWorkDir(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "work")
	mustSymlink(t, real, link)
	if err := os.Mkdir(filepath.Join(real, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, "b", filepath.Join(real, "a"))
	if err := sanitizeSymlinks(link, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(real, "a")) {
		t.Fatal("internal link removed when src sits under a symlink")
	}
}

func gitFixture(t *testing.T, setup func(dir string)) string {
	t.Helper()
	dir := t.TempDir()
	setup(dir)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git unavailable: %s", out)
		}
	}
	return "file://" + dir
}

func TestInspectRootDirSymlinkEscape(t *testing.T) {
	repo := gitFixture(t, func(dir string) {
		mustSymlink(t, "/", filepath.Join(dir, "x"))
		mustSymlink(t, "b", filepath.Join(dir, "a"))
		_ = os.Mkdir(filepath.Join(dir, "b"), 0o755)
		_ = os.WriteFile(filepath.Join(dir, "b", "f"), []byte("x"), 0o644)
	})
	b := New("", t.TempDir(), 1)
	b.AllowLocalGit = true
	rep := b.Inspect(context.Background(), proto.DetectRequest{Repo: repo, RootDir: "x"})
	if !strings.Contains(rep.Error, `root directory "x" not found`) {
		t.Fatalf("want not found, got %q", rep.Error)
	}
}

func TestVetRepoRejects(t *testing.T) {
	b := New("", t.TempDir(), 1)
	for _, repo := range []string{
		"http://github.com/a/b",
		"https://127.0.0.1/a/b",
		"https://localhost/a/b",
		"https://[::1]/x",
		"https://user:pw@github.com/a/b",
		"https://169.254.169.254/latest",
		"git@github.com:a/b",
		"file:///tmp/x", // AllowLocalGit is off
	} {
		if _, err := b.vetRepo(context.Background(), repo); err == nil {
			t.Errorf("%s should be rejected", repo)
		}
		// checkout must fail before git is ever started: dest is never created.
		dest := filepath.Join(t.TempDir(), "dest")
		if _, _, err := b.checkout(context.Background(), proto.BuildRequest{Repo: repo}, dest, func(string) {}); err == nil {
			t.Errorf("checkout %s should fail", repo)
		}
		if exists(dest) {
			t.Errorf("checkout %s ran git", repo)
		}
	}
}

func TestVetRepoLocal(t *testing.T) {
	b := New("", t.TempDir(), 1)
	b.AllowLocalGit = true
	args, err := b.vetRepo(context.Background(), "file:///tmp/x")
	if err != nil || !reflect.DeepEqual(args, gitLocalArgs()) {
		t.Fatalf("got %v, %v", args, err)
	}
}

func TestGitGuardArgs(t *testing.T) {
	want := []string{
		"-c", "http.curloptResolve=github.com:443:140.82.112.3",
		"-c", "http.followRedirects=false",
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
	}
	if got := gitGuardArgs("github.com", "443", net.ParseIP("140.82.112.3")); !reflect.DeepEqual(got, want) {
		t.Errorf("v4: %v", got)
	}
	if got := gitGuardArgs("h.example", "8443", net.ParseIP("2606:4700::1111")); got[1] != "http.curloptResolve=h.example:8443:[2606:4700::1111]" {
		t.Errorf("v6: %v", got)
	}
}
