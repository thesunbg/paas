// Package server is the control plane: REST API, scheduler, deployment
// pipeline, reconciler and the bus endpoints agents and proxies talk to.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/thesunbg/paas/internal/github"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/thesunbg/paas/internal/bus"
	"github.com/thesunbg/paas/internal/config"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/network"
	"github.com/thesunbg/paas/internal/pki"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/s3"
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

	auth           *bus.Auth // NATS client authenticator; Internal secret lives only in this process
	internalSecret string
	ca             *pki.CA
	caPEM          string // public cert, handed to nodes at enroll

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

	clusterBase [2]int

	s3 *s3.Config

	traffic *traffic
	scaleMu sync.Mutex
	scale   map[string]*scaleState

	ghMu sync.Mutex
	gh   *github.Client

	pitrRunning atomic.Bool
	haMu        sync.Mutex
	haRunning   map[string]time.Time
	ha          haMemory // leader-only HA observations, guarded by haMu
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
	base, err := network.ParseBase(cfg.ClusterBase)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, store: st, box: box, locks: map[string]*sync.Mutex{}, evictedOn: map[string]time.Time{}, wake: newWakeGroup(), clusterBase: base,
		traffic: newTraffic(), scale: map[string]*scaleState{},
		s3: &s3.Config{Endpoint: cfg.S3.Endpoint, Region: cfg.S3.Region, Bucket: cfg.S3.Bucket, AccessKey: cfg.S3.AccessKey,
			SecretKey: cfg.S3.SecretKey, Prefix: cfg.S3.Prefix, VirtualHost: cfg.S3.VirtualHost}}
	if err := s.bootstrap(ctx); err != nil {
		return nil, err
	}
	if err := s.initTrust(ctx); err != nil {
		return nil, err
	}
	s.ns, err = bus.StartServer(cfg.NATSAddr, s.auth, s.ca.ServerTLS(), filepath.Join(cfg.DataDir, "nats"),
		bus.ClusterOptions{Name: cfg.ServerName, Listen: cfg.ClusterAddr, Routes: cfg.ClusterRoutes, Secret: s.ClusterToken, TLS: s.ca.RouteTLS()})
	if err != nil {
		return nil, fmt.Errorf("start nats: %w", err)
	}
	s.nc, err = bus.ConnectInProcess(s.ns, s.internalSecret, "control-plane")
	if err != nil {
		return nil, err
	}
	s.hub = NewLogHub(st, s.nc)
	return s, nil
}

// NATS exposes the embedded server for in-process agents and proxies.
func (s *Server) NATS() *natsserver.Server { return s.ns }

// EnsureLocalNode returns the id of this control plane's own node row (created on first use), so the
// embedded agent can use paas.node.<id>.* from its first message.
func (s *Server) EnsureLocalNode(ctx context.Context, name string, roles []string) (string, error) {
	n, err := s.store.EnsureLocalNode(ctx, name, roles, network.MaxNodes(s.clusterBase))
	if err != nil {
		return "", err
	}
	return n.ID, nil
}

// InternalSecret authenticates in-process NATS connections (bus.ConnectInProcess).
func (s *Server) InternalSecret() string { return s.internalSecret }

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
	return nil
}

// Run serves HTTP and background loops until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	if err := s.registerBusHandlers(); err != nil {
		return err
	}
	if err := s.hub.Start(); err != nil {
		return err
	}
	go s.leaderLoop(ctx)
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
				s.publishMesh(ctx)
			}
		}
	}()

	s.http = &http.Server{Addr: s.cfg.HTTPAddr, Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10}
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

// lock serializes operations (deploy, scale, sleep, wake) on one service, across
// every control plane: an in-process mutex plus a PostgreSQL advisory lock.
func (s *Server) lock(serviceID string) func() {
	m := s.serviceMutex(serviceID)
	m.Lock()
	release := s.store.AdvisoryLock(context.Background(), "service:"+serviceID)
	return func() {
		release()
		m.Unlock()
	}
}

// tryLock is lock without waiting: false when the service is busy.
func (s *Server) tryLock(serviceID string) (func(), bool) {
	m := s.serviceMutex(serviceID)
	if !m.TryLock() {
		return nil, false
	}
	release, ok := s.store.TryAdvisoryLock(context.Background(), "service:"+serviceID)
	if !ok {
		m.Unlock()
		return nil, false
	}
	return func() {
		release()
		m.Unlock()
	}, true
}

