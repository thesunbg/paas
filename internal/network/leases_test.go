package network

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReleaseLeases(t *testing.T) {
	m := &Manager{dataDir: t.TempDir()}
	root := m.ipamDir()
	write := func(netw, name, content string) string {
		d := filepath.Join(root, netw)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(d, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	gone := []string{
		write("pn1", "10.0.0.2", "abc\r\neth0"),
		write("pn2", "10.0.1.2", "abc\neth0"),
	}
	kept := []string{
		write("pn1", "10.0.0.3", "abcd\r\neth0"),
		write("pn2", "10.0.1.3", "xabc\r\neth0"),
		write("pn1", "last_reserved_ip.0", "abc"),
		write("pn1", "lock", "abc"),
	}
	if n := m.ReleaseLeases("abc"); n != 2 {
		t.Fatalf("removed %d, want 2", n)
	}
	for _, p := range gone {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should be removed", p)
		}
	}
	for _, p := range kept {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s should remain: %v", p, err)
		}
	}
	if n := (&Manager{dataDir: t.TempDir()}).ReleaseLeases("abc"); n != 0 {
		t.Errorf("missing ipam dir: removed %d", n)
	}
}
