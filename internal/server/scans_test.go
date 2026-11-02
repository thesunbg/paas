package server

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/thesunbg/paas/internal/bus"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/scan"
	"github.com/thesunbg/paas/internal/store"
)

type scanFixture struct {
	s       *Server
	ownerID string
	team    *store.Team
	project *store.Project
	svc     *store.Service
	buildID string
	mu      sync.Mutex
	reqs    []proto.ScanRequest
}

// newScanFixture seeds a team owned by owner@example.com, a project with an
// image service and an online build node.
func newScanFixture(t *testing.T) *scanFixture {
	t.Helper()
	s := newTestServerBus(t)
	ctx := context.Background()
	owner := mkUser(t, s, "owner@example.com", "password1", false)
	team, err := s.store.CreateTeam(ctx, "t", "t", owner.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	proj, err := s.store.CreateProject(ctx, team.ID, "p", "p")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := s.store.CreateService(ctx, &store.Service{
		ProjectID: proj.ID, Name: "web", Slug: "web", Type: model.TypeWeb,
		Source: model.ServiceSource{Kind: "image", Image: "nginx:1.25"},
	})
	if err != nil {
		t.Fatal(err)
	}
	bn, err := s.store.UpsertNodeLegacy(ctx, &proto.RegisterRequest{Name: "build", Address: "10.0.0.1", Roles: []string{"build"}, CPUCores: 2, MemoryMB: 2048, Capabilities: map[string]string{}}, 16)
	if err != nil {
		t.Fatal(err)
	}
	return &scanFixture{s: s, ownerID: owner.ID, team: team, project: proj, svc: svc, buildID: bn.ID}
}

// fakeScanAgent answers scan requests on the build node with rep.
func (f *scanFixture) fakeScanAgent(t *testing.T, rep proto.ScanReply) {
	t.Helper()
	_, err := bus.Handle(f.s.nc, proto.AgentSubject(f.buildID, proto.CmdScan), func(_ context.Context, r *proto.ScanRequest) *proto.ScanReply {
		f.mu.Lock()
		f.reqs = append(f.reqs, *r)
		f.mu.Unlock()
		out := rep
		return &out
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.nc.Flush(); err != nil {
		t.Fatal(err)
	}
}

func (f *scanFixture) setPolicy(t *testing.T, mode, threshold string) {
	t.Helper()
	if err := f.s.store.SetProjectScanPolicy(context.Background(), f.project.ID, scan.Policy{Mode: mode, Threshold: threshold}); err != nil {
		t.Fatal(err)
	}
}

func (f *scanFixture) deployment(t *testing.T) *store.Deployment {
	t.Helper()
	d, err := f.s.store.CreateDeployment(context.Background(), &store.Deployment{ServiceID: f.svc.ID, Status: model.DeployBuilding, Trigger: "test", Image: "docker.io/library/nginx:1.25"})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func fixtureResult(t *testing.T) *proto.ScanResult {
	t.Helper()
	b, err := os.ReadFile("../scan/testdata/trivy-node-alpine.json")
	if err != nil {
		t.Fatal(err)
	}
	res, err := scan.ParseTrivy(b)
	if err != nil {
		t.Fatal(err)
	}
	res.ScannerVersion = "0.65.0"
	return res
}

// warn: findings at or above the threshold are recorded and the deployment goes on.
func TestScanWarnRecordsAndDeploys(t *testing.T) {
	f := newScanFixture(t)
	f.fakeScanAgent(t, proto.ScanReply{Result: fixtureResult(t)})
	ctx := context.Background()
	d := f.deployment(t)
	if err := f.s.scanDeployment(ctx, f.svc, d, ""); err != nil {
		t.Fatalf("warn must not block: %v", err)
	}
	if len(f.reqs) != 1 || !f.reqs[0].Pull || f.reqs[0].Image != "docker.io/library/nginx:1.25" {
		t.Fatalf("scan requests = %+v (an image deployment is pulled by the build node)", f.reqs)
	}
	x, err := f.s.store.ImageScanForDeployment(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if x.Status != store.ScanScanned || x.Critical != 1 || x.High != 2 || x.Blocked || x.PolicyMode != "warn" || x.Threshold != "CRITICAL" || len(x.Findings) != 8 || x.ScannerVersion != "0.65.0" {
		t.Fatalf("scan = %+v", x)
	}
	if !strings.Contains(x.Message, "deploying anyway") {
		t.Fatalf("message = %q", x.Message)
	}
	if fresh, _ := f.s.store.DeploymentByID(ctx, d.ID); fresh.Image == "" {
		t.Fatal("warn must keep the image")
	}
}

// A built image is scanned on its build node, without a pull.
func TestScanBuiltImageOnBuildNode(t *testing.T) {
	f := newScanFixture(t)
	f.fakeScanAgent(t, proto.ScanReply{Result: &proto.ScanResult{Scanner: "trivy", Findings: []proto.ScanFinding{}}})
	d := f.deployment(t)
	if err := f.s.scanDeployment(context.Background(), f.svc, d, f.buildID); err != nil {
		t.Fatal(err)
	}
	if len(f.reqs) != 1 || f.reqs[0].Pull {
		t.Fatalf("scan requests = %+v", f.reqs)
	}
}

// block: the deployment fails before it goes live, and its image cannot be rolled back to.
func TestScanBlockFailsDeployment(t *testing.T) {
	f := newScanFixture(t)
	f.fakeScanAgent(t, proto.ScanReply{Result: fixtureResult(t)})
	f.setPolicy(t, scan.ModeBlock, "HIGH")
	ctx := context.Background()
	if _, err := f.s.store.CreateDeployment(ctx, &store.Deployment{ServiceID: f.svc.ID, Status: model.DeployQueued, Trigger: "test"}); err != nil {
		t.Fatal(err)
	}
	d, err := f.s.store.ClaimQueuedDeployment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.s.runDeployment(ctx, d)
	got, err := f.s.store.DeploymentByID(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.DeployFailed || !strings.Contains(got.Error, "blocked by the vulnerability policy: 3 finding(s) at or above HIGH") {
		t.Fatalf("deployment = %s %q", got.Status, got.Error)
	}
	if got.Image != "" {
		t.Fatalf("blocked deployment keeps image %q", got.Image)
	}
	if _, err := f.s.redeploy(ctx, f.svc, got, "rollback", nil); err == nil {
		t.Fatal("rollback to a blocked deployment must fail")
	}
	x, err := f.s.store.ImageScanForDeployment(ctx, d.ID)
	if err != nil || !x.Blocked || x.Image != "docker.io/library/nginx:1.25" {
		t.Fatalf("scan = %+v %v", x, err)
	}
	if inst := instanceCount(t, f.s, f.svc.ID); inst != 0 {
		t.Fatalf("instances = %d", inst)
	}
}

// No scanner on the build node: warn skips with a log line, block fails closed.
func TestScanMissingScanner(t *testing.T) {
	f := newScanFixture(t)
	f.fakeScanAgent(t, proto.ScanReply{Missing: true, Error: scan.ErrMissing.Error()})
	ctx := context.Background()

	d := f.deployment(t)
	if err := f.s.scanDeployment(ctx, f.svc, d, ""); err != nil {
		t.Fatalf("warn without scanner: %v", err)
	}
	x, err := f.s.store.ImageScanForDeployment(ctx, d.ID)
	if err != nil || x.Status != store.ScanUnavailable || x.Blocked || !strings.Contains(x.Message, "skipped: trivy is not installed on build node build") {
		t.Fatalf("warn scan = %+v %v", x, err)
	}

	f.setPolicy(t, scan.ModeBlock, "CRITICAL")
	d2 := f.deployment(t)
	err = f.s.scanDeployment(ctx, f.svc, d2, "")
	if err == nil || !strings.Contains(err.Error(), "required by the project policy (block)") || !strings.Contains(err.Error(), "trivy is not installed on build node build") {
		t.Fatalf("block without scanner: %v", err)
	}
}

// An agent that predates scanning does not answer: same as a missing scanner.
func TestScanOldAgentAndNoBuildNode(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()
	f.setPolicy(t, scan.ModeBlock, "CRITICAL")
	if err := f.s.scanDeployment(ctx, f.svc, f.deployment(t), ""); err == nil || !strings.Contains(err.Error(), "does not answer scan requests") {
		t.Fatalf("old agent, block: %v", err)
	}
	f.setPolicy(t, scan.ModeWarn, "CRITICAL")
	if err := f.s.scanDeployment(ctx, f.svc, f.deployment(t), ""); err != nil {
		t.Fatalf("old agent, warn: %v", err)
	}
	if _, err := f.s.store.Q(ctx).Exec(ctx, `UPDATE nodes SET status='offline'`); err != nil {
		t.Fatal(err)
	}
	f.setPolicy(t, scan.ModeBlock, "CRITICAL")
	if err := f.s.scanDeployment(ctx, f.svc, f.deployment(t), ""); err == nil || !strings.Contains(err.Error(), "no online build node") {
		t.Fatalf("no build node, block: %v", err)
	}
}

func TestScanPolicyOffSkips(t *testing.T) {
	f := newScanFixture(t)
	f.fakeScanAgent(t, proto.ScanReply{Result: fixtureResult(t)})
	f.setPolicy(t, scan.ModeOff, "CRITICAL")
	d := f.deployment(t)
	if err := f.s.scanDeployment(context.Background(), f.svc, d, ""); err != nil {
		t.Fatal(err)
	}
	if len(f.reqs) != 0 {
		t.Fatal("policy off must not scan")
	}
	if _, err := f.s.store.ImageScanForDeployment(context.Background(), d.ID); err == nil {
		t.Fatal("policy off must not record a scan")
	}
}

func TestScanAPI(t *testing.T) {
	f := newScanFixture(t)
	s := f.s
	h := s.routes()
	ctx := context.Background()
	f.fakeScanAgent(t, proto.ScanReply{Result: fixtureResult(t)})
	d := f.deployment(t)
	if err := s.scanDeployment(ctx, f.svc, d, ""); err != nil {
		t.Fatal(err)
	}
	dev := mkUser(t, s, "dev@example.com", "password1", false)
	viewer := mkUser(t, s, "viewer@example.com", "password1", false)
	mkUser(t, s, "stranger@example.com", "password1", false)
	if err := s.store.AddMember(ctx, f.team.ID, dev.ID, "developer"); err != nil {
		t.Fatal(err)
	}
	if err := s.store.AddMember(ctx, f.team.ID, viewer.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	owner := mustLogin(t, h, "owner@example.com", "password1")
	devTok := mustLogin(t, h, "dev@example.com", "password1")
	viewTok := mustLogin(t, h, "viewer@example.com", "password1")
	stranger := mustLogin(t, h, "stranger@example.com", "password1")

	// Deployment scan: viewers read it, outsiders do not.
	w := do(t, h, "GET", "/api/v1/deployments/"+d.ID+"/scan", viewTok, nil)
	if w.Code != 200 {
		t.Fatalf("deployment scan = %d %s", w.Code, w.Body)
	}
	var x store.ImageScan
	_ = json.Unmarshal(w.Body.Bytes(), &x)
	if x.Critical != 1 || len(x.Findings) != 8 || x.Findings[0].ID != "CVE-2021-44906" {
		t.Fatalf("scan body = %+v", x)
	}
	if w := do(t, h, "GET", "/api/v1/deployments/"+d.ID+"/scan", stranger, nil); w.Code != 403 {
		t.Fatalf("stranger = %d", w.Code)
	}
	other := f.deployment(t)
	if w := do(t, h, "GET", "/api/v1/deployments/"+other.ID+"/scan", viewTok, nil); w.Code != 404 {
		t.Fatalf("unscanned deployment = %d", w.Code)
	}

	// Service: the latest scan plus the policy in force.
	w = do(t, h, "GET", "/api/v1/services/"+f.svc.ID+"/scan", viewTok, nil)
	var svcScan struct {
		Scan   *store.ImageScan `json:"scan"`
		Policy scan.Policy      `json:"policy"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &svcScan)
	if w.Code != 200 || svcScan.Scan == nil || svcScan.Scan.DeploymentID != d.ID || svcScan.Policy != scan.DefaultPolicy() {
		t.Fatalf("service scan = %d %s", w.Code, w.Body)
	}

	// Policy: viewers read, only admins change.
	pol := "/api/v1/projects/" + f.project.ID + "/scan-policy"
	if w := do(t, h, "GET", pol, viewTok, nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"mode":"warn"`) {
		t.Fatalf("get policy = %d %s", w.Code, w.Body)
	}
	if w := do(t, h, "PUT", pol, devTok, map[string]string{"mode": "off"}); w.Code != 403 {
		t.Fatalf("developer put = %d", w.Code)
	}
	if w := do(t, h, "PUT", pol, owner, map[string]string{"mode": "block", "threshold": "nope"}); w.Code != 400 {
		t.Fatalf("bad threshold = %d", w.Code)
	}
	if w := do(t, h, "PUT", pol, owner, map[string]string{"mode": "Block", "threshold": "high"}); w.Code != 200 || !strings.Contains(w.Body.String(), `"threshold":"HIGH"`) {
		t.Fatalf("owner put = %d %s", w.Code, w.Body)
	}
	if p, err := s.store.ProjectScanPolicy(ctx, f.project.ID); err != nil || p != (scan.Policy{Mode: "block", Threshold: "HIGH"}) {
		t.Fatalf("stored policy = %+v %v", p, err)
	}
	if w := do(t, h, "GET", pol, stranger, nil); w.Code != 403 {
		t.Fatalf("stranger get policy = %d", w.Code)
	}
}
