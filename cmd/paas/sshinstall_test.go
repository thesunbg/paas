package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"slices"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/thesunbg/paas/internal/remote"
)

func TestParseSSHTarget(t *testing.T) {
	for _, c := range []struct {
		in, user, host string
		port           int
	}{
		{"root@1.2.3.4", "root", "1.2.3.4", 22},
		{"deploy@host.example:2222", "deploy", "host.example", 2222},
		{"1.2.3.4", "root", "1.2.3.4", 22},
		{"admin@[2001:db8::1]:2200", "admin", "2001:db8::1", 2200},
	} {
		u, h, p, err := parseSSHTarget(c.in, 22)
		if err != nil || u != c.user || h != c.host || p != c.port {
			t.Errorf("%s: %s %s %d %v", c.in, u, h, p, err)
		}
	}
	if _, _, _, err := parseSSHTarget("@host", 22); err == nil {
		t.Error("empty user accepted")
	}
}

func TestSplitArgsAndSSHFlag(t *testing.T) {
	pos, flags, rest := splitArgs([]string{"root@h", "--password", "--roles", "worker,build", "--yes", "--", "--mirror", "m"}, "password", "yes")
	if !slices.Equal(pos, []string{"root@h"}) || !slices.Equal(flags, []string{"--password", "--roles", "worker,build", "--yes"}) || !slices.Equal(rest, []string{"--mirror", "m"}) {
		t.Fatalf("pos %v flags %v rest %v", pos, flags, rest)
	}
	if !hasSSHFlag([]string{"--domain", "d", "--ssh", "root@h"}) || !hasSSHFlag([]string{"--ssh=root@h"}) || hasSSHFlag([]string{"wordpress", "--domain", "x"}) {
		t.Fatal("hasSSHFlag")
	}
}

func TestConfirmHostKeyFingerprint(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, _ := ssh.NewSignerFromKey(priv)
	key := s.PublicKey()
	if err := confirmHostKey("192.0.2.9:22", key, remote.Fingerprint(key), false); err != nil {
		t.Fatal(err)
	}
	var mm *remote.HostKeyMismatchError
	if err := confirmHostKey("192.0.2.9:22", key, "SHA256:AAAA", true); !errors.As(err, &mm) {
		t.Fatalf("mismatch with --yes must still fail: %v", err)
	}
}
