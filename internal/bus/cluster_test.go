package bus

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// A client connected to one control plane learns the other's advertised address
// through the cluster, and keeps working when its own server goes away.
func TestClusterAdvertisesControlPlanes(t *testing.T) {
	auth := &Auth{Internal: "internal-secret"}
	ca, cb := freePort(t), freePort(t)
	pa, pb := freePort(t), freePort(t)
	a, err := StartServer(fmt.Sprintf("127.0.0.1:%d", pa), auth, nil, t.TempDir(),
		ClusterOptions{Name: "cp-a", Listen: fmt.Sprintf("127.0.0.1:%d", ca), Secret: "route", Advertise: fmt.Sprintf("localhost:%d", pa)})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Shutdown()
	b, err := StartServer(fmt.Sprintf("127.0.0.1:%d", pb), auth, nil, t.TempDir(),
		ClusterOptions{Name: "cp-b", Listen: fmt.Sprintf("127.0.0.1:%d", cb), Secret: "route", Advertise: fmt.Sprintf("localhost:%d", pb),
			Routes: []string{fmt.Sprintf("nats://127.0.0.1:%d", ca)}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Shutdown()

	nc, err := nats.Connect(fmt.Sprintf("nats://127.0.0.1:%d", pa), nats.Token("internal-secret"), nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	want := fmt.Sprintf("localhost:%d", pb)
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(strings.Join(nc.DiscoveredServers(), ","), want) {
		if time.Now().After(deadline) {
			t.Fatalf("client never learned %s; discovered %v", want, nc.DiscoveredServers())
		}
		time.Sleep(50 * time.Millisecond)
	}

	a.Shutdown()
	deadline = time.Now().Add(10 * time.Second)
	for !nc.IsConnected() || !strings.HasSuffix(nc.ConnectedUrl(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("client did not fail over to %s (connected to %q)", want, nc.ConnectedUrl())
		}
		time.Sleep(50 * time.Millisecond)
	}
}
