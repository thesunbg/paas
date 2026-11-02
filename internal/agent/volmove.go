package agent

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thesunbg/paas/internal/network"
	"github.com/thesunbg/paas/internal/proto"
)

// Volume moves: the volumes of a stopped service travel from its node (the sender) straight to
// another node (the receiver), as a plain tar stream over one TCP connection.
//
// The connection only ever exists on the WireGuard mesh: the receiver listens on its mesh
// address and nowhere else, the sender dials from its own mesh address, so the bytes are
// encrypted and authenticated by the mesh keys of both nodes. On top of that the receiver only
// accepts one connection, from the sender's mesh address, that opens with the one-time token the
// control plane gave both sides over the (authenticated, TLS) bus. Everything else is dropped.
//
// Both sides checksum what they move (treeSum); the control plane compares the sender's sum
// with a fresh sum of what the receiver wrote to disk before it switches the service over.

const (
	volMoveMagic = "PAASVOLMOVE1"
	// volMoveAcceptWait is how long a receiver waits for the sender to connect.
	volMoveAcceptWait = 2 * time.Minute
	// volMoveIdle cuts a transfer that moved no byte for this long.
	volMoveIdle = 2 * time.Minute
)

// treeSum is a checksum of a directory tree: every entry's name, type, permissions, owner, size,
// link target and content, in walk order. Times are left out (they are copied, not compared).
type treeSum struct {
	h     hash.Hash
	bytes int64
	files int64
}

func newTreeSum() *treeSum { return &treeSum{h: sha256.New()} }

func (t *treeSum) header(h *tar.Header) {
	size := int64(0)
	if h.Typeflag == tar.TypeReg {
		size = h.Size
		t.bytes += size
	}
	t.files++
	fmt.Fprintf(t.h, "%s\x00%c\x00%o\x00%d\x00%d\x00%d\x00%s\x00", h.Name, h.Typeflag, h.Mode&0o777, h.Uid, h.Gid, size, h.Linkname)
}

func (t *treeSum) result() proto.VolumeSum {
	return proto.VolumeSum{Bytes: t.bytes, Files: t.files, Digest: hex.EncodeToString(t.h.Sum(nil))}
}

// sumTree checksums a directory on disk the way a transfer of it is checksummed.
func sumTree(dir string) (proto.VolumeSum, error) {
	sum := newTreeSum()
	err := walkTree(dir, func(path string, h *tar.Header) error {
		sum.header(h)
		if h.Typeflag != tar.TypeReg {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.CopyN(sum.h, io.MultiReader(f, zeroReader{}), h.Size)
		return err
	})
	return sum.result(), err
}

// idleConn renews the deadline of a connection before each read or write, so a transfer only
// fails when it stalls.
type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c idleConn) Read(p []byte) (int, error) {
	_ = c.Conn.SetDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(p)
}

func (c idleConn) Write(p []byte) (int, error) {
	_ = c.Conn.SetDeadline(time.Now().Add(c.idle))
	return c.Conn.Write(p)
}

// receiveVolume serves one transfer on ln into dir (which must not exist yet) and closes ln.
// Connections from another address than from, or without the token, are closed unanswered.
func receiveVolume(ctx context.Context, ln net.Listener, token, from, dir string) (proto.VolumeSum, error) {
	defer ln.Close()
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return proto.VolumeSum{}, ctx.Err()
			}
			return proto.VolumeSum{}, err
		}
		if from != "" {
			host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
			if host != from {
				slog.Warn("volume move: connection from an unexpected address", "addr", conn.RemoteAddr().String(), "want", from)
				conn.Close()
				continue
			}
		}
		c := idleConn{conn, volMoveIdle}
		br := bufio.NewReader(c)
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		line, err := br.ReadString('\n')
		want := volMoveMagic + " " + token + "\n"
		if err != nil || subtle.ConstantTimeCompare([]byte(line), []byte(want)) != 1 {
			slog.Warn("volume move: connection without the token", "addr", conn.RemoteAddr().String())
			conn.Close()
			continue
		}
		_ = ln.Close() // one transfer per token
		stopConn := context.AfterFunc(ctx, func() { _ = conn.Close() })
		sum, err := receiveStream(br, dir)
		stopConn()
		if err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			fmt.Fprintf(c, "ERR %s\n", strings.ReplaceAll(err.Error(), "\n", " "))
		} else {
			fmt.Fprintf(c, "OK %d %d %s\n", sum.Bytes, sum.Files, sum.Digest)
		}
		conn.Close()
		return sum, err
	}
}

