package network

import (
	"net"
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

func TestSubnet(t *testing.T) {
	m := &Manager{base: [2]int{10, 200}}
	if s, g := m.Subnet(3); s != "10.200.3.0/24" || g != "10.200.3.1" {
		t.Fatal(s, g)
	}
	if s, _ := m.Subnet(300); s != "10.201.44.0/24" {
		t.Fatal(s)
	}
}
