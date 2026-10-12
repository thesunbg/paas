package network

import (
	"bufio"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// InternalZone is the DNS suffix for service discovery: <service>.<project>.internal
const InternalZone = "internal."

// DNSServer answers *.internal from a table pushed by the control plane and
// forwards everything else to the host's resolvers.
type DNSServer struct {
	mu        sync.RWMutex
	records   map[string][]string
	upstreams []string
	listening map[string]*dns.Server
	client    *dns.Client
}

func NewDNSServer() *DNSServer {
	return &DNSServer{
		records:   map[string][]string{},
		upstreams: hostResolvers(),
		listening: map[string]*dns.Server{},
		client:    &dns.Client{Timeout: 3 * time.Second},
	}
}

func hostResolvers() []string {
	var out []string
	for _, path := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) >= 2 && fields[0] == "nameserver" {
				out = append(out, net.JoinHostPort(fields[1], "53"))
			}
		}
		f.Close()
		if len(out) > 0 {
			return out
		}
	}
	return []string{"1.1.1.1:53", "8.8.8.8:53"}
}

// SetRecords replaces the record table. Keys are FQDNs without trailing dot.
func (s *DNSServer) SetRecords(r map[string][]string) {
	norm := make(map[string][]string, len(r))
	for k, v := range r {
		norm[strings.ToLower(strings.TrimSuffix(k, "."))+"."] = v
	}
	s.mu.Lock()
	s.records = norm
	s.mu.Unlock()
}

// Lookup returns the IPs for an internal name.
func (s *DNSServer) Lookup(name string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.records[strings.ToLower(dns.Fqdn(name))]
}

// ListenOn starts UDP and TCP listeners on ip:53 once.
func (s *DNSServer) ListenOn(ip string) error {
	s.mu.Lock()
	if _, ok := s.listening[ip]; ok {
		s.mu.Unlock()
		return nil
	}
	addr := net.JoinHostPort(ip, "53")
	udp := &dns.Server{Addr: addr, Net: "udp", Handler: s}
	tcp := &dns.Server{Addr: addr, Net: "tcp", Handler: s}
	s.listening[ip] = udp
	s.mu.Unlock()

	errc := make(chan error, 2)
	started := make(chan struct{}, 2)
	udp.NotifyStartedFunc = func() { started <- struct{}{} }
	tcp.NotifyStartedFunc = func() { started <- struct{}{} }
	go func() { errc <- udp.ListenAndServe() }()
	go func() { errc <- tcp.ListenAndServe() }()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case err := <-errc:
			// Tear down the half that started so a later call can retry cleanly.
			_ = udp.Shutdown()
			_ = tcp.Shutdown()
			s.mu.Lock()
			delete(s.listening, ip)
			s.mu.Unlock()
			return err
		case <-time.After(3 * time.Second):
			return nil
		}
	}
	slog.Info("internal dns listening", "addr", addr)
	return nil
}

func (s *DNSServer) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, srv := range s.listening {
		_ = srv.Shutdown()
	}
}

func (s *DNSServer) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	if len(req.Question) == 0 {
		dns.HandleFailed(w, req)
		return
	}
	q := req.Question[0]
	name := strings.ToLower(q.Name)
	if strings.HasSuffix(name, "."+InternalZone) || name == InternalZone {
		m := new(dns.Msg)
		m.SetReply(req)
		m.Authoritative = true
		ips := s.Lookup(name)
		if len(ips) == 0 {
			m.Rcode = dns.RcodeNameError
		} else if q.Qtype == dns.TypeA || q.Qtype == dns.TypeANY {
			for _, ip := range ips {
				if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() != nil {
					m.Answer = append(m.Answer, &dns.A{
						Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 5},
						A:   parsed.To4(),
					})
				}
			}
		}
		_ = w.WriteMsg(m)
		return
	}
	netw := "udp"
	if _, ok := w.RemoteAddr().(*net.TCPAddr); ok {
		netw = "tcp"
	}
	for _, up := range s.upstreams {
		c := *s.client
		c.Net = netw
		resp, _, err := c.Exchange(req, up)
		if err == nil && resp != nil {
			_ = w.WriteMsg(resp)
			return
		}
	}
	dns.HandleFailed(w, req)
}
