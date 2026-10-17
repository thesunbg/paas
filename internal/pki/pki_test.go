package pki

import (
	"crypto/tls"
	"net"
	"testing"
	"time"
)

func newTestCA(t *testing.T) *CA {
	t.Helper()
	cp, kp, err := NewCA()
	if err != nil {
		t.Fatal(err)
	}
	ca, err := ParseCA(cp, kp)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// handshake runs a TLS handshake over a pipe and returns both errors.
func handshake(srv, cli *tls.Config) (srvErr, cliErr error) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	deadline := time.Now().Add(5 * time.Second)
	a.SetDeadline(deadline)
	b.SetDeadline(deadline)
	done := make(chan error, 1)
	go func() {
		s := tls.Server(a, srv)
		err := s.Handshake()
		if err != nil {
			a.Close() // unblock the client
		}
		done <- err
	}()
	cliErr = tls.Client(b, cli).Handshake()
	if cliErr != nil {
		b.Close()
	}
	return <-done, cliErr
}

func clientCfg(ca *CA) *tls.Config {
	return &tls.Config{RootCAs: ca.Pool(), ServerName: ServerName, MinVersion: tls.VersionTLS12}
}

func TestServerTLSVerifies(t *testing.T) {
	ca := newTestCA(t)
	if s, c := handshake(ca.ServerTLS(), clientCfg(ca)); s != nil || c != nil {
		t.Fatalf("handshake: server=%v client=%v", s, c)
	}
}

func TestServerTLSWrongCAFails(t *testing.T) {
	ca, other := newTestCA(t), newTestCA(t)
	if _, c := handshake(ca.ServerTLS(), clientCfg(other)); c == nil {
		t.Fatal("client trusting a different CA must fail")
	}
}

func TestServerTLSWrongNameFails(t *testing.T) {
	ca := newTestCA(t)
	cfg := clientCfg(ca)
	cfg.ServerName = "evil.example"
	if _, c := handshake(ca.ServerTLS(), cfg); c == nil {
		t.Fatal("wrong server name must fail")
	}
}

func TestServerTLSRemintsNearExpiry(t *testing.T) {
	ca := newTestCA(t)
	cfg := ca.ServerTLS()
	first, err := cfg.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := cfg.GetCertificate(nil)
	if first != again {
		t.Fatal("cert should be cached")
	}
	base := ca.now
	ca.now = func() time.Time { return base().Add(leafTTL - renewLeft + time.Hour) }
	renewed, err := cfg.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if renewed == first || !renewed.Leaf.NotAfter.After(first.Leaf.NotAfter) {
		t.Fatal("cert with <30d left should be re-minted")
	}
}

func TestRouteTLSMutual(t *testing.T) {
	ca := newTestCA(t)
	if s, c := handshake(ca.RouteTLS(), ca.RouteTLS()); s != nil || c != nil {
		t.Fatalf("route handshake: server=%v client=%v", s, c)
	}
}

func TestRouteTLSRejectsOtherCAClient(t *testing.T) {
	ca, other := newTestCA(t), newTestCA(t)
	if s, _ := handshake(ca.RouteTLS(), other.RouteTLS()); s == nil {
		t.Fatal("route server must reject peer from another CA")
	}
}

func TestRouteTLSRejectsNoClientCert(t *testing.T) {
	ca := newTestCA(t)
	if s, _ := handshake(ca.RouteTLS(), clientCfg(ca)); s == nil {
		t.Fatal("route server must require a client cert")
	}
}
