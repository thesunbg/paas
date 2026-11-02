package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/thesunbg/paas/internal/netguard"
	"github.com/thesunbg/paas/internal/store"
)

// Uptime checks run from the control plane leader: it already holds the state
// in PostgreSQL and the notification code, so results need no new bus messages
// and every check runs exactly once per interval cluster-wide. (Running them
// on the edge nodes would test from inside the cluster's own network, which is
// no better a vantage point than the control plane.)

const (
	uptimeTick         = 10 * time.Second
	uptimeParallel     = 16
	uptimeMinInterval  = 30
	uptimeBodyLimit    = 1 << 20
	uptimeCertWarn     = 14 * 24 * time.Hour
	maxUptimeChecks    = 20 // per project
	uptimeResultsLimit = 1000
)

// AllowPrivateUptimeTargets lets checks reach private addresses (tests / trusted
// networks); by default a check may only dial public addresses, like webhooks.
var AllowPrivateUptimeTargets = os.Getenv("PAAS_UPTIME_ALLOW_PRIVATE") == "true"

// uptimeRunner remembers the checks in flight, so a slow check is not started twice.
type uptimeRunner struct {
	mu      sync.Mutex
	running map[string]bool
	client  *http.Client
}

func (u *uptimeRunner) start(id string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.running == nil {
		u.running = map[string]bool{}
	}
	if u.running[id] {
		return false
	}
	u.running[id] = true
	return true
}

func (u *uptimeRunner) done(id string) {
	u.mu.Lock()
	delete(u.running, id)
	u.mu.Unlock()
}

// newUptimeClient dials only public addresses unless allowPrivate. Connections
// are not reused, so every check measures (and sees the certificate of) a fresh one.
func newUptimeClient(allowPrivate bool, tlsConf *tls.Config) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: func(network, address string, _ syscall.RawConn) error {
		if allowPrivate {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if !netguard.Public(net.ParseIP(host)) {
			return fmt.Errorf("%s is not a public address", host)
		}
		return nil
	}}
	return &http.Client{
		Transport: &http.Transport{DialContext: dialer.DialContext, TLSClientConfig: tlsConf, DisableKeepAlives: true, TLSHandshakeTimeout: 10 * time.Second},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

// probe runs one check. cert is the expiry of the https certificate served (the
// final response's, after redirects), nil for plain http or a failed handshake.
func probe(ctx context.Context, client *http.Client, c *store.UptimeCheck) (res store.UptimeResult, cert *time.Time) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(max(c.TimeoutSeconds, 1))*time.Second)
	defer cancel()
	start := time.Now()
	res.TS = start.UTC().Truncate(time.Millisecond)
	req, err := http.NewRequestWithContext(ctx, c.Method, c.URL, nil)
	if err != nil {
		res.Error = err.Error()
		return
	}
	req.Header.Set("User-Agent", "paas-uptime/1")
	resp, err := client.Do(req)
	if err != nil {
		res.Error = trimErr(err)
		res.LatencyMS = int(time.Since(start).Milliseconds())
		return
	}
	defer resp.Body.Close()
	res.StatusCode = resp.StatusCode
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		exp := resp.TLS.PeerCertificates[0].NotAfter
		cert = &exp
	}
	var body []byte
	if c.Keyword != "" {
		body, err = io.ReadAll(io.LimitReader(resp.Body, uptimeBodyLimit))
	} else {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, uptimeBodyLimit))
	}
	res.LatencyMS = int(time.Since(start).Milliseconds())
	switch {
	case err != nil:
		res.Error = trimErr(err)
	case c.ExpectedStatus != 0 && resp.StatusCode != c.ExpectedStatus:
		res.Error = fmt.Sprintf("status %d, expected %d", resp.StatusCode, c.ExpectedStatus)
	case c.ExpectedStatus == 0 && (resp.StatusCode < 200 || resp.StatusCode >= 400):
		res.Error = fmt.Sprintf("status %d", resp.StatusCode)
	case c.Keyword != "" && !bytes.Contains(body, []byte(c.Keyword)):
		res.Error = fmt.Sprintf("keyword %q not found", c.Keyword)
	default:
		res.OK = true
	}
	return
}

