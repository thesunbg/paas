package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/bus"
	"github.com/thesunbg/paas/internal/network"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/update"
)

func hexSum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// fakeReleases is a GitHub-like release API: tag -> binary content, with or without SHA256SUMS.
type fakeReleases struct {
	srv      *httptest.Server
	latest   string
	bins     map[string][]byte // asset name -> content (same for every tag)
	withSums bool
	badSum   bool // SHA256SUMS lists a wrong checksum
}

func newFakeReleases(t *testing.T, latest string) *fakeReleases {
	f := &fakeReleases{latest: latest, withSums: true, bins: map[string][]byte{
		"paasd-linux-amd64": []byte("paasd amd64 " + latest), "paasd-linux-arm64": []byte("paasd arm64 " + latest)}}
	mux := http.NewServeMux()
	release := func(w http.ResponseWriter, tag string) {
		assets := []map[string]string{}
		for name := range f.bins {
			assets = append(assets, map[string]string{"name": name, "browser_download_url": f.srv.URL + "/dl/" + name})
		}
		if f.withSums {
			assets = append(assets, map[string]string{"name": "SHA256SUMS", "browser_download_url": f.srv.URL + "/dl/SHA256SUMS"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "name": "paas " + tag, "body": "Release notes of " + tag,
			"html_url": "https://github.test/r/" + tag, "published_at": "2026-11-01T00:00:00Z", "assets": assets})
	}
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, _ *http.Request) { release(w, f.latest) })
	mux.HandleFunc("/releases/tags/{tag}", func(w http.ResponseWriter, r *http.Request) { release(w, r.PathValue("tag")) })
	mux.HandleFunc("/dl/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "SHA256SUMS" {
			for n, b := range f.bins {
				sum := hexSum(b)
				if f.badSum {
					sum = hexSum([]byte("tampered"))
				}
				fmt.Fprintf(w, "%s  %s\n", sum, n)
			}
			return
		}
		_, _ = w.Write(f.bins[name])
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func setupUpdateServer(t *testing.T, latest string) (*Server, http.Handler, *fakeReleases, string, string) {
	t.Helper()
	s := newTestServerBus(t)
	f := newFakeReleases(t, latest)
	s.cfg.UpdateURL = f.srv.URL + "/releases/latest"
	s.cfg.UpdateCheck = true
	s.cfg.PublicURL = "https://paas.test"
	s.cfg.DataDir = t.TempDir()
	s.cfg.ServerName = "cp-1"
	s.ver = "v1.0.0"
	h := s.routes()
	mkUser(t, s, "admin@example.com", "password1", true)
	mkUser(t, s, "dev@example.com", "password1", false)
	_, admin := login(t, h, "admin@example.com", "password1", "198.51.100.1")
	_, dev := login(t, h, "dev@example.com", "password1", "198.51.100.2")
	return s, h, f, admin, dev
}

// fakeMachines plays control planes and nodes on the bus: told to update, they answer
// and come back reporting the version (unless broken); told to roll back, the old one.
type fakeMachines struct {
	t        *testing.T
	s        *Server
	mu       sync.Mutex
	order    []string
	requests map[string]proto.UpdateBinaryRequest
	broken   map[string]bool
	rolled   []string
	nodeIDs  map[string]string // name -> node id
}

func newFakeMachines(t *testing.T, s *Server) *fakeMachines {
	return &fakeMachines{t: t, s: s, requests: map[string]proto.UpdateBinaryRequest{}, broken: map[string]bool{}, nodeIDs: map[string]string{}}
}

func (fm *fakeMachines) record(name string, req *proto.UpdateBinaryRequest) bool {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	fm.order = append(fm.order, name)
	fm.requests[name] = *req
	return fm.broken[name]
}

func (fm *fakeMachines) controlPlane(name, version string, leader bool) {
	ctx := context.Background()
	s := fm.s
	report := func(v string) {
		if err := s.store.ReportControlPlane(ctx, name, v, "amd64", nil, time.Now()); err != nil {
			fm.t.Error(err)
		}
	}
	report(version)
	if leader {
		_ = s.store.MarkControlPlaneLeader(ctx, name, 7)
	}
	_, err := bus.Handle(s.nc, proto.SubjCPUpdate(name), func(_ context.Context, req *proto.UpdateBinaryRequest) *proto.UpdateBinaryReply {
		if !fm.record(name, req) {
			go report(req.Version)
		}
		return &proto.UpdateBinaryReply{}
	})
	if err != nil {
		fm.t.Fatal(err)
	}
	_, _ = bus.Handle(s.nc, proto.SubjCPRollback(name), func(_ context.Context, _ *struct{}) *proto.UpdateBinaryReply {
		fm.mu.Lock()
		fm.rolled = append(fm.rolled, name)
		fm.mu.Unlock()
		report(version)
		return &proto.UpdateBinaryReply{}
	})
	_ = s.nc.Flush()
}

