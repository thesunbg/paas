package bus

import (
	"crypto/tls"
	"crypto/x509"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/thesunbg/paas/internal/pki"
)

type testNode struct {
	seed string
	pub  string
}

func newNode(t *testing.T) testNode {
	t.Helper()
	kp, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := kp.Seed()
	pub, _ := kp.PublicKey()
	return testNode{string(seed), pub}
}

// testPKI returns a server TLS config (SAN nats.paas.internal) and the CA pool.
func testPKI(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	certPEM, keyPEM, err := pki.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	ca, err := pki.ParseCA(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return ca.ServerTLS(), ca.Pool()
}

type env struct {
	ns    *server.Server
	auth  *Auth
	ca    *x509.CertPool
	url   string
	nodes map[string]string // nkey -> nodeID
}

func newEnv(t *testing.T, enroll map[testNode]string, legacy ...string) *env {
	t.Helper()
	tlsCfg, pool := testPKI(t)
	e := &env{ca: pool, nodes: map[string]string{}}
	for n, id := range enroll {
		e.nodes[n.pub] = id
	}
	e.auth = &Auth{Internal: "internal-secret", Lookup: func(nkey string) (string, []string, bool) {
		id, ok := e.nodes[nkey]
		return id, nil, ok
	}}
	if len(legacy) > 0 {
		e.auth.Legacy = legacy[0]
	}
	ns, err := StartServer("127.0.0.1:-1", e.auth, tlsCfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ns.Shutdown)
	e.ns = ns
	e.url = ns.ClientURL()
	return e
}

func (e *env) connect(t *testing.T, n testNode, id string, errs chan<- error) *nats.Conn {
	t.Helper()
	kp, _ := nkeys.FromSeed([]byte(n.seed))
	nc, err := nats.Connect(e.url, nats.Nkey(n.pub, kp.Sign),
		nats.Secure(&tls.Config{RootCAs: e.ca, ServerName: pki.ServerName, MinVersion: tls.VersionTLS12}),
		nats.CustomInboxPrefix("_INBOX."+id), nats.MaxReconnects(0),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			if errs != nil {
				errs <- err
			}
		}))
	if err != nil {
		t.Fatalf("connect %s: %v", id, err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func TestAuthNkey(t *testing.T) {
	a, stranger := newNode(t), newNode(t)
	e := newEnv(t, map[testNode]string{a: "A"})
	e.connect(t, a, "A", nil)

	kp, _ := nkeys.FromSeed([]byte(stranger.seed))
	tlsc := nats.Secure(&tls.Config{RootCAs: e.ca, ServerName: pki.ServerName})
	if nc, err := nats.Connect(e.url, nats.Nkey(stranger.pub, kp.Sign), tlsc, nats.MaxReconnects(0)); err == nil {
		nc.Close()
		t.Fatal("unknown nkey connected")
	}
	// Known public key, signature made by another key.
	wrong, _ := nkeys.FromSeed([]byte(stranger.seed))
	if nc, err := nats.Connect(e.url, nats.Nkey(a.pub, wrong.Sign), tlsc, nats.MaxReconnects(0)); err == nil {
		nc.Close()
		t.Fatal("bad signature connected")
	}
	// ConnectNode helper works end to end.
	nc, err := ConnectNode(e.url, []byte(a.seed), "A", e.ca, "t")
	if err != nil {
		t.Fatal(err)
	}
	nc.Close()
}

func TestAuthPlaintextRejected(t *testing.T) {
	a := newNode(t)
	e := newEnv(t, map[testNode]string{a: "A"})
	kp, _ := nkeys.FromSeed([]byte(a.seed))
	if nc, err := nats.Connect(e.url, nats.Nkey(a.pub, kp.Sign), nats.MaxReconnects(0)); err == nil {
		nc.Close()
		t.Fatal("plaintext connection accepted")
	}
}

func TestAuthInternalAndBadToken(t *testing.T) {
	e := newEnv(t, nil)
	nc, err := ConnectInProcess(e.ns, "internal-secret", "x")
	if err != nil {
		t.Fatal(err)
	}
	nc.Close()
	if nc, err := ConnectInProcess(e.ns, "nope", "x"); err == nil {
		nc.Close()
		t.Fatal("bad internal secret accepted")
	}
}

func TestPermissions(t *testing.T) {
	a, b := newNode(t), newNode(t)
	e := newEnv(t, map[testNode]string{a: "A", b: "B"})
	full, err := ConnectInProcess(e.ns, "internal-secret", "cp")
	if err != nil {
		t.Fatal(err)
	}
	defer full.Close()

	errs := make(chan error, 8)
	ca := e.connect(t, a, "A", errs)
	cb := e.connect(t, b, "B", nil)

	// A may not subscribe to B's subjects.
	if _, err := ca.Subscribe("paas.agent.B.x", func(*nats.Msg) {}); err != nil {
		t.Fatal(err)
	}
	ca.Flush()
	select {
	case err := <-errs:
		if err == nil || !contains(err.Error(), "Permissions Violation") {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no permission violation for foreign subscribe")
	}

	// A publishing to B's agent subject reaches nobody.
	got := make(chan *nats.Msg, 1)
	sub, _ := full.Subscribe("paas.agent.B.run", func(m *nats.Msg) { got <- m })
	defer sub.Unsubscribe()
	full.Flush()
	ca.Publish("paas.agent.B.run", []byte("x"))
	ca.Flush()
	select {
	case <-got:
		t.Fatal("forbidden publish was delivered")
	case <-time.After(300 * time.Millisecond):
	}
	select {
	case err := <-errs:
		if !contains(err.Error(), "Permissions Violation") {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no permission violation for foreign publish")
	}

	// Request/reply: control plane asks A, A answers through allowed response.
	if _, err := ca.Subscribe("paas.agent.A.ping", func(m *nats.Msg) { m.Respond([]byte("pong")) }); err != nil {
		t.Fatal(err)
	}
	ca.Flush()
	rep, err := full.Request("paas.agent.A.ping", nil, 2*time.Second)
	if err != nil || string(rep.Data) != "pong" {
		t.Fatalf("request: %v %v", rep, err)
	}
	_ = cb
}

func TestKick(t *testing.T) {
	a := newNode(t)
	e := newEnv(t, map[testNode]string{a: "A"})
	nc := e.connect(t, a, "A", nil)
	closed := make(chan struct{})
	nc.SetClosedHandler(func(*nats.Conn) { close(closed) })
	e.auth.Kick(e.ns, a.pub)
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("client not disconnected after Kick")
	}
}

func TestLegacyToken(t *testing.T) {
	e := newEnv(t, nil, "legacy")
	// Legacy agents predate the cluster CA and dial plain.
	nc, err := nats.Connect(strings.Replace(e.url, "tls://", "nats://", 1), nats.Token("legacy"), nats.MaxReconnects(0))
	if err != nil {
		t.Fatal(err)
	}
	nc.Close()
}

func TestPlaintextRefusedWithoutLegacy(t *testing.T) {
	e := newEnv(t, nil)
	if nc, err := nats.Connect(strings.Replace(e.url, "tls://", "nats://", 1), nats.Token("internal-secret"), nats.MaxReconnects(0)); err == nil {
		nc.Close()
		t.Fatal("plain connect accepted with legacy off")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func TestNodePermissions(t *testing.T) {
	worker := &server.Permissions{
		Publish: &server.SubjectPermission{Allow: []string{"paas.node.n1.>", "paas.agent.binary",
			"paas.dns.get", "paas.mesh.get", "paas.logs.deploy.*", "paas.logs.live.*.*"}},
		Subscribe: &server.SubjectPermission{Allow: []string{"_INBOX.n1.>", "paas.agent.n1.>", "paas.dns", "paas.mesh", "paas.logs.stop.*"}},
		Response:  &server.ResponsePermission{MaxMsgs: 1},
	}
	both := &server.Permissions{
		Publish:   &server.SubjectPermission{Allow: append(append([]string{}, worker.Publish.Allow...), "paas.proxy.routes.get", "paas.proxy.stats", "paas.proxy.wake")},
		Subscribe: &server.SubjectPermission{Allow: append(append([]string{}, worker.Subscribe.Allow...), "paas.proxy.routes")},
		Response:  &server.ResponsePermission{MaxMsgs: 1},
	}
	for name, tc := range map[string]struct {
		roles []string
		want  *server.Permissions
	}{
		"worker":      {[]string{"worker"}, worker},
		"build":       {[]string{"build"}, worker},
		"empty":       {nil, worker},
		"edge":        {[]string{"edge"}, both},
		"worker+edge": {[]string{"worker", "edge"}, both},
	} {
		if got := NodePermissions("n1", tc.roles); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %+v / %+v", name, got.Publish, got.Subscribe)
		}
	}
}
