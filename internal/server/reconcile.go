package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
)

func (s *Server) onHeartbeat(m *nats.Msg) {
	var hb proto.Heartbeat
	if err := json.Unmarshal(m.Data, &hb); err != nil || !s.validToken(hb.Token) || hb.NodeID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.store.NodeHeartbeat(ctx, hb.NodeID, hb.Usage); err != nil {
		return
	}
	known, err := s.store.InstancesForNode(ctx, hb.NodeID)
	if err != nil {
		return
	}
	byContainer := map[string]*store.Instance{}
	for i := range known {
		byContainer[known[i].ContainerID] = &known[i]
	}
	reported := map[string]bool{}
	changed := false
	for _, c := range hb.Containers {
		reported[c.ContainerID] = true
		in := byContainer[c.ContainerID]
		if in == nil {
			// Not ours any more (e.g. deleted while the node was offline): clean it up.
			go func(id string) {
				_ = s.requestAgent(context.Background(), hb.NodeID, proto.CmdStop, proto.StopRequest{ContainerID: id, GraceSec: 5}, &proto.GenericReply{}, time.Minute)
			}(c.ContainerID)
			continue
		}
		if c.Evicted && in.Status != model.InstStopping {
			go s.onEvicted(hb.NodeID, *in)
			continue
		}
		wasRunning := in.Status == model.InstRunning
		_ = s.store.ApplyContainerStatus(ctx, hb.NodeID, c)
		if (in.Status == model.InstRunning || in.Status == model.InstFailed) && wasRunning != c.Running {
			changed = true
		}
	}
	for _, in := range known {
		if !reported[in.ContainerID] && in.Status == model.InstRunning && time.Since(in.UpdatedAt) > 30*time.Second {
			_ = s.store.SetInstanceStatus(ctx, in.ID, model.InstLost)
			changed = true
		}
	}
	if changed {
		s.publishAll(ctx)
	}
}

// onEvicted handles an instance the agent stopped under memory pressure.
func (s *Server) onEvicted(nodeID string, in store.Instance) {
	ctx := context.Background()
	slog.Warn("instance evicted under memory pressure", "container", in.ContainerID, "node", nodeID)
	s.evictMu.Lock()
	s.evictedOn[in.ServiceID+"/"+nodeID] = time.Now()
	s.evictMu.Unlock()
	s.stopInstance(ctx, &in, 1)
	svc, err := s.store.ServiceByID(ctx, in.ServiceID)
	if err == nil && (svc.Spec.Sleep.Enabled || svc.Spec.QoS == model.QoSBestEffort) && len(s.instancesOf(ctx, svc.ID)) == 0 {
		// Sleep-capable services wake up on the next request instead of being rescheduled.
		_ = s.store.SetServiceStatus(ctx, svc.ID, model.StatusSleeping)
	}
	s.publishAll(ctx)
}

func (s *Server) excludedNodes(serviceID string) map[string]bool {
	s.evictMu.Lock()
	defer s.evictMu.Unlock()
	out := map[string]bool{}
	for k, t := range s.evictedOn {
		if time.Since(t) > 5*time.Minute {
			delete(s.evictedOn, k)
			continue
		}
		if svc, node, ok := strings.Cut(k, "/"); ok && svc == serviceID {
			out[node] = true
		}
	}
	return out
}

func (s *Server) reconcileLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if ids, err := s.store.MarkStaleNodesOffline(ctx, 30*time.Second); err == nil && len(ids) > 0 {
			for _, id := range ids {
				slog.Warn("node offline", "id", id)
				_, _ = s.store.DB.Exec(ctx, `UPDATE instances SET status='lost' WHERE node_id=$1 AND status IN ('running','starting')`, id)
			}
			s.publishAll(ctx)
		}
		services, err := s.store.AllServices(ctx)
		if err != nil {
			continue
		}
		var wg sync.WaitGroup
		sem := make(chan struct{}, 4)
		for _, svc := range services {
			if svc.Status != model.StatusRunning || svc.CurrentDeploymentID == nil {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(id string) {
				defer func() { <-sem; wg.Done() }()
				s.reconcileService(ctx, id)
			}(svc.ID)
		}
		wg.Wait()
		s.sleepIdle(ctx, services)
	}
}

