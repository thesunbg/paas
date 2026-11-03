package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseAndCompare(t *testing.T) {
	order := []string{"v0.9.0", "v1.0.0-alpha", "v1.0.0-alpha.1", "v1.0.0-alpha.beta", "v1.0.0-beta", "v1.0.0-beta.2", "v1.0.0-beta.11", "v1.0.0-rc.1", "v1.0.0", "1.0.1", "v1.2.0", "v1.10.0", "v2.0.0"}
	for i := 0; i+1 < len(order); i++ {
		a, ok1 := Parse(order[i])
		b, ok2 := Parse(order[i+1])
		if !ok1 || !ok2 {
			t.Fatalf("parse %s / %s", order[i], order[i+1])
		}
		if Compare(a, b) != -1 || Compare(b, a) != 1 || Compare(a, a) != 0 {
			t.Errorf("%s < %s expected", order[i], order[i+1])
		}
	}
	for _, bad := range []string{"", "dev", "1.2", "v1.2.x", "v01.2.3", "v1.2.3-", "abc1234", "v1.2.3-../../etc", "v1.2.3-a..b"} {
		if _, ok := Parse(bad); ok {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
	if v, ok := Parse("v1.2.3+build.5"); !ok || v.String() != "v1.2.3" {
		t.Errorf("build metadata: %v %v", v, ok)
	}
}

func TestDevBuildsAndNewer(t *testing.T) {
	for _, dev := range []string{"", "dev", "abc1234", "v1.2.3-4-gdeadbee", "v1.2.3-dirty", "v1.2.3-4-gdeadbee-dirty"} {
		if !IsDev(dev) {
			t.Errorf("IsDev(%q) = false", dev)
		}
		if Newer("v99.0.0", dev) {
			t.Errorf("a development build %q must never be told a release is newer", dev)
		}
	}
	for _, rel := range []string{"v1.2.3", "1.2.3", "v1.3.0-rc.1"} {
		if IsDev(rel) {
			t.Errorf("IsDev(%q) = true", rel)
		}
	}
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v1.3.0", "v1.2.9", true},
		{"v1.2.9", "v1.3.0", false},
		{"v1.3.0", "v1.3.0", false},
		{"v1.3.0", "v1.3.0-rc.2", true}, // the release after its candidate
		{"v1.3.0-rc.2", "v1.3.0-rc.1", true},
		{"v1.3.0-rc.1", "v1.2.0", true},
		{"garbage", "v1.2.0", false},
	}
	for _, c := range cases {
		if got := Newer(c.latest, c.current); got != c.want {
			t.Errorf("Newer(%s, %s) = %v", c.latest, c.current, got)
		}
	}
	if !Same("v1.2.3", "1.2.3") || Same("v1.2.3", "v1.2.4") || Same("", "") || !Same("dev", "dev") || Same("v1.2.3-4-gdeadbee", "v1.2.3") {
		t.Error("Same")
	}
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// releaseServer serves a GitHub-like release API with binaries and SHA256SUMS.
func releaseServer(t *testing.T, tag string, bins map[string][]byte, sums string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	release := func(w http.ResponseWriter, _ *http.Request) {
		assets := []map[string]string{}
		for name := range bins {
			assets = append(assets, map[string]string{"name": name, "browser_download_url": srv.URL + "/dl/" + name})
		}
		if sums != "" {
			assets = append(assets, map[string]string{"name": SumsFile, "browser_download_url": srv.URL + "/dl/" + SumsFile})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "name": "paas " + tag, "body": "## Changes\n- faster", "html_url": "https://example.test/r/" + tag,
			"published_at": "2026-10-30T10:00:00Z", "prerelease": false, "draft": false, "assets": assets})
	}
	mux.HandleFunc("/repos/x/paas/releases/latest", release)
	mux.HandleFunc("/repos/x/paas/releases/tags/{tag}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("tag") != tag {
			http.NotFound(w, r)
			return
		}
		release(w, r)
	})
	mux.HandleFunc("/dl/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == SumsFile {
			_, _ = w.Write([]byte(sums))
			return
		}
		b, ok := bins[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchReleaseAndChecksums(t *testing.T) {
	bin := []byte("new paasd amd64")
	sums := fmt.Sprintf("%s  paasd-linux-amd64\n%s *paas-linux-amd64\nnot a line\n", sum(bin), sum([]byte("cli")))
	srv := releaseServer(t, "v1.4.0", map[string][]byte{"paasd-linux-amd64": bin}, sums)
	ctx := context.Background()
	latest := srv.URL + "/repos/x/paas/releases/latest"
	r, err := FetchRelease(ctx, srv.Client(), latest)
	if err != nil {
		t.Fatal(err)
	}
	if r.Tag != "v1.4.0" || r.Name != "paas v1.4.0" || !strings.Contains(r.Notes, "faster") || r.PublishedAt.IsZero() || r.Assets["paasd-linux-amd64"] == "" {
		t.Fatalf("release = %+v", r)
	}
	if _, err := FetchRelease(ctx, srv.Client(), TagURL(latest, "v1.4.0")); err != nil {
		t.Fatalf("by tag: %v", err)
	}
	if _, err := FetchRelease(ctx, srv.Client(), TagURL(latest, "v9.9.9")); err == nil {
		t.Fatal("missing tag accepted")
	}
	got, err := r.Checksums(ctx, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if got["paasd-linux-amd64"] != sum(bin) || got["paas-linux-amd64"] != sum([]byte("cli")) || len(got) != 2 {
		t.Fatalf("sums = %v", got)
	}

	// No SHA256SUMS in the release: refused unless unverified downloads are allowed.
	srv2 := releaseServer(t, "v1.5.0", map[string][]byte{"paasd-linux-amd64": bin}, "")
	r2, err := FetchRelease(ctx, srv2.Client(), srv2.URL+"/repos/x/paas/releases/latest")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r2.Checksums(ctx, srv2.Client()); !errors.Is(err, ErrNoChecksum) {
		t.Fatalf("missing sums: %v", err)
	}
	if _, err := Expected(nil, "paasd-linux-amd64", false); !errors.Is(err, ErrNoChecksum) {
		t.Fatalf("Expected without sums: %v", err)
	}
	if s, err := Expected(nil, "paasd-linux-amd64", true); err != nil || s != "" {
		t.Fatalf("Expected unverified: %q %v", s, err)
	}
}

func TestDownloadVerifies(t *testing.T) {
	bin := []byte("the real binary")
	srv := releaseServer(t, "v1.4.0", map[string][]byte{"paasd-linux-amd64": bin}, "")
	url := srv.URL + "/dl/paasd-linux-amd64"
	dir := t.TempDir()
	ctx := context.Background()

	p, err := Download(ctx, srv.Client(), url, dir, sum(bin))
	if err != nil {
		t.Fatalf("good checksum: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != string(bin) {
		t.Fatal("content")
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm()&0o100 == 0 {
		t.Fatal("not executable")
	}
	os.Remove(p)

	if _, err := Download(ctx, srv.Client(), url, dir, sum([]byte("something else"))); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("bad checksum: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a refused download left files: %v", entries)
	}
	if _, err := Download(ctx, srv.Client(), srv.URL+"/dl/missing", dir, ""); err == nil {
		t.Fatal("404 accepted")
	}
}

func TestInstallKeepsPreviousAndRollback(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "paasd")
	if err := os.WriteFile(exe, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Rollback(exe); !errors.Is(err, ErrNoPrevious) {
		t.Fatalf("rollback without .prev: %v", err)
	}
	staged := filepath.Join(dir, ".staged")
	_ = os.WriteFile(staged, []byte("v2"), 0o755)
	if err := Install(exe, staged); err != nil {
		t.Fatal(err)
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	if read(exe) != "v2" || read(exe+PrevSuffix) != "v1" || !HasPrevious(exe) {
		t.Fatalf("after install: exe=%q prev=%q", read(exe), read(exe+PrevSuffix))
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatal("staged file still there")
	}
	// A second install keeps the binary it replaces (v2), not the older one.
	_ = os.WriteFile(staged, []byte("v3"), 0o755)
	if err := Install(exe, staged); err != nil {
		t.Fatal(err)
	}
	if read(exe) != "v3" || read(exe+PrevSuffix) != "v2" {
		t.Fatalf("after second install: exe=%q prev=%q", read(exe), read(exe+PrevSuffix))
	}
	if err := Rollback(exe); err != nil {
		t.Fatal(err)
	}
	if read(exe) != "v2" || HasPrevious(exe) {
		t.Fatalf("after rollback: exe=%q prev exists=%v", read(exe), HasPrevious(exe))
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
	if err := Install(exe, filepath.Join(t.TempDir(), "elsewhere")); err == nil {
		t.Fatal("staged file in another directory accepted")
	}
}

func TestSelfApplyAndGuard(t *testing.T) {
	newBin := []byte("paasd v2 for " + runtime.GOARCH)
	name := BinaryName(runtime.GOARCH)
	srv := releaseServer(t, "v2.0.0", map[string][]byte{name: newBin}, "")
	dir := t.TempDir()
	exe := filepath.Join(dir, "paasd")
	_ = os.WriteFile(exe, []byte("paasd v1"), 0o755)
	data := t.TempDir()
	var guard []string
	s := &Self{DataDir: data, Version: "v1.0.0", Client: srv.Client(), Exe: exe, systemdRun: func(args ...string) error { guard = args; return nil }}
	urls := map[string]string{runtime.GOARCH: srv.URL + "/dl/" + name}
	ctx := context.Background()

	if _, err := s.Apply(ctx, "v2.0.0", urls, nil, false, time.Minute); !errors.Is(err, ErrRejected) {
		t.Fatalf("no checksum: %v", err)
	}
	if _, err := s.Apply(ctx, "v2.0.0", urls, map[string]string{runtime.GOARCH: sum([]byte("other"))}, false, time.Minute); !errors.Is(err, ErrRejected) {
		t.Fatalf("bad checksum: %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "paasd v1" || HasPrevious(exe) {
		t.Fatal("a refused update changed the binary")
	}
	sums := map[string]string{runtime.GOARCH: sum(newBin)}
	restart, err := s.Apply(ctx, "v2.0.0", urls, sums, false, 12*time.Minute)
	if err != nil || !restart {
		t.Fatalf("apply: %v %v", restart, err)
	}
	if b, _ := os.ReadFile(exe); string(b) != string(newBin) {
		t.Fatal("binary not replaced")
	}
	if b, _ := os.ReadFile(exe + PrevSuffix); string(b) != "paasd v1" {
		t.Fatal("previous binary not kept")
	}
	if len(guard) == 0 || !strings.Contains(strings.Join(guard, " "), "--on-active=720s") {
		t.Fatalf("guard args = %v", guard)
	}
	script := guard[len(guard)-1]
	if !strings.Contains(script, "mv -f '"+exe+".prev' '"+exe+"'") || !strings.Contains(script, "systemctl restart paasd.service") {
		t.Fatalf("guard script:\n%s", script)
	}
	// Asked again before the restart: must not install over .prev.
	if restart, err := s.Apply(ctx, "v2.0.0", urls, sums, false, time.Minute); err != nil || !restart {
		t.Fatalf("repeat: %v %v", restart, err)
	}
	if b, _ := os.ReadFile(exe + PrevSuffix); string(b) != "paasd v1" {
		t.Fatal("a repeated command overwrote the previous binary")
	}
	// Already running that version: nothing to do.
	if restart, err := (&Self{Version: "2.0.0", Exe: exe}).Apply(ctx, "v2.0.0", urls, sums, false, 0); err != nil || restart {
		t.Fatalf("same version: %v %v", restart, err)
	}

	pending := filepath.Join(data, pendingFile)
	Confirm(data, "v1.0.0") // the old binary (after a rollback) does not confirm the new version
	if _, err := os.Stat(pending); err != nil {
		t.Fatal("confirmed by the wrong version")
	}
	Confirm(data, "v2.0.0")
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Fatal("pending update not confirmed")
	}

	if err := s.Rollback(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "paasd v1" {
		t.Fatal("rollback did not restore the previous binary")
	}
}

// fakeCluster is an in-memory cluster: machines change version when told to.
type fakeCluster struct {
	now      time.Time
	version  string
	machines map[string]*fakeMachine
	started  []string
	rolled   []string
	backups  int
	backErr  error
}

type fakeMachine struct {
	version, prev       string
	online, cordoned    bool
	broken              bool // the new version never comes up
	unreachable, reject bool
	gone                bool
}

func (f *fakeCluster) Backup(context.Context) (string, error) {
	f.backups++
	return "state-1", f.backErr
}
func (f *fakeCluster) Now() time.Time { return f.now }
func (f *fakeCluster) Observe(_ context.Context, st Step) Observation {
	m := f.machines[st.Name]
	if m == nil || m.gone {
		return Observation{}
	}
	return Observation{Found: true, Online: m.online, Cordoned: m.cordoned, Version: m.version}
}
func (f *fakeCluster) Start(_ context.Context, st Step) error {
	m := f.machines[st.Name]
	if m.reject {
		return fmt.Errorf("%w: bad checksum", ErrRejected)
	}
	if m.unreachable {
		return errors.New("no responders")
	}
	f.started = append(f.started, st.Name)
	m.prev = m.version
	if m.broken {
		m.online = false
	} else {
		m.version = f.version
	}
	return nil
}
func (f *fakeCluster) Rollback(_ context.Context, st Step) error {
	m := f.machines[st.Name]
	f.rolled = append(f.rolled, st.Name)
	m.version, m.online = m.prev, true
	return nil
}

func newFake(version string) *fakeCluster {
	return &fakeCluster{now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), version: version, machines: map[string]*fakeMachine{}}
}

func (f *fakeCluster) add(name, version string) *fakeMachine {
	m := &fakeMachine{version: version, online: true}
	f.machines[name] = m
	return m
}

// run ticks until the rollout stops changing or finishes, persisting it through JSON
// between ticks like the control plane does.
func run(t *testing.T, r *Rollout, f *fakeCluster, maxTicks int) *Rollout {
	t.Helper()
	for i := 0; i < maxTicks && r.Status == StatusRunning; i++ {
		r.Tick(context.Background(), f, 10*time.Minute)
		b, _ := json.Marshal(r)
		var next Rollout
		if err := json.Unmarshal(b, &next); err != nil {
			t.Fatal(err)
		}
		r = &next
		f.now = f.now.Add(time.Minute)
	}
	return r
}

func newRollout(f *fakeCluster, cps []Machine, nodes []Machine) *Rollout {
	return &Rollout{Version: f.version, Status: StatusRunning, Steps: Plan(cps, nodes)}
}

func TestRolloutOrderAndSkips(t *testing.T) {
	f := newFake("v2.0.0")
	for _, n := range []string{"cp-b", "cp-a", "cp-c"} {
		f.add(n, "v1.0.0")
	}
	f.add("worker-2", "v1.0.0")
	f.add("edge-1", "v1.0.0")
	f.add("worker-1", "v1.0.0").online = false
	f.add("build-1", "v1.0.0").cordoned = true
	f.add("worker-3", "v2.0.0")
	cps := []Machine{{Name: "cp-b", Leader: true}, {Name: "cp-c"}, {Name: "cp-a"}}
	nodes := []Machine{{Name: "worker-2"}, {Name: "worker-1"}, {Name: "edge-1", Roles: []string{"edge"}}, {Name: "build-1"}, {Name: "worker-3"}}
	r := run(t, newRollout(f, cps, nodes), f, 100)
	if r.Status != StatusDone {
		t.Fatalf("status = %s (%s)", r.Status, r.Message)
	}
	want := []string{"cp-a", "cp-c", "cp-b", "edge-1", "worker-2"}
	if strings.Join(f.started, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v (non-leaders, leader, then nodes one at a time)", f.started, want)
	}
	if f.backups != 1 || r.Steps[0].Kind != KindBackup || r.Steps[0].Status != StepDone {
		t.Fatalf("backup step: %+v (%d backups)", r.Steps[0], f.backups)
	}
	status := map[string]string{}
	for _, st := range r.Steps {
		status[st.Name] = st.Status
	}
	if status["worker-1"] != StepSkipped || status["build-1"] != StepSkipped || status["worker-3"] != StepDone {
		t.Fatalf("skips: %v", status)
	}
}

func TestRolloutFailureRollsBackAndStops(t *testing.T) {
	f := newFake("v2.0.0")
	f.add("cp-1", "v1.0.0")
	f.add("node-a", "v1.0.0")
	f.add("node-b", "v1.0.0").broken = true
	f.add("node-c", "v1.0.0")
	r := run(t, newRollout(f, []Machine{{Name: "cp-1", Leader: true}}, []Machine{{Name: "node-a"}, {Name: "node-b"}, {Name: "node-c"}}), f, 100)
	if r.Status != StatusFailed || !strings.Contains(r.Message, "failed at node node-b, rolled back to v1.0.0") {
		t.Fatalf("status = %s: %s", r.Status, r.Message)
	}
	if strings.Join(f.rolled, ",") != "node-b" || f.machines["node-c"].version != "v1.0.0" {
		t.Fatalf("rolled = %v, node-c = %s (must not be touched)", f.rolled, f.machines["node-c"].version)
	}
	if !r.HoldsNodes() {
		t.Fatal("a failed rollout must keep nodes from following the control plane on their own")
	}
	// A rejected command (bad checksum) fails at once without a rollback.
	f2 := newFake("v2.0.0")
	f2.add("cp-1", "v1.0.0").reject = true
	r2 := run(t, newRollout(f2, []Machine{{Name: "cp-1"}}, nil), f2, 10)
	if r2.Status != StatusFailed || len(f2.rolled) != 0 || !strings.Contains(r2.Message, "bad checksum") {
		t.Fatalf("rejected: %s %s %v", r2.Status, r2.Message, f2.rolled)
	}
	// A failed backup: nothing is touched.
	f3 := newFake("v2.0.0")
	f3.add("cp-1", "v1.0.0")
	f3.backErr = errors.New("pg_dump missing")
	r3 := run(t, newRollout(f3, []Machine{{Name: "cp-1"}}, nil), f3, 10)
	if r3.Status != StatusFailed || len(f3.started) != 0 {
		t.Fatalf("backup failure: %s %v", r3.Status, f3.started)
	}
}

func TestRolloutRetriesUnreachableThenTimesOut(t *testing.T) {
	f := newFake("v2.0.0")
	m := f.add("cp-1", "v1.0.0")
	m.unreachable = true
	r := newRollout(f, []Machine{{Name: "cp-1"}}, nil)
	r = run(t, r, f, 5)
	if r.Status != StatusRunning || r.Steps[1].Status != StepStarting {
		t.Fatalf("should keep retrying: %s %s", r.Status, r.Steps[1].Status)
	}
	m.unreachable = false
	r = run(t, r, f, 10)
	if r.Status != StatusDone {
		t.Fatalf("after it became reachable: %s %s", r.Status, r.Message)
	}
	f2 := newFake("v2.0.0")
	f2.add("cp-1", "v1.0.0").unreachable = true
	r2 := run(t, newRollout(f2, []Machine{{Name: "cp-1"}}, nil), f2, 30)
	if r2.Status != StatusFailed || !strings.Contains(r2.Message, "was not changed") {
		t.Fatalf("timeout: %s %s", r2.Status, r2.Message)
	}
}

// The leader updates itself: it persists the step, sends the command and dies before
// recording that it was accepted. A new leader, from the database alone, carries on.
func TestRolloutResumesAfterLeaderChange(t *testing.T) {
	f := newFake("v2.0.0")
	f.add("cp-1", "v1.0.0")
	f.add("cp-2", "v1.0.0")
	f.add("node-a", "v1.0.0")
	r := newRollout(f, []Machine{{Name: "cp-1", Leader: true}, {Name: "cp-2"}}, []Machine{{Name: "node-a"}})
	ctx := context.Background()
	for r.Steps[2].Status != StepStarting { // backup, cp-2, then cp-1 (the leader) is decided
		r.Tick(ctx, f, 10*time.Minute)
	}
	saved, _ := json.Marshal(r)
	// The old leader sends the command (the machine updates and restarts) but its own
	// write of "updating" is lost with it.
	_ = f.Start(ctx, r.Steps[2])
	var resumed Rollout
	_ = json.Unmarshal(saved, &resumed)
	out := run(t, &resumed, f, 20)
	if out.Status != StatusDone {
		t.Fatalf("resumed rollout: %s %s", out.Status, out.Message)
	}
	if n := strings.Count(strings.Join(f.started, ","), "cp-1"); n != 1 {
		t.Fatalf("cp-1 started %d times: %v", n, f.started)
	}
	if f.machines["node-a"].version != "v2.0.0" {
		t.Fatal("node not updated after the leader change")
	}
}

func TestRolloutPauseResumeAbort(t *testing.T) {
	f := newFake("v2.0.0")
	f.add("cp-1", "v1.0.0")
	f.add("node-a", "v1.0.0")
	f.add("node-b", "v1.0.0")
	r := newRollout(f, []Machine{{Name: "cp-1"}}, []Machine{{Name: "node-a"}, {Name: "node-b"}})
	ctx := context.Background()
	for len(f.started) < 1 {
		r.Tick(ctx, f, time.Minute)
	}
	if err := r.Pause(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if r.Tick(ctx, f, time.Minute) {
			t.Fatal("a paused rollout changed")
		}
	}
	if len(f.started) != 1 {
		t.Fatalf("paused rollout started more machines: %v", f.started)
	}
	if err := r.Pause(); err == nil {
		t.Fatal("pausing twice")
	}
	if err := r.Resume(); err != nil {
		t.Fatal(err)
	}
	for len(f.started) < 2 {
		r.Tick(ctx, f, time.Minute)
	}
	if err := r.Abort(f.now); err != nil || r.Status != StatusAborted || r.Active() {
		t.Fatalf("abort: %v %s", err, r.Status)
	}
	for i := 0; i < 10; i++ {
		r.Tick(ctx, f, time.Minute)
	}
	if len(f.started) != 2 || f.machines["node-b"].version != "v1.0.0" {
		t.Fatalf("aborted rollout went on: %v", f.started)
	}
	if err := r.Resume(); err == nil {
		t.Fatal("resumed an aborted rollout")
	}
}
