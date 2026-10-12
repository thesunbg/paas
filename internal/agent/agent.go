// Package agent runs on every node. It registers with the control plane,
// executes commands (run/stop/build/exec/logs) and reports node health.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/thesunbg/paas/internal/build"
	"github.com/thesunbg/paas/internal/bus"
	"github.com/thesunbg/paas/internal/config"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/network"
	"github.com/thesunbg/paas/internal/proto"
	rt "github.com/thesunbg/paas/internal/runtime"
)

const Version = "0.1.0"

type Agent struct {
	cfg     config.Agent
	nc      *nats.Conn
	rt      *rt.Runtime
	net     *network.Manager
	builder *build.Builder
	nodeID  string

	cpuMu       sync.Mutex
	lastCPU     [2]uint64
	lastEvictAt time.Time
}

// New prepares an agent. If nc is nil the agent dials cfg.NATSURL.
func New(ctx context.Context, cfg config.Agent, nc *nats.Conn) (*Agent, error) {
	if cfg.NodeAddress == "" {
		cfg.NodeAddress = network.HostIP()
	}
	nm, err := network.NewManager(cfg.CNIBinDir, cfg.DataDir, cfg.SubnetPrefix)
	if err != nil {
		return nil, err
	}
	r, err := rt.New(ctx, cfg.ContainerdSock, cfg.Namespace, cfg.DataDir, cfg.OCIRuntime, nm)
	if err != nil {
		return nil, err
	}
	if nc == nil {
		nc, err = bus.Connect(cfg.NATSURL, cfg.ClusterToken, "agent-"+cfg.NodeName)
		if err != nil {
			return nil, err
		}
	}
	a := &Agent{cfg: cfg, nc: nc, rt: r, net: nm}
	if slices.Contains(cfg.Roles, "build") {
		a.builder = build.New(cfg.BuildkitAddr, cfg.DataDir, 2)
	}
	return a, nil
}

// Run registers the node and serves commands until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.register(ctx); err != nil {
		return err
	}
	a.rt.Recover(ctx)

	subs := []struct {
		cmd string
		fn  func(*nats.Conn, string) (*nats.Subscription, error)
	}{
		{proto.CmdRun, func(nc *nats.Conn, s string) (*nats.Subscription, error) { return bus.Handle(nc, s, a.handleRun) }},
		{proto.CmdStop, func(nc *nats.Conn, s string) (*nats.Subscription, error) { return bus.Handle(nc, s, a.handleStop) }},
		{proto.CmdExec, func(nc *nats.Conn, s string) (*nats.Subscription, error) { return bus.Handle(nc, s, a.handleExec) }},
		{proto.CmdLogs, func(nc *nats.Conn, s string) (*nats.Subscription, error) { return bus.Handle(nc, s, a.handleLogs) }},
		{proto.CmdImageGC, func(nc *nats.Conn, s string) (*nats.Subscription, error) { return bus.Handle(nc, s, a.handleImageGC) }},
		{proto.CmdRemoveFiles, func(nc *nats.Conn, s string) (*nats.Subscription, error) {
			return bus.Handle(nc, s, a.handleRemoveFiles)
		}},
		{proto.CmdBuild, func(nc *nats.Conn, s string) (*nats.Subscription, error) { return bus.Handle(nc, s, a.handleBuild) }},
	}
	for _, s := range subs {
		if s.cmd == proto.CmdBuild && a.builder == nil {
			continue
		}
		sub, err := s.fn(a.nc, proto.AgentSubject(a.nodeID, s.cmd))
		if err != nil {
			return err
		}
		defer sub.Unsubscribe()
	}
	dnsSub, err := a.nc.Subscribe(proto.SubjDNS, func(m *nats.Msg) { a.applyDNS(m.Data) })
	if err != nil {
		return err
	}
	defer dnsSub.Unsubscribe()
	if msg, err := a.nc.Request(proto.SubjDNSGet, nil, 5*time.Second); err == nil {
		a.applyDNS(msg.Data)
	}

	slog.Info("agent ready", "node", a.cfg.NodeName, "id", a.nodeID, "roles", a.cfg.Roles, "address", a.cfg.NodeAddress)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	a.heartbeat(ctx)
	for {
		select {
		case <-ctx.Done():
			a.net.DNS.Shutdown()
			return nil
		case <-tick.C:
			a.heartbeat(ctx)
			a.checkMemoryPressure(ctx)
		}
	}
}