func (s *Server) serviceMutex(serviceID string) *sync.Mutex {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	m, ok := s.locks[serviceID]
	if !ok {
		m = &sync.Mutex{}
		s.locks[serviceID] = m
	}
	return m
}

func (s *Server) validToken(tok string) bool {
	return subtle.ConstantTimeCompare([]byte(tok), []byte(s.ClusterToken)) == 1
}

func (s *Server) registerBusHandlers() error {
	const q = "control"
	// Legacy subjects: the sender is named in the payload and trusted by the shared cluster token.
	// Remove when no node has nkey IS NULL (plan 261017-cluster-trust). Enrolled nodes and the
	// embedded agent use paas.node.<id>.* below.
	if s.cfg.NATSLegacy {
		if _, err := bus.HandleQueue(s.nc, proto.SubjRegister, q, s.onRegister); err != nil {
			return err
		}
		if _, err := s.nc.QueueSubscribe(proto.SubjHeartbeat, q, s.onHeartbeat); err != nil {
			return err
		}
	}
	if _, err := s.nc.QueueSubscribe(proto.SubjNodeRegisterAll, q, s.onNodeRegister); err != nil {
		return err
	}
	if _, err := s.nc.QueueSubscribe(proto.SubjNodeHeartbeatAll, q, s.onNodeHeartbeat); err != nil {
		return err
	}
	// Every control plane must see a kick (no queue group): the node's connection may be on any of them.
	if _, err := s.nc.Subscribe(proto.SubjCPKick, s.onKick); err != nil {
		return err
	}
	if _, err := bus.HandleQueue(s.nc, proto.SubjBinary, q, s.onBinary); err != nil {
		return err
	}
	if _, err := s.nc.QueueSubscribe(proto.SubjRoutesGet, q, func(m *nats.Msg) {
		t, err := s.routeTable(context.Background())
		if err == nil {
			_ = respondJSON(m, t)
		}
	}); err != nil {
		return err
	}
	if _, err := s.nc.QueueSubscribe(proto.SubjDNSGet, q, func(m *nats.Msg) {
		t, err := s.dnsTable(context.Background())
		if err == nil {
			_ = respondJSON(m, t)
		}
	}); err != nil {
		return err
	}
	if _, err := s.nc.QueueSubscribe(proto.SubjMeshGet, q, func(m *nats.Msg) {
		t, err := s.meshTable(context.Background())
		if err == nil {
			_ = respondJSON(m, t)
		}
	}); err != nil {
		return err
	}
	if _, err := s.nc.Subscribe(proto.SubjProxyStats, s.onProxyStats); err != nil {
		return err
	}
	if _, err := bus.HandleQueue(s.nc, proto.SubjWake, q, s.onWake); err != nil {
		return err
	}
	return nil
}

// onRegister serves the legacy subject: the node is named by the payload and found or created by name.
// Legacy: remove when no node has nkey IS NULL (plan 261017-cluster-trust).
func (s *Server) onRegister(ctx context.Context, req *proto.RegisterRequest) *proto.RegisterReply {
	if !s.validToken(req.Token) {
		return &proto.RegisterReply{Error: "invalid cluster token"}
	}
	if req.Name == "" {
		return &proto.RegisterReply{Error: "node name required"}
	}
	n, err := s.store.UpsertNodeLegacy(ctx, req, network.MaxNodes(s.clusterBase))
	if err != nil {
		return &proto.RegisterReply{Error: err.Error()}
	}
	return s.registered(n)
}

// onNodeRegister serves paas.node.<id>.register: id comes from the subject, which NATS
// permissions bind to the sender. It only updates a node that enroll created.
func (s *Server) onNodeRegister(m *nats.Msg) {
	id := proto.NodeIDFromSubject(m.Subject)
	go func() {
		var req proto.RegisterRequest
		if id == "" || json.Unmarshal(m.Data, &req) != nil || m.Reply == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		rep := &proto.RegisterReply{}
		n, err := s.store.UpdateNodeRegistration(ctx, id, &req, network.MaxNodes(s.clusterBase))
		if errors.Is(err, store.ErrNotFound) {
			rep.Error = "node is not enrolled"
		} else if err != nil {
			rep.Error = err.Error()
		} else {
			rep = s.registered(n)
		}
		_ = respondJSON(m, rep)
	}()
}

