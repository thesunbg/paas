package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"io"
	"math/big"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/config"
	"github.com/thesunbg/paas/internal/model"
)

func selfSigned(t *testing.T) tls.Certificate {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "db.test"}, DNSNames: []string{"db.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// echo is a fake database server that echoes what it receives.
func echo(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

func freePort(t *testing.T) int {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func roundTrip(t *testing.T, c net.Conn, msg string) string {
	t.Helper()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	return string(buf)
}

func TestTCPPostgresTLSAndAllowlist(t *testing.T) {
	p := New(config.Proxy{}, nil)
	cert := selfSigned(t)
	p.setDBTLS(&tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }})
	up := echo(t)
	port, denied := freePort(t), freePort(t)
	p.SetRoutes(&model.RouteTable{Version: 1, DBHost: "db.test", TCP: []model.TCPRoute{
		{Port: port, Upstreams: []string{up}, Mode: "postgres", Allow: []string{"127.0.0.0/8"}},
		{Port: denied, Upstreams: []string{up}, Mode: "plain", Allow: []string{"10.0.0.0/8"}},
	}})
	addr := "127.0.0.1:" + strconv.Itoa(port)

	// SSLRequest -> 'S' -> TLS, then bytes reach the server decrypted.
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	req := make([]byte, 8)
	binary.BigEndian.PutUint32(req, 8)
	binary.BigEndian.PutUint32(req[4:], pgSSLRequest)
	c.Write(req)
	b := make([]byte, 1)
	if _, err := io.ReadFull(c, b); err != nil || b[0] != 'S' {
		t.Fatalf("want S, got %q %v", b, err)
	}
	tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true, ServerName: "db.test"})
	if got := roundTrip(t, tc, "startup-over-tls"); got != "startup-over-tls" {
		t.Fatalf("got %q", got)
	}
	tc.Close()

	// A plain startup message passes through.
	c2, _ := net.Dial("tcp", addr)
	if got := roundTrip(t, c2, "\x00\x00\x00\x09plain"); got != "\x00\x00\x00\x09plain" {
		t.Fatalf("plain got %q", got)
	}
	c2.Close()

	// The allowlist closes connections from other addresses.
	c3, _ := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(denied))
	c3.SetDeadline(time.Now().Add(2 * time.Second))
	c3.Write([]byte("x"))
	if _, err := c3.Read(b); err == nil {
		t.Fatal("connection outside the allowlist was not closed")
	}

	// Removing the route closes the port.
	p.SetRoutes(&model.RouteTable{Version: 2})
	time.Sleep(50 * time.Millisecond)
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatal("port still open after route removal")
	}
}

// tcpFixture starts a plain-mode route on a free port and returns the proxy and address.
func tcpFixture(t *testing.T, r model.TCPRoute) (*Proxy, model.TCPRoute, string) {
	p := New(config.Proxy{}, nil)
	r.Port, r.Mode = freePort(t), "plain"
	p.SetRoutes(&model.RouteTable{Version: 1, TCP: []model.TCPRoute{r}})
	t.Cleanup(func() { p.tcp.apply(nil) }) // closes the port; its l.mu also orders handle's reads before later test writes
	return p, r, "127.0.0.1:" + strconv.Itoa(r.Port)
}

func dialEcho(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if got := roundTrip(t, c, "ping"); got != "ping" {
		t.Fatalf("got %q", got)
	}
	return c
}

func wantClosed(t *testing.T, c net.Conn) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected connection closed")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("connection still open")
	}
}

func TestTCPReapplyKeepsConn(t *testing.T) {
	p, r, addr := tcpFixture(t, model.TCPRoute{Upstreams: []string{echo(t)}, Allow: []string{"127.0.0.1/32"}})
	c := dialEcho(t, addr)
	p.tcp.apply([]model.TCPRoute{r})
	if got := roundTrip(t, c, "again"); got != "again" {
		t.Fatalf("got %q", got)
	}
}

func TestTCPAllowRevokeClosesConn(t *testing.T) {
	p, r, addr := tcpFixture(t, model.TCPRoute{Upstreams: []string{echo(t)}, Allow: []string{"127.0.0.1/32"}})
	c := dialEcho(t, addr)
	r.Allow = []string{"10.0.0.0/8"}
	p.tcp.apply([]model.TCPRoute{r})
	wantClosed(t, c)
}

func TestTCPRouteRemovedClosesConn(t *testing.T) {
	p, _, addr := tcpFixture(t, model.TCPRoute{Upstreams: []string{echo(t)}, Allow: []string{"127.0.0.1/32"}})
	c := dialEcho(t, addr)
	p.tcp.apply(nil)
	wantClosed(t, c)
}

func TestTCPUpstreamChangeClosesConn(t *testing.T) {
	a, b := echo(t), echo(t)
	p, r, addr := tcpFixture(t, model.TCPRoute{Upstreams: []string{a}, Allow: []string{"127.0.0.1/32"}})
	c := dialEcho(t, addr)
	r.Upstreams = []string{b}
	p.tcp.apply([]model.TCPRoute{r})
	wantClosed(t, c)
	dialEcho(t, addr)
}

func TestTCPConnCap(t *testing.T) {
	old := maxConnsPerRoute
	maxConnsPerRoute = 2
	t.Cleanup(func() { maxConnsPerRoute = old })
	_, _, addr := tcpFixture(t, model.TCPRoute{Upstreams: []string{echo(t)}, Allow: []string{"127.0.0.1/32"}})
	c1, c2 := dialEcho(t, addr), dialEcho(t, addr)
	c3, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c3.Close()
	wantClosed(t, c3)
	c1.Close()
	time.Sleep(200 * time.Millisecond)
	dialEcho(t, addr)
	_ = c2
}