// reconcileService converges a running service to its desired replica count.
func (s *Server) reconcileService(ctx context.Context, serviceID string) {
	unlock := s.lock(serviceID)
	defer unlock()
	svc, err := s.store.ServiceByID(ctx, serviceID)
	if err != nil || svc.Status != model.StatusRunning || svc.CurrentDeploymentID == nil {
		return
	}
	d, err := s.store.DeploymentByID(ctx, *svc.CurrentDeploymentID)
	if err != nil || d.StaticPath != "" {
		return
	}
	proj, err := s.store.ProjectByID(ctx, svc.ProjectID)
	if err != nil {
		return
	}
	desired := svc.Spec.Replicas
	if desired <= 0 {
		desired = 1
	}
	var healthy []store.Instance
	changed := false
	for _, in := range s.instancesOf(ctx, svc.ID) {
		current := in.DeploymentID != nil && *in.DeploymentID == d.ID
		switch {
		case in.Status == model.InstLost:
			if node, err := s.store.NodeByID(ctx, in.NodeID); err != nil || node.Status != "online" {
				_ = s.store.DeleteInstance(ctx, in.ID)
			} else {
				s.stopInstance(ctx, &in, 2)
			}
			changed = true
		case !current && in.Status == model.InstRunning:
			// Leftover from an interrupted rollout.
			s.stopInstance(ctx, &in, 10)
			changed = true
		case current && (in.Status == model.InstRunning || in.Status == model.InstFailed || in.Status == model.InstStarting):
			healthy = append(healthy, in)
		}
	}
	for len(healthy) > desired {
		last := healthy[len(healthy)-1]
		s.stopInstance(ctx, &last, 10)
		healthy = healthy[:len(healthy)-1]
		changed = true
	}
	for i := len(healthy); i < desired; i++ {
		in, err := s.startInstance(ctx, svc, proj, d, "", s.excludedNodes(svc.ID))
		if err != nil {
			slog.Warn("reconcile: cannot start instance", "service", svc.Slug, "err", err)
			break
		}
		healthy = append(healthy, *in)
		changed = true
	}
	if changed {
		s.publishAll(ctx)
	}
}

func (s *Server) onProxyStats(m *nats.Msg) {
	var st proto.ProxyStats
	if err := json.Unmarshal(m.Data, &st); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for id, h := range st.Hits {
		if h.LastRequest > 0 {
			_ = s.store.TouchServiceRequest(ctx, id, time.Unix(h.LastRequest, 0))
		}
	}
}

// sleepIdle stops services that opted into sleep mode and had no traffic for a while.
func (s *Server) sleepIdle(ctx context.Context, services []store.Service) {
	for _, svc := range services {
		if svc.Status != model.StatusRunning || !svc.Spec.Sleep.Enabled || svc.Type != model.TypeWeb || svc.CurrentDeploymentID == nil {
			continue
		}
		last := svc.UpdatedAt
		if svc.LastRequestAt != nil && svc.LastRequestAt.After(last) {
			last = *svc.LastRequestAt
		}
		if time.Since(last) < time.Duration(svc.Spec.Sleep.IdleMinutes)*time.Minute {
			continue
		}
		go s.putToSleep(context.Background(), svc.ID)
	}
}

func (s *Server) putToSleep(ctx context.Context, serviceID string) {
	unlock := s.lock(serviceID)
	defer unlock()
	svc, err := s.store.ServiceByID(ctx, serviceID)
	if err != nil || svc.Status != model.StatusRunning {
		return
	}
	slog.Info("service idle, going to sleep", "service", svc.Slug)
	_ = s.store.SetServiceStatus(ctx, svc.ID, model.StatusSleeping)
	s.publishRoutes(ctx) // new requests are now held by the proxy and trigger a wake-up
	for _, in := range s.instancesOf(ctx, svc.ID) {
		s.stopInstance(ctx, &in, 5)
	}
	s.publishDNS(ctx)
}

