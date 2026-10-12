// Package server is the control plane: REST API, scheduler, deployment
// pipeline, reconciler and the bus endpoints agents and proxies talk to.
package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/thesunbg/paas/internal/bus"
	"github.com/thesunbg/paas/internal/config"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/secrets"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

const (
	settingClusterToken = "cluster_token"
	settingAllowSignup  = "allow_signup"
	systemProjectSlug   = "system"
)

type Server struct {
	cfg   config.Server
	store *store.Store
	box   *secrets.Box
	ns    *natsserver.Server
	nc    *nats.Conn
	hub   *LogHub

	ClusterToken string

	systemTeamID    string
	systemProjectID string
	sharedNetIndex  int

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex

	routesMu      sync.Mutex
	routesVersion int64

	evictMu   sync.Mutex
	evictedOn map[string]time.Time // node id -> last eviction, per service key "svc/node"

	wake *wakeGroup
	http *http.Server
}

// New connects to Postgres, applies migrations and starts the embedded NATS server.
func New(ctx context.Context, cfg config.Server) (*Server, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	if err := st.Migrate(ctx); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	key, err := secrets.LoadOrCreateKey(filepath.Join(cfg.DataDir, "master.key"), cfg.MasterKey)
	if err != nil {
		return nil, err
	}
	box, err := secrets.New(key)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, store: st, box: box, locks: map[string]*sync.Mutex{}, evictedOn: map[string]time.Time{}, wake: newWakeGroup()}
	if err := s.bootstrap(ctx); err != nil {
		return nil, err
	}
	s.ns, err = bus.StartServer(cfg.NATSAddr, s.ClusterToken, filepath.Join(cfg.DataDir, "nats"))
	if err != nil {
		return nil, fmt.Errorf("start nats: %w", err)
	}
	s.nc, err = bus.ConnectInProcess(s.ns, s.ClusterToken, "control-plane")
	if err != nil {
		return nil, err
	}
	s.hub = NewLogHub(st, s.nc)
	return s, nil
}

// NATS exposes the embedded server for in-process agents and proxies.
func (s *Server) NATS() *natsserver.Server { return s.ns }

func (s *Server) Store() *store.Store { return s.store }

func (s *Server) bootstrap(ctx context.Context) error {
	tok := s.cfg.ClusterToken
	if tok == "" {
		existing, err := s.store.GetSetting(ctx, settingClusterToken)
		switch {
		case err == nil:
			tok = existing
		case errors.Is(err, store.ErrNotFound):
			tok = util.RandomToken(24)
			if err := s.store.SetSetting(ctx, settingClusterToken, tok); err != nil {
				return err
			}
		default:
			return err
		}
	}
	s.ClusterToken = tok

	team, err := s.store.SystemTeam(ctx)
	if errors.Is(err, store.ErrNotFound) {
		team, err = s.store.CreateTeam(ctx, "System", "system-"+util.RandomToken(3), "", true)
	}
	if err != nil {
		return err
	}
	s.systemTeamID = team.ID
	proj, err := s.store.ProjectBySlug(ctx, team.ID, systemProjectSlug)
	if errors.Is(err, store.ErrNotFound) {
		proj, err = s.store.CreateProject(ctx, team.ID, "Shared services", systemProjectSlug)
	}
	if err != nil {
		return err
	}
	s.systemProjectID = proj.ID
	s.sharedNetIndex = proj.NetIndex
	return s.store.FailStuckDeployments(ctx)
}

// Run serves HTTP and background loops until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	if err := s.registerBusHandlers(); err != nil {
		return err
	}
	if err := s.hub.Start(); err != nil {
		return err
	}
	go s.deployWorker(ctx)
	go s.resumeProvisioning(ctx)
	go s.reconcileLoop(ctx)
	go s.maintenanceLoop(ctx)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.publishRoutes(ctx)
				s.publishDNS(ctx)
			}
		}
	}()

	s.http = &http.Server{Addr: s.cfg.HTTPAddr, Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- s.http.ListenAndServe() }()
	slog.Info("control plane listening", "http", s.cfg.HTTPAddr, "nats", s.cfg.NATSAddr, "base_domain", s.cfg.BaseDomain)
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.http.Shutdown(sctx)
		s.nc.Close()
		s.ns.Shutdown()
		s.store.Close()
		return nil
	case err := <-errc:
		return err
	}
}

