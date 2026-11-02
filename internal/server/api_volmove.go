package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/store"
)

// nodeAliveWindow: a node heard from this recently is alive. A cordoned node never turns offline,
// so its status alone does not say whether it still runs.
const nodeAliveWindow = time.Minute

// nodeAlive reports whether a node's agent still reports in (online or cordoned alike). Its data
// can be copied from it; a restore from a backup would throw that data away.
func (s *Server) nodeAlive(ctx context.Context, id *string) bool {
	if id == nil {
		return false
	}
	n, err := s.store.NodeByID(ctx, *id)
	return err == nil && n.Status != "offline" && time.Since(n.LastSeenAt) < nodeAliveWindow
}

// movableReason says why a service's volumes cannot be moved by a volume move ("" if they can).
func (s *Server) movableReason(svc *store.Service) string {
	switch {
	case len(svc.Spec.Volumes) == 0:
		return "this service has no volumes: it moves by itself (drain its node)"
	case svc.Type == model.TypeDatabase:
		return "database servers move with the database tools (upgrade, high availability), not as volumes"
	case svc.Spec.Role != "" || svc.ProjectID == s.systemProjectID:
		return "platform services are recreated on demand and are not moved"
	case svc.ParentID != nil:
		return "preview environments are not moved"
	}
	return ""
}

// volumeMoveTarget resolves the target node of a move or restore: want (an id or a name), or
// the online worker with the most free memory. avoid is the service's current node.
func (s *Server) volumeMoveTarget(ctx context.Context, svc *store.Service, avoid, want string) (*store.Node, error) {
	if want == "" {
		n, err := s.pickReplicaNode(ctx, avoid, svc.Spec.Resources.MemoryLimitMB)
		if err != nil {
			return nil, errors.New("no other online worker node")
		}
		if n.NetIndex == nil {
			return nil, fmt.Errorf("node %s is not on the mesh", n.Name)
		}
		return n, nil
	}
	nodes, err := s.store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	for i := range nodes {
		n := &nodes[i]
		if n.ID != want && n.Name != want {
			continue
		}
		switch {
		case n.ID == avoid:
			return nil, fmt.Errorf("the service is already on %s", n.Name)
		case n.Status != "online":
			return nil, fmt.Errorf("node %s is %s", n.Name, n.Status)
		case !hasRole(n.Roles, "worker"):
			return nil, fmt.Errorf("node %s does not run workloads (no worker role)", n.Name)
		case n.NetIndex == nil:
			return nil, fmt.Errorf("node %s is not on the mesh", n.Name)
		}
		return n, nil
	}
	return nil, fmt.Errorf("node %q not found", want)
}

// startVolumeMove records a live move and runs it in the background.
func (s *Server) startVolumeMove(ctx context.Context, svc *store.Service, from, to string, retainDays int, by string) (*store.VolumeMove, error) {
	m, err := s.store.CreateVolumeMove(ctx, &store.VolumeMove{ServiceID: svc.ID, Kind: "move", FromNode: &from, ToNode: &to, RetainDays: retainDays, RequestedBy: by})
	if err != nil {
		return nil, err
	}
	go s.runVolumeMove(context.Background(), m.ID)
	return m, nil
}

