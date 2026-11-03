package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/thesunbg/paas/internal/agent"
	"github.com/thesunbg/paas/internal/bus"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/update"
)

// Updates of paas itself (Settings → System → Updates, `paas update`).
//
// The leader checks the release endpoint (PAAS_UPDATE_URL, GitHub's latest release by
// default) every 6 hours and on demand. An administrator starts a rolling upgrade to a
// release; the leader drives it (internal/update.Rollout): state backup, control planes
// (non-leaders first, the leader last), then every other node one at a time. Binaries
// are verified against the release's SHA256SUMS before they are installed; control
// planes download theirs from the release, nodes from a control plane
// (GET /download/release/{version}/paasd-linux-<arch>, verified there and again on the node).
//
// Database migrations. A new binary applies its migrations when it starts, and nothing
// can undo them: rolling a machine back restores only its previous binary, never the
// schema (there are no down migrations, and a release is never installed over a newer
// one: downgrades are refused). That is safe because migrations are written additive
// (new tables, new columns with defaults, see internal/store/migrations) so the previous
// binary keeps working on the newer schema. The convention is not enforced by code (014
// dropped a sequence default, 017 deleted rows); a release whose migrations are not
// additive must say so in its notes, and the way back from it is the state backup taken
// as the rollout's first step (`paasd restore-state`).

const (
	settingUpdateCheck    = "update_check"    // last check: release, time, error
	settingUpdateNotified = "update_notified" // newest version update.available was sent for
	updateCheckEvery      = 6 * time.Hour
	updateTickEvery       = 5 * time.Second
	controlPlaneEvery     = 10 * time.Second
	controlPlaneOnline    = 30 * time.Second
)

// rolloutStepTimeout is N in "a machine that does not come back healthy with the new
// version within N is rolled back" (variable for tests).
var rolloutStepTimeout = 10 * time.Minute

var updateClient = &http.Client{Timeout: 30 * time.Second}

// version is the paasd version this control plane runs.
func (s *Server) version() string {
	if s.ver != "" {
		return s.ver
	}
	return agent.Version
}

type updateCheck struct {
	Release   *update.Release `json:"release,omitempty"`
	CheckedAt time.Time       `json:"checked_at"`
	Error     string          `json:"error,omitempty"`
}

func (s *Server) lastUpdateCheck(ctx context.Context) updateCheck {
	var c updateCheck
	if v, err := s.store.GetSetting(ctx, settingUpdateCheck); err == nil {
		_ = json.Unmarshal([]byte(v), &c)
	}
	return c
}

// checkForUpdate fetches the latest release and records it. update.available is sent
// once per version, and never to a development build.
func (s *Server) checkForUpdate(ctx context.Context) updateCheck {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := updateCheck{CheckedAt: time.Now().UTC()}
	rel, err := update.FetchRelease(ctx, updateClient, s.cfg.UpdateURL)
	if err != nil {
		c.Error = err.Error()
		c.Release = s.lastUpdateCheck(ctx).Release // keep showing what was known
	} else {
		c.Release = rel
	}
	b, _ := json.Marshal(c)
	_ = s.store.SetSetting(ctx, settingUpdateCheck, string(b))
	if rel != nil && update.Newer(rel.Tag, s.version()) {
		if last, _ := s.store.GetSetting(ctx, settingUpdateNotified); !update.Same(last, rel.Tag) {
			if s.store.SetSetting(ctx, settingUpdateNotified, rel.Tag) == nil {
				s.notifyAdmins(ctx, "update.available", fmt.Sprintf("paas %s is available (this cluster runs %s): Settings → System → Updates", rel.Tag, s.version()),
					map[string]any{"version": rel.Tag, "current": s.version(), "url": rel.URL})
			}
		}
	}
	return c
}