func trimErr(err error) string {
	s := err.Error()
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// applyUptimeResult updates c with a result and returns the events to notify:
// uptime.down after failure_threshold consecutive failures, uptime.up on the
// first success after that, uptime.cert once per certificate expiring soon.
func applyUptimeResult(c *store.UptimeCheck, res store.UptimeResult, cert *time.Time, now time.Time) []string {
	var events []string
	c.LastCheckedAt = &now
	c.LastError = res.Error
	if res.OK {
		c.ConsecutiveFailures = 0
		if c.State != "up" {
			if c.State == "down" {
				events = append(events, "uptime.up")
			}
			c.State, c.StateChangedAt = "up", &now
		}
	} else {
		c.ConsecutiveFailures++
		if c.ConsecutiveFailures >= max(c.FailureThreshold, 1) && c.State != "down" {
			c.State, c.StateChangedAt = "down", &now
			events = append(events, "uptime.down")
		}
	}
	if cert != nil {
		c.CertExpiresAt = cert
		if cert.Sub(now) < uptimeCertWarn && (c.CertWarnedExpiry == nil || !c.CertWarnedExpiry.Equal(*cert)) {
			c.CertWarnedExpiry = cert
			events = append(events, "uptime.cert")
		}
	}
	return events
}

func (s *Server) uptimeLoop(ctx context.Context) {
	t := time.NewTicker(uptimeTick)
	defer t.Stop()
	sem := make(chan struct{}, uptimeParallel)
	lastPrune := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if time.Since(lastPrune) > time.Hour {
			lastPrune = time.Now()
			if err := s.store.PruneUptimeResults(ctx); err != nil {
				slog.Warn("prune uptime results", "err", err)
			}
		}
		checks, err := s.store.AllUptimeChecks(ctx)
		if err != nil {
			continue
		}
		now := time.Now()
		for i := range checks {
			c := checks[i]
			if !c.Enabled || (c.LastCheckedAt != nil && now.Sub(*c.LastCheckedAt) < time.Duration(c.IntervalSeconds)*time.Second-uptimeTick/2) {
				continue
			}
			if !s.uptime.start(c.ID) {
				continue
			}
			go func() {
				defer s.uptime.done(c.ID)
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()
				s.runUptimeCheck(ctx, &c)
			}()
		}
	}
}

func (s *Server) uptimeClient() *http.Client {
	s.uptime.mu.Lock()
	defer s.uptime.mu.Unlock()
	if s.uptime.client == nil {
		s.uptime.client = newUptimeClient(AllowPrivateUptimeTargets, nil)
	}
	return s.uptime.client
}

// runUptimeCheck probes c, records the result and notifies state changes.
func (s *Server) runUptimeCheck(ctx context.Context, c *store.UptimeCheck) {
	res, cert := probe(ctx, s.uptimeClient(), c)
	if ctx.Err() != nil {
		return
	}
	events := applyUptimeResult(c, res, cert, time.Now())
	if err := s.store.RecordUptimeResult(ctx, c, &res); err != nil {
		slog.Warn("record uptime result", "check", c.Name, "err", err)
		return
	}
	if len(events) == 0 {
		return
	}
	p, err := s.store.ProjectByID(ctx, c.ProjectID)
	if err != nil {
		return
	}
	for _, ev := range events {
		var text string
		switch ev {
		case "uptime.down":
			text = fmt.Sprintf("%s (%s) is down: %s (%d consecutive failures)", c.Name, c.URL, c.LastError, c.ConsecutiveFailures)
		case "uptime.up":
			text = fmt.Sprintf("%s (%s) is up again (HTTP %d, %d ms)", c.Name, c.URL, res.StatusCode, res.LatencyMS)
		case "uptime.cert":
			days := int(time.Until(*c.CertExpiresAt).Hours() / 24)
			text = fmt.Sprintf("The TLS certificate of %s (%s) expires in %d days (%s)", c.Name, c.URL, days, c.CertExpiresAt.UTC().Format("2006-01-02"))
		}
		slog.Info(ev, "check", c.Name, "url", c.URL)
		s.notifyVia(ctx, p.TeamID, c.ChannelIDs, ev, text, map[string]any{"check_id": c.ID, "check": c.Name, "url": c.URL, "project": p.Slug,
			"status_code": res.StatusCode, "error": res.Error, "cert_expires_at": c.CertExpiresAt})
	}
}

