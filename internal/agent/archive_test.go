package agent

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestArchiveRoundTrip(t *testing.T) {
	src := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(src, "a/b"), 0o750))
	must(os.WriteFile(filepath.Join(src, "a/b/data.db"), []byte("hello"), 0o640))
	must(os.Symlink("/etc", filepath.Join(src, "link")))
	file := filepath.Join(t.TempDir(), "v.tar.gz")
	if _, err := archiveDir(src, file); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "out")
	must(extractArchive(file, dest))
	got, err := os.ReadFile(filepath.Join(dest, "a/b/data.db"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("%q %v", got, err)
	}
	if st, _ := os.Stat(filepath.Join(dest, "a")); st.Mode().Perm() != 0o750 {
		t.Fatalf("dir mode %v", st.Mode().Perm())
	}
	if l, _ := os.Readlink(filepath.Join(dest, "link")); l != "/etc" {
		t.Fatalf("symlink %q", l)
	}
}

// A symlink followed by a file "inside" it must not write outside the target.
func TestExtractRefusesSymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	file := filepath.Join(t.TempDir(), "evil.tar.gz")
	f, _ := os.Create(file)
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	_ = tw.WriteHeader(&tar.Header{Name: "x", Typeflag: tar.TypeSymlink, Linkname: outside})
	_ = tw.WriteHeader(&tar.Header{Name: "x/pwned", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3})
	_, _ = tw.Write([]byte("bad"))
	tw.Close()
	zw.Close()
	f.Close()
	if err := extractArchive(file, filepath.Join(t.TempDir(), "dest")); err == nil {
		t.Fatal("extraction through a symlink succeeded")
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned")); err == nil {
		t.Fatal("wrote outside the target")
	}
}