// reportControlPlaneLoop keeps this control plane's row in control_planes fresh: the
// version it runs is what a rollout waits for.
func (s *Server) reportControlPlaneLoop(ctx context.Context) {
	var nodeID *string
	if s.localNodeID != "" {
		nodeID = &s.localNodeID
	}
	for {
		if err := s.store.ReportControlPlane(ctx, s.cfg.ServerName, s.version(), runtime.GOARCH, nodeID, s.startedAt); err != nil && ctx.Err() == nil {
			slog.Warn("report control plane", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(controlPlaneEvery):
		}
	}
}

// updateLoop runs on the leader: periodic release checks and the rollout.
func (s *Server) updateLoop(ctx context.Context, epoch int64) {
	_ = s.store.MarkControlPlaneLeader(ctx, s.cfg.ServerName, epoch)
	check := time.NewTimer(time.Minute)
	if !s.cfg.UpdateCheck {
		check.Stop()
	}
	defer check.Stop()
	tick := time.NewTicker(updateTickEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-check.C:
			if c := s.checkForUpdate(ctx); c.Error != "" {
				slog.Warn("update check", "err", c.Error)
			}
			check.Reset(updateCheckEvery)
		case <-tick.C:
			s.rolloutTick(ctx)
		}
	}
}

// rolloutTick advances the current rollout by one step action and persists it.
func (s *Server) rolloutTick(ctx context.Context) {
	r, err := s.store.LatestRollout(ctx)
	if err != nil || r.Status != update.StatusRunning {
		return
	}
	before := r.Status
	if !r.Tick(ctx, &rolloutCluster{s: s, r: r}, rolloutStepTimeout) {
		return
	}
	ok, err := s.store.SaveRollout(ctx, r, before)
	if err != nil || !ok {
		return // deposed, or an administrator paused or aborted it meanwhile: re-read next tick
	}
	switch r.Status {
	case update.StatusFailed:
		slog.Error("update failed", "version", r.Version, "msg", r.Message)
		s.notifyAdmins(ctx, "update.failed", fmt.Sprintf("Update to paas %s stopped: %s", r.Version, r.Message), map[string]any{"version": r.Version})
	case update.StatusDone:
		slog.Info("update finished", "version", r.Version)
	}
}

// rolloutCluster is the cluster as a rollout sees it (update.Cluster).
type rolloutCluster struct {
	s *Server
	r *update.Rollout
}

func (c *rolloutCluster) Now() time.Time { return time.Now() }

func (c *rolloutCluster) Backup(ctx context.Context) (string, error) {
	if c.s.updateBackup != nil {
		return c.s.updateBackup(ctx)
	}
	info, err := c.s.backupState(ctx)
	return info.Name, err
}

func (c *rolloutCluster) Observe(ctx context.Context, st update.Step) update.Observation {
	if st.Kind == update.KindControlPlane {
		cps, err := c.s.store.ControlPlanes(ctx)
		if err != nil {
			return update.Observation{Found: true} // unknown: neither skip nor count as updated
		}
		for _, cp := range cps {
			if cp.Name == st.ID {
				return update.Observation{Found: true, Online: time.Since(cp.LastSeenAt) < controlPlaneOnline, Version: cp.Version}
			}
		}
		return update.Observation{}
	}
	n, err := c.s.store.NodeByID(ctx, st.ID)
	if errors.Is(err, store.ErrNotFound) {
		return update.Observation{}
	}
	if err != nil {
		return update.Observation{Found: true}
	}
	return update.Observation{Found: true, Online: n.Status != "offline" && time.Since(n.LastSeenAt) < controlPlaneOnline,
		Cordoned: n.Status == "cordoned", Version: n.Version}
}

func (c *rolloutCluster) Start(ctx context.Context, st update.Step) error {
	if st.Kind == update.KindControlPlane {
		return c.s.requestControlPlane(ctx, proto.SubjCPUpdate(st.ID), c.s.binaryRequest(c.r, false), 20*time.Minute)
	}
	// The rollout decides this node's binary from now on.
	if err := c.s.store.SetNodeBinaryPinned(ctx, st.ID, false); err != nil {
		return err
	}
	return c.s.requestNodeUpdate(ctx, st.ID, proto.CmdUpdateBinary, c.s.binaryRequest(c.r, true), 25*time.Minute)
}

func (c *rolloutCluster) Rollback(ctx context.Context, st update.Step) error {
	if st.Kind == update.KindControlPlane {
		return c.s.requestControlPlane(ctx, proto.SubjCPRollback(st.ID), struct{}{}, time.Minute)
	}
	_ = c.s.store.SetNodeBinaryPinned(ctx, st.ID, true)
	return c.s.requestNodeUpdate(ctx, st.ID, proto.CmdRollbackBinary, struct{}{}, time.Minute)
}