// API

type uptimeView struct {
	store.UptimeCheck
	Stats store.UptimeStats `json:"stats"`
}

type uptimeInput struct {
	Name             *string   `json:"name"`
	ServiceID        *string   `json:"service_id"`
	URL              *string   `json:"url"`
	Method           *string   `json:"method"`
	ExpectedStatus   *int      `json:"expected_status"`
	Keyword          *string   `json:"keyword"`
	TimeoutSeconds   *int      `json:"timeout_seconds"`
	IntervalSeconds  *int      `json:"interval_seconds"`
	FailureThreshold *int      `json:"failure_threshold"`
	ChannelIDs       *[]string `json:"channel_ids"`
	Enabled          *bool     `json:"enabled"`
}

func (in *uptimeInput) apply(c *store.UptimeCheck) {
	if in.Name != nil {
		c.Name = strings.TrimSpace(*in.Name)
	}
	if in.URL != nil {
		c.URL = strings.TrimSpace(*in.URL)
	}
	if in.Method != nil {
		c.Method = strings.ToUpper(strings.TrimSpace(*in.Method))
	}
	if in.ExpectedStatus != nil {
		c.ExpectedStatus = *in.ExpectedStatus
	}
	if in.Keyword != nil {
		c.Keyword = *in.Keyword
	}
	if in.TimeoutSeconds != nil {
		c.TimeoutSeconds = *in.TimeoutSeconds
	}
	if in.IntervalSeconds != nil {
		c.IntervalSeconds = *in.IntervalSeconds
	}
	if in.FailureThreshold != nil {
		c.FailureThreshold = *in.FailureThreshold
	}
	if in.ChannelIDs != nil {
		c.ChannelIDs = *in.ChannelIDs
	}
	if in.Enabled != nil {
		c.Enabled = *in.Enabled
	}
}

func validateUptimeCheck(c *store.UptimeCheck) error {
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || len(c.URL) > 2000 {
		return errors.New("url must be an http(s) URL")
	}
	if u.User != nil {
		return errors.New("url must not contain credentials")
	}
	if !slices.Contains([]string{"GET", "HEAD", "POST"}, c.Method) {
		return errors.New("method must be GET, HEAD or POST")
	}
	if c.ExpectedStatus != 0 && (c.ExpectedStatus < 100 || c.ExpectedStatus > 599) {
		return errors.New("expected_status must be 0 (any 2xx/3xx) or an HTTP status")
	}
	if len(c.Keyword) > 200 || (c.Keyword != "" && c.Method == "HEAD") {
		return errors.New("keyword must be at most 200 characters, and needs a response body (not HEAD)")
	}
	if c.IntervalSeconds < uptimeMinInterval || c.IntervalSeconds > 86400 {
		return fmt.Errorf("interval_seconds must be between %d and 86400", uptimeMinInterval)
	}
	if c.TimeoutSeconds < 1 || c.TimeoutSeconds > 30 || c.TimeoutSeconds >= c.IntervalSeconds {
		return errors.New("timeout_seconds must be between 1 and 30")
	}
	if c.FailureThreshold < 1 || c.FailureThreshold > 20 {
		return errors.New("failure_threshold must be between 1 and 20")
	}
	if len(c.Name) > 100 {
		return errors.New("name must be at most 100 characters")
	}
	return nil
}

