package remote

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeShell answers one exec request: what the command prints, its exit code. stdin is
// what the client sent (read until EOF or a newline-terminated line for sudo -S).
type fakeShell func(cmd string, stdin string, out io.Writer, closed <-chan struct{}) int

// testServer is an in-process SSH server on 127.0.0.1 with a fixed host key.
type testServer struct {
	addr     string
	hostKey  ssh.Signer
	authHits atomic.Int32 // authentication attempts seen (proves a mismatch is refused before auth)

	mu       sync.Mutex
	commands []string
	stdins   []string
}

func newTestSSHServer(t *testing.T, user, password string, authorized ssh.PublicKey, shell fakeShell) *testServer {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	hk, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	ts := &testServer{hostKey: hk}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			ts.authHits.Add(1)
			if c.User() == user && password != "" && string(pw) == password {
				return nil, nil
			}
			return nil, io.EOF
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			ts.authHits.Add(1)
			if c.User() == user && authorized != nil && bytes.Equal(k.Marshal(), authorized.Marshal()) {
				return nil, nil
			}
			return nil, io.EOF
		},
	}
	cfg.AddHostKey(hk)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ts.addr = ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go ts.serve(c, cfg, shell)
		}
	}()
	return ts
}

func (ts *testServer) serve(c net.Conn, cfg *ssh.ServerConfig, shell fakeShell) {
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		_ = c.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go ts.session(ch, creqs, shell)
	}
}

func (ts *testServer) session(ch ssh.Channel, reqs <-chan *ssh.Request, shell fakeShell) {
	defer ch.Close()
	closed := make(chan struct{})
	var once sync.Once
	for req := range reqs {
		switch req.Type {
		case "exec":
			n := binary.BigEndian.Uint32(req.Payload[:4])
			cmd := string(req.Payload[4 : 4+n])
			_ = req.Reply(true, nil)
			var smu sync.Mutex
			var stdin bytes.Buffer
			done := make(chan struct{})
			go func() {
				// Collect what the client sends (a sudo -S password line).
				buf := make([]byte, 4096)
				for {
					n, err := ch.Read(buf)
					smu.Lock()
					stdin.Write(buf[:n])
					smu.Unlock()
					if err != nil {
						return
					}
				}
			}()
			go func() {
				for r := range reqs {
					if r.Type == "signal" {
						once.Do(func() { close(closed) })
					}
					if r.WantReply {
						_ = r.Reply(false, nil)
					}
				}
				once.Do(func() { close(closed) })
			}()
			// Give a sudo -S password line a moment to arrive.
			time.Sleep(20 * time.Millisecond)
			smu.Lock()
			in := stdin.String()
			smu.Unlock()
			ts.mu.Lock()
			ts.commands = append(ts.commands, cmd)
			ts.stdins = append(ts.stdins, in)
			ts.mu.Unlock()
			go func() {
				code := shell(cmd, in, ch, closed)
				st := make([]byte, 4)
				binary.BigEndian.PutUint32(st, uint32(code))
				_, _ = ch.SendRequest("exit-status", false, st)
				close(done)
				_ = ch.Close()
			}()
			<-done
			return
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

func (ts *testServer) seen() ([]string, []string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]string(nil), ts.commands...), append([]string(nil), ts.stdins...)
}