// binaryRequest names the version's binaries: control planes take them from the
// release, nodes from a control plane (they may not reach the internet).
func (s *Server) binaryRequest(r *update.Rollout, forNodes bool) proto.UpdateBinaryRequest {
	req := proto.UpdateBinaryRequest{Version: r.Version, URLs: map[string]string{}, SHA256: r.Checksums,
		AllowUnverified: s.cfg.UpdateAllowUnverified, GuardSec: int((rolloutStepTimeout + 2*time.Minute).Seconds())}
	if r.Release != nil {
		for name, url := range r.Release.Assets {
			arch, ok := strings.CutPrefix(name, "paasd-linux-")
			if !ok {
				continue
			}
			if forNodes {
				url = s.cfg.PublicURL + "/download/release/" + r.Version + "/" + name
			}
			req.URLs[arch] = url
		}
	}
	return req
}

func (s *Server) requestControlPlane(ctx context.Context, subject string, req any, timeout time.Duration) error {
	if err := s.store.CheckFence(ctx); err != nil {
		return err
	}
	var rep proto.UpdateBinaryReply
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := bus.Request(rctx, s.nc, subject, req, &rep); err != nil {
		return err
	}
	return replyError(&rep)
}

func (s *Server) requestNodeUpdate(ctx context.Context, nodeID, cmd string, req any, timeout time.Duration) error {
	var rep proto.UpdateBinaryReply
	if err := s.requestAgent(ctx, nodeID, cmd, req, &rep, timeout); err != nil {
		return err
	}
	return replyError(&rep)
}

func replyError(rep *proto.UpdateBinaryReply) error {
	switch {
	case rep.Error == "":
		return nil
	case rep.Rejected:
		return fmt.Errorf("%w: %s", update.ErrRejected, strings.TrimPrefix(rep.Error, update.ErrRejected.Error()+": "))
	default:
		return errors.New(rep.Error)
	}
}

// Bus side: this control plane updating or rolling back its own binary.

func (s *Server) registerUpdateHandlers() error {
	if _, err := bus.Handle(s.nc, proto.SubjCPUpdate(s.cfg.ServerName), s.onCPUpdate); err != nil {
		return err
	}
	if _, err := bus.Handle(s.nc, proto.SubjCPRollback(s.cfg.ServerName), s.onCPRollback); err != nil {
		return err
	}
	_, err := s.nc.QueueSubscribe(proto.SubjNodeBinaryAll, "control", s.onNodeBinary)
	return err
}

func (s *Server) self() *update.Self {
	return &update.Self{DataDir: s.cfg.DataDir, Version: s.version(), Client: &http.Client{Timeout: 15 * time.Minute}}
}

func (s *Server) onCPUpdate(ctx context.Context, req *proto.UpdateBinaryRequest) *proto.UpdateBinaryReply {
	restart, err := s.self().Apply(ctx, req.Version, req.URLs, req.SHA256, req.AllowUnverified, time.Duration(req.GuardSec)*time.Second)
	if err != nil {
		slog.Error("update this control plane", "version", req.Version, "err", err)
		return &proto.UpdateBinaryReply{Error: err.Error(), Rejected: errors.Is(err, update.ErrRejected)}
	}
	if restart {
		restartSoon()
	}
	return &proto.UpdateBinaryReply{}
}

func (s *Server) onCPRollback(_ context.Context, _ *struct{}) *proto.UpdateBinaryReply {
	if err := s.self().Rollback(); err != nil {
		return &proto.UpdateBinaryReply{Error: err.Error(), Rejected: errors.Is(err, update.ErrNoPrevious)}
	}
	restartSoon()
	return &proto.UpdateBinaryReply{}
}

// restartSoon restarts this process once the reply has gone out (update.Restart).
func restartSoon() {
	time.AfterFunc(time.Second, func() {
		exe, err := update.Executable()
		if err != nil {
			slog.Error("restart after update", "err", err)
			return
		}
		update.Restart(exe)
	})
}

