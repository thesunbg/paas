package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thesunbg/paas/internal/config"
	"github.com/thesunbg/paas/internal/s3"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

// First-run setup checklist: the steps an administrator walks through after
// creating the first account (domain and DNS, HTTPS, ports, git, off-site
// backups, notifications, a first app). Each step has a server-side check;
// results are cached briefly because some checks resolve names and dial out.
// The administrator's progress (skipped / completed steps, dismissed) is kept
// per instance in the settings table.

const (
	settingSetupWizard = "setup_wizard"
	setupCacheTTL      = 30 * time.Second
	certWarnBefore     = 14 * 24 * time.Hour
)

// setupCheckTimeout bounds each check (a variable for tests).
var setupCheckTimeout = 8 * time.Second

// Step states.
const (
	setupOK   = "ok"
	setupWarn = "warn"
	setupFail = "fail"
)

type setupStepMeta struct {
	ID    string
	Title string
	HowTo string
}

// setupSteps lists the checklist in wizard order.
var setupSteps = []setupStepMeta{
	{"admin", "Admin account", "You are the instance administrator. Turn on two-factor authentication now: this account can change everything on the platform."},
	{"dns", "Domain & DNS", "Apps get <name>.<base domain>, so a wildcard record *.<base domain> and the dashboard domain must point to the public IP of the server running the edge proxy. Create the records below at your DNS provider; changes can take a few minutes to propagate."},
	{"https", "HTTPS", "Certificates are issued automatically by Let's Encrypt with the HTTP-01 challenge: the domain must resolve to this server and port 80 must be reachable from the internet. Install with --email (or set PAAS_TLS=true and PAAS_ACME_EMAIL) to turn HTTPS on."},
	{"ports", "Ports & firewall", "Open TCP 80 and 443 to the internet on edge servers. For more nodes, also allow UDP 51820 (WireGuard mesh) between all nodes and TCP 4222 (control bus) from the nodes to the control plane."},
	{"github", "Git", "Create a GitHub App in one click to deploy private repositories and get push-to-deploy webhooks. GitLab, Gitea/Forgejo and Bitbucket use access tokens under Settings → Git hosts."},
	{"backups", "Off-site backups", "Store database dumps, volume backups and the platform's own state in an S3-compatible bucket outside this cluster, so they survive the loss of a server."},
	{"notifications", "Notifications", "Create a channel (Slack, Discord, Telegram or a webhook) to hear about failed deploys, crash loops, offline nodes and failed backups."},
	{"first_app", "First app", "Deploy a template or a repository to check the whole path end to end: build, scheduling, routing, DNS and certificates."},
}

func setupStepByID(id string) (setupStepMeta, bool) {
	for _, m := range setupSteps {
		if m.ID == id {
			return m, true
		}
	}
	return setupStepMeta{}, false
}

// dnsRecord is a record the administrator should create.
type dnsRecord struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// setupResult is the outcome of one check.
type setupResult struct {
	Status    string         `json:"status"`
	Summary   string         `json:"summary"`
	Details   []string       `json:"details,omitempty"`
	Records   []dnsRecord    `json:"records,omitempty"`
	Info      map[string]any `json:"info,omitempty"`
	CheckedAt time.Time      `json:"checked_at"`
}

type setupStepView struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	HowTo     string `json:"howto"`
	Skipped   bool   `json:"skipped"`
	Completed bool   `json:"completed"`
	setupResult
}

