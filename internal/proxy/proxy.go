// Package proxy is the edge router: it maps hostnames to app instances or
// static sites, terminates TLS (automatic certificates) and wakes sleeping apps.
package proxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/nats-io/nats.go"

	"github.com/thesunbg/paas/internal/bus"
	"github.com/thesunbg/paas/internal/config"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/util"
)

type entry struct {
	route model.Route
	next  atomic.Uint32
}

type table struct {
	version int64
	hosts   map[string]*entry
}

type Proxy struct {
	cfg       config.Proxy
	nc        *nats.Conn
	id        string
	table     atomic.Pointer[table]
	transport *http.Transport
	dashboard *httputil.ReverseProxy

	statsMu sync.Mutex
	stats   map[string]*proto.ServiceHits

	wakeMu sync.Mutex
	waking map[string]chan struct{}
	woken  map[string][]string // service id -> upstreams obtained by a wake-up
}

func New(cfg config.Proxy, nc *nats.Conn) *Proxy {
	p := &Proxy{
		cfg: cfg, nc: nc, id: util.RandomToken(4),
		stats:  map[string]*proto.ServiceHits{},
		waking: map[string]chan struct{}{},
		woken:  map[string][]string{},
		transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          2000,
			MaxIdleConnsPerHost:   256,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 5 * time.Minute,
			ForceAttemptHTTP2:     false,
		},
	}
	p.table.Store(&table{hosts: map[string]*entry{}})
	if cfg.DashboardUpstream != "" {
		u := &url.URL{Scheme: "http", Host: cfg.DashboardUpstream}
		p.dashboard = httputil.NewSingleHostReverseProxy(u)
		p.dashboard.Transport = p.transport
		p.dashboard.FlushInterval = -1 // stream SSE immediately
	}
	return p
}

// SetRoutes swaps the routing table (used directly in tests).
func (p *Proxy) SetRoutes(t *model.RouteTable) {
	cur := p.table.Load()
	if cur != nil && t.Version != 0 && t.Version < cur.version {
		return
	}
	nt := &table{version: t.Version, hosts: make(map[string]*entry, len(t.Routes))}
	for _, r := range t.Routes {
		e := &entry{route: r}
		if old, ok := cur.hosts[r.Host]; ok {
			e.next.Store(old.next.Load())
		}
		nt.hosts[strings.ToLower(r.Host)] = e
	}
	p.table.Store(nt)
	p.wakeMu.Lock()
	for id := range p.woken {
		for _, r := range t.Routes {
			if r.ServiceID == id && r.Kind != "sleeping" {
				delete(p.woken, id)
				break
			}
		}
	}
	p.wakeMu.Unlock()
}