func receiveStream(r io.Reader, dir string) (proto.VolumeSum, error) {
	if _, err := os.Lstat(dir); err == nil {
		return proto.VolumeSum{}, fmt.Errorf("%s already exists", filepath.Base(dir))
	}
	sum := newTreeSum()
	if err := extractTar(tar.NewReader(r), dir, sum); err != nil {
		return proto.VolumeSum{}, err
	}
	return sum.result(), nil
}

// sendVolume streams dir to a receiver at addr and returns what it sent and what the receiver
// says it wrote. local, when set, is the address to dial from (the sender's mesh address).
func sendVolume(ctx context.Context, local, addr, token, dir string) (sent, got proto.VolumeSum, err error) {
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		// A service whose volumes were never written to has no directory yet: send an empty tree.
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return sent, got, err
		}
		if err == nil {
			return sent, got, fmt.Errorf("%s is not a directory", dir)
		}
	}
	d := net.Dialer{Timeout: 15 * time.Second}
	if local != "" {
		d.LocalAddr = &net.TCPAddr{IP: net.ParseIP(local)}
	}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return sent, got, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	c := idleConn{conn, volMoveIdle}
	bw := bufio.NewWriterSize(c, 256<<10)
	if _, err := io.WriteString(bw, volMoveMagic+" "+token+"\n"); err != nil {
		return sent, got, err
	}
	sum := newTreeSum()
	tw := tar.NewWriter(bw)
	if err := errors.Join(writeTree(tw, dir, sum), tw.Close(), bw.Flush()); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return sent, got, fmt.Errorf("send: %w", err)
	}
	sent = sum.result()
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return sent, got, fmt.Errorf("no answer from the receiving node: %w", err)
	}
	line = strings.TrimSpace(line)
	if msg, ok := strings.CutPrefix(line, "ERR "); ok {
		return sent, got, fmt.Errorf("receiving node: %s", msg)
	}
	f := strings.Fields(line)
	if len(f) != 4 || f[0] != "OK" {
		return sent, got, fmt.Errorf("unexpected answer from the receiving node: %q", line)
	}
	got.Bytes, _ = strconv.ParseInt(f[1], 10, 64)
	got.Files, _ = strconv.ParseInt(f[2], 10, 64)
	got.Digest = f[3]
	return sent, got, nil
}

// volumeRecvs are the receives in progress, by move id: cancelling one ends its transfer.
type volumeRecvs struct {
	mu sync.Mutex
	m  map[string]context.CancelFunc
}

func (v *volumeRecvs) add(id string, cancel context.CancelFunc) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.m == nil {
		v.m = map[string]context.CancelFunc{}
	}
	if _, ok := v.m[id]; ok {
		return false
	}
	v.m[id] = cancel
	return true
}

func (v *volumeRecvs) done(id string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.m, id)
}

func (v *volumeRecvs) cancel(id string) {
	v.mu.Lock()
	c := v.m[id]
	v.mu.Unlock()
	if c != nil {
		c()
	}
}

// meshIP is this node's WireGuard address, empty when the mesh is not up.
func (a *Agent) meshIP() string {
	if a.moveIP != "" {
		return a.moveIP
	}
	if a.mesh == nil {
		return ""
	}
	return network.NodeMeshIP(a.base, a.index)
}

const noMesh = "the WireGuard mesh is not up on this node: volumes only move over the mesh"