// holdNodes: nodes must not follow the control plane's binary on their own (agent
// self-update) while a rollout is in progress or stopped at a failure.
func (s *Server) holdNodes(ctx context.Context) bool {
	r, err := s.store.LatestRollout(ctx)
	return err == nil && r.HoldsNodes()
}

// onNodeBinary answers paas.node.<id>.binary: the shared answer (onBinary), or none
// for a node whose binary was pinned by a rollback.
func (s *Server) onNodeBinary(m *nats.Msg) {
	id := proto.NodeIDFromSubject(m.Subject)
	if id == "" || m.Reply == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if pinned, err := s.store.NodeBinaryPinned(ctx, id); err != nil || pinned {
			_ = respondJSON(m, proto.BinaryInfo{})
			return
		}
		_ = respondJSON(m, s.onBinary(ctx, nil))
	}()
}

// GET /download/release/{version}/paasd-linux-<arch>: a binary of the version being
// rolled out, for nodes. Fetched once from the release into the data dir, verified
// against the rollout's checksums, served from there (the node verifies it again).
var releaseFileRe = regexp.MustCompile(`^paasd-linux-(amd64|arm64)$`)

func (s *Server) handleReleaseDownload(w http.ResponseWriter, r *http.Request) {
	version, file := r.PathValue("version"), r.PathValue("file")
	m := releaseFileRe.FindStringSubmatch(file)
	ro, err := s.store.LatestRollout(r.Context())
	if m == nil || err != nil || !update.Same(ro.Version, version) || ro.Release == nil {
		http.NotFound(w, r)
		return
	}
	arch, want := m[1], ro.Checksums[m[1]]
	if want == "" && !s.cfg.UpdateAllowUnverified {
		http.Error(w, "no checksum for "+file, http.StatusNotFound)
		return
	}
	if update.Same(version, s.version()) && arch == runtime.GOARCH {
		if exe, err := update.Executable(); err == nil {
			if sum, err := update.FileSHA256(exe); err == nil && (want == "" || sum == want) {
				w.Header().Set("Content-Type", "application/octet-stream")
				http.ServeFile(w, r, exe)
				return
			}
		}
	}
	dir := filepath.Join(s.cfg.DataDir, "updates", ro.Version)
	path := filepath.Join(dir, file)
	if _, err := os.Stat(path); err != nil {
		url := ro.Release.Assets[file]
		if url == "" {
			http.NotFound(w, r)
			return
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
		defer cancel()
		tmp, err := update.Download(ctx, &http.Client{}, url, dir, want)
		if err != nil {
			slog.Error("download release binary", "file", file, "err", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, path)
}

// REST API (platform administrators).

type cpVersion struct {
	store.ControlPlane
	Online bool `json:"online"`
}

type nodeVersion struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Arch         string   `json:"arch"`
	Roles        []string `json:"roles"`
	Status       string   `json:"status"`
	Online       bool     `json:"online"`
	Pinned       bool     `json:"pinned"`
	ControlPlane string   `json:"control_plane,omitempty"` // the control plane this node is part of
}

type updateStatus struct {
	Version         string          `json:"version"`
	Development     bool            `json:"development"`
	CheckEnabled    bool            `json:"check_enabled"`
	AllowUnverified bool            `json:"allow_unverified"`
	Latest          *update.Release `json:"latest"`
	CheckedAt       *time.Time      `json:"checked_at"`
	CheckError      string          `json:"check_error,omitempty"`
	UpdateAvailable bool            `json:"update_available"`
	ControlPlanes   []cpVersion     `json:"control_planes"`
	Nodes           []nodeVersion   `json:"nodes"`
	Rollout         *update.Rollout `json:"rollout"`
}

func (s *Server) updateStatus(ctx context.Context) (*updateStatus, error) {
	out := &updateStatus{Version: s.version(), Development: update.IsDev(s.version()), CheckEnabled: s.cfg.UpdateCheck,
		AllowUnverified: s.cfg.UpdateAllowUnverified, ControlPlanes: []cpVersion{}, Nodes: []nodeVersion{}}
	c := s.lastUpdateCheck(ctx)
	out.Latest, out.CheckError = c.Release, c.Error
	if !c.CheckedAt.IsZero() {
		out.CheckedAt = &c.CheckedAt
	}
	cps, err := s.store.ControlPlanes(ctx)
	if err != nil {
		return nil, err
	}
	cpOfNode := map[string]string{}
	for _, cp := range cps {
		out.ControlPlanes = append(out.ControlPlanes, cpVersion{ControlPlane: cp, Online: time.Since(cp.LastSeenAt) < controlPlaneOnline})
		if cp.NodeID != nil {
			cpOfNode[*cp.NodeID] = cp.Name
		}
		if out.Latest != nil && update.Newer(out.Latest.Tag, cp.Version) {
			out.UpdateAvailable = true
		}
	}
	nodes, err := s.store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	pinned, _ := s.store.PinnedNodes(ctx)
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, nodeVersion{ID: n.ID, Name: n.Name, Version: n.Version, Arch: n.Capabilities["arch"], Roles: n.Roles, Status: n.Status,
			Online: n.Status != "offline" && time.Since(n.LastSeenAt) < controlPlaneOnline, Pinned: pinned[n.ID], ControlPlane: cpOfNode[n.ID]})
	}
	if out.Latest != nil && update.Newer(out.Latest.Tag, s.version()) {
		out.UpdateAvailable = true
	}
	if r, err := s.store.LatestRollout(ctx); err == nil {
		out.Rollout = r
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return out, nil
}

// GET /api/v1/admin/update
func (s *Server) handleGetUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	st, err := s.updateStatus(r.Context())
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, st)
}