func (p *Proxy) lookup(host string) *entry {
	host = strings.ToLower(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return p.table.Load().hosts[strings.TrimSuffix(host, ".")]
}

// Run subscribes to route updates and serves HTTP(S) until ctx is cancelled.
func (p *Proxy) Run(ctx context.Context) error {
	if _, err := p.nc.Subscribe(proto.SubjRoutes, func(m *nats.Msg) {
		var t model.RouteTable
		if err := json.Unmarshal(m.Data, &t); err == nil {
			p.SetRoutes(&t)
		}
	}); err != nil {
		return err
	}
	go p.fetchInitialRoutes(ctx)
	go p.statsLoop(ctx)

	handler := newRateLimiter(p.cfg.RateLimit, p.cfg.RateBurst).wrap(p)
	var servers []*http.Server
	errc := make(chan error, 2)
	if p.cfg.ACMEEmail != "" || p.cfg.TLS {
		magic, issuer := p.certmagic()
		tlsCfg := magic.TLSConfig()
		tlsCfg.NextProtos = append([]string{"h2", "http/1.1"}, tlsCfg.NextProtos...)
		tlsCfg.MinVersion = tls.VersionTLS12
		httpsSrv := &http.Server{Addr: p.cfg.HTTPSAddr, Handler: handler, TLSConfig: tlsCfg, ReadHeaderTimeout: 15 * time.Second}
		httpSrv := &http.Server{Addr: p.cfg.HTTPAddr, Handler: issuer.HTTPChallengeHandler(http.HandlerFunc(redirectHTTPS)), ReadHeaderTimeout: 15 * time.Second}
		servers = append(servers, httpsSrv, httpSrv)
		go func() {
			ln, err := tls.Listen("tcp", p.cfg.HTTPSAddr, tlsCfg)
			if err != nil {
				errc <- err
				return
			}
			errc <- httpsSrv.Serve(ln)
		}()
		go func() { errc <- httpSrv.ListenAndServe() }()
		slog.Info("edge proxy listening with automatic HTTPS", "http", p.cfg.HTTPAddr, "https", p.cfg.HTTPSAddr)
	} else {
		srv := &http.Server{Addr: p.cfg.HTTPAddr, Handler: handler, ReadHeaderTimeout: 15 * time.Second}
		servers = append(servers, srv)
		go func() { errc <- srv.ListenAndServe() }()
		slog.Info("edge proxy listening (HTTP only; set PAAS_TLS=true for automatic HTTPS)", "http", p.cfg.HTTPAddr)
	}
	select {
	case <-ctx.Done():
		for _, s := range servers {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.Shutdown(sctx)
			cancel()
		}
		return nil
	case err := <-errc:
		return err
	}
}

func (p *Proxy) certmagic() (*certmagic.Config, *certmagic.ACMEIssuer) {
	var magic *certmagic.Config
	cache := certmagic.NewCache(certmagic.CacheOptions{GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) { return magic, nil }})
	magic = certmagic.New(cache, certmagic.Config{
		Storage: &certmagic.FileStorage{Path: p.cfg.DataDir + "/certs"},
		OnDemand: &certmagic.OnDemandConfig{DecisionFunc: func(ctx context.Context, name string) error {
			// Only request certificates for hostnames that are actually routed.
			if p.lookup(name) == nil {
				return fmt.Errorf("unknown host %s", name)
			}
			return nil
		}},
	})
	tmpl := certmagic.ACMEIssuer{Email: p.cfg.ACMEEmail, Agreed: true}
	if p.cfg.ACMECA != "" {
		tmpl.CA = p.cfg.ACMECA
	}
	issuer := certmagic.NewACMEIssuer(magic, tmpl)
	magic.Issuers = []certmagic.Issuer{issuer}
	return magic, issuer
}

func redirectHTTPS(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
}

