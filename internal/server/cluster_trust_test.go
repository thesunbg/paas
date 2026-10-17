package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nkeys"

	"github.com/thesunbg/paas/internal/bus"
	"github.com/thesunbg/paas/internal/network"
	"github.com/thesunbg/paas/internal/proto"
)

func newNkey(t *testing.T) (seed []byte, pub string) {
	t.Helper()
	kp, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	if seed, err = kp.Seed(); err != nil {
		t.Fatal(err)
	}
	if pub, err = kp.PublicKey(); err != nil {
		t.Fatal(err)
	}
	return seed, pub
}

type enrollOut struct {
	NodeID  string `json:"node_id"`
	NatsURL string `json:"nats_url"`
	CAPEM   string `json:"ca_pem"`
}

func enroll(t *testing.T, h http.Handler, tok, name, nkey string) (int, enrollOut) {
	t.Helper()
	w := do(t, h, "POST", "/api/v1/cluster/enroll", "", map[string]string{"token": tok, "name": name, "nkey": nkey})
	var out enrollOut
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func joinToken(t *testing.T, s *Server, ttl time.Duration, roles ...string) string {
	t.Helper()
	tok := "tok-" + time.Now().Format("150405.000000000")
	if _, err := s.store.CreateJoinToken(context.Background(), hashJoinToken(tok), roles, ttl, ""); err != nil {
		t.Fatal(err)
	}
	return tok
}

func trustServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s := newTestServerBus(t)
	s.cfg.PublicURL = "https://paas.test"
	s.cfg.NATSAddr = "0.0.0.0:4222"
	base, err := network.ParseBase("10.100")
	if err != nil {
		t.Fatal(err)
	}
	s.clusterBase = base
	return s, s.routes()
}