func (fm *fakeMachines) node(name, version string, roles ...string) string {
	ctx := context.Background()
	s := fm.s
	n, err := s.store.EnsureLocalNode(ctx, name, roles, network.MaxNodes(s.clusterBase))
	if err != nil {
		fm.t.Fatal(err)
	}
	register := func(v string) {
		if _, err := s.store.UpdateNodeRegistration(ctx, n.ID, &proto.RegisterRequest{Name: name, Address: "10.0.0.1", Version: v,
			Capabilities: map[string]string{"arch": "amd64"}}, network.MaxNodes(s.clusterBase)); err != nil {
			fm.t.Error(err)
		}
	}
	register(version)
	fm.nodeIDs[name] = n.ID
	_, _ = bus.Handle(s.nc, proto.AgentSubject(n.ID, proto.CmdUpdateBinary), func(_ context.Context, req *proto.UpdateBinaryRequest) *proto.UpdateBinaryReply {
		if !fm.record(name, req) {
			go register(req.Version)
		}
		return &proto.UpdateBinaryReply{}
	})
	_, _ = bus.Handle(s.nc, proto.AgentSubject(n.ID, proto.CmdRollbackBinary), func(_ context.Context, _ *struct{}) *proto.UpdateBinaryReply {
		fm.mu.Lock()
		fm.rolled = append(fm.rolled, name)
		fm.mu.Unlock()
		register(version)
		return &proto.UpdateBinaryReply{}
	})
	_ = s.nc.Flush()
	return n.ID
}

// tickUntil runs the leader's rollout ticks until cond holds.
func tickUntil(t *testing.T, s *Server, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			r, _ := s.store.LatestRollout(context.Background())
			b, _ := json.MarshalIndent(r, "", "  ")
			t.Fatalf("timed out waiting for %s; rollout:\n%s", what, b)
		}
		s.rolloutTick(context.Background())
		time.Sleep(20 * time.Millisecond)
	}
}

func rolloutStatus(t *testing.T, s *Server) (string, map[string]string) {
	r, err := s.store.LatestRollout(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	steps := map[string]string{}
	for _, st := range r.Steps {
		steps[st.Name] = st.Status
	}
	return r.Status, steps
}

func TestUpdateAPIAdminOnly(t *testing.T) {
	_, h, _, _, dev := setupUpdateServer(t, "v2.0.0")
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/admin/update"}, {"POST", "/api/v1/admin/update/check"}, {"POST", "/api/v1/admin/update"},
		{"POST", "/api/v1/admin/update/pause"}, {"POST", "/api/v1/admin/update/resume"}, {"POST", "/api/v1/admin/update/abort"},
		{"POST", "/api/v1/nodes/00000000-0000-0000-0000-000000000000/rollback-binary"},
	} {
		if w := do(t, h, c.method, c.path, dev, map[string]any{"confirm": true}); w.Code != 403 {
			t.Errorf("%s %s as non-admin = %d", c.method, c.path, w.Code)
		}
	}
}

func TestUpdateCheckAndStatus(t *testing.T) {
	s, h, _, admin, _ := setupUpdateServer(t, "v2.0.0")
	fm := newFakeMachines(t, s)
	fm.controlPlane("cp-1", "v1.0.0", true)
	fm.controlPlane("cp-2", "v1.0.0", false)
	fm.node("worker-1", "v1.0.0", "worker")

	w := do(t, h, "GET", "/api/v1/admin/update", admin, nil)
	if w.Code != 200 {
		t.Fatalf("status = %d %s", w.Code, w.Body)
	}
	var st updateStatus
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if st.Latest != nil || st.UpdateAvailable || st.Version != "v1.0.0" || len(st.ControlPlanes) != 2 || len(st.Nodes) != 1 {
		t.Fatalf("before check: %+v", st)
	}
	if !st.ControlPlanes[0].Leader || st.ControlPlanes[1].Leader || !st.ControlPlanes[0].Online {
		t.Fatalf("leader/online: %+v", st.ControlPlanes)
	}
	w = do(t, h, "POST", "/api/v1/admin/update/check", admin, nil)
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if w.Code != 200 || st.Latest == nil || st.Latest.Tag != "v2.0.0" || !st.UpdateAvailable || st.CheckedAt == nil || !strings.Contains(st.Latest.Notes, "Release notes") {
		t.Fatalf("after check = %d %+v", w.Code, st)
	}
	if v, _ := s.store.GetSetting(context.Background(), settingUpdateNotified); v != "v2.0.0" {
		t.Fatalf("update.available recorded for %q", v)
	}
	// A development build is never told a release is newer.
	s.ver = "v1.0.0-3-gdeadbee"
	s.checkForUpdate(context.Background())
	st2, _ := s.updateStatus(context.Background())
	if !st2.Development {
		t.Fatal("dev build not reported")
	}
	// (cp-1 and cp-2 still run v1.0.0, so the cluster as a whole has an update.)
	if !st2.UpdateAvailable {
		t.Fatal("control planes on a release should still see the update")
	}
}