func (p *Proxy) fetchInitialRoutes(ctx context.Context) {
	for {
		msg, err := p.nc.Request(proto.SubjRoutesGet, nil, 5*time.Second)
		if err == nil {
			var t model.RouteTable
			if json.Unmarshal(msg.Data, &t) == nil {
				p.SetRoutes(&t)
				slog.Info("routes loaded", "count", len(t.Routes))
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (p *Proxy) record(serviceID string, status int) {
	if serviceID == "" {
		return
	}
	p.statsMu.Lock()
	h := p.stats[serviceID]
	if h == nil {
		h = &proto.ServiceHits{}
		p.stats[serviceID] = h
	}
	h.Requests++
	h.LastRequest = time.Now().Unix()
	if status >= 500 {
		h.Errors5xx++
	}
	p.statsMu.Unlock()
}

func (p *Proxy) statsLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		p.statsMu.Lock()
		hits := make(map[string]proto.ServiceHits, len(p.stats))
		for k, v := range p.stats {
			hits[k] = *v
		}
		p.stats = map[string]*proto.ServiceHits{}
		p.statsMu.Unlock()
		if len(hits) > 0 {
			_ = bus.Publish(p.nc, proto.SubjProxyStats, proto.ProxyStats{ProxyID: p.id, Hits: hits})
		}
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e := p.lookup(r.Host)
	if e == nil {
		errorPage(w, http.StatusNotFound, "No app is deployed at this address", "Check the domain settings of your service.")
		return
	}
	sw := &statusWriter{ResponseWriter: w, status: 200}
	defer func() { p.record(e.route.ServiceID, sw.status) }()
	switch e.route.Kind {
	case "dashboard":
		if p.dashboard == nil {
			errorPage(sw, http.StatusBadGateway, "Dashboard unavailable", "")
			return
		}
		p.dashboard.ServeHTTP(sw, r)
	case "static":
		p.serveStaticRoute(sw, r, e)
	case "upstream":
		p.forward(sw, r, e.route.Upstreams, e)
	case "sleeping":
		ups, err := p.wake(r.Context(), e.route.ServiceID)
		if err != nil {
			errorPage(sw, http.StatusServiceUnavailable, "This app is waking up", "It could not be started: "+err.Error())
			return
		}
		p.forward(sw, r, ups, e)
	default:
		errorPage(sw, http.StatusServiceUnavailable, "This app is not running", "There is no healthy instance right now. Check the deployment logs.")
	}
}

// wake asks the control plane to start a sleeping service and waits for it,
// coalescing concurrent requests for the same service.
func (p *Proxy) wake(ctx context.Context, serviceID string) ([]string, error) {
	p.wakeMu.Lock()
	if ups, ok := p.woken[serviceID]; ok {
		p.wakeMu.Unlock()
		return ups, nil
	}
	if ch, ok := p.waking[serviceID]; ok {
		p.wakeMu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		p.wakeMu.Lock()
		ups, ok := p.woken[serviceID]
		p.wakeMu.Unlock()
		if !ok {
			return nil, errors.New("wake-up failed")
		}
		return ups, nil
	}
	ch := make(chan struct{})
	p.waking[serviceID] = ch
	p.wakeMu.Unlock()

	var rep proto.WakeReply
	wctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	err := bus.Request(wctx, p.nc, proto.SubjWake, proto.WakeRequest{ServiceID: serviceID}, &rep)
	cancel()
	if err == nil && rep.Error != "" {
		err = errors.New(rep.Error)
	}
	p.wakeMu.Lock()
	delete(p.waking, serviceID)
	if err == nil {
		p.woken[serviceID] = rep.Upstreams
	}
	p.wakeMu.Unlock()
	close(ch)
	if err != nil {
		return nil, err
	}
	return rep.Upstreams, nil
}

// forward proxies to one of the upstreams (round robin), retrying the next one on connection errors.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, upstreams []string, e *entry) {
	if len(upstreams) == 0 {
		errorPage(w, http.StatusServiceUnavailable, "This app is not running", "")
		return
	}
	start := int(e.next.Add(1))
	attempts := len(upstreams)
	if attempts > 3 {
		attempts = 3
	}
	canRetry := r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0
	for i := 0; i < attempts; i++ {
		target := upstreams[(start+i)%len(upstreams)]
		failed := false
		rp := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(&url.URL{Scheme: "http", Host: target})
				pr.Out.Host = r.Host
				pr.SetXForwarded()
			},
			Transport:     p.transport,
			FlushInterval: -1,
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				failed = true
				if i == attempts-1 || !canRetry {
					slog.Debug("upstream error", "target", target, "err", err)
					errorPage(w, http.StatusBadGateway, "The app did not respond", "The upstream instance returned an error or is restarting.")
				}
			},
		}
		if i < attempts-1 && canRetry {
			// Buffer nothing: only retry when the request has no body and nothing was written.
			bw := &bufferedErrWriter{ResponseWriter: w}
			rp.ServeHTTP(bw, r)
			if !failed || bw.wrote {
				return
			}
			continue
		}
		rp.ServeHTTP(w, r)
		return
	}
}

// bufferedErrWriter suppresses the response of a failed attempt so it can be retried.
type bufferedErrWriter struct {
	http.ResponseWriter
	wrote bool
}

func (b *bufferedErrWriter) WriteHeader(code int) {
	b.wrote = true
	b.ResponseWriter.WriteHeader(code)
}

func (b *bufferedErrWriter) Write(p []byte) (int, error) {
	b.wrote = true
	return b.ResponseWriter.Write(p)
}

func (b *bufferedErrWriter) Flush() {
	if f, ok := b.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (b *bufferedErrWriter) Unwrap() http.ResponseWriter { return b.ResponseWriter }

// serveStaticRoute serves from local disk when the build output lives on this
// node, otherwise from the build node's static origin over the mesh.
func (p *Proxy) serveStaticRoute(w http.ResponseWriter, r *http.Request, e *entry) {
	root := e.route.StaticRoot
	if _, err := os.Stat(root); err == nil || e.route.StaticOrigin == "" {
		ServeStatic(w, r, root, e.route.SPA)
		return
	}
	dep := filepath.Base(root)
	svc := filepath.Base(filepath.Dir(root))
	target, err := url.Parse(e.route.StaticOrigin)
	if err != nil {
		errorPage(w, http.StatusBadGateway, "Static origin misconfigured", "")
		return
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = "/" + svc + "/" + dep + pr.In.URL.Path
			q := pr.Out.URL.Query()
			if e.route.SPA {
				q.Set("spa", "1")
			}
			pr.Out.URL.RawQuery = q.Encode()
			pr.Out.Host = r.Host
		},
		Transport: p.transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			errorPage(w, http.StatusBadGateway, "Static site unavailable", "The node holding this build is unreachable.")
		},
	}
	rp.ServeHTTP(w, r)
}