func TestJoinTokenRequiresHTTPS(t *testing.T) {
	s, h := trustServer(t)
	mkUser(t, s, "root@example.com", "password1", true)
	mkUser(t, s, "user@example.com", "password1", false)
	_, admin := login(t, h, "root@example.com", "password1", "198.51.100.1")
	_, user := login(t, h, "user@example.com", "password1", "198.51.100.2")

	if w := do(t, h, "POST", "/api/v1/cluster/join-tokens", user, `{}`); w.Code != 403 {
		t.Fatalf("non-admin = %d", w.Code)
	}
	s.cfg.PublicURL = "http://paas.test"
	if w := do(t, h, "POST", "/api/v1/cluster/join-tokens", admin, `{"roles":["worker"]}`); w.Code != 409 {
		t.Fatalf("http PublicURL = %d %s, want 409", w.Code, w.Body)
	}
	s.cfg.PublicURL = "https://paas.test"
	w := do(t, h, "POST", "/api/v1/cluster/join-tokens", admin, `{"roles":["worker","edge"]}`)
	if w.Code != 200 {
		t.Fatalf("https PublicURL = %d %s", w.Code, w.Body)
	}
	var out struct {
		Token, Command string
		ExpiresAt      time.Time `json:"expires_at"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	want := "curl -fsSL https://paas.test/install.sh | sudo sh -s -- join --url https://paas.test --token " + out.Token + " --roles worker,edge"
	if out.Command != want || out.ExpiresAt.Before(time.Now()) {
		t.Fatalf("command = %q expires %v", out.Command, out.ExpiresAt)
	}
	if w := do(t, h, "GET", "/api/v1/cluster/join", admin, nil); w.Code == 200 {
		t.Fatalf("old GET /cluster/join still answers 200")
	}
	// The minted token enrolls once.
	_, pub := newNkey(t)
	if code, _ := enroll(t, h, out.Token, "n1", pub); code != 200 {
		t.Fatalf("enroll = %d", code)
	}
	if code, _ := enroll(t, h, out.Token, "n2", pub); code != 401 {
		t.Fatalf("token reuse = %d, want 401", code)
	}
}

func TestEnrollRejectsExpiredAndBadKey(t *testing.T) {
	s, h := trustServer(t)
	_, pub := newNkey(t)
	if code, _ := enroll(t, h, joinToken(t, s, -time.Minute, "worker"), "n", pub); code != 401 {
		t.Fatalf("expired token = %d, want 401", code)
	}
	tok := joinToken(t, s, time.Hour, "worker")
	if code, _ := enroll(t, h, tok, "n", "not-a-key"); code != 400 {
		t.Fatalf("bad nkey = %d, want 400", code)
	}
	// A rejected request must not have burned the token.
	if code, out := enroll(t, h, tok, "n", pub); code != 200 || out.NodeID == "" ||
		out.NatsURL != "tls://paas.test:4222" || out.CAPEM != s.caPEM {
		t.Fatalf("enroll after bad key = %d %+v", code, out)
	}
}

func TestReenrollKeepsIndexReplacesNkey(t *testing.T) {
	s, h := trustServer(t)
	_, pub1 := newNkey(t)
	_, pub2 := newNkey(t)
	_, out1 := enroll(t, h, joinToken(t, s, time.Hour, "worker"), "n", pub1)
	code, out2 := enroll(t, h, joinToken(t, s, time.Hour, "worker"), "n", pub2)
	if code != 200 || out1.NodeID == "" || out1.NodeID != out2.NodeID {
		t.Fatalf("re-enroll = %d %q vs %q", code, out1.NodeID, out2.NodeID)
	}
	ctx := context.Background()
	n, err := s.store.NodeByNkey(ctx, pub2)
	if err != nil || n.ID != out1.NodeID || n.NetIndex == nil || n.Legacy {
		t.Fatalf("by new nkey: %+v %v", n, err)
	}
	if _, err := s.store.NodeByNkey(ctx, pub1); err == nil {
		t.Fatal("old nkey still resolves")
	}
	_, out3 := enroll(t, h, joinToken(t, s, time.Hour, "worker"), "other", pub1)
	n3, _ := s.store.NodeByID(ctx, out3.NodeID)
	if *n3.NetIndex == *n.NetIndex {
		t.Fatalf("two nodes share net_index %d", *n.NetIndex)
	}
	again, _ := s.store.NodeByID(ctx, out1.NodeID)
	if *again.NetIndex != *n.NetIndex {
		t.Fatalf("net_index changed on re-enroll: %d -> %d", *n.NetIndex, *again.NetIndex)
	}
}

func TestNodeSubjectsTrustSubjectNotPayload(t *testing.T) {
	s, h := trustServer(t)
	if err := s.registerBusHandlers(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, pubX := newNkey(t)
	_, pubY := newNkey(t)
	_, x := enroll(t, h, joinToken(t, s, time.Hour, "worker"), "x", pubX)
	_, y := enroll(t, h, joinToken(t, s, time.Hour, "worker"), "y", pubY)

	var rep proto.RegisterReply
	req := proto.RegisterRequest{Name: "y", Address: "10.9.9.9", Roles: []string{"edge"}, CPUCores: 8, MemoryMB: 4096, Capabilities: map[string]string{}}
	if err := bus.Request(ctx, s.nc, proto.SubjNodeRegister(x.NodeID), req, &rep); err != nil || rep.Error != "" || rep.NodeID != x.NodeID {
		t.Fatalf("register: %v %+v", err, rep)
	}
	nx, _ := s.store.NodeByID(ctx, x.NodeID)
	ny, _ := s.store.NodeByID(ctx, y.NodeID)
	if nx.CPUCores != 8 || nx.Address != "10.9.9.9" {
		t.Fatalf("node x not updated: %+v", nx)
	}
	if ny.CPUCores != 0 || ny.Address != "" {
		t.Fatalf("node y touched by a payload naming it: %+v", ny)
	}
	if len(nx.Roles) != 1 || nx.Roles[0] != "worker" {
		t.Fatalf("register changed roles (they drive permissions): %v", nx.Roles)
	}
	// Unknown id: no node is created.
	if err := bus.Request(ctx, s.nc, proto.SubjNodeRegister("00000000-0000-0000-0000-000000000000"), req, &rep); err != nil || rep.Error == "" {
		t.Fatalf("unknown id: %v %+v", err, rep)
	}
	if nodes, _ := s.store.Nodes(ctx); len(nodes) != 2 {
		t.Fatalf("register created a node: %d nodes", len(nodes))
	}
	// Heartbeat: id from the subject even if the payload names y.
	var ack proto.HeartbeatAck
	hb := proto.Heartbeat{NodeID: y.NodeID, Usage: proto.NodeUsage{MemoryUsedMB: 77}}
	if err := bus.Request(ctx, s.nc, proto.SubjNodeHeartbeat(x.NodeID), hb, &ack); err != nil {
		t.Fatal(err)
	}
	nx, _ = s.store.NodeByID(ctx, x.NodeID)
	ny, _ = s.store.NodeByID(ctx, y.NodeID)
	if nx.Usage.MemoryUsedMB != 77 || ny.Usage.MemoryUsedMB == 77 {
		t.Fatalf("heartbeat attributed by payload: x=%d y=%d", nx.Usage.MemoryUsedMB, ny.Usage.MemoryUsedMB)
	}
}

func TestDeleteNodeKicksConnection(t *testing.T) {
	s, h := trustServer(t)
	if err := s.registerBusHandlers(); err != nil {
		t.Fatal(err)
	}
	mkUser(t, s, "root@example.com", "password1", true)
	_, admin := login(t, h, "root@example.com", "password1", "198.51.100.1")
	seed, pub := newNkey(t)
	_, out := enroll(t, h, joinToken(t, s, time.Hour, "worker"), "n", pub)

	nc, err := bus.ConnectNode(s.ns.ClientURL(), seed, out.NodeID, s.ca.Pool(), "kick-test")
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpdateNodeSettings(context.Background(), out.NodeID, "cordoned", 1.5, 4, 0, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if w := do(t, h, "DELETE", "/api/v1/nodes/"+out.NodeID, admin, nil); w.Code != 200 {
		t.Fatalf("delete = %d %s", w.Code, w.Body)
	}
	deadline := time.Now().Add(3 * time.Second)
	for nc.IsConnected() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if nc.IsConnected() {
		t.Fatal("deleted node is still connected to NATS")
	}
}

// The embedded agent has no join token: EnsureLocalNode gives it a row and an id, and with legacy off
// it registers and heartbeats on paas.node.<id>.* over the internal in-process connection, while the
// old subjects have no handler at all.
func TestEmbeddedAgentUsesNodeSubjectsWithLegacyOff(t *testing.T) {
	s, _ := trustServer(t)
	if s.cfg.NATSLegacy {
		t.Fatal("test needs legacy off")
	}
	if err := s.registerBusHandlers(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id, err := s.EnsureLocalNode(ctx, "cp-1", []string{"worker", "build"})
	if err != nil || id == "" {
		t.Fatalf("ensure: %v", err)
	}
	if again, _ := s.EnsureLocalNode(ctx, "cp-1", []string{"worker", "build"}); again != id {
		t.Fatalf("id changed: %s -> %s", id, again)
	}
	n, _ := s.store.NodeByID(ctx, id)
	if n.Legacy || n.NetIndex == nil {
		t.Fatalf("local node should count as enrolled with an index: %+v", n)
	}
	req := proto.RegisterRequest{Name: "cp-1", Address: "10.0.0.1", CPUCores: 4, MemoryMB: 2048, Capabilities: map[string]string{}}
	var rep proto.RegisterReply
	if err := bus.Request(ctx, s.nc, proto.SubjNodeRegister(id), req, &rep); err != nil || rep.Error != "" || rep.NodeID != id {
		t.Fatalf("register: %v %+v", err, rep)
	}
	var ack proto.HeartbeatAck
	if err := bus.Request(ctx, s.nc, proto.SubjNodeHeartbeat(id), proto.Heartbeat{Usage: proto.NodeUsage{MemoryUsedMB: 5}}, &ack); err != nil {
		t.Fatal(err)
	}
	if n, _ = s.store.NodeByID(ctx, id); n.Usage.MemoryUsedMB != 5 || n.CPUCores != 4 {
		t.Fatalf("not recorded: %+v", n)
	}
	cctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := bus.Request(cctx, s.nc, proto.SubjRegister, req, &rep); err == nil {
		t.Fatal("legacy register subject has a handler with legacy off")
	}
}

// A join-token holder must not take over the control plane's node row by enrolling with its name,
// and the refused attempt must not burn the token.
func TestEnrollRefusesControlPlaneName(t *testing.T) {
	s, h := trustServer(t)
	ctx := context.Background()
	id, err := s.EnsureLocalNode(ctx, "cp-1", []string{"worker"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.store.NodeNkey(ctx, id)
	if !strings.HasPrefix(before, "internal:") {
		t.Fatalf("cp nkey = %q", before)
	}
	_, pub := newNkey(t)
	tok := joinToken(t, s, time.Hour, "worker")
	if code, _ := enroll(t, h, tok, "cp-1", pub); code != 409 {
		t.Fatalf("enroll as cp name = %d, want 409", code)
	}
	if after, _ := s.store.NodeNkey(ctx, id); after != before {
		t.Fatalf("cp nkey changed: %q -> %q", before, after)
	}
	if code, out := enroll(t, h, tok, "w1", pub); code != 200 || out.NodeID == id {
		t.Fatalf("token burned or reused cp id: %d %+v", code, out)
	}
}