func TestStartUpdateValidation(t *testing.T) {
	s, h, f, admin, _ := setupUpdateServer(t, "v2.0.0")
	fm := newFakeMachines(t, s)
	fm.controlPlane("cp-1", "v1.0.0", true)

	if w := do(t, h, "POST", "/api/v1/admin/update", admin, map[string]any{}); w.Code != 400 {
		t.Fatalf("without confirm = %d", w.Code)
	}
	f.withSums = false
	if w := do(t, h, "POST", "/api/v1/admin/update", admin, map[string]any{"confirm": true}); w.Code != 400 || !strings.Contains(w.Body.String(), "SHA256SUMS") {
		t.Fatalf("missing checksums = %d %s", w.Code, w.Body)
	}
	f.withSums = true
	if w := do(t, h, "POST", "/api/v1/admin/update", admin, map[string]any{"confirm": true, "version": "v0.9.0"}); w.Code != 400 || !strings.Contains(w.Body.String(), "downgrade") {
		t.Fatalf("downgrade = %d %s", w.Code, w.Body)
	}
	delete(f.bins, "paasd-linux-amd64")
	if w := do(t, h, "POST", "/api/v1/admin/update", admin, map[string]any{"confirm": true}); w.Code != 400 || !strings.Contains(w.Body.String(), "paasd-linux-amd64") {
		t.Fatalf("missing arch = %d %s", w.Code, w.Body)
	}
	f.bins["paasd-linux-amd64"] = []byte("paasd amd64")
	w := do(t, h, "POST", "/api/v1/admin/update", admin, map[string]any{"confirm": true, "version": "v2.0.0"})
	if w.Code != 201 {
		t.Fatalf("start = %d %s", w.Code, w.Body)
	}
	var r update.Rollout
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	if r.Version != "v2.0.0" || r.Status != "running" || r.Checksums["amd64"] != hexSum(f.bins["paasd-linux-amd64"]) || len(r.Steps) != 2 {
		t.Fatalf("rollout = %+v", r)
	}
	if w := do(t, h, "POST", "/api/v1/admin/update", admin, map[string]any{"confirm": true}); w.Code != 409 {
		t.Fatalf("second update = %d", w.Code)
	}
	// Pause, resume, abort.
	for _, c := range []struct {
		action string
		code   int
		status string
	}{{"resume", 409, ""}, {"pause", 200, "paused"}, {"pause", 409, ""}, {"resume", 200, "running"}, {"abort", 200, "aborted"}, {"resume", 409, ""}, {"bogus", 404, ""}} {
		w := do(t, h, "POST", "/api/v1/admin/update/"+c.action, admin, nil)
		if w.Code != c.code {
			t.Fatalf("%s = %d %s", c.action, w.Code, w.Body)
		}
		if c.status != "" {
			if st, _ := rolloutStatus(t, s); st != c.status {
				t.Fatalf("after %s: %s", c.action, st)
			}
		}
	}
	// An aborted update lets a new one start.
	if w := do(t, h, "POST", "/api/v1/admin/update", admin, map[string]any{"confirm": true}); w.Code != 201 {
		t.Fatalf("after abort = %d %s", w.Code, w.Body)
	}
}