// POST /api/v1/admin/update/check
func (s *Server) handleCheckUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	s.checkForUpdate(r.Context())
	s.handleGetUpdate(w, r)
}

// POST /api/v1/admin/update {version, confirm: true}
func (s *Server) handleStartUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in struct {
		Version string `json:"version"` // empty: the latest release
		Confirm bool   `json:"confirm"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if !in.Confirm {
		writeError(w, 400, "updating restarts every control plane and node one after the other; send confirm: true")
		return
	}
	ctx := r.Context()
	if cur, err := s.store.LatestRollout(ctx); err == nil && cur.Active() {
		writeError(w, 409, "an update to "+cur.Version+" is already "+cur.Status)
		return
	}
	url := s.cfg.UpdateURL
	if in.Version != "" {
		if _, ok := update.Parse(in.Version); !ok {
			writeError(w, 400, "version must look like v1.2.3")
			return
		}
		url = update.TagURL(url, in.Version)
	}
	fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rel, err := update.FetchRelease(fctx, updateClient, url)
	if err != nil {
		writeError(w, 502, "release: "+err.Error())
		return
	}
	cps, err := s.store.ControlPlanes(ctx)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	nodes, err := s.store.Nodes(ctx)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	target, _ := update.Parse(rel.Tag)
	arches := map[string]bool{}
	cpNodes := map[string]bool{}
	var cpMachines, nodeMachines []update.Machine
	for _, cp := range cps {
		if time.Since(cp.LastSeenAt) > 24*time.Hour {
			continue // decommissioned
		}
		if v, ok := update.Parse(cp.Version); ok && !update.IsDev(cp.Version) && update.Compare(target, v) < 0 {
			writeError(w, 400, fmt.Sprintf("control plane %s runs %s, newer than %s: downgrades are not supported (database migrations only go forward)", cp.Name, cp.Version, rel.Tag))
			return
		}
		if cp.Arch != "" {
			arches[cp.Arch] = true
		}
		if cp.NodeID != nil {
			cpNodes[*cp.NodeID] = true
		}
		cpMachines = append(cpMachines, update.Machine{ID: cp.Name, Name: cp.Name, Leader: cp.Leader})
	}
	if len(cpMachines) == 0 {
		writeError(w, 409, "no control plane has reported its version yet; try again in a few seconds")
		return
	}
	for _, n := range nodes {
		if cpNodes[n.ID] {
			continue // updated with its control plane
		}
		if a := n.Capabilities["arch"]; a != "" {
			arches[a] = true
		}
		nodeMachines = append(nodeMachines, update.Machine{ID: n.ID, Name: n.Name, Roles: n.Roles})
	}
	sums, err := rel.Checksums(fctx, updateClient)
	if err != nil && !(errors.Is(err, update.ErrNoChecksum) && s.cfg.UpdateAllowUnverified) {
		if errors.Is(err, update.ErrNoChecksum) {
			writeError(w, 400, fmt.Sprintf("release %s publishes no %s: refusing to install binaries that cannot be verified (PAAS_UPDATE_ALLOW_UNVERIFIED=true skips the check, unsafe)", rel.Tag, update.SumsFile))
			return
		}
		writeError(w, 502, "checksums: "+err.Error())
		return
	}
	checksums := map[string]string{}
	for name := range rel.Assets {
		if arch, ok := strings.CutPrefix(name, "paasd-linux-"); ok && sums[name] != "" {
			checksums[arch] = sums[name]
		}
	}
	need := make([]string, 0, len(arches))
	for a := range arches {
		need = append(need, a)
	}
	sort.Strings(need)
	for _, a := range need {
		name := update.BinaryName(a)
		if rel.Assets[name] == "" {
			writeError(w, 400, fmt.Sprintf("release %s has no %s, which this cluster needs", rel.Tag, name))
			return
		}
		if _, err := update.Expected(sums, name, s.cfg.UpdateAllowUnverified); err != nil {
			writeError(w, 400, "release "+rel.Tag+": "+err.Error())
			return
		}
	}
	u := currentUser(r)
	ro, err := s.store.CreateRollout(ctx, &update.Rollout{Version: rel.Tag, Status: update.StatusRunning, Steps: update.Plan(cpMachines, nodeMachines),
		Release: rel, Checksums: checksums, RequestedBy: u.Email})
	if errors.Is(err, store.ErrRolloutActive) {
		writeError(w, 409, err.Error())
		return
	}
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	s.store.Audit(ctx, &u.ID, nil, "update.start", rel.Tag)
	writeJSON(w, 201, ro)
}

// POST /api/v1/admin/update/{action}: pause | resume | abort
func (s *Server) handleUpdateAction(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	action := r.PathValue("action")
	ro, err := s.store.LatestRollout(r.Context())
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, 404, "no update has been started")
		return
	}
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	before := ro.Status
	switch action {
	case "pause":
		err = ro.Pause()
	case "resume":
		err = ro.Resume()
	case "abort":
		err = ro.Abort(time.Now())
	default:
		writeError(w, 404, "unknown action "+action)
		return
	}
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	ok, err := s.store.SaveRollout(r.Context(), ro, before)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if !ok {
		writeError(w, 409, "the update changed meanwhile; reload and try again")
		return
	}
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, nil, "update."+action, ro.Version)
	writeJSON(w, 200, ro)
}

// POST /api/v1/nodes/{id}/rollback-binary: put the node's previous paasd binary back
// (a control plane's node rolls back that control plane). The node then keeps that
// binary (pinned) until a rollout updates it again.
func (s *Server) handleRollbackNodeBinary(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	ctx := r.Context()
	n, err := s.store.NodeByID(ctx, r.PathValue("id"))
	if err != nil {
		writeError(w, 404, "node not found")
		return
	}
	cps, err := s.store.ControlPlanes(ctx)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	cpName := ""
	for _, cp := range cps {
		if cp.NodeID != nil && *cp.NodeID == n.ID {
			cpName = cp.Name
		}
	}
	if cpName != "" {
		err = s.requestControlPlane(ctx, proto.SubjCPRollback(cpName), struct{}{}, time.Minute)
	} else {
		if err := s.store.SetNodeBinaryPinned(ctx, n.ID, true); err != nil {
			writeError(w, 500, err.Error())
			return
		}
		err = s.requestNodeUpdate(ctx, n.ID, proto.CmdRollbackBinary, struct{}{}, time.Minute)
	}
	switch {
	case errors.Is(err, update.ErrRejected):
		writeError(w, 409, err.Error())
		return
	case errors.Is(err, nats.ErrNoResponders) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, nats.ErrTimeout):
		writeError(w, 502, "the node did not answer (offline, or its paasd predates rollbacks): "+err.Error())
		return
	case err != nil:
		writeError(w, 502, err.Error())
		return
	}
	u := currentUser(r)
	s.store.Audit(ctx, &u.ID, nil, "node.rollback_binary", n.Name)
	writeJSON(w, 202, map[string]any{"status": "rolling back", "node": n.Name, "control_plane": cpName})
}
