package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/scheduler"
	"github.com/thesunbg/paas/internal/store"
)

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	nodes, err := s.store.Nodes(r.Context())
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	committed, _ := s.store.NodeCommitted(r.Context())
	instances, _ := s.store.AllInstances(r.Context())
	type nodeOut struct {
		store.Node
		CommittedMemoryMB int     `json:"committed_memory_mb"`
		CommittedCPU      int     `json:"committed_cpu_millis"`
		CapacityMemoryMB  int     `json:"capacity_memory_mb"`
		CapacityCPU       int     `json:"capacity_cpu_millis"`
		Instances         int     `json:"instances"`
		UsedByAppsMB      int     `json:"used_by_apps_mb"`
		OvercommitRatio   float64 `json:"overcommit_ratio"`
	}
	sleeping := 0
	services, _ := s.store.AllServices(r.Context())
	for _, svc := range services {
		if svc.Status == model.StatusSleeping {
			sleeping++
		}
	}
	out := []nodeOut{}
	for _, n := range nodes {
		memCap, cpuCap := scheduler.Capacity(&n, model.QoSBurstable)
		no := nodeOut{Node: n, CommittedMemoryMB: committed[n.ID][0], CommittedCPU: committed[n.ID][1], CapacityMemoryMB: memCap, CapacityCPU: cpuCap}
		for _, in := range instances {
			if in.NodeID == n.ID && in.Status == model.InstRunning {
				no.Instances++
				no.UsedByAppsMB += in.MemoryUsageMB
			}
		}
		if real := n.MemoryMB - n.ReservedMB; real > 0 {
			no.OvercommitRatio = float64(no.CommittedMemoryMB) / float64(real)
		}
		out = append(out, no)
	}
	writeJSON(w, 200, map[string]any{"nodes": out, "sleeping_services": sleeping})
}

func (s *Server) handleUpdateNode(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	n, err := s.store.NodeByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "node not found")
		return
	}
	var in struct {
		Status           *string           `json:"status"`
		OvercommitMemory *float64          `json:"overcommit_memory"`
		OvercommitCPU    *float64          `json:"overcommit_cpu"`
		ReservedMB       *int              `json:"reserved_mb"`
		Labels           map[string]string `json:"labels"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if in.Status != nil {
		if *in.Status != "online" && *in.Status != "cordoned" {
			writeError(w, 400, "status must be online or cordoned")
			return
		}
		n.Status = *in.Status
	}
	if in.OvercommitMemory != nil {
		if *in.OvercommitMemory < 1 || *in.OvercommitMemory > 4 {
			writeError(w, 400, "overcommit_memory must be between 1 and 4")
			return
		}
		n.OvercommitMemory = *in.OvercommitMemory
	}
	if in.OvercommitCPU != nil {
		if *in.OvercommitCPU < 1 || *in.OvercommitCPU > 16 {
			writeError(w, 400, "overcommit_cpu must be between 1 and 16")
			return
		}
		n.OvercommitCPU = *in.OvercommitCPU
	}
	if in.ReservedMB != nil && *in.ReservedMB >= 0 {
		n.ReservedMB = *in.ReservedMB
	}
	if in.Labels != nil {
		n.Labels = in.Labels
	}
	if err := s.store.UpdateNodeSettings(r.Context(), n.ID, n.Status, n.OvercommitMemory, n.OvercommitCPU, n.ReservedMB, n.Labels); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, n)
}

func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	n, err := s.store.NodeByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "node not found")
		return
	}
	if n.Status == "online" {
		writeError(w, 400, "cordon the node and stop its agent before removing it")
		return
	}
	if err := s.store.DeleteNode(r.Context(), n.ID); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// handleJoinInfo returns the command to add a node to the cluster.
func (s *Server) handleJoinInfo(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	_, port, _ := net.SplitHostPort(s.cfg.NATSAddr)
	host := strings.TrimPrefix(strings.TrimPrefix(s.cfg.PublicURL, "https://"), "http://")
	host = strings.SplitN(host, "/", 2)[0]
	host = strings.SplitN(host, ":", 2)[0]
	natsURL := fmt.Sprintf("nats://%s:%s", host, port)
	cmd := fmt.Sprintf("curl -fsSL %s/install.sh | sudo sh -s -- join --server %s --token %s --binary %s/download/paasd --roles worker", s.cfg.PublicURL, natsURL, s.ClusterToken, s.cfg.PublicURL)
	writeJSON(w, 200, map[string]string{"nats_url": natsURL, "token": s.ClusterToken, "command": cmd})
}

// handleDrainNode cordons a node and moves its stateless instances elsewhere without downtime.
func (s *Server) handleDrainNode(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	n, err := s.store.NodeByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "node not found")
		return
	}
	if err := s.store.UpdateNodeSettings(r.Context(), n.ID, "cordoned", n.OvercommitMemory, n.OvercommitCPU, n.ReservedMB, n.Labels); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	instances, _ := s.store.InstancesForNode(r.Context(), n.ID)
	moved, pinned := 0, []string{}
	for _, in := range instances {
		svc, err := s.store.ServiceByID(r.Context(), in.ServiceID)
		if err != nil {
			continue
		}
		if svc.NodeID != nil && *svc.NodeID == n.ID {
			pinned = append(pinned, svc.Slug) // stateful: its volume lives here
			continue
		}
		moved++
		go s.moveInstance(context.Background(), svc.ID, in, n.ID)
	}
	writeJSON(w, 202, map[string]any{"status": "draining", "moving": moved, "pinned_stateful": pinned})
}

func (s *Server) moveInstance(ctx context.Context, serviceID string, in store.Instance, from string) {
	unlock := s.lock(serviceID)
	defer unlock()
	svc, err := s.store.ServiceByID(ctx, serviceID)
	if err != nil || svc.CurrentDeploymentID == nil {
		return
	}
	d, err := s.store.DeploymentByID(ctx, *svc.CurrentDeploymentID)
	if err != nil {
		return
	}
	proj, err := s.store.ProjectByID(ctx, svc.ProjectID)
	if err != nil {
		return
	}
	if in.DeploymentID != nil && *in.DeploymentID == d.ID {
		if _, err := s.startInstance(ctx, svc, proj, d, "", map[string]bool{from: true}); err != nil {
			slog.Warn("drain: no room elsewhere, keeping instance", "service", svc.Slug, "err", err)
			return
		}
		s.publishAll(ctx)
		time.Sleep(2 * time.Second)
	}
	s.stopInstance(ctx, &in, 15)
	s.publishAll(ctx)
}
