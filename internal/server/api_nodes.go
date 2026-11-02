package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/thesunbg/paas/internal/bus"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
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
		CommittedMemoryMB int            `json:"committed_memory_mb"`
		CommittedCPU      int            `json:"committed_cpu_millis"`
		CapacityMemoryMB  int            `json:"capacity_memory_mb"`
		CapacityCPU       int            `json:"capacity_cpu_millis"`
		Instances         int            `json:"instances"`
		UsedByAppsMB      int            `json:"used_by_apps_mb"`
		OvercommitRatio   float64        `json:"overcommit_ratio"`
		Traffic           trafficSummary `json:"traffic"`
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
		no := nodeOut{Node: n, CommittedMemoryMB: committed[n.ID][0], CommittedCPU: committed[n.ID][1], CapacityMemoryMB: memCap, CapacityCPU: cpuCap,
			Traffic: s.trafficSummary(r.Context(), "node_traffic_daily", "node_id", n.ID)}
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
	// Removing a node unpins what lived on it: the next start would be on
	// another node with empty volumes. Refuse while there is any, unless the
	// administrator recovered them first or accepts losing their data.
	if stuck := s.stuckOnNode(r.Context(), n.ID); len(stuck) > 0 && r.URL.Query().Get("force") != "true" {
		writeJSON(w, 409, map[string]any{
			"error": fmt.Sprintf("%d service(s) or database(s) keep their data on this node; recover them on another node first, or delete with force=true to start them empty", len(stuck)),
			"stuck": stuck,
		})
		return
	}
	nkey, _ := s.store.NodeNkey(r.Context(), n.ID)
	if err := s.store.DeleteNode(r.Context(), n.ID); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if nkey != "" {
		// Row is gone, so reconnects fail the lookup; the kick cuts the live connections on every control plane.
		_ = bus.Publish(s.nc, proto.SubjCPKick, kickMsg{Nkey: nkey})
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
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
	var in struct {
		// MoveVolumes moves the services with volumes too (each stops while its volumes are
		// copied to another node); without it they stay on the node.
		MoveVolumes bool `json:"move_volumes"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if err := s.store.UpdateNodeSettings(r.Context(), n.ID, "cordoned", n.OvercommitMemory, n.OvercommitCPU, n.ReservedMB, n.Labels); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	movingStateful := []string{}
	if in.MoveVolumes {
		movingStateful = s.drainStateful(context.WithoutCancel(r.Context()), n, currentUser(r).Email)
	}
	instances, _ := s.store.InstancesForNode(r.Context(), n.ID)
	moved, pinned := 0, []string{}
	for _, in := range instances {
		svc, err := s.store.ServiceByID(r.Context(), in.ServiceID)
		if err != nil {
			continue
		}
		if svc.NodeID != nil && *svc.NodeID == n.ID {
			if !slices.Contains(movingStateful, svc.Slug) {
				pinned = append(pinned, svc.Slug) // stateful: its volume lives here
			}
			continue
		}
		moved++
		go s.moveInstance(context.Background(), svc.ID, in, n.ID)
	}
	writeJSON(w, 202, map[string]any{"status": "draining", "moving": moved, "pinned_stateful": pinned, "moving_stateful": movingStateful})
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

// stuckItem is something whose data lives only on a node.
type stuckItem struct {
	Kind      string `json:"kind"` // service | database | shared-database
	ID        string `json:"id"`
	Name      string `json:"name"`
	Project   string `json:"project,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	Backup    string `json:"backup,omitempty"` // newest off-site backup it can be recovered from
}

// stuckOnNode lists the services with volumes and the database servers pinned to a node.
func (s *Server) stuckOnNode(ctx context.Context, nodeID string) []stuckItem {
	out := []stuckItem{}
	svcs, err := s.store.AllServices(ctx)
	if err != nil {
		return out
	}
	projects := map[string]string{}
	projName := func(id string) string {
		if n, ok := projects[id]; ok {
			return n
		}
		if p, err := s.store.ProjectByID(ctx, id); err == nil {
			projects[id] = p.Name
		}
		return projects[id]
	}
	dbs, _ := s.store.AllDatabases(ctx)
	for i := range svcs {
		svc := &svcs[i]
		node := s.serviceNode(ctx, svc)
		if node == nil || *node != nodeID || len(svc.Spec.Volumes) == 0 {
			continue
		}
		if svc.Type == model.TypeDatabase {
			for j := range dbs {
				db := &dbs[j]
				if db.ServerServiceID == nil || *db.ServerServiceID != svc.ID {
					continue
				}
				it := stuckItem{Kind: "database", ID: db.ID, Name: db.Name, Project: projName(db.ProjectID), ProjectID: db.ProjectID}
				if db.Mode == "shared" {
					it.Kind = "shared-database"
				}
				if b := s.latestOffsiteDump(ctx, db.ID); b != nil {
					it.Backup = b.CreatedAt.UTC().Format(time.RFC3339)
				}
				out = append(out, it)
			}
			continue
		}
		if svc.Spec.Role != "" || svc.ProjectID == s.systemProjectID {
			continue // platform services (registry, studio) are recreated on demand
		}
		it := stuckItem{Kind: "service", ID: svc.ID, Name: svc.Name, Project: projName(svc.ProjectID), ProjectID: svc.ProjectID}
		if bs, err := s.store.VolumeBackups(ctx, svc.ID, 10); err == nil {
			for _, b := range bs {
				if b.Status == "completed" && b.S3Key != "" {
					it.Backup = b.CreatedAt.UTC().Format(time.RFC3339)
					break
				}
			}
		}
		out = append(out, it)
	}
	return out
}

// GET /api/v1/nodes/{id}/stuck
func (s *Server) handleNodeStuck(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	writeJSON(w, 200, s.stuckOnNode(r.Context(), r.PathValue("id")))
}