func (s *Server) uptimeViews(ctx context.Context, checks []store.UptimeCheck) ([]uptimeView, error) {
	ids := make([]string, 0, len(checks))
	for _, c := range checks {
		ids = append(ids, c.ID)
	}
	stats, err := s.store.UptimeStatsFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]uptimeView, 0, len(checks))
	for _, c := range checks {
		out = append(out, uptimeView{UptimeCheck: c, Stats: stats[c.ID]})
	}
	return out, nil
}

func (s *Server) handleListUptime(w http.ResponseWriter, r *http.Request) {
	p := s.projectFor(w, r, r.PathValue("id"), "viewer")
	if p == nil {
		return
	}
	checks, err := s.store.UptimeChecksForProject(r.Context(), p.ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if f := r.URL.Query().Get("service"); f != "" {
		checks = slices.DeleteFunc(checks, func(c store.UptimeCheck) bool { return c.ServiceID == nil || *c.ServiceID != f })
	}
	out, err := s.uptimeViews(r.Context(), checks)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"checks": out})
}

func (s *Server) handleCreateUptime(w http.ResponseWriter, r *http.Request) {
	p := s.projectFor(w, r, r.PathValue("id"), "developer")
	if p == nil {
		return
	}
	var in uptimeInput
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	ctx := r.Context()
	svcID, err := s.projectServiceRef(ctx, p.ID, in.ServiceID)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	c := &store.UptimeCheck{ProjectID: p.ID, ServiceID: svcID, Method: "GET", TimeoutSeconds: 10, IntervalSeconds: 60, FailureThreshold: 3, Enabled: true}
	in.apply(c)
	if c.Name == "" {
		if u, err := url.Parse(c.URL); err == nil {
			c.Name = u.Host
		}
	}
	if err := validateUptimeCheck(c); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if err := s.checkChannels(ctx, p.TeamID, c.ChannelIDs); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if existing, err := s.store.UptimeChecksForProject(ctx, p.ID); err == nil && len(existing) >= maxUptimeChecks {
		writeError(w, 400, fmt.Sprintf("a project has at most %d uptime checks", maxUptimeChecks))
		return
	}
	out, err := s.store.CreateUptimeCheck(ctx, c)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, out)
}

func (s *Server) uptimeFor(w http.ResponseWriter, r *http.Request, min string) (*store.UptimeCheck, *store.Project) {
	c, err := s.store.UptimeCheckByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "uptime check not found")
		return nil, nil
	}
	p := s.projectFor(w, r, c.ProjectID, min)
	if p == nil {
		return nil, nil
	}
	return c, p
}

func (s *Server) handleGetUptime(w http.ResponseWriter, r *http.Request) {
	c, _ := s.uptimeFor(w, r, "viewer")
	if c == nil {
		return
	}
	ctx := r.Context()
	limit := 100
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, uptimeResultsLimit)
	}
	views, err := s.uptimeViews(ctx, []store.UptimeCheck{*c})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	results, err := s.store.UptimeResults(ctx, c.ID, limit)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if results == nil {
		results = []store.UptimeResult{}
	}
	writeJSON(w, 200, map[string]any{"check": views[0], "results": results})
}

func (s *Server) handleUpdateUptime(w http.ResponseWriter, r *http.Request) {
	c, p := s.uptimeFor(w, r, "developer")
	if c == nil {
		return
	}
	var in uptimeInput
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if in.ServiceID != nil {
		writeError(w, 400, "service_id cannot be changed: create another check")
		return
	}
	in.apply(c)
	if err := validateUptimeCheck(c); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if err := s.checkChannels(r.Context(), p.TeamID, c.ChannelIDs); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	out, err := s.store.UpdateUptimeCheck(r.Context(), c)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleDeleteUptime(w http.ResponseWriter, r *http.Request) {
	c, _ := s.uptimeFor(w, r, "developer")
	if c == nil {
		return
	}
	if err := s.store.DeleteUptimeCheck(r.Context(), c.ID); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}