// setupProgress is what the administrator did with the wizard, per instance.
type setupProgress struct {
	Dismissed bool      `json:"dismissed"`
	Finished  bool      `json:"finished"`
	Completed []string  `json:"completed"`
	Skipped   []string  `json:"skipped"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

func (p setupProgress) pending() bool { return !p.Dismissed && !p.Finished }

// setupProbe holds what the checks touch outside the process, so tests can fake it.
type setupProbe struct {
	lookup       func(ctx context.Context, host string) ([]net.IP, error) // A and AAAA
	dial         func(ctx context.Context, network, addr string) (net.Conn, error)
	roots        *x509.CertPool // nil: the system roots
	now          func() time.Time
	proxyHTTP    string // edge proxy listeners on this host
	proxyHTTPS   string
	proxyTLS     bool // the local proxy serves HTTPS (PAAS_TLS / PAAS_ACME_EMAIL)
	storageCheck func(*s3.Config) error
}

type setupState struct {
	mu    sync.Mutex
	probe *setupProbe
	cache map[string]setupResult
	runMu sync.Mutex // one check run at a time; concurrent requests reuse its results
}

func defaultSetupProbe() *setupProbe {
	pcfg := config.LoadProxy()
	resolver := &net.Resolver{}
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	return &setupProbe{
		lookup: func(ctx context.Context, host string) ([]net.IP, error) {
			ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			return resolver.LookupIP(ctx, "ip", host)
		},
		dial:         dialer.DialContext,
		now:          time.Now,
		proxyHTTP:    pcfg.HTTPAddr,
		proxyHTTPS:   pcfg.HTTPSAddr,
		proxyTLS:     pcfg.TLS || pcfg.ACMEEmail != "",
		storageCheck: checkStorage,
	}
}

func (s *Server) setupProbe() *setupProbe {
	s.setup.mu.Lock()
	defer s.setup.mu.Unlock()
	if s.setup.probe == nil {
		s.setup.probe = defaultSetupProbe()
	}
	return s.setup.probe
}

// --- persistence ---

func (s *Server) setupProgress(ctx context.Context) (setupProgress, error) {
	var p setupProgress
	v, err := s.store.GetSetting(ctx, settingSetupWizard)
	if errors.Is(err, store.ErrNotFound) {
		// No progress saved yet. An instance that already runs apps was set up before the
		// wizard existed: don't push its admins into it after an upgrade (the checklist
		// stays available under Settings → System).
		if n, err := s.store.LiveServiceCount(ctx, s.systemProjectID); err == nil && n > 0 {
			p.Dismissed = true
		}
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal([]byte(v), &p); err != nil {
		slog.Warn("setup wizard progress", "err", err)
		return setupProgress{}, nil
	}
	return p, nil
}

func (s *Server) saveSetupProgress(ctx context.Context, p setupProgress) error {
	p.UpdatedAt = time.Now().UTC()
	b, _ := json.Marshal(p)
	return s.store.SetSetting(ctx, settingSetupWizard, string(b))
}

// --- checks ---

// cachedSetupChecks are the instance-wide checks; "admin" depends on the caller and is never cached.
var cachedSetupChecks = []string{"dns", "https", "ports", "github", "backups", "notifications", "first_app"}

func (s *Server) runSetupCheck(ctx context.Context, id string) setupResult {
	p := s.setupProbe()
	ctx, cancel := context.WithTimeout(ctx, setupCheckTimeout)
	defer cancel()
	done := make(chan setupResult, 1)
	go func() {
		var r setupResult
		switch id {
		case "dns":
			r = checkDNS(ctx, p.lookup, s.cfg.BaseDomain, s.cfg.DashboardDomain, s.ownIPs(ctx))
		case "https":
			r = s.checkHTTPS(ctx, p)
		case "ports":
			r = s.checkPorts(ctx, p)
		case "github":
			r = s.checkGitHubSetup(ctx)
		case "backups":
			r = s.checkBackupsSetup(p)
		case "notifications":
			r = s.checkNotificationsSetup(ctx)
		case "first_app":
			r = s.checkFirstApp(ctx)
		default:
			r = setupResult{Status: setupFail, Summary: "unknown step"}
		}
		done <- r
	}()
	var r setupResult
	select {
	case r = <-done:
	case <-ctx.Done():
		r = setupResult{Status: setupFail, Summary: "The check timed out after " + setupCheckTimeout.String() + "."}
	}
	r.CheckedAt = p.now().UTC()
	return r
}

// setupResults returns the cached checks, re-running the stale ones (or the
// given ones) concurrently.
func (s *Server) setupResults(ctx context.Context, force ...string) map[string]setupResult {
	s.setup.runMu.Lock()
	defer s.setup.runMu.Unlock()
	now := s.setupProbe().now()
	s.setup.mu.Lock()
	if s.setup.cache == nil {
		s.setup.cache = map[string]setupResult{}
	}
	out := map[string]setupResult{}
	var stale []string
	for _, id := range cachedSetupChecks {
		r, ok := s.setup.cache[id]
		if ok && now.Sub(r.CheckedAt) < setupCacheTTL && !slices.Contains(force, id) {
			out[id] = r
		} else {
			stale = append(stale, id)
		}
	}
	s.setup.mu.Unlock()

	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, id := range stale {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := s.runSetupCheck(ctx, id)
			mu.Lock()
			out[id] = r
			mu.Unlock()
		}()
	}
	wg.Wait()
	s.setup.mu.Lock()
	for _, id := range stale {
		s.setup.cache[id] = out[id]
	}
	s.setup.mu.Unlock()
	return out
}

func (s *Server) checkAdminSetup(u *store.User) setupResult {
	r := setupResult{Info: map[string]any{"email": u.Email, "totp_enabled": u.TOTPEnabled}}
	if u.TOTPEnabled {
		r.Status, r.Summary = setupOK, "Signed in as "+u.Email+" with two-factor authentication."
	} else {
		r.Status, r.Summary = setupWarn, "Signed in as "+u.Email+"; two-factor authentication is off."
	}
	r.CheckedAt = time.Now().UTC()
	return r
}

// ownIPs are the addresses DNS records should point to: PAAS_PUBLIC_IPS when
// set, otherwise the addresses of edge nodes and of this host's node.
func (s *Server) ownIPs(ctx context.Context) []net.IP {
	var ips []net.IP
	add := func(v string) {
		ip := net.ParseIP(strings.TrimSpace(v))
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
			return
		}
		for _, x := range ips {
			if x.Equal(ip) {
				return
			}
		}
		ips = append(ips, ip)
	}
	for _, v := range s.cfg.PublicIPs {
		add(v)
	}
	if len(ips) > 0 {
		return ips
	}
	if u, err := url.Parse(s.cfg.PublicURL); err == nil {
		add(u.Hostname())
	}
	local := map[string]bool{s.cfg.ServerName: true}
	if h, err := os.Hostname(); err == nil {
		local[h] = true
	}
	if v := os.Getenv("PAAS_NODE_NAME"); v != "" {
		local[v] = true
	}
	nodes, err := s.store.Nodes(ctx)
	if err == nil {
		for _, n := range nodes {
			if n.HasRole("edge") || local[n.Name] {
				add(n.Address)
			}
		}
	}
	return ips
}

func isPublicIP(ip net.IP) bool {
	return !(ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() ||
		// 100.64.0.0/10 (carrier-grade NAT)
		(ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xc0 == 64))
}

func recordType(ip net.IP) string {
	if ip.To4() != nil {
		return "A"
	}
	return "AAAA"
}

func joinIPs(ips []net.IP) string {
	var s []string
	for _, ip := range ips {
		s = append(s, ip.String())
	}
	return strings.Join(s, ", ")
}

// checkDNS resolves a random label under the base domain (the wildcard) and
// the dashboard domain, and compares them with this platform's addresses.
func checkDNS(ctx context.Context, lookup func(context.Context, string) ([]net.IP, error), base, dashboard string, own []net.IP) setupResult {
	r := setupResult{Info: map[string]any{"base_domain": base, "dashboard_domain": dashboard, "server_ips": ipStrings(own)}}
	if base == "" || base == "localhost" || !strings.Contains(base, ".") {
		r.Status = setupFail
		r.Summary = "No base domain is configured (PAAS_BASE_DOMAIN)."
		r.Details = []string{"Set PAAS_BASE_DOMAIN in /etc/paas/paasd.env (or reinstall with --domain) and restart paasd."}
		return r
	}
	var public, private []net.IP
	for _, ip := range own {
		if isPublicIP(ip) {
			public = append(public, ip)
		} else {
			private = append(private, ip)
		}
	}
	target := public
	if len(target) == 0 {
		target = own
	}
	wildcard := "*." + base
	hosts := []struct{ label, name, lookup string }{
		{"wildcard " + wildcard, wildcard, "paas-check-" + strings.ToLower(util.RandomToken(4)) + "." + base},
	}
	if dashboard != "" {
		hosts = append(hosts, struct{ label, name, lookup string }{"dashboard " + dashboard, dashboard, dashboard})
	}

	type res struct {
		ips []net.IP
		err error
	}
	results := make([]res, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ips, err := lookup(ctx, h.lookup)
			results[i] = res{ips, err}
		}()
	}
	wg.Wait()

	worst := setupOK
	raise := func(st string) {
		if st == setupFail || (st == setupWarn && worst == setupOK) {
			worst = st
		}
	}
	var bad []int
	for i, h := range hosts {
		res := results[i]
		switch {
		case res.err != nil || len(res.ips) == 0:
			msg := "does not resolve"
			if res.err != nil {
				msg += " (" + dnsErrText(res.err) + ")"
			}
			r.Details = append(r.Details, h.label+": "+msg+".")
			raise(setupFail)
			bad = append(bad, i)
		case len(own) == 0:
			r.Details = append(r.Details, h.label+" → "+joinIPs(res.ips)+"; this server's public IP is unknown, set PAAS_PUBLIC_IPS to compare.")
			raise(setupWarn)
		default:
			match, extra := 0, []net.IP{}
			for _, ip := range res.ips {
				if containsIP(own, ip) {
					match++
				} else {
					extra = append(extra, ip)
				}
			}
			switch {
			case match > 0 && len(extra) == 0:
				r.Details = append(r.Details, h.label+" → "+joinIPs(res.ips)+": ok.")
			case match > 0:
				r.Details = append(r.Details, h.label+" → "+joinIPs(res.ips)+": also points to "+joinIPs(extra)+", which is not this platform.")
				raise(setupWarn)
				bad = append(bad, i)
			case len(public) == 0:
				// Only private addresses known (behind NAT): the public one may well be right.
				r.Details = append(r.Details, h.label+" → "+joinIPs(res.ips)+"; this server only knows its private address "+joinIPs(private)+
					" (behind NAT?). Set PAAS_PUBLIC_IPS to the public IP to check it.")
				raise(setupWarn)
			default:
				r.Details = append(r.Details, h.label+" → "+joinIPs(res.ips)+", expected "+joinIPs(target)+".")
				raise(setupFail)
				bad = append(bad, i)
			}
		}
	}
	r.Status = worst
	switch worst {
	case setupOK:
		r.Summary = "*." + base + " and " + dashboard + " point to this platform."
	case setupWarn:
		r.Summary = "DNS resolves, but could not be fully confirmed."
	default:
		r.Summary = "DNS records are missing or point elsewhere."
	}
	if len(bad) > 0 {
		for _, i := range bad {
			if len(target) == 0 {
				r.Records = append(r.Records, dnsRecord{Name: hosts[i].name, Type: "A", Value: "<this server's public IPv4>"})
				continue
			}
			for _, ip := range target {
				r.Records = append(r.Records, dnsRecord{Name: hosts[i].name, Type: recordType(ip), Value: ip.String()})
			}
		}
	}
	return r
}

func ipStrings(ips []net.IP) []string {
	out := []string{}
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out
}

func containsIP(list []net.IP, ip net.IP) bool {
	for _, x := range list {
		if x.Equal(ip) {
			return true
		}
	}
	return false
}

func dnsErrText(err error) string {
	var de *net.DNSError
	if errors.As(err, &de) {
		switch {
		case de.IsNotFound:
			return "no such host"
		case de.IsTimeout:
			return "timeout"
		}
		return de.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return err.Error()
}

// loopbackAddr turns a listen address (":443", "0.0.0.0:443") into one to dial on this host.
func loopbackAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// checkHTTPS connects to the dashboard domain with TLS (falling back to the
// local edge proxy with the same SNI when it does not resolve) and inspects
// the certificate it is served.
func (s *Server) checkHTTPS(ctx context.Context, p *setupProbe) setupResult {
	host := s.cfg.DashboardDomain
	httpsOn := p.proxyTLS || strings.HasPrefix(s.cfg.PublicURL, "https://")
	if host == "" {
		return setupResult{Status: setupFail, Summary: "No dashboard domain is configured (PAAS_DASHBOARD_DOMAIN)."}
	}
	addrs := []string{net.JoinHostPort(host, "443")}
	if p.proxyHTTPS != "" {
		addrs = append(addrs, loopbackAddr(p.proxyHTTPS))
	}
	var errs []string
	for _, addr := range addrs {
		cs, err := tlsState(ctx, p.dial, addr, host)
		if err != nil {
			errs = append(errs, addr+": "+err.Error())
			continue
		}
		r := inspectCert(cs, host, p.roots, p.now())
		r.Info["address"] = addr
		if !httpsOn && r.Status == setupOK {
			r.Status = setupWarn
			r.Details = append(r.Details, "PAAS_PUBLIC_URL still starts with http://; set it to https://"+host+".")
		}
		return r
	}
	r := setupResult{Status: setupFail, Info: map[string]any{"host": host}, Details: errs}
	if !httpsOn {
		r.Status = setupWarn
		r.Summary = "HTTPS is off: the dashboard and apps are served over plain HTTP."
		r.Details = append(r.Details, "Reinstall with --email you@example.com, or set PAAS_TLS=true and PAAS_ACME_EMAIL in /etc/paas/paasd.env and restart paasd.")
		return r
	}
	r.Summary = "No certificate is served for " + host + " yet."
	r.Details = append(r.Details, "Check that DNS points here and that port 80 is reachable from the internet (HTTP-01 challenge); the proxy logs show ACME errors.")
	return r
}

// tlsState dials addr and completes a handshake with SNI host without
// verifying: verification is done by inspectCert, to tell what is wrong.
func tlsState(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error), addr, host string) (tls.ConnectionState, error) {
	raw, err := dial(ctx, "tcp", addr)
	if err != nil {
		return tls.ConnectionState{}, err
	}
	defer raw.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(dl)
	}
	c := tls.Client(raw, &tls.Config{ServerName: host, InsecureSkipVerify: true}) //nolint:gosec // verified by inspectCert
	if err := c.HandshakeContext(ctx); err != nil {
		return tls.ConnectionState{}, fmt.Errorf("TLS handshake: %w", err)
	}
	return c.ConnectionState(), nil
}

func inspectCert(cs tls.ConnectionState, host string, roots *x509.CertPool, now time.Time) setupResult {
	r := setupResult{Info: map[string]any{"host": host}}
	if len(cs.PeerCertificates) == 0 {
		r.Status, r.Summary = setupFail, "The server sent no certificate."
		return r
	}
	leaf := cs.PeerCertificates[0]
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	issuer := leaf.Issuer.CommonName
	if issuer == "" && len(leaf.Issuer.Organization) > 0 {
		issuer = leaf.Issuer.Organization[0]
	}
	r.Info["issuer"] = issuer
	r.Info["not_after"] = leaf.NotAfter.UTC()
	r.Info["names"] = leaf.DNSNames
	switch {
	case now.After(leaf.NotAfter):
		r.Status = setupFail
		r.Summary = fmt.Sprintf("The certificate for %s expired on %s.", host, leaf.NotAfter.UTC().Format("2006-01-02"))
		r.Details = []string{"Renewal uses the HTTP-01 challenge: check that port 80 is reachable from the internet and look for ACME errors in the proxy logs."}
		return r
	case now.Before(leaf.NotBefore):
		r.Status, r.Summary = setupFail, "The certificate is not valid yet (check the server clock)."
		return r
	}
	if err := leaf.VerifyHostname(host); err != nil {
		r.Status = setupFail
		r.Summary = "The certificate served is not for " + host + "."
		r.Details = []string{"It covers: " + strings.Join(append([]string{leaf.Subject.CommonName}, leaf.DNSNames...), ", ") + "."}
		return r
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: roots, Intermediates: inter, CurrentTime: now}); err != nil {
		r.Status = setupFail
		r.Summary = "The certificate for " + host + " is not trusted (self-signed or from a test CA, issued by " + issuer + ")."
		r.Details = []string{err.Error(), "Unset PAAS_ACME_CA if it points to a staging directory, then restart the proxy."}
		return r
	}
	left := leaf.NotAfter.Sub(now)
	if left < certWarnBefore {
		r.Status = setupWarn
		r.Summary = fmt.Sprintf("The certificate for %s expires in %d days and has not been renewed yet.", host, int(left.Hours()/24))
		r.Details = []string{"Renewal needs port 80 reachable from the internet (HTTP-01 challenge)."}
		return r
	}
	r.Status = setupOK
	r.Summary = fmt.Sprintf("Valid certificate for %s from %s, until %s.", host, issuer, leaf.NotAfter.UTC().Format("2006-01-02"))
	return r
}

type portProbe struct {
	Host  string `json:"host"`
	Addr  string `json:"addr"`
	Port  int    `json:"port"`
	Open  bool   `json:"open"`
	Error string `json:"error,omitempty"`
}

func probePort(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error), host, addr string) portProbe {
	pp := portProbe{Host: host, Addr: addr}
	if _, p, err := net.SplitHostPort(addr); err == nil {
		pp.Port, _ = strconv.Atoi(p)
	}
	c, err := dial(ctx, "tcp", addr)
	if err != nil {
		pp.Error = err.Error()
		return pp
	}
	c.Close()
	pp.Open = true
	return pp
}

// checkPorts checks that the edge proxy listens on 80 and 443, on this host
// and on edge nodes. Reachability from the internet cannot be seen from here.
func (s *Server) checkPorts(ctx context.Context, p *setupProbe) setupResult {
	type target struct{ host, http, https string }
	targets := []target{{"this server", loopbackAddr(p.proxyHTTP), loopbackAddr(p.proxyHTTPS)}}
	nodes, _ := s.store.Nodes(ctx)
	multi := len(nodes) > 1
	for _, n := range nodes {
		if n.HasRole("edge") && n.Address != "" {
			targets = append(targets, target{n.Name, net.JoinHostPort(n.Address, "80"), net.JoinHostPort(n.Address, "443")})
		}
	}
	results := make([][2]portProbe, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = [2]portProbe{probePort(ctx, p.dial, t.host, t.http), probePort(ctx, p.dial, t.host, t.https)}
		}()
	}
	wg.Wait()
	r := setupResult{Info: map[string]any{}}
	var probes []portProbe
	serving := 0
	for i, t := range targets {
		pr := results[i]
		probes = append(probes, pr[0], pr[1])
		var state []string
		for _, x := range pr {
			if x.Open {
				state = append(state, fmt.Sprintf("%d listening", x.Port))
			} else {
				state = append(state, fmt.Sprintf("%d not listening", x.Port))
			}
		}
		r.Details = append(r.Details, t.host+": "+strings.Join(state, ", ")+".")
		if pr[0].Open && pr[1].Open {
			serving++
		}
	}
	r.Info["probes"] = probes
	switch {
	case serving > 0:
		r.Status = setupOK
		r.Summary = "The edge proxy listens on ports 80 and 443."
	default:
		r.Status = setupFail
		r.Summary = "Nothing listens on ports 80 and 443: the edge proxy is not running here."
	}
	r.Details = append(r.Details, "From inside the server, a cloud firewall or security group cannot be seen: make sure TCP 80 and 443 are open to the internet.")
	if multi {
		r.Details = append(r.Details, "Multi-node: allow UDP 51820 between all nodes (WireGuard mesh) and TCP 4222 from the nodes to the control plane (control bus).")
	} else {
		r.Details = append(r.Details, "Adding nodes later needs UDP 51820 between nodes (WireGuard mesh) and TCP 4222 from the nodes to the control plane.")
	}
	return r
}

func (s *Server) checkGitHubSetup(ctx context.Context) setupResult {
	c, err := s.githubApp(ctx)
	switch {
	case errors.Is(err, errNoGitHubApp):
		return setupResult{Status: setupWarn, Summary: "No GitHub App yet: only public repositories and other git hosts with a token can be deployed.",
			Info: map[string]any{"configured": false}}
	case err != nil:
		return setupResult{Status: setupFail, Summary: "The GitHub App cannot be loaded: " + err.Error(), Info: map[string]any{"configured": false}}
	}
	app := c.App()
	return setupResult{Status: setupOK, Summary: "GitHub App " + app.Slug + " is configured.",
		Info: map[string]any{"configured": true, "slug": app.Slug, "html_url": app.HTMLURL}}
}

func (s *Server) checkBackupsSetup(p *setupProbe) setupResult {
	c := s.storage()
	info := map[string]any{"enabled": c.Enabled(), "from_env": s.s3Env}
	if !c.Enabled() {
		return setupResult{Status: setupWarn, Summary: "Off-site storage is off: backups stay on the servers and are lost with them.", Info: info}
	}
	info["endpoint"], info["bucket"] = c.Endpoint, c.Bucket
	cp := *c
	if err := p.storageCheck(&cp); err != nil {
		return setupResult{Status: setupFail, Summary: "Off-site storage check failed: " + err.Error(), Info: info}
	}
	return setupResult{Status: setupOK, Summary: "Bucket " + c.Bucket + " at " + c.Endpoint + " accepts uploads, downloads and deletes.", Info: info}
}

func (s *Server) checkNotificationsSetup(ctx context.Context) setupResult {
	teams, err := s.store.AdminTeamIDs(ctx)
	if err != nil {
		return setupResult{Status: setupFail, Summary: err.Error()}
	}
	n := 0
	for _, t := range teams {
		chs, err := s.store.Channels(ctx, t)
		if err != nil {
			return setupResult{Status: setupFail, Summary: err.Error()}
		}
		n += len(chs)
	}
	if n == 0 {
		return setupResult{Status: setupWarn, Summary: "No notification channel: nobody hears about failures.", Info: map[string]any{"channels": 0}}
	}
	return setupResult{Status: setupOK, Summary: fmt.Sprintf("%d notification channel(s) receive cluster events.", n), Info: map[string]any{"channels": n}}
}

func (s *Server) checkFirstApp(ctx context.Context) setupResult {
	n, err := s.store.LiveServiceCount(ctx, s.systemProjectID)
	if err != nil {
		return setupResult{Status: setupFail, Summary: err.Error()}
	}
	if n == 0 {
		return setupResult{Status: setupWarn, Summary: "Nothing is deployed yet.", Info: map[string]any{"services": 0}}
	}
	return setupResult{Status: setupOK, Summary: fmt.Sprintf("%d service(s) deployed.", n), Info: map[string]any{"services": n}}
}

// --- API ---

type setupChecklist struct {
	setupProgress
	Pending bool            `json:"pending"`
	Steps   []setupStepView `json:"steps"`
}

func (s *Server) setupChecklist(ctx context.Context, u *store.User, force ...string) (*setupChecklist, error) {
	prog, err := s.setupProgress(ctx)
	if err != nil {
		return nil, err
	}
	results := s.setupResults(ctx, force...)
	results["admin"] = s.checkAdminSetup(u)
	out := &setupChecklist{setupProgress: prog, Pending: prog.pending()}
	if out.Completed == nil {
		out.Completed = []string{}
	}
	if out.Skipped == nil {
		out.Skipped = []string{}
	}
	for _, m := range setupSteps {
		out.Steps = append(out.Steps, setupStepView{ID: m.ID, Title: m.Title, HowTo: m.HowTo,
			Skipped: slices.Contains(prog.Skipped, m.ID), Completed: slices.Contains(prog.Completed, m.ID), setupResult: results[m.ID]})
	}
	return out, nil
}

// GET /api/v1/admin/setup-checklist[?refresh=1]
func (s *Server) handleGetSetupChecklist(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var force []string
	if v := r.URL.Query().Get("refresh"); v == "1" || v == "true" {
		force = cachedSetupChecks
	}
	cl, err := s.setupChecklist(r.Context(), currentUser(r), force...)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, cl)
}

// POST /api/v1/admin/setup-checklist/{step}/check runs one check again, bypassing the cache.
func (s *Server) handleCheckSetupStep(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("step")
	m, ok := setupStepByID(id)
	if !ok {
		writeError(w, 404, "unknown step")
		return
	}
	prog, err := s.setupProgress(r.Context())
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	var res setupResult
	if id == "admin" {
		res = s.checkAdminSetup(currentUser(r))
	} else {
		res = s.setupResults(r.Context(), id)[id]
	}
	writeJSON(w, 200, setupStepView{ID: m.ID, Title: m.Title, HowTo: m.HowTo,
		Skipped: slices.Contains(prog.Skipped, id), Completed: slices.Contains(prog.Completed, id), setupResult: res})
}

// PUT /api/v1/admin/setup-checklist {dismissed?, finished?, completed?: [step], skipped?: [step]}
// Fields left out keep their value.
func (s *Server) handlePutSetupChecklist(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in struct {
		Dismissed *bool     `json:"dismissed"`
		Finished  *bool     `json:"finished"`
		Completed *[]string `json:"completed"`
		Skipped   *[]string `json:"skipped"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	clean := func(ids []string) ([]string, error) {
		out := []string{}
		for _, id := range ids {
			if _, ok := setupStepByID(id); !ok {
				return nil, fmt.Errorf("unknown step %q", id)
			}
			if !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
		return out, nil
	}
	ctx := r.Context()
	release := s.store.AdvisoryLock(ctx, settingSetupWizard)
	defer release()
	prog, err := s.setupProgress(ctx)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if in.Dismissed != nil {
		prog.Dismissed = *in.Dismissed
	}
	if in.Finished != nil {
		prog.Finished = *in.Finished
	}
	if in.Completed != nil {
		if prog.Completed, err = clean(*in.Completed); err != nil {
			writeError(w, 400, err.Error())
			return
		}
	}
	if in.Skipped != nil {
		if prog.Skipped, err = clean(*in.Skipped); err != nil {
			writeError(w, 400, err.Error())
			return
		}
	}
	if err := s.saveSetupProgress(ctx, prog); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if prog.Completed == nil {
		prog.Completed = []string{}
	}
	if prog.Skipped == nil {
		prog.Skipped = []string{}
	}
	writeJSON(w, 200, map[string]any{"dismissed": prog.Dismissed, "finished": prog.Finished, "completed": prog.Completed,
		"skipped": prog.Skipped, "updated_at": prog.UpdatedAt, "pending": prog.pending()})
}
