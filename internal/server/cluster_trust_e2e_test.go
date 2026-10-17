package server

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/thesunbg/paas/internal/bus"
	"github.com/thesunbg/paas/internal/pki"
	"github.com/thesunbg/paas/internal/proto"
)

// End to end through the real stack: join tokens and enroll over the HTTP API, then two nodes
// connect to the TLS NATS server with the credentials they were given. Complements the permission
// table test in internal/bus and the handler tests in cluster_trust_test.go.
func TestClusterTrustEndToEnd(t *testing.T) {
	s, h := trustServer(t)
	if err := s.registerBusHandlers(); err != nil {
		t.Fatal(err)
	}
	mkUser(t, s, "root@example.com", "password1", true)
	_, admin := login(t, h, "root@example.com", "password1", "198.51.100.1")

	type worker struct {
		id   string
		seed []byte
		pub  string
		nc   *nats.Conn
		errs chan error
	}
	join := func(name string) *worker {
		w := do(t, h, "POST", "/api/v1/cluster/join-tokens", admin, `{"roles":["worker"]}`)
		if w.Code != 200 {
			t.Fatalf("join token = %d %s", w.Code, w.Body)
		}
		var tk struct{ Token string }
		if err := json.Unmarshal(w.Body.Bytes(), &tk); err != nil {
			t.Fatal(err)
		}
		seed, pub := newNkey(t)
		code, out := enroll(t, h, tk.Token, name, pub)
		if code != 200 {
			t.Fatalf("enroll %s = %d", name, code)
		}
		if !strings.HasPrefix(out.NatsURL, "tls://") || out.CAPEM != s.caPEM {
			t.Fatalf("enroll reply: %+v", out)
		}
		// nats_url names the public host; the test dials the embedded server but verifies with the returned CA.
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(out.CAPEM)) {
			t.Fatal("ca_pem does not parse")
		}
		nc, err := bus.ConnectNode(s.ns.ClientURL(), seed, out.NodeID, pool, name)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(nc.Close)
		wk := &worker{id: out.NodeID, seed: seed, pub: pub, nc: nc, errs: make(chan error, 32)}
		nc.SetErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) { wk.errs <- err })
		if err := nc.Flush(); err != nil {
			t.Fatal(err)
		}
		return wk
	}
	a, b := join("a"), join("b")

	denied := func(what string, trigger func()) {
		t.Helper()
		trigger()
		if err := a.nc.Flush(); err != nil {
			t.Fatalf("%s: flush: %v", what, err)
		}
		select {
		case err := <-a.errs:
			if !strings.Contains(strings.ToLower(err.Error()), "permissions violation") {
				t.Fatalf("%s: error = %v", what, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: no permission violation", what)
		}
	}
	for _, subj := range []string{
		"paas.agent." + b.id + ".>",
		"_INBOX.>",
		proto.SubjBinary,
		"paas.node." + b.id + ".>",
		proto.SubjCPKick,
	} {
		subj := subj
		denied("sub "+subj, func() {
			if _, err := a.nc.SubscribeSync(subj); err != nil {
				t.Fatal(err)
			}
		})
	}

	// A's publishes to B's subjects are dropped: nothing reaches a full-permission subscriber.
	runSub, _ := s.nc.SubscribeSync(proto.AgentSubject(b.id, proto.CmdRun))
	hbSub, _ := s.nc.SubscribeSync(proto.SubjNodeHeartbeat(b.id))
	_ = s.nc.Flush()
	for _, subj := range []string{proto.AgentSubject(b.id, proto.CmdRun), proto.SubjNodeHeartbeat(b.id)} {
		subj := subj
		denied("pub "+subj, func() {
			if err := a.nc.Publish(subj, []byte(`{}`)); err != nil {
				t.Fatal(err)
			}
		})
	}
	_ = s.nc.Flush()
	for name, sub := range map[string]*nats.Subscription{"run": runSub, "heartbeat": hbSub} {
		if m, err := sub.NextMsg(300 * time.Millisecond); err == nil {
			t.Fatalf("B's %s subject received A's message: %q", name, m.Data)
		}
	}

	// Control plane -> A command, A replies.
	if _, err := a.nc.Subscribe(proto.AgentSubject(a.id, "ping"), func(m *nats.Msg) { _ = m.Respond([]byte(`{"pong":true}`)) }); err != nil {
		t.Fatal(err)
	}
	if err := a.nc.Flush(); err != nil {
		t.Fatal(err)
	}
	var pong struct{ Pong bool }
	if err := s.requestAgent(t.Context(), a.id, "ping", struct{}{}, &pong, 2*time.Second); err != nil || !pong.Pong {
		t.Fatalf("requestAgent(A): %v %+v", err, pong)
	}

	// A's heartbeat on its own subject updates A and only A.
	before, _ := s.store.NodeByID(t.Context(), a.id)
	beforeB, _ := s.store.NodeByID(t.Context(), b.id)
	time.Sleep(50 * time.Millisecond)
	var ack proto.HeartbeatAck
	if err := bus.Request(t.Context(), a.nc, proto.SubjNodeHeartbeat(a.id), proto.Heartbeat{Usage: proto.NodeUsage{MemoryUsedMB: 11}}, &ack); err != nil {
		t.Fatal(err)
	}
	after, _ := s.store.NodeByID(t.Context(), a.id)
	afterB, _ := s.store.NodeByID(t.Context(), b.id)
	if !after.LastSeenAt.After(before.LastSeenAt) || after.Usage.MemoryUsedMB != 11 {
		t.Fatalf("A not updated: last_seen %v -> %v usage %d", before.LastSeenAt, after.LastSeenAt, after.Usage.MemoryUsedMB)
	}
	if !afterB.LastSeenAt.Equal(beforeB.LastSeenAt) {
		t.Fatal("B's last_seen_at changed by A's heartbeat")
	}

	// Deleting A (cordoned first, as the API requires) disconnects it and its credential no longer connects.
	if err := s.store.UpdateNodeSettings(t.Context(), a.id, "cordoned", 1.5, 4, 0, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if w := do(t, h, "DELETE", "/api/v1/nodes/"+a.id, admin, nil); w.Code != 200 {
		t.Fatalf("delete = %d %s", w.Code, w.Body)
	}
	waitFor(t, "A disconnected", func() bool { return !a.nc.IsConnected() })
	time.Sleep(2500 * time.Millisecond) // two reconnect attempts at ReconnectWait=1s
	if a.nc.IsConnected() {
		t.Fatal("deleted node reconnected")
	}
	if !b.nc.IsConnected() {
		t.Fatal("B was disconnected by A's removal")
	}
	kp, err := nkeys.FromSeed(a.seed)
	if err != nil {
		t.Fatal(err)
	}
	pool := s.ca.Pool()
	if nc, err := nats.Connect(s.ns.ClientURL(), nats.Nkey(a.pub, kp.Sign), nats.Secure(&tls.Config{RootCAs: pool, ServerName: pki.ServerName}), nats.MaxReconnects(0)); err == nil {
		nc.Close()
		t.Fatal("deleted node's nkey connected")
	}
}

// The legacy flag decides whether the shared cluster token is a NATS credential.
func TestClusterTrustLegacyFlag(t *testing.T) {
	// Plain nats:// like an agent from before the cluster CA.
	tokenConnect := func(s *Server) error {
		nc, err := nats.Connect(strings.Replace(s.ns.ClientURL(), "tls://", "nats://", 1), nats.Token(s.ClusterToken), nats.MaxReconnects(0))
		if err == nil {
			nc.Close()
		}
		return err
	}
	off, _ := trustServer(t)
	if err := tokenConnect(off); err == nil {
		t.Fatal("cluster token connected with legacy off")
	}

	on, _ := trustServer(t)
	on.cfg.NATSLegacy = true
	if err := on.initTrust(t.Context()); err != nil {
		t.Fatal(err)
	}
	on.ns.Shutdown()
	var err error
	if on.ns, err = bus.StartServer("127.0.0.1:-1", on.auth, on.ca.ServerTLS(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(on.ns.Shutdown)
	if err := tokenConnect(on); err != nil {
		t.Fatalf("cluster token refused with legacy on: %v", err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timeout: %s", what)
}