// POST /api/v1/services/{id}/move {node, retain_days, confirm_downtime}
func (s *Server) handleStartVolumeMove(w http.ResponseWriter, r *http.Request) {
	svc, p := s.serviceFor(w, r, "admin")
	if svc == nil {
		return
	}
	var in struct {
		Node            string `json:"node"`
		RetainDays      int    `json:"retain_days"`
		ConfirmDowntime bool   `json:"confirm_downtime"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	ctx := r.Context()
	if why := s.movableReason(svc); why != "" {
		writeError(w, 409, why)
		return
	}
	if in.RetainDays < 0 || in.RetainDays > 90 {
		writeError(w, 400, "retain_days must be between 1 and 90 (default 7)")
		return
	}
	from := s.serviceNode(ctx, svc)
	if from == nil {
		writeError(w, 409, "the service has not been placed on a node yet")
		return
	}
	if !s.nodeAlive(ctx, from) {
		writeError(w, 409, "the service's node is not reachable: if it is not coming back, recover the service from a volume backup instead (POST /services/{id}/recover)")
		return
	}
	target, err := s.volumeMoveTarget(ctx, svc, *from, in.Node)
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	if !in.ConfirmDowntime {
		writeError(w, 400, "confirm_downtime is required: the service stops while its volumes are copied and checked, and starts on the new node after")
		return
	}
	u := currentUser(r)
	m, err := s.startVolumeMove(context.WithoutCancel(ctx), svc, *from, target.ID, in.RetainDays, u.Email)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, 409, "a move or restore of this service is already in progress")
		return
	}
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	s.store.Audit(ctx, &u.ID, &p.TeamID, "service.move", fmt.Sprintf("%s: %s -> %s", svc.Slug, s.nodeName(ctx, from), target.Name))
	writeJSON(w, 202, m)
}

type moveTargetOut struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	MemoryAvailableMB int    `json:"memory_available_mb"`
}

// GET /api/v1/services/{id}/move: the move in progress, the last ones, and (team admins) where it can go.
func (s *Server) handleVolumeMoveStatus(w http.ResponseWriter, r *http.Request) {
	svc, p := s.serviceFor(w, r, "viewer")
	if svc == nil {
		return
	}
	ctx := r.Context()
	hist, err := s.store.VolumeMoves(ctx, svc.ID, 10)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if hist == nil {
		hist = []store.VolumeMove{}
	}
	var active *store.VolumeMove
	if len(hist) > 0 && !store.VolumeMoveTerminal(hist[0].Phase) {
		active = &hist[0]
	}
	out := map[string]any{"active": active, "history": hist, "movable": s.movableReason(svc) == "", "reason": s.movableReason(svc)}
	node := s.serviceNode(ctx, svc)
	if node != nil {
		out["node"] = map[string]any{"id": *node, "name": s.nodeName(ctx, node), "online": s.nodeAlive(ctx, node)}
	}
	targets := []moveTargetOut{}
	if role, err := s.teamRole(r, p.TeamID); err == nil && roleRank[role] >= roleRank["admin"] {
		if nodes, err := s.store.Nodes(ctx); err == nil {
			for _, n := range nodes {
				if (node != nil && n.ID == *node) || n.Status != "online" || !hasRole(n.Roles, "worker") || n.NetIndex == nil {
					continue
				}
				targets = append(targets, moveTargetOut{ID: n.ID, Name: n.Name, MemoryAvailableMB: n.Usage.MemoryAvailableMB})
			}
		}
	}
	out["targets"] = targets
	writeJSON(w, 200, out)
}

// DELETE /api/v1/services/{id}/move: abort the move in progress (only before it switched over).
func (s *Server) handleAbortVolumeMove(w http.ResponseWriter, r *http.Request) {
	svc, p := s.serviceFor(w, r, "admin")
	if svc == nil {
		return
	}
	ctx := r.Context()
	m, err := s.store.ActiveVolumeMove(ctx, svc.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, 404, "no move in progress")
		return
	}
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	ok, err := s.store.RequestVolumeMoveAbort(ctx, m.ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if !ok {
		writeError(w, 409, "the service already switched to its new node: it finishes there, or goes back by itself if it does not start")
		return
	}
	u := currentUser(r)
	s.store.Audit(ctx, &u.ID, &p.TeamID, "service.move_abort", svc.Slug)
	writeJSON(w, 202, map[string]string{"status": "aborting"})
}

// latestOffsiteVolumeBackup is a service's newest completed volume backup with an off-site copy.
func (s *Server) latestOffsiteVolumeBackup(ctx context.Context, serviceID string) *store.VolumeBackup {
	bs, err := s.store.VolumeBackups(ctx, serviceID, 30)
	if err != nil {
		return nil
	}
	for i := range bs {
		if bs[i].Status == "completed" && bs[i].S3Key != "" {
			return &bs[i]
		}
	}
	return nil
}

// startVolumeRestore records a restore of svc on target from b (nil: empty volumes) and runs it.
func (s *Server) startVolumeRestore(ctx context.Context, svc *store.Service, oldNode string, target *store.Node, b *store.VolumeBackup, by string, run bool) (*store.VolumeMove, error) {
	m := &store.VolumeMove{ServiceID: svc.ID, Kind: "restore", FromNode: &oldNode, ToNode: &target.ID, RequestedBy: by}
	if b != nil {
		m.BackupID, m.RPOAt = &b.ID, &b.CreatedAt
	}
	m, err := s.store.CreateVolumeMove(ctx, m)
	if err != nil {
		return nil, err
	}
	if run {
		go s.runVolumeMove(context.Background(), m.ID)
	}
	return m, nil
}

// POST /api/v1/services/{id}/recover {backup_id, allow_empty, node}
// Restores a service whose node is offline on another node, from a volume backup (default: the
// newest), or with empty volumes when allow_empty is set. The data written after the backup is
// lost: the answer and the recorded restore carry the backup's time (rpo_at).
func (s *Server) handleRecoverService(w http.ResponseWriter, r *http.Request) {
	svc, p := s.serviceFor(w, r, "admin")
	if svc == nil {
		return
	}
	var in struct {
		BackupID   string `json:"backup_id"`
		AllowEmpty bool   `json:"allow_empty"`
		Node       string `json:"node"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	ctx := r.Context()
	old := s.serviceNode(ctx, svc)
	if old == nil || s.nodeAlive(ctx, old) {
		writeError(w, 409, "the service's node is still reachable: move the service instead (its data would be lost)")
		return
	}
	var b *store.VolumeBackup
	if len(svc.Spec.Volumes) > 0 && !in.AllowEmpty {
		if in.BackupID != "" {
			var err error
			b, err = s.store.VolumeBackupByID(ctx, in.BackupID)
			if err == nil && (b.ServiceID != svc.ID || b.Status != "completed" || b.S3Key == "") {
				b = nil
			}
		} else {
			b = s.latestOffsiteVolumeBackup(ctx, svc.ID)
		}
		if b == nil {
			writeError(w, 409, "no off-site volume backup to recover from: pass allow_empty to start it with empty volumes")
			return
		}
		if !s.storage().Enabled() {
			writeError(w, 409, "off-site storage is not configured")
			return
		}
	}
	node, err := s.volumeMoveTarget(ctx, svc, *old, in.Node)
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	u := currentUser(r)
	m, err := s.startVolumeRestore(context.WithoutCancel(ctx), svc, *old, node, b, u.Email, true)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, 409, "a move or restore of this service is already in progress")
		return
	}
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	what := "empty volumes"
	if b != nil {
		what = "volumes from " + b.CreatedAt.UTC().Format(time.RFC3339)
	}
	s.store.Audit(ctx, &u.ID, &p.TeamID, "service.recover", svc.Slug+" on "+node.Name+" with "+what)
	writeJSON(w, 202, map[string]any{"node": node.Name, "backup": b, "move": m, "rpo_at": m.RPOAt})
}

