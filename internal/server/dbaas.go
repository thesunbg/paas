package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/thesunbg/paas/internal/databases"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/store"
)

// External access: a database can be reached from outside the cluster on
// db.<base domain>:<port>. Every edge proxy listens on the port, enforces the
// allowlist and terminates TLS (see proxy/tcp.go).

func tcpMode(engine string) string {
	switch engine {
	case databases.Postgres:
		return "postgres"
	case databases.Redis, databases.Mongo:
		return "tls"
	}
	return "plain"
}

// tlsAtEdge reports whether the edge proxies terminate TLS (automatic HTTPS on).
// It reads the control plane's PublicURL; on a multi-node edge an individual
// proxy may still lack TLS, in which case it keeps the database ports closed.
func (s *Server) tlsAtEdge() bool { return strings.HasPrefix(s.cfg.PublicURL, "https://") }

// tcpRoutes lists the public database ports for the route table.
func (s *Server) tcpRoutes(ctx context.Context, upstreams map[string][]string) []model.TCPRoute {
	dbs, err := s.store.AllDatabases(ctx)
	if err != nil {
		return nil
	}
	var out []model.TCPRoute
	for _, db := range dbs {
		if db.Options.PublicPort == 0 || db.ServerServiceID == nil {
			continue
		}
		mode := tcpMode(db.Engine)
		if mode == "plain" { // no TLS for this engine: never exposed
			slog.Warn("public port closed: engine has no TLS support", "database", db.Name, "engine", db.Engine, "port", db.Options.PublicPort)
			continue
		}
		out = append(out, model.TCPRoute{
			Port: db.Options.PublicPort, DatabaseID: db.ID, Mode: mode,
			Upstreams: upstreams[*db.ServerServiceID], Allow: db.Options.AllowedCIDRs,
		})
	}
	return out
}

// externalURL is the connection string for clients outside the cluster.
func (s *Server) externalURL(db *store.Database, c databases.Credentials) string {
	if db.Options.PublicPort == 0 {
		return ""
	}
	c.Host, c.Port = s.cfg.DBHost, db.Options.PublicPort
	u, err := url.Parse(databases.URL(db.Engine, c))
	if err != nil {
		return ""
	}
	q := u.Query()
	// Public ports only open with TLS, so every URL asks for it.
	switch db.Engine {
	case databases.Postgres:
		q.Set("sslmode", "require")
	case databases.Redis:
		u.Scheme = "rediss"
	case databases.Mongo:
		q.Set("tls", "true")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *Server) allocatePublicPort(ctx context.Context) (int, error) {
	used, err := s.store.UsedPublicPorts(ctx)
	if err != nil {
		return 0, err
	}
	for p := s.cfg.DBPortMin; p <= s.cfg.DBPortMax; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, errors.New("no free public database port; widen PAAS_DB_PORT_MIN/MAX")
}

func normalizeCIDRs(in []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range in {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		var pfx netip.Prefix
		if p, err := netip.ParsePrefix(v); err == nil {
			pfx = p.Masked()
		} else if a, err := netip.ParseAddr(v); err == nil {
			pfx = netip.PrefixFrom(a, a.BitLen())
		} else {
			return nil, fmt.Errorf("invalid IP or CIDR %q", v)
		}
		if !seen[pfx.String()] {
			seen[pfx.String()] = true
			out = append(out, pfx.String())
		}
	}
	if len(out) > 100 {
		return nil, errors.New("at most 100 allowlist entries")
	}
	return out, nil
}

// POST /api/v1/databases/{id}/public {enabled, allowed_cidrs}
func (s *Server) handleDatabasePublic(w http.ResponseWriter, r *http.Request) {
	db, p := s.databaseFor(w, r, "admin")
	if db == nil {
		return
	}
	var in struct {
		Enabled      bool     `json:"enabled"`
		AllowedCIDRs []string `json:"allowed_cidrs"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	ctx := r.Context()
	release := s.store.AdvisoryLock(ctx, "db-public-port")
	defer release()
	db, err := s.store.DatabaseByID(ctx, db.ID)
	if err != nil {
		writeError(w, 404, "database not found")
		return
	}
	var cidrs []string
	if in.Enabled {
		var err error
		if !s.tlsAtEdge() {
			writeError(w, 409, "public database access needs HTTPS: set PAAS_TLS / an https PublicURL first")
			return
		}
		if tcpMode(db.Engine) == "plain" {
			writeError(w, 409, db.Engine+" has no TLS support on the public port, so it cannot be exposed")
			return
		}
		if cidrs, err = normalizeCIDRs(in.AllowedCIDRs); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if len(cidrs) == 0 {
			writeError(w, 400, "add at least one allowed IP or CIDR (0.0.0.0/0 allows everyone)")
			return
		}
	}
	// db-public-port is held, so the port allocated for a fresh PublicPort==0 is unique.
	// The closure sees options only, so If-Match is checked against the version read
	// above (after taking the lock); a retry means that version moved on.
	var portErr error
	calls := 0
	o, err := s.store.UpdateDatabaseOptions(ctx, db.ID, func(o *store.DBOptions) error {
		calls++
		if _, ok := ifMatch(r); ok && calls > 1 {
			return store.ErrStale
		}
		if err := checkIfMatch(r, db.OptionsVersion); err != nil {
			return err
		}
		if !in.Enabled {
			o.PublicPort, o.AllowedCIDRs = 0, nil
			return nil
		}
		o.AllowedCIDRs = cidrs
		if o.PublicPort == 0 {
			port, err := s.allocatePublicPort(ctx)
			if err != nil {
				portErr = err
				return err
			}
			o.PublicPort = port
		}
		return nil
	})
	if portErr != nil {
		writeError(w, 503, portErr.Error())
		return
	}
	if err != nil {
		writeUpdateError(w, err)
		return
	}
	u := currentUser(r)
	s.store.Audit(ctx, &u.ID, &p.TeamID, "database.public", fmt.Sprintf("%s %v %s", db.Name, in.Enabled, strings.Join(o.AllowedCIDRs, ",")))
	s.publishRoutes(context.WithoutCancel(ctx))
	writeJSON(w, 200, map[string]any{"public_port": o.PublicPort, "allowed_cidrs": o.AllowedCIDRs, "host": s.cfg.DBHost, "port": strconv.Itoa(o.PublicPort)})
}