func (a *Agent) register(ctx context.Context) error {
	req := proto.RegisterRequest{
		Token: a.cfg.ClusterToken, Name: a.cfg.NodeName, Address: a.cfg.NodeAddress, Roles: a.cfg.Roles,
		CPUCores: runtime.NumCPU(), MemoryMB: memTotalMB(), Capabilities: a.rt.Capabilities(), Version: Version,
	}
	for attempt := 0; ; attempt++ {
		var rep proto.RegisterReply
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := bus.Request(rctx, a.nc, proto.SubjRegister, req, &rep)
		cancel()
		if err == nil && rep.Error == "" {
			a.nodeID = rep.NodeID
			a.net.SetShared(rep.SharedNetIndex)
			if a.builder != nil {
				a.builder.AllowLocalGit = rep.AllowLocalGit
			}
			return nil
		}
		if err == nil {
			return fmt.Errorf("registration rejected: %s", rep.Error)
		}
		if attempt%5 == 0 {
			slog.Warn("waiting for control plane", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (a *Agent) applyDNS(data []byte) {
	var t model.DNSTable
	if err := json.Unmarshal(data, &t); err == nil && t.Records != nil {
		a.net.DNS.SetRecords(t.Records)
	}
}

func (a *Agent) heartbeat(ctx context.Context) {
	hb := proto.Heartbeat{NodeID: a.nodeID, Token: a.cfg.ClusterToken, Usage: a.usage(), Containers: a.rt.Statuses(ctx)}
	if err := bus.Publish(a.nc, proto.SubjHeartbeat, hb); err != nil {
		slog.Warn("heartbeat", "err", err)
	}
}

func (a *Agent) handleRun(ctx context.Context, req *proto.RunRequest) *proto.RunReply {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	ip, err := a.rt.Run(ctx, *req)
	if err != nil {
		slog.Warn("run failed", "container", req.ContainerID, "err", err)
		return &proto.RunReply{ContainerID: req.ContainerID, Error: err.Error()}
	}
	return &proto.RunReply{ContainerID: req.ContainerID, IP: ip}
}

func (a *Agent) handleStop(ctx context.Context, req *proto.StopRequest) *proto.GenericReply {
	if err := a.rt.Remove(ctx, req.ContainerID, time.Duration(req.GraceSec)*time.Second); err != nil {
		return &proto.GenericReply{Error: err.Error()}
	}
	return &proto.GenericReply{}
}

func (a *Agent) handleExec(ctx context.Context, req *proto.ExecRequest) *proto.ExecReply {
	var err error
	if req.StdinFile, err = a.dataPath(req.StdinFile); err != nil {
		return &proto.ExecReply{Error: err.Error()}
	}
	if req.StdoutFile, err = a.dataPath(req.StdoutFile); err != nil {
		return &proto.ExecReply{Error: err.Error()}
	}
	if req.StdoutFile != "" {
		if err := os.MkdirAll(filepath.Dir(req.StdoutFile), 0o700); err != nil {
			return &proto.ExecReply{Error: err.Error()}
		}
	}
	rep := a.rt.Exec(ctx, *req)
	return &rep
}

// dataPath resolves a path relative to the agent data directory, refusing escapes.
func (a *Agent) dataPath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	root := filepath.Clean(a.cfg.DataDir)
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	clean := filepath.Clean(p)
	if clean != root && !strings.HasPrefix(clean, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q is outside the data directory", p)
	}
	return clean, nil
}

func (a *Agent) handleLogs(ctx context.Context, req *proto.LogsRequest) *proto.LogsReply {
	lines, err := a.rt.Logs(req.ContainerID, req.Tail)
	if err != nil && !os.IsNotExist(err) {
		return &proto.LogsReply{Error: err.Error()}
	}
	if req.Follow && req.Subject != "" {
		fctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		if req.StopSubject != "" {
			sub, err := a.nc.Subscribe(req.StopSubject, func(*nats.Msg) { cancel() })
			if err == nil {
				go func() { <-fctx.Done(); _ = sub.Unsubscribe() }()
			}
		}
		go func() {
			defer cancel()
			_ = a.rt.Follow(fctx, req.ContainerID, func(line string) { _ = a.nc.Publish(req.Subject, []byte(line)) })
		}()
	}
	return &proto.LogsReply{Lines: lines}
}

func (a *Agent) handleImageGC(ctx context.Context, req *proto.ImageGCRequest) *proto.GenericReply {
	n, err := a.rt.GCImages(ctx, req.Prefix, req.Keep)
	if err != nil {
		return &proto.GenericReply{Error: err.Error()}
	}
	if n > 0 {
		slog.Info("image gc", "removed", n)
	}
	return &proto.GenericReply{}
}

// handleRemoveFiles deletes static outputs / backups, restricted to the agent data dir.
func (a *Agent) handleRemoveFiles(ctx context.Context, req *proto.RemoveFilesRequest) *proto.GenericReply {
	for _, p := range req.Paths {
		clean, err := a.dataPath(p)
		if err != nil || clean == filepath.Clean(a.cfg.DataDir) {
			continue
		}
		_ = os.RemoveAll(clean)
	}
	return &proto.GenericReply{}
}

func (a *Agent) handleBuild(ctx context.Context, req *proto.BuildRequest) *proto.BuildReply {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()
	lb := newLineBatcher(a.nc, req.LogSubject)
	defer lb.Close()
	rep := a.builder.Build(ctx, *req, lb.Add)
	if rep.Error != "" {
		lb.Add("==> " + rep.Error)
	}
	return &rep
}

// lineBatcher groups log lines into messages to reduce bus traffic.
type lineBatcher struct {
	nc      *nats.Conn
	subject string
	mu      sync.Mutex
	buf     []string
	done    chan struct{}
	wg      sync.WaitGroup
}

func newLineBatcher(nc *nats.Conn, subject string) *lineBatcher {
	b := &lineBatcher{nc: nc, subject: subject, done: make(chan struct{})}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				b.flush()
			case <-b.done:
				b.flush()
				return
			}
		}
	}()
	return b
}