// POST /api/v1/nodes/{id}/recover {node, allow_empty, confirm_data_loss}
// Restores every service with volumes of a node that is not coming back on other nodes, each from
// its newest off-site volume backup, one after the other. Databases are listed, not restored:
// they have their own recovery (POST /databases/{id}/recover).
func (s *Server) handleRecoverNode(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	ctx := r.Context()
	n, err := s.store.NodeByID(ctx, r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "node not found")
		return
	}
	var in struct {
		Node            string `json:"node"`
		AllowEmpty      bool   `json:"allow_empty"`
		ConfirmDataLoss bool   `json:"confirm_data_loss"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if s.nodeAlive(ctx, &n.ID) {
		writeError(w, 409, "the node is still reachable: drain it with move_volumes instead (its data would be lost)")
		return
	}
	if !in.ConfirmDataLoss {
		writeError(w, 400, "confirm_data_loss is required: each service restarts from its latest backup, and what it wrote after that backup is lost")
		return
	}
	if !s.storage().Enabled() && !in.AllowEmpty {
		writeError(w, 409, "off-site storage is not configured, so there is no backup to restore from")
		return
	}
	type started struct {
		Service string     `json:"service"`
		ID      string     `json:"id"`
		Move    string     `json:"move"`
		Node    string     `json:"node"`
		RPOAt   *time.Time `json:"rpo_at"`
	}
	type skipped struct {
		Kind   string `json:"kind"`
		Name   string `json:"name"`
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	out := struct {
		Started []started `json:"started"`
		Skipped []skipped `json:"skipped"`
	}{Started: []started{}, Skipped: []skipped{}}
	u := currentUser(r)
	var ids []string
	for _, it := range s.stuckOnNode(ctx, n.ID) {
		if it.Kind != "service" {
			out.Skipped = append(out.Skipped, skipped{it.Kind, it.Name, it.ID, "databases are recovered with POST /databases/{id}/recover"})
			continue
		}
		svc, err := s.store.ServiceByID(ctx, it.ID)
		if err != nil {
			continue
		}
		b := s.latestOffsiteVolumeBackup(ctx, svc.ID)
		if b == nil && !in.AllowEmpty {
			out.Skipped = append(out.Skipped, skipped{it.Kind, it.Name, it.ID, "no off-site volume backup (allow_empty starts it with empty volumes)"})
			continue
		}
		target, err := s.volumeMoveTarget(ctx, svc, n.ID, in.Node)
		if err != nil {
			out.Skipped = append(out.Skipped, skipped{it.Kind, it.Name, it.ID, err.Error()})
			continue
		}
		m, err := s.startVolumeRestore(context.WithoutCancel(ctx), svc, n.ID, target, b, u.Email, false)
		if err != nil {
			reason := err.Error()
			if errors.Is(err, store.ErrConflict) {
				reason = "a move or restore is already in progress"
			}
			out.Skipped = append(out.Skipped, skipped{it.Kind, it.Name, it.ID, reason})
			continue
		}
		ids = append(ids, m.ID)
		out.Started = append(out.Started, started{svc.Slug, svc.ID, m.ID, target.Name, m.RPOAt})
		s.store.Audit(ctx, &u.ID, nil, "service.recover", fmt.Sprintf("%s on %s (node %s lost)", svc.Slug, target.Name, n.Name))
	}
	go func() {
		for _, id := range ids { // one at a time: each downloads and unpacks a whole backup
			s.runVolumeMove(context.Background(), id)
		}
	}()
	writeJSON(w, 202, out)
}

// drainStateful starts a move, one after the other, for every movable service with volumes on a
// node being drained. It returns the slugs it moves; the others stay (pinned_stateful).
func (s *Server) drainStateful(ctx context.Context, n *store.Node, by string) []string {
	moving := []string{}
	svcs, err := s.store.AllServices(ctx)
	if err != nil {
		return moving
	}
	var ids []string
	for i := range svcs {
		svc := &svcs[i]
		node := s.serviceNode(ctx, svc)
		if node == nil || *node != n.ID || len(svc.Spec.Volumes) == 0 {
			continue
		}
		if s.movableReason(svc) != "" {
			continue
		}
		target, err := s.volumeMoveTarget(ctx, svc, n.ID, "")
		if err != nil {
			continue
		}
		m, err := s.store.CreateVolumeMove(ctx, &store.VolumeMove{ServiceID: svc.ID, Kind: "move", FromNode: &n.ID, ToNode: &target.ID, RequestedBy: by})
		if err != nil {
			continue
		}
		ids = append(ids, m.ID)
		moving = append(moving, svc.Slug)
	}
	go func() {
		for _, id := range ids { // one at a time: one service down at a time, one copy on the mesh
			s.runVolumeMove(context.Background(), id)
		}
	}()
	return moving
}