func TestRolloutOverBus(t *testing.T) {
	old := rolloutStepTimeout
	rolloutStepTimeout = 2 * time.Second
	t.Cleanup(func() { rolloutStepTimeout = old })
	s, h, f, admin, _ := setupUpdateServer(t, "v2.0.0")
	s.updateBackup = func(context.Context) (string, error) { return "state-test", nil }
	fm := newFakeMachines(t, s)
	fm.controlPlane("cp-1", "v1.0.0", true)
	fm.controlPlane("cp-2", "v1.0.0", false)
	fm.node("worker-b", "v1.0.0", "worker")
	fm.node("edge-a", "v1.0.0", "edge")
	off := fm.node("worker-c", "v1.0.0", "worker")
	if _, err := s.store.Q(context.Background()).Exec(context.Background(), `UPDATE nodes SET status='offline', last_seen_at=now()-interval '1 hour' WHERE id=$1`, off); err != nil {
		t.Fatal(err)
	}

	if w := do(t, h, "POST", "/api/v1/admin/update", admin, map[string]any{"confirm": true}); w.Code != 201 {
		t.Fatalf("start = %d %s", w.Code, w.Body)
	}
	// While it runs, nodes do not follow the control plane's binary on their own.
	if info := s.onBinary(context.Background(), nil); info.SHA256 != "" {
		t.Fatal("onBinary answered during a rollout")
	}
	// The leader (cp-1) goes last; when it has been told to update, another control
	// plane takes over and finishes from the database alone.
	tickUntil(t, s, "the leader's update", func() bool { _, st := rolloutStatus(t, s); return st["cp-1"] == "updating" })
	cfg2 := s.cfg
	cfg2.ServerName = "cp-2"
	s2 := &Server{cfg: cfg2, store: s.store, box: s.box, nc: s.nc, ver: "v2.0.0", updateBackup: s.updateBackup}
	tickUntil(t, s2, "the end", func() bool { st, _ := rolloutStatus(t, s); return st != "running" })

	status, steps := rolloutStatus(t, s)
	if status != "done" {
		t.Fatalf("status = %s %v", status, steps)
	}
	if got := strings.Join(fm.order, ","); got != "cp-2,cp-1,edge-a,worker-b" {
		t.Fatalf("order = %s", got)
	}
	if steps["worker-c"] != "skipped" || steps["platform state backup"] != "done" {
		t.Fatalf("steps = %v", steps)
	}
	cpReq, nodeReq := fm.requests["cp-2"], fm.requests["edge-a"]
	if cpReq.URLs["amd64"] != f.srv.URL+"/dl/paasd-linux-amd64" || cpReq.SHA256["amd64"] != hexSum(f.bins["paasd-linux-amd64"]) || cpReq.GuardSec <= 0 {
		t.Fatalf("control plane request = %+v", cpReq)
	}
	if nodeReq.URLs["arm64"] != "https://paas.test/download/release/v2.0.0/paasd-linux-arm64" || nodeReq.SHA256["arm64"] == "" {
		t.Fatalf("node request = %+v", nodeReq)
	}
	if info := s.onBinary(context.Background(), nil); info.SHA256 == "" && ownBinarySHA256() != "" {
		t.Fatal("nodes still held after the rollout finished")
	}
}

func TestRolloutFailureRollsBack(t *testing.T) {
	old := rolloutStepTimeout
	rolloutStepTimeout = 500 * time.Millisecond
	t.Cleanup(func() { rolloutStepTimeout = old })
	s, h, _, admin, _ := setupUpdateServer(t, "v2.0.0")
	s.updateBackup = func(context.Context) (string, error) { return "state-test", nil }
	fm := newFakeMachines(t, s)
	fm.controlPlane("cp-1", "v1.0.0", true)
	fm.node("node-a", "v1.0.0", "worker")
	bad := fm.node("node-b", "v1.0.0", "worker")
	fm.node("node-c", "v1.0.0", "worker")
	fm.broken["node-b"] = true

	if w := do(t, h, "POST", "/api/v1/admin/update", admin, map[string]any{"confirm": true}); w.Code != 201 {
		t.Fatalf("start = %d %s", w.Code, w.Body)
	}
	tickUntil(t, s, "the failure", func() bool { st, _ := rolloutStatus(t, s); return st != "running" })
	r, _ := s.store.LatestRollout(context.Background())
	if r.Status != "failed" || !strings.Contains(r.Message, "failed at node node-b, rolled back") {
		t.Fatalf("rollout = %s: %s", r.Status, r.Message)
	}
	if strings.Join(fm.rolled, ",") != "node-b" || strings.Contains(strings.Join(fm.order, ","), "node-c") {
		t.Fatalf("rolled = %v, order = %v", fm.rolled, fm.order)
	}
	if pinned, _ := s.store.NodeBinaryPinned(context.Background(), bad); !pinned {
		t.Fatal("rolled back node not pinned")
	}
	if !s.holdNodes(context.Background()) {
		t.Fatal("a failed rollout must hold nodes back")
	}
	// A new update retries: the rolled back node is unpinned when it is started.
	fm.broken["node-b"] = false
	if w := do(t, h, "POST", "/api/v1/admin/update", admin, map[string]any{"confirm": true}); w.Code != 201 {
		t.Fatalf("retry = %d %s", w.Code, w.Body)
	}
	tickUntil(t, s, "the retry", func() bool { st, _ := rolloutStatus(t, s); return st != "running" })
	if st, steps := rolloutStatus(t, s); st != "done" || steps["node-a"] != "done" || steps["node-c"] != "done" {
		t.Fatalf("retry: %s %v", st, steps)
	}
	if pinned, _ := s.store.NodeBinaryPinned(context.Background(), bad); pinned {
		t.Fatal("still pinned after a successful update")
	}
}

