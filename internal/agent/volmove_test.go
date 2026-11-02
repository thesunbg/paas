package agent

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/config"
	"github.com/thesunbg/paas/internal/proto"
)

func moveAgent(t *testing.T) *Agent {
	return &Agent{cfg: config.Agent{DataDir: t.TempDir()}, moveIP: "127.0.0.1"}
}

func writeTreeFixture(t *testing.T, root string) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "data/sub"), 0o750))
	must(os.WriteFile(filepath.Join(root, "data/sub/db.sqlite"), []byte(strings.Repeat("row\n", 50000)), 0o640))
	must(os.WriteFile(filepath.Join(root, "data/exec.sh"), []byte("#!/bin/sh\n"), 0o755))
	must(os.WriteFile(filepath.Join(root, "data/empty"), nil, 0o600))
	must(os.Symlink("sub/db.sqlite", filepath.Join(root, "data/link")))
}

func recvSend(t *testing.T, a, b *Agent, id, token, sendToken string) (*proto.VolumeSendReply, string) {
	t.Helper()
	rr := b.handleVolumeRecv(context.Background(), &proto.VolumeRecvRequest{ID: id, Token: token, From: "127.0.0.1", Dir: "moves/" + id})
	if rr.Error != "" {
		t.Fatal(rr.Error)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(rr.Port))
	rep := a.handleVolumeSend(context.Background(), &proto.VolumeSendRequest{ID: id, Addr: addr, Token: sendToken, Dir: "volumes/svc"})
	return rep, "moves/" + id
}

func TestVolumeMoveRoundTrip(t *testing.T) {
	a, b := moveAgent(t), moveAgent(t)
	writeTreeFixture(t, filepath.Join(a.cfg.DataDir, "volumes/svc"))
	tok := strings.Repeat("ab", 32)
	rep, staging := recvSend(t, a, b, "m1", tok, tok)
	if rep.Error != "" {
		t.Fatal(rep.Error)
	}
	if rep.Sent != rep.Received || rep.Sent.Files != 6 || rep.Sent.Bytes != 200000+10 {
		t.Fatalf("sent %+v received %+v", rep.Sent, rep.Received)
	}
	// The receiver's disk holds what was sent, and so does the sender's (sumTree = stream sum).
	d := b.handleVolumeDigest(context.Background(), &proto.VolumeDigestRequest{Dir: staging})
	if d.Error != "" || d.Sum != rep.Sent {
		t.Fatalf("digest on disk %+v (%s), sent %+v", d.Sum, d.Error, rep.Sent)
	}
	if src := a.handleVolumeDigest(context.Background(), &proto.VolumeDigestRequest{Dir: "volumes/svc"}); src.Sum != rep.Sent {
		t.Fatalf("source sum %+v, stream sum %+v", src.Sum, rep.Sent)
	}
	if c := b.handleVolumeCommit(context.Background(), &proto.VolumeCommitRequest{From: staging, To: "volumes/svc"}); c.Error != "" {
		t.Fatal(c.Error)
	}
	dst := filepath.Join(b.cfg.DataDir, "volumes/svc/data")
	if st, err := os.Stat(filepath.Join(dst, "exec.sh")); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("exec.sh: %v %v", st, err)
	}
	if st, _ := os.Stat(filepath.Join(dst, "sub")); st.Mode().Perm() != 0o750 {
		t.Fatalf("dir mode %v", st.Mode().Perm())
	}
	if l, _ := os.Readlink(filepath.Join(dst, "link")); l != "sub/db.sqlite" {
		t.Fatalf("symlink %q", l)
	}
	if _, err := os.Stat(filepath.Join(b.cfg.DataDir, staging)); !os.IsNotExist(err) {
		t.Fatal("staging directory left behind")
	}
	// A changed byte changes the sum.
	if err := os.WriteFile(filepath.Join(dst, "empty"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if d2 := b.handleVolumeDigest(context.Background(), &proto.VolumeDigestRequest{Dir: "volumes/svc"}); d2.Sum.Digest == rep.Sent.Digest {
		t.Fatal("a modified tree has the same digest")
	}
}

// A service whose volumes were never written: an empty tree arrives.
func TestVolumeMoveMissingSource(t *testing.T) {
	a, b := moveAgent(t), moveAgent(t)
	tok := strings.Repeat("cd", 32)
	rep, staging := recvSend(t, a, b, "m2", tok, tok)
	if rep.Error != "" || rep.Sent.Files != 0 || rep.Sent != rep.Received {
		t.Fatalf("%+v", rep)
	}
	if st, err := os.Stat(filepath.Join(b.cfg.DataDir, staging)); err != nil || !st.IsDir() {
		t.Fatalf("staging: %v", err)
	}
}

func TestVolumeMoveWrongToken(t *testing.T) {
	a, b := moveAgent(t), moveAgent(t)
	writeTreeFixture(t, filepath.Join(a.cfg.DataDir, "volumes/svc"))
	rep, staging := recvSend(t, a, b, "m3", strings.Repeat("ef", 32), strings.Repeat("00", 32))
	if rep.Error == "" {
		t.Fatal("a sender without the token was accepted")
	}
	if _, err := os.Stat(filepath.Join(b.cfg.DataDir, staging, "data")); err == nil {
		t.Fatal("data written without the token")
	}
	// Too short a token is refused outright.
	if r := b.handleVolumeRecv(context.Background(), &proto.VolumeRecvRequest{ID: "m4", Token: "short", Dir: "moves/m4"}); r.Error == "" {
		t.Fatal("short token accepted")
	}
}

func TestVolumeReceiveChecksSource(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "in")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	tok := strings.Repeat("aa", 32)
	go func() {
		_, err := receiveVolume(ctx, ln, tok, "10.9.9.9", dir)
		done <- err
	}()
	src := t.TempDir()
	writeTreeFixture(t, src)
	if _, _, err := sendVolume(context.Background(), "", ln.Addr().String(), tok, src); err == nil {
		t.Fatal("a connection from another address was served")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("receive succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not end the receive")
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("data written from a foreign address")
	}
}