// handleVolumeRecv opens a one-shot receiver on the mesh address and answers with its port; the
// transfer itself runs in the background. Cancel ends a receive and removes what it wrote.
func (a *Agent) handleVolumeRecv(ctx context.Context, req *proto.VolumeRecvRequest) *proto.VolumeRecvReply {
	dir, err := a.dataPath(req.Dir)
	if err != nil || dir == "" || req.ID == "" {
		return &proto.VolumeRecvReply{Error: "id and dir are required"}
	}
	if req.Cancel {
		a.recvs.cancel(req.ID)
		_ = os.RemoveAll(dir)
		return &proto.VolumeRecvReply{}
	}
	if len(req.Token) < 32 {
		return &proto.VolumeRecvReply{Error: "a token of at least 32 characters is required"}
	}
	ip := a.meshIP()
	if ip == "" {
		return &proto.VolumeRecvReply{Error: noMesh}
	}
	// What an earlier attempt of the same move left behind is not trusted.
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return &proto.VolumeRecvReply{Error: err.Error()}
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		return &proto.VolumeRecvReply{Error: err.Error()}
	}
	rctx, cancel := context.WithCancel(context.Background())
	if !a.recvs.add(req.ID, cancel) {
		cancel()
		ln.Close()
		return &proto.VolumeRecvReply{Error: "a receive for this move is already running"}
	}
	done := a.work()
	go func() {
		defer done()
		defer a.recvs.done(req.ID)
		defer cancel()
		// Nobody connecting within the wait ends the receive.
		t := time.AfterFunc(volMoveAcceptWait, func() { _ = ln.Close() })
		sum, err := receiveVolume(rctx, acceptOnce{ln, t}, req.Token, req.From, dir)
		if err != nil {
			slog.Warn("volume move: receive", "move", req.ID, "err", err)
			_ = os.RemoveAll(dir)
			return
		}
		slog.Info("volume move: received", "move", req.ID, "bytes", sum.Bytes, "files", sum.Files)
	}()
	return &proto.VolumeRecvReply{Port: ln.Addr().(*net.TCPAddr).Port}
}

// acceptOnce stops the accept timer once a connection arrives.
type acceptOnce struct {
	net.Listener
	t *time.Timer
}

func (l acceptOnce) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.t.Stop()
	}
	return c, err
}

func (a *Agent) handleVolumeSend(ctx context.Context, req *proto.VolumeSendRequest) *proto.VolumeSendReply {
	defer a.work()()
	dir, err := a.dataPath(req.Dir)
	if err != nil || dir == "" || req.Addr == "" {
		return &proto.VolumeSendReply{Error: "dir and addr are required"}
	}
	ip := a.meshIP()
	if ip == "" {
		return &proto.VolumeSendReply{Error: noMesh}
	}
	host, _, err := net.SplitHostPort(req.Addr)
	if err != nil || !a.onMesh(host) {
		return &proto.VolumeSendReply{Error: "the receiver is not a mesh address"}
	}
	sent, got, err := sendVolume(ctx, ip, req.Addr, req.Token, dir)
	if err != nil {
		return &proto.VolumeSendReply{Sent: sent, Error: err.Error()}
	}
	return &proto.VolumeSendReply{Sent: sent, Received: got}
}

// onMesh reports whether ip is a node address of this cluster's mesh (never a public address).
func (a *Agent) onMesh(ip string) bool {
	if a.moveIP != "" {
		return ip == a.moveIP // tests: loopback
	}
	for i := 1; i <= network.MaxNodes(a.base); i++ {
		if network.NodeMeshIP(a.base, i) == ip {
			return true
		}
	}
	return false
}

func (a *Agent) handleVolumeDigest(ctx context.Context, req *proto.VolumeDigestRequest) *proto.VolumeDigestReply {
	defer a.work()()
	dir, err := a.dataPath(req.Dir)
	if err != nil || dir == "" {
		return &proto.VolumeDigestReply{Error: "dir is required"}
	}
	if _, err := os.Stat(dir); err != nil {
		return &proto.VolumeDigestReply{Error: err.Error()}
	}
	sum, err := sumTree(dir)
	if err != nil {
		return &proto.VolumeDigestReply{Error: err.Error()}
	}
	return &proto.VolumeDigestReply{Sum: sum}
}

// handleVolumeCommit puts a received tree in place of a service's volumes (whatever was there is
// a stale copy from an earlier stay on this node). Both paths must be on the same filesystem.
func (a *Agent) handleVolumeCommit(ctx context.Context, req *proto.VolumeCommitRequest) *proto.GenericReply {
	from, err := a.dataPath(req.From)
	if err != nil {
		return &proto.GenericReply{Error: err.Error()}
	}
	to, err := a.dataPath(req.To)
	if err != nil || from == "" || to == "" || to == filepath.Clean(a.cfg.DataDir) {
		return &proto.GenericReply{Error: "from and to are required"}
	}
	if _, err := os.Stat(from); err != nil {
		return &proto.GenericReply{Error: err.Error()}
	}
	if err := os.RemoveAll(to); err != nil {
		return &proto.GenericReply{Error: err.Error()}
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return &proto.GenericReply{Error: err.Error()}
	}
	if err := os.Rename(from, to); err != nil {
		return &proto.GenericReply{Error: err.Error()}
	}
	return &proto.GenericReply{}
}