func TestRollbackNodeBinaryAPI(t *testing.T) {
	s, h, _, admin, _ := setupUpdateServer(t, "v2.0.0")
	if err := s.registerUpdateHandlers(); err != nil {
		t.Fatal(err)
	}
	fm := newFakeMachines(t, s)
	id := fm.node("node-a", "v2.0.0", "worker")
	if w := do(t, h, "POST", "/api/v1/nodes/"+id+"/rollback-binary", admin, nil); w.Code != 202 {
		t.Fatalf("rollback = %d %s", w.Code, w.Body)
	}
	if strings.Join(fm.rolled, ",") != "node-a" {
		t.Fatalf("rolled = %v", fm.rolled)
	}
	if pinned, _ := s.store.NodeBinaryPinned(context.Background(), id); !pinned {
		t.Fatal("not pinned")
	}
	// A pinned node gets no binary to follow on the per-node subject.
	var info proto.BinaryInfo
	if err := bus.Request(context.Background(), s.nc, proto.SubjNodeBinary(id), struct{}{}, &info); err != nil || info.SHA256 != "" {
		t.Fatalf("pinned node binary info = %+v %v", info, err)
	}
	if w := do(t, h, "POST", "/api/v1/nodes/00000000-0000-0000-0000-000000000000/rollback-binary", admin, nil); w.Code != 404 {
		t.Fatalf("unknown node = %d", w.Code)
	}
}

func TestReleaseDownloadForNodes(t *testing.T) {
	s, h, f, admin, _ := setupUpdateServer(t, "v2.0.0")
	fm := newFakeMachines(t, s)
	fm.controlPlane("cp-1", "v1.0.0", true)
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	if w := get("/download/release/v2.0.0/paasd-linux-arm64"); w.Code != 404 {
		t.Fatalf("without a rollout = %d", w.Code)
	}
	if w := do(t, h, "POST", "/api/v1/admin/update", admin, map[string]any{"confirm": true}); w.Code != 201 {
		t.Fatalf("start = %d %s", w.Code, w.Body)
	}
	arch := "arm64"
	if runtime.GOARCH == "arm64" {
		arch = "amd64"
	}
	name := "paasd-linux-" + arch
	if w := get("/download/release/v2.0.0/" + name); w.Code != 200 || w.Body.String() != string(f.bins[name]) {
		t.Fatalf("download = %d %q", w.Code, w.Body)
	}
	for _, p := range []string{"/download/release/v3.0.0/" + name, "/download/release/v2.0.0/paas-linux-amd64", "/download/release/v2.0.0/..%2fmaster.key"} {
		if w := get(p); w.Code != 404 {
			t.Fatalf("%s = %d", p, w.Code)
		}
	}
	// A tampered release file is never served (the cache is only written after verifying).
	s2, h2, f2, admin2, _ := setupUpdateServer(t, "v2.0.0")
	newFakeMachines(t, s2).controlPlane("cp-1", "v1.0.0", true)
	if w := do(t, h2, "POST", "/api/v1/admin/update", admin2, map[string]any{"confirm": true}); w.Code != 201 {
		t.Fatalf("start = %d %s", w.Code, w.Body)
	}
	f2.badSum = false
	f2.bins[name] = []byte("tampered after the rollout recorded its checksum")
	w := httptest.NewRecorder()
	h2.ServeHTTP(w, httptest.NewRequest("GET", "/download/release/v2.0.0/"+name, nil))
	if w.Code != 502 || !strings.Contains(w.Body.String(), "checksum mismatch") {
		t.Fatalf("tampered = %d %s", w.Code, w.Body)
	}
}