func TestVolumeSendRefusesNonMesh(t *testing.T) {
	a := moveAgent(t)
	rep := a.handleVolumeSend(context.Background(), &proto.VolumeSendRequest{ID: "x", Addr: "203.0.113.5:4000", Token: strings.Repeat("a", 64), Dir: "volumes/svc"})
	if rep.Error == "" {
		t.Fatal("sent to a public address")
	}
	noMeshAgent := &Agent{cfg: config.Agent{DataDir: t.TempDir()}}
	if r := noMeshAgent.handleVolumeRecv(context.Background(), &proto.VolumeRecvRequest{ID: "x", Token: strings.Repeat("a", 64), Dir: "moves/x"}); r.Error != noMesh {
		t.Fatalf("receive without a mesh: %q", r.Error)
	}
}

func TestVolumeReceiveCancel(t *testing.T) {
	b := moveAgent(t)
	tok := strings.Repeat("bb", 32)
	rr := b.handleVolumeRecv(context.Background(), &proto.VolumeRecvRequest{ID: "m5", Token: tok, From: "127.0.0.1", Dir: "moves/m5"})
	if rr.Error != "" {
		t.Fatal(rr.Error)
	}
	if r := b.handleVolumeRecv(context.Background(), &proto.VolumeRecvRequest{ID: "m5", Token: tok, Dir: "moves/m5"}); r.Error == "" {
		t.Fatal("a second receive of the same move was opened")
	}
	if r := b.handleVolumeRecv(context.Background(), &proto.VolumeRecvRequest{ID: "m5", Dir: "moves/m5", Cancel: true}); r.Error != "" {
		t.Fatal(r.Error)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(rr.Port)), time.Second)
		if err != nil {
			break // listener closed
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Fatal("the receiver still listens after a cancel")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
