package proxy

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thesunbg/paas/internal/model"
)

// TCP forwarding for managed databases: every edge proxy listens on each
// database's public port, checks the client address against its allowlist,
// terminates TLS where the protocol allows it and forwards to the server.

type tcpRoute struct {
	route model.TCPRoute
	allow []netip.Prefix
	next  atomic.Uint32
}

// maxConnsPerRoute caps live connections per database port. Must stay above the
// largest max_connections in databases.Presets (300). var only so tests can lower it.
var maxConnsPerRoute = 512

type tcpConn struct {
	ip       netip.Addr
	client   net.Conn
	upstream string // "" until dialed
	server   net.Conn
	dead     bool
}

// kill closes both ends. Caller holds l.mu.
func (c *tcpConn) kill() {
	c.dead = true
	_ = c.client.Close()
	if c.server != nil {
		_ = c.server.Close()
	}
}

type tcpListener struct {
	ln     net.Listener
	route  atomic.Pointer[tcpRoute]
	mu     sync.Mutex // guards conns, closed and every tcpConn's fields
	conns  map[*tcpConn]struct{}
	closed bool
}

// stale reports whether c may no longer live under route r.
func stale(r *tcpRoute, c *tcpConn) bool {
	return r == nil || !allowed(r, c.ip) ||
		(c.upstream != "" && !slices.Contains(r.route.Upstreams, c.upstream))
}

type tcpManager struct {
	p         *Proxy
	mu        sync.Mutex
	listeners map[int]*tcpListener
}

func newTCPManager(p *Proxy) *tcpManager {
	return &tcpManager{p: p, listeners: map[int]*tcpListener{}}
}

// apply opens listeners for new ports, updates existing ones and closes removed ones.
func (m *tcpManager) apply(routes []model.TCPRoute) {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := map[int]bool{}
	// Public database ports are TLS-only: without TLS at the edge nothing listens.
	if m.p.dbTLS.Load() == nil && len(routes) > 0 {
		slog.Warn("database ports stay closed: TLS is not enabled at the edge", "routes", len(routes))
		routes = nil
	}
	for _, r := range routes {
		if r.Port <= 0 {
			continue
		}
		want[r.Port] = true
		tr := &tcpRoute{route: r}
		for _, c := range r.Allow {
			if pfx, err := netip.ParsePrefix(c); err == nil {
				tr.allow = append(tr.allow, pfx.Masked())
			} else if a, err := netip.ParseAddr(c); err == nil {
				tr.allow = append(tr.allow, netip.PrefixFrom(a, a.BitLen()))
			}
		}
		if l, ok := m.listeners[r.Port]; ok {
			// No race with handle: Store happens before Lock here, while admit/attach
			// Lock before Load, so each conn is either seen by this sweep or loads the
			// new route itself. The dial-to-attach window is closed by attach's re-check.
			l.route.Store(tr)
			l.mu.Lock()
			for c := range l.conns {
				if stale(tr, c) {
					c.kill()
				}
			}
			l.mu.Unlock()
			continue
		}
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(r.Port))
		if err != nil {
			slog.Warn("database port", "port", r.Port, "err", err)
			continue
		}
		l := &tcpListener{ln: ln, conns: map[*tcpConn]struct{}{}}
		l.route.Store(tr)
		m.listeners[r.Port] = l
		go m.serve(l)
		slog.Info("database port open", "port", r.Port, "mode", r.Mode)
	}
	for port, l := range m.listeners {
		if !want[port] {
			l.mu.Lock()
			l.closed = true
			for c := range l.conns {
				c.kill()
			}
			l.mu.Unlock()
			_ = l.ln.Close()
			delete(m.listeners, port)
			slog.Info("database port closed", "port", port)
		}
	}
}

func (m *tcpManager) serve(l *tcpListener) {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go m.handle(c, l)
	}
}

func allowed(r *tcpRoute, ip netip.Addr) bool {
	for _, p := range r.allow {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

const (
	pgSSLRequest    = 80877103
	pgGSSENCRequest = 80877104
	pgCancelRequest = 80877102
)

func (m *tcpManager) handle(client net.Conn, l *tcpListener) {
	defer client.Close()
	ap, err := netip.ParseAddrPort(client.RemoteAddr().String())
	if err != nil {
		return
	}
	c := &tcpConn{ip: ap.Addr().Unmap(), client: client}
	l.mu.Lock()
	r := l.route.Load()
	if l.closed || len(l.conns) >= maxConnsPerRoute || r == nil || len(r.route.Upstreams) == 0 || !allowed(r, c.ip) {
		l.mu.Unlock()
		slog.Debug("database connection refused", "from", client.RemoteAddr())
		return
	}
	l.conns[c] = struct{}{}
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.conns, c)
		l.mu.Unlock()
	}()
	_ = client.SetDeadline(time.Now().Add(30 * time.Second))
	tlsCfg := m.p.dbTLSConfig(r.route.Mode)
	var conn net.Conn = client
	switch r.route.Mode {
	case "postgres":
		br := bufio.NewReader(client)
		for {
			head, err := br.Peek(1)
			if err != nil {
				return
			}
			if head[0] == 0x16 { // direct TLS (sslnegotiation=direct, PostgreSQL 17)
				if tlsCfg == nil {
					return
				}
				tc := tls.Server(&bufConn{Conn: client, r: br}, tlsCfg)
				if err := tc.Handshake(); err != nil {
					return
				}
				conn = tc
				break
			}
			msg, err := br.Peek(8)
			if err != nil {
				return
			}
			length, code := binary.BigEndian.Uint32(msg[:4]), binary.BigEndian.Uint32(msg[4:8])
			if length == 8 && code == pgGSSENCRequest {
				_, _ = br.Discard(8)
				if _, err := client.Write([]byte{'N'}); err != nil {
					return
				}
				continue
			}
			if length == 8 && code == pgSSLRequest {
				_, _ = br.Discard(8)
				if tlsCfg == nil {
					return
				}
				if _, err := client.Write([]byte{'S'}); err != nil {
					return
				}
				tc := tls.Server(&bufConn{Conn: client, r: br}, tlsCfg)
				if err := tc.Handshake(); err != nil {
					return
				}
				conn = tc
				break
			}
			// A CancelRequest carries only a pid and secret, so it may pass in the
			// clear (libpq sends it unencrypted); a plain startup would carry the
			// password, so it is dropped.
			if length == 16 && code == pgCancelRequest {
				conn = &bufConn{Conn: client, r: br}
				break
			}
			return
		}
	case "tls":
		if tlsCfg == nil {
			return
		}
		tc := tls.Server(client, tlsCfg)
		if err := tc.Handshake(); err != nil {
			return
		}
		conn = tc
	}
	_ = client.SetDeadline(time.Time{})

	n := r.next.Add(1)
	var server net.Conn
	var addr string
	for i := 0; i < len(r.route.Upstreams); i++ {
		addr = r.route.Upstreams[(int(n)+i)%len(r.route.Upstreams)]
		server, err = net.DialTimeout("tcp", addr, 5*time.Second)
		if err == nil {
			break
		}
	}
	if err != nil {
		slog.Debug("database upstream", "port", r.route.Port, "err", err)
		return
	}
	defer server.Close()
	l.mu.Lock()
	c.upstream = addr
	if c.dead || stale(l.route.Load(), c) {
		l.mu.Unlock()
		return
	}
	c.server = server
	l.mu.Unlock()
	pipe(conn, server)
}

// pipe copies both ways until either side closes.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}

// bufConn reads through a bufio.Reader that may hold already-peeked bytes.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *bufConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}