// lock serializes operations (deploy, scale, sleep, wake) on one service.
func (s *Server) lock(serviceID string) func() {
	s.locksMu.Lock()
	m, ok := s.locks[serviceID]
	if !ok {
		m = &sync.Mutex{}
		s.locks[serviceID] = m
	}
	s.locksMu.Unlock()
	m.Lock()
	return m.Unlock
}

func (s *Server) validToken(tok string) bool {
	return subtle.ConstantTimeCompare([]byte(tok), []byte(s.ClusterToken)) == 1
}

func (s *Server) registerBusHandlers() error {
	if _, err := bus.Handle(s.nc, proto.SubjRegister, s.onRegister); err != nil {
		return err
	}
	if _, err := s.nc.Subscribe(proto.SubjHeartbeat, s.onHeartbeat); err != nil {
		return err
	}
	if _, err := s.nc.Subscribe(proto.SubjRoutesGet, func(m *nats.Msg) {
		t, err := s.routeTable(context.Background())
		if err == nil {
			_ = respondJSON(m, t)
		}
	}); err != nil {
		return err
	}
	if _, err := s.nc.Subscribe(proto.SubjDNSGet, func(m *nats.Msg) {
		t, err := s.dnsTable(context.Background())
		if err == nil {
			_ = respondJSON(m, t)
		}
	}); err != nil {
		return err
	}
	if _, err := s.nc.Subscribe(proto.SubjProxyStats, s.onProxyStats); err != nil {
		return err
	}
	if _, err := bus.Handle(s.nc, proto.SubjWake, s.onWake); err != nil {
		return err
	}
	return nil
}

func (s *Server) onRegister(ctx context.Context, req *proto.RegisterRequest) *proto.RegisterReply {
	if !s.validToken(req.Token) {
		return &proto.RegisterReply{Error: "invalid cluster token"}
	}
	if req.Name == "" {
		return &proto.RegisterReply{Error: "node name required"}
	}
	n, err := s.store.UpsertNode(ctx, req)
	if err != nil {
		return &proto.RegisterReply{Error: err.Error()}
	}
	slog.Info("node registered", "name", n.Name, "id", n.ID, "roles", n.Roles, "memory_mb", n.MemoryMB, "cpus", n.CPUCores)
	go s.publishDNS(context.Background())
	return &proto.RegisterReply{NodeID: n.ID, SharedNetIndex: s.sharedNetIndex, AllowLocalGit: s.cfg.AllowLocalGit}
}

// requestAgent sends a command to an agent and decodes the reply.
func (s *Server) requestAgent(ctx context.Context, nodeID, cmd string, req, rep any, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return bus.Request(ctx, s.nc, proto.AgentSubject(nodeID, cmd), req, rep)
}

func (s *Server) generatedDomain(svcSlug, projSlug string) string {
	return fmt.Sprintf("%s-%s.%s", svcSlug, projSlug, s.cfg.BaseDomain)
}

func internalHost(svcSlug, projSlug string) string {
	return fmt.Sprintf("%s.%s.internal", svcSlug, projSlug)
}

// resumeProvisioning restarts database provisioning interrupted by a restart.
func (s *Server) resumeProvisioning(ctx context.Context) {
	dbs, err := s.store.AllDatabases(ctx)
	if err != nil {
		return
	}
	for _, db := range dbs {
		switch db.Status {
		case "provisioning":
			go s.provisionDatabase(ctx, db.ID)
		case "upgrading", "deleting":
			_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", "operation interrupted by a control plane restart")
		}
	}
	_ = model.TypeWeb
}