// wakeGroup coalesces concurrent wake-up requests for the same service.
type wakeGroup struct {
	mu    sync.Mutex
	calls map[string]*wakeCall
}

type wakeCall struct {
	done chan struct{}
	rep  proto.WakeReply
}

func newWakeGroup() *wakeGroup { return &wakeGroup{calls: map[string]*wakeCall{}} }

func (s *Server) onWake(ctx context.Context, req *proto.WakeRequest) *proto.WakeReply {
	s.wake.mu.Lock()
	if c, ok := s.wake.calls[req.ServiceID]; ok {
		s.wake.mu.Unlock()
		<-c.done
		return &c.rep
	}
	c := &wakeCall{done: make(chan struct{})}
	s.wake.calls[req.ServiceID] = c
	s.wake.mu.Unlock()

	c.rep = s.wakeService(ctx, req.ServiceID)
	close(c.done)
	s.wake.mu.Lock()
	delete(s.wake.calls, req.ServiceID)
	s.wake.mu.Unlock()
	return &c.rep
}

func (s *Server) wakeService(ctx context.Context, serviceID string) proto.WakeReply {
	start := time.Now()
	svc, err := s.store.ServiceByID(ctx, serviceID)
	if err != nil {
		return proto.WakeReply{Error: "service not found"}
	}
	if svc.Status == model.StatusSleeping {
		_ = s.store.TouchServiceRequest(ctx, svc.ID, time.Now())
		if err := s.store.SetServiceStatus(ctx, svc.ID, model.StatusRunning); err != nil {
			return proto.WakeReply{Error: err.Error()}
		}
	}
	s.reconcileService(ctx, svc.ID)
	var ups []string
	for _, in := range s.instancesOf(ctx, svc.ID) {
		if in.Status == model.InstRunning && in.IP != "" && in.Port > 0 {
			ups = append(ups, fmt.Sprintf("%s:%d", in.IP, in.Port))
		}
	}
	if len(ups) == 0 {
		return proto.WakeReply{Error: "service could not be started"}
	}
	slog.Info("service woke up", "service", svc.Slug, "took", time.Since(start).Round(time.Millisecond))
	return proto.WakeReply{Upstreams: ups}
}

// maintenanceLoop runs periodic housekeeping: image GC, backups, session cleanup.
func (s *Server) maintenanceLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	tick := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		tick++
		s.runScheduledBackups(ctx)
		if tick%30 == 1 {
			s.store.PurgeExpiredSessions(ctx)
			s.imageGC(ctx)
		}
	}
}

func (s *Server) imageGC(ctx context.Context) {
	keep, err := s.store.RetainedImages(ctx, s.cfg.ImageRetention)
	if err != nil {
		return
	}
	nodes, err := s.store.Nodes(ctx)
	if err != nil {
		return
	}
	for _, n := range nodes {
		if n.Status != "online" {
			continue
		}
		var rep proto.GenericReply
		if err := s.requestAgent(ctx, n.ID, proto.CmdImageGC, proto.ImageGCRequest{Prefix: "paas.local/", Keep: keep}, &rep, 2*time.Minute); err != nil {
			slog.Warn("image gc", "node", n.Name, "err", err)
		}
	}
}

var errNoInstance = errors.New("service has no running instance")

// firstInstance returns a running instance of a service (for exec, logs, DB commands).
func (s *Server) firstInstance(ctx context.Context, serviceID string) (*store.Instance, error) {
	for _, in := range s.instancesOf(ctx, serviceID) {
		if in.Status == model.InstRunning {
			return &in, nil
		}
	}
	return nil, errNoInstance
}
