package network

import (
	"net"
	"strings"
	"testing"

	"github.com/miekg/dns"
)

type fakeWriter struct {
	dns.ResponseWriter
	msg *dns.Msg
}

func (f *fakeWriter) WriteMsg(m *dns.Msg) error { f.msg = m; return nil }
func (f *fakeWriter) RemoteAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 5000}
}

func TestInternalLookup(t *testing.T) {
	s := NewDNSServer()
	s.SetRecords(map[string][]string{"api.shop.internal": {"10.200.3.5", "10.200.3.6"}})

	req := new(dns.Msg)
	req.SetQuestion("API.shop.internal.", dns.TypeA)
	w := &fakeWriter{}
	s.ServeDNS(w, req)
	if w.msg == nil || len(w.msg.Answer) != 2 {
		t.Fatalf("expected 2 answers, got %+v", w.msg)
	}

	req.SetQuestion("missing.shop.internal.", dns.TypeA)
	s.ServeDNS(w, req)
	if w.msg.Rcode != dns.RcodeNameError {
		t.Fatalf("expected NXDOMAIN, got %d", w.msg.Rcode)
	}
}

func TestAddressPlan(t *testing.T) {
	base := [2]int{10, 100}
	if s, g := subnetFor(base, 1, 3); s != "10.101.0.192/26" || g != "10.101.0.193" {
		t.Fatal(s, g)
	}
	if s, _ := subnetFor(base, 2, 300); s != "10.102.75.0/26" {
		t.Fatal(s)
	}
	if NodeMeshIP(base, 7) != "10.107.255.254" || NodeCIDR(base, 7) != "10.107.0.0/16" {
		t.Fatal("mesh address plan")
	}
	// The infrastructure slot never collides with a project subnet.
	if s, _ := subnetFor(base, 1, BuildNetIndex); !strings.HasPrefix(s, "10.101.255.128") {
		t.Fatal(s)
	}
	if s, _ := subnetFor(base, 1, MaxProjects-1); !strings.HasPrefix(s, "10.101.255.64") {
		t.Fatal(s)
	}
	if _, err := ParseBase("10.10"); err != nil {
		t.Fatal(err)
	}
	if MaxNodes([2]int{10, 100}) != 155 || MaxNodes([2]int{10, 0}) != 250 {
		t.Fatal("max nodes")
	}
}