func (b *lineBatcher) Add(line string) {
	b.mu.Lock()
	b.buf = append(b.buf, line)
	n := len(b.buf)
	b.mu.Unlock()
	if n >= 200 {
		b.flush()
	}
}

func (b *lineBatcher) flush() {
	b.mu.Lock()
	lines := b.buf
	b.buf = nil
	b.mu.Unlock()
	if len(lines) == 0 || b.subject == "" {
		return
	}
	_ = bus.Publish(b.nc, b.subject, lines)
}

func (b *lineBatcher) Close() {
	close(b.done)
	b.wg.Wait()
	_ = b.nc.Flush()
}

// checkMemoryPressure evicts the least important workload when the node runs out of memory.
func (a *Agent) checkMemoryPressure(ctx context.Context) {
	u := a.usage()
	pressure := u.PSIMemorySome10 >= a.cfg.EvictPSI || (u.MemoryAvailableMB > 0 && u.MemoryAvailableMB < a.cfg.EvictMinFreeMB)
	if !pressure || time.Since(a.lastEvictAt) < 30*time.Second {
		return
	}
	snap := a.rt.Snapshot()
	pick := func(qos string) string {
		best, bestMem := "", -1
		for id, c := range snap {
			if c.Live && c.QoS == qos && c.MemMB > bestMem {
				best, bestMem = id, c.MemMB
			}
		}
		return best
	}
	victim := pick(model.QoSBestEffort)
	if victim == "" {
		victim = pick(model.QoSBurstable)
	}
	if victim == "" {
		return
	}
	a.lastEvictAt = time.Now()
	slog.Warn("memory pressure: evicting container", "id", victim, "psi", u.PSIMemorySome10, "available_mb", u.MemoryAvailableMB)
	if err := a.rt.Evict(ctx, victim); err != nil {
		slog.Error("evict", "id", victim, "err", err)
	}
	a.heartbeat(ctx)
}