func (s *Server) registered(n *store.Node) *proto.RegisterReply {
	slog.Info("node registered", "name", n.Name, "id", n.ID, "index", *n.NetIndex, "roles", n.Roles, "memory_mb", n.MemoryMB, "cpus", n.CPUCores, "legacy", n.Legacy)
	go func() {
		s.publishMesh(context.Background())
		s.publishDNS(context.Background())
	}()
	return &proto.RegisterReply{NodeID: n.ID, NodeIndex: *n.NetIndex, ClusterBase: s.cfg.ClusterBase,
		SharedNetIndex: s.sharedNetIndex, AllowLocalGit: s.cfg.AllowLocalGit}
}

// requestAgent sends a command to an agent and decodes the reply.
func (s *Server) requestAgent(ctx context.Context, nodeID, cmd string, req, rep any, timeout time.Duration) error {
	// Intentional debt: a few-ms window remains between this check and the send, so a
	// just-deposed leader can still land one command. Remove/upgrade when a real incident
	// happens or a non-idempotent agent command is added: move to agent-side epoch checks.
	if err := s.store.CheckFence(ctx); err != nil {
		return err
	}
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

// leaderLoop runs the background controllers on exactly one control plane at a time.
// Leadership is a PostgreSQL session advisory lock held on a dedicated connection.
func (s *Server) leaderLoop(ctx context.Context) {
	for ctx.Err() == nil {
		conn, err := pgx.Connect(ctx, s.cfg.DatabaseURL)
		if err != nil {
			time.Sleep(3 * time.Second)
			continue
		}
		var leader bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(7313001)`).Scan(&leader); err != nil || !leader {
			_ = conn.Close(context.Background())
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		// Bump the epoch (waits for the previous leader's in-flight writes): from here on every
		// store call on lctx fails with ErrFenced once a newer leader exists.
		epoch, err := s.store.TakeLeadership(ctx, conn)
		if err != nil {
			slog.Warn("take leadership", "err", err)
			_ = conn.Close(context.Background())
			time.Sleep(3 * time.Second)
			continue
		}
		slog.Info("this control plane is now the leader", "server", s.cfg.ServerName, "epoch", epoch)
		s.haBecameLeader()
		lctx, cancel := context.WithCancel(ctx)
		lctx = store.WithEpoch(lctx, epoch)
		_ = s.store.FailInterruptedJobs(lctx)
		_ = s.store.FailStuckDeployments(lctx)
		go s.deployWorker(lctx)
		go s.resumeProvisioning(lctx)
		go s.syncBaseDomain(lctx)
		go s.cronLoop(lctx)
		go s.metricsLoop(lctx)
		go s.reconcileLoop(lctx)
		go s.maintenanceLoop(lctx)
		go s.dbMetricsLoop(lctx)
		go s.haLoop(lctx)
		go s.walShipLoop(lctx)
		for lctx.Err() == nil {
			time.Sleep(5 * time.Second)
			pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
			err := conn.Ping(pctx)
			pcancel()
			if err != nil {
				slog.Warn("lost leadership (database connection)", "err", err)
				break
			}
		}
		cancel()
		_ = conn.Close(context.Background())
	}
}

// syncBaseDomain moves generated domains to the current base domain after it
// changed (e.g. from <ip>.sslip.io to a real domain) and points the GitHub App
// webhook at the current public URL.
func (s *Server) syncBaseDomain(ctx context.Context) {
	doms, err := s.store.AllDomains(ctx)
	if err != nil {
		return
	}
	moved := 0
	for _, d := range doms {
		if !d.IsGenerated || strings.HasSuffix(d.Hostname, "."+s.cfg.BaseDomain) {
			continue
		}
		svc, err := s.store.ServiceByID(ctx, d.ServiceID)
		if err != nil {
			continue
		}
		proj, err := s.store.ProjectByID(ctx, svc.ProjectID)
		if err != nil {
			continue
		}
		host := s.generatedDomain(svc.Slug, proj.Slug)
		if svc.Spec.Role == roleStudio {
			host = s.studioHost(proj)
		}
		if _, err := s.store.Q(ctx).Exec(ctx, `UPDATE domains SET hostname=$2 WHERE id=$1`, d.ID, host); err != nil {
			slog.Warn("move generated domain", "from", d.Hostname, "to", host, "err", err)
			continue
		}
		slog.Info("generated domain moved", "from", d.Hostname, "to", host)
		moved++
	}
	if moved > 0 {
		s.publishRoutes(ctx)
	}
	if c, err := s.githubApp(ctx); err == nil {
		hook := s.cfg.PublicURL + "/api/v1/webhooks/github-app"
		if err := c.SetHookURL(ctx, hook); err != nil {
			slog.Warn("github app webhook url", "err", err)
		}
	}
}
