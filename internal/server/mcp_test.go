package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/store"
)

type mcpFixture struct {
	s                         *Server
	h                         http.Handler
	owner, viewer, outsider   string // API tokens
	ownerID                   string
	team, project             string
	svc                       *store.Service
	webhookSecret, envSecret  string
	ownerSession, adminToken  string
	deployment                *store.Deployment
	viewerUserID, outsiderUID string
}

func newMCPFixture(t *testing.T, s *Server, h http.Handler) *mcpFixture {
	t.Helper()
	ctx := context.Background()
	f := &mcpFixture{s: s, h: h, webhookSecret: "whsec-0123456789abcdef", envSecret: "s3cr3t-value-zz9"}
	f.ownerID = userWithTeam(t, s, "owner@example.com", false)
	f.viewerUserID = userWithTeam(t, s, "viewer@example.com", false)
	f.outsiderUID = userWithTeam(t, s, "outsider@example.com", false)
	userWithTeam(t, s, "admin@example.com", true)
	f.ownerSession = mustLogin(t, h, "owner@example.com", "password1")
	f.owner = mkAPIToken(t, h, f.ownerSession)
	f.viewer = mkAPIToken(t, h, mustLogin(t, h, "viewer@example.com", "password1"))
	f.outsider = mkAPIToken(t, h, mustLogin(t, h, "outsider@example.com", "password1"))
	f.adminToken = mkAPIToken(t, h, mustLogin(t, h, "admin@example.com", "password1"))
	team, err := s.store.CreateTeam(ctx, "Acme", "acme", f.ownerID, false)
	if err != nil {
		t.Fatal(err)
	}
	f.team = team.ID
	if err := s.store.AddMember(ctx, team.ID, f.viewerUserID, "viewer"); err != nil {
		t.Fatal(err)
	}
	proj, err := s.store.CreateProject(ctx, team.ID, "Shop", "shop")
	if err != nil {
		t.Fatal(err)
	}
	f.project = proj.ID
	f.svc, err = s.store.CreateService(ctx, &store.Service{
		ProjectID: proj.ID, Name: "web", Slug: "web", Type: model.TypeWeb, WebhookSecret: f.webhookSecret,
		Source: model.ServiceSource{Kind: "git", Repo: "https://example.invalid/r.git", Branch: "main"},
		Spec:   model.ServiceSpec{Replicas: 1, Port: 8080},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetEnvVar(ctx, f.svc.ID, "API_KEY_X", s.box.MustEncrypt(f.envSecret)); err != nil {
		t.Fatal(err)
	}
	f.deployment, err = s.store.CreateDeployment(ctx, &store.Deployment{ServiceID: f.svc.ID, Status: model.DeployQueued, Trigger: "manual", CommitSHA: "abc1234"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetDeploymentStatus(ctx, f.deployment.ID, model.DeployFailed, "container exited with code 1"); err != nil {
		t.Fatal(err)
	}
	if err := s.store.AppendDeploymentLogs(ctx, f.deployment.ID, []string{
		"==> building",
		"connecting to postgres://app:hunter22pass@db.internal:5432/app",
		"using key " + f.envSecret,
		"Error: Cannot find module 'express'",
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func mcpPost(t *testing.T, h http.Handler, token, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	r.RemoteAddr = "192.0.2.1:1234"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type rpcOut struct {
	ID     json.RawMessage `json:"id"`
	Result map[string]any  `json:"result"`
	Error  *rpcError       `json:"error"`
}

func rpc(t *testing.T, h http.Handler, token, method string, params any) rpcOut {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": params})
	w := mcpPost(t, h, token, string(b))
	if w.Code != 200 {
		t.Fatalf("%s = HTTP %d %s", method, w.Code, w.Body)
	}
	var out rpcOut
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: decode %s: %v", method, w.Body, err)
	}
	return out
}

// callTool returns the tool result text and whether it is an error.
func callTool(t *testing.T, h http.Handler, token, name string, args any) (string, bool, rpcOut) {
	t.Helper()
	out := rpc(t, h, token, "tools/call", map[string]any{"name": name, "arguments": args})
	if out.Error != nil {
		return "", false, out
	}
	content, _ := out.Result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("%s: content = %v", name, out.Result)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	isErr, _ := out.Result["isError"].(bool)
	return text, isErr, out
}

func TestMCPProtocol(t *testing.T) {
	s, h := newTestServer(t)
	f := newMCPFixture(t, s, h)

	for _, c := range []struct{ asked, want string }{
		{"2025-06-18", "2025-06-18"}, {"2025-03-26", "2025-03-26"}, {"2024-11-05", "2024-11-05"}, {"1999-01-01", mcpProtocolVersion},
	} {
		out := rpc(t, h, f.owner, "initialize", map[string]any{"protocolVersion": c.asked, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1"}})
		if out.Error != nil || out.Result["protocolVersion"] != c.want {
			t.Fatalf("initialize %s = %+v, want %s", c.asked, out, c.want)
		}
		caps, _ := out.Result["capabilities"].(map[string]any)
		if _, ok := caps["tools"]; !ok {
			t.Fatalf("no tools capability: %v", out.Result)
		}
		if string(out.ID) != "7" {
			t.Fatalf("id = %s", out.ID)
		}
	}
	if w := mcpPost(t, h, f.owner, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, "MCP-Protocol-Version", "2025-06-18"); w.Code != 202 || w.Body.Len() != 0 {
		t.Fatalf("notification = %d %q, want 202 without body", w.Code, w.Body)
	}
	if out := rpc(t, h, f.owner, "ping", nil); out.Error != nil || out.Result == nil {
		t.Fatalf("ping = %+v", out)
	}
	if out := rpc(t, h, f.owner, "resources/list", nil); out.Error == nil || out.Error.Code != rpcMethodNotFound {
		t.Fatalf("unknown method = %+v", out)
	}
	if w := mcpPost(t, h, f.owner, `{"jsonrpc":"2.0","id":1,`); !strings.Contains(w.Body.String(), "-32700") {
		t.Fatalf("parse error = %d %s", w.Code, w.Body)
	}
	if w := mcpPost(t, h, f.owner, `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":"b","method":"ping"}]`); w.Code != 200 || strings.Count(w.Body.String(), `"jsonrpc"`) != 2 {
		t.Fatalf("batch = %d %s", w.Code, w.Body)
	}
	// Transport-level checks.
	if w := mcpPost(t, h, "", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("no token = %d", w.Code)
	}
	if w := mcpPost(t, h, f.ownerSession, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); w.Code != 401 {
		t.Fatalf("session token = %d, want 401 (API tokens only)", w.Code)
	}
	if w := mcpPost(t, h, "paas_notarealtoken", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); w.Code != 401 {
		t.Fatalf("bad token = %d", w.Code)
	}
	if w := mcpPost(t, h, f.owner, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "MCP-Protocol-Version", "2000-01-01"); w.Code != 400 {
		t.Fatalf("bad protocol header = %d", w.Code)
	}
	if w := mcpPost(t, h, f.owner, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "Origin", "https://evil.example"); w.Code != 403 {
		t.Fatalf("foreign origin = %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+f.owner)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 405 {
		t.Fatalf("GET /mcp = %d, want 405", w.Code)
	}
}

func TestMCPToolsList(t *testing.T) {
	s, h := newTestServer(t)
	f := newMCPFixture(t, s, h)
	out := rpc(t, h, f.owner, "tools/list", map[string]any{})
	tools, _ := out.Result["tools"].([]any)
	if len(tools) != len(mcpTools) {
		t.Fatalf("tools = %d, want %d", len(tools), len(mcpTools))
	}
	names := map[string]bool{}
	for _, x := range tools {
		tool := x.(map[string]any)
		name, _ := tool["name"].(string)
		names[name] = true
		schema, _ := tool["inputSchema"].(map[string]any)
		if schema["type"] != "object" || tool["description"] == "" {
			t.Errorf("%s: bad schema/description: %v", name, tool)
		}
		ann, _ := tool["annotations"].(map[string]any)
		if _, ok := ann["readOnlyHint"].(bool); !ok {
			t.Errorf("%s: no readOnlyHint", name)
		}
		for _, bad := range []string{"token", "password", "secret", "2fa", "join"} {
			if strings.Contains(name, bad) {
				t.Errorf("credential tool exposed: %s", name)
			}
		}
	}
	for _, want := range []string{"list_projects", "list_services", "get_service", "get_deployment", "get_build_logs", "get_runtime_logs",
		"list_deployments", "deploy_service", "rollback_service", "restart_service", "scale_service", "set_env", "list_databases", "get_metrics", "list_nodes"} {
		if !names[want] {
			t.Errorf("missing tool %s", want)
		}
	}
	for _, tl := range mcpTools {
		if tl.Write && tl.ReadOnly {
			t.Errorf("%s: write tool marked read-only", tl.Name)
		}
	}
}

func TestMCPToolCalls(t *testing.T) {
	s, h := newTestServer(t)
	f := newMCPFixture(t, s, h)
	ctx := context.Background()

	text, isErr, _ := callTool(t, h, f.owner, "list_projects", map[string]any{})
	if isErr || !strings.Contains(text, f.project) {
		t.Fatalf("list_projects = %v %s", isErr, text)
	}
	text, isErr, _ = callTool(t, h, f.owner, "list_services", map[string]any{"project_id": f.project})
	if isErr || !strings.Contains(text, f.svc.ID) {
		t.Fatalf("list_services = %v %s", isErr, text)
	}
	text, isErr, out := callTool(t, h, f.owner, "get_service", map[string]any{"service_id": f.svc.ID})
	if isErr || !strings.Contains(text, `"slug": "web"`) || out.Result["structuredContent"] == nil {
		t.Fatalf("get_service = %v %s", isErr, text)
	}
	text, isErr, _ = callTool(t, h, f.owner, "get_deployment", map[string]any{"deployment_id": f.deployment.ID})
	if isErr || !strings.Contains(text, "container exited") {
		t.Fatalf("get_deployment = %v %s", isErr, text)
	}
	text, isErr, _ = callTool(t, h, f.owner, "get_build_logs", map[string]any{"deployment_id": f.deployment.ID, "tail": 3})
	if isErr || !strings.Contains(text, "Cannot find module") || strings.Contains(text, "==> building") {
		t.Fatalf("get_build_logs tail = %v %s", isErr, text)
	}
	if strings.Contains(text, "hunter22pass") || strings.Contains(text, f.envSecret) || !strings.Contains(text, redacted) {
		t.Fatalf("build logs not redacted: %s", text)
	}
	text, isErr, _ = callTool(t, h, f.owner, "list_deployments", map[string]any{"service_id": f.svc.ID})
	if isErr || !strings.Contains(text, f.deployment.ID) {
		t.Fatalf("list_deployments = %v %s", isErr, text)
	}
	text, isErr, _ = callTool(t, h, f.owner, "list_env_names", map[string]any{"service_id": f.svc.ID})
	if isErr || !strings.Contains(text, "API_KEY_X") || strings.Contains(text, f.envSecret) {
		t.Fatalf("list_env_names = %v %s", isErr, text)
	}

	// set_env: values are stored but never echoed; the call is audited.
	text, isErr, _ = callTool(t, h, f.owner, "set_env", map[string]any{"service_id": f.svc.ID, "vars": map[string]string{"NEW_SECRET": "very-secret-123"}, "unset": []string{"API_KEY_X"}})
	if isErr || !strings.Contains(text, "NEW_SECRET") || strings.Contains(text, "very-secret-123") {
		t.Fatalf("set_env = %v %s", isErr, text)
	}
	env, err := s.userEnv(ctx, f.svc)
	if err != nil || env["NEW_SECRET"] != "very-secret-123" || env["API_KEY_X"] != "" {
		t.Fatalf("env after set_env = %v %v", env, err)
	}
	var action, target string
	if err := s.store.Q(ctx).QueryRow(ctx, `SELECT action, target FROM audit_logs WHERE action='mcp.set_env' ORDER BY id DESC LIMIT 1`).Scan(&action, &target); err != nil {
		t.Fatalf("no audit row: %v", err)
	}
	if !strings.Contains(target, "shop/web") || strings.Contains(target, "very-secret-123") {
		t.Fatalf("audit target = %q", target)
	}

	// Errors: unknown tool and invalid arguments are protocol errors.
	if _, _, out := callTool(t, h, f.owner, "create_token", map[string]any{}); out.Error == nil || out.Error.Code != rpcInvalidParams {
		t.Fatalf("unknown tool = %+v", out)
	}
	for _, args := range []map[string]any{
		{},                                      // missing service_id
		{"service_id": f.svc.ID, "replicas": 0}, // below minimum
		{"service_id": f.svc.ID, "replicas": "2"},
		{"service_id": f.svc.ID, "replicas": 2, "force": true}, // unknown argument
		{"service_id": "../tokens", "replicas": 2},
	} {
		if _, _, out := callTool(t, h, f.owner, "scale_service", args); out.Error == nil || out.Error.Code != rpcInvalidParams {
			t.Fatalf("scale_service %v = %+v, want invalid params", args, out)
		}
	}
	// Not found is a tool error the agent can read.
	if text, isErr, _ := callTool(t, h, f.owner, "get_service", map[string]any{"service_id": "00000000-0000-0000-0000-000000000000"}); !isErr || !strings.Contains(text, "not found") {
		t.Fatalf("missing service = %v %s", isErr, text)
	}
}

func TestMCPPermissions(t *testing.T) {
	s, h := newTestServer(t)
	f := newMCPFixture(t, s, h)
	ctx := context.Background()

	// A viewer reads but cannot write.
	if text, isErr, _ := callTool(t, h, f.viewer, "get_service", map[string]any{"service_id": f.svc.ID}); isErr {
		t.Fatalf("viewer get_service = %s", text)
	}
	text, isErr, _ := callTool(t, h, f.viewer, "set_env", map[string]any{"service_id": f.svc.ID, "vars": map[string]string{"X_VAR": "1"}})
	if !isErr || !strings.Contains(text, "requires role developer") {
		t.Fatalf("viewer set_env = %v %s", isErr, text)
	}
	if env, _ := s.userEnv(ctx, f.svc); env["X_VAR"] != "" {
		t.Fatal("viewer changed the environment")
	}
	var target string
	if err := s.store.Q(ctx).QueryRow(ctx, `SELECT target FROM audit_logs WHERE action='mcp.set_env' ORDER BY id DESC LIMIT 1`).Scan(&target); err != nil || !strings.Contains(target, "failed") {
		t.Fatalf("denied write not audited: %q %v", target, err)
	}
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"scale_service", map[string]any{"service_id": f.svc.ID, "replicas": 2}},
		{"restart_service", map[string]any{"service_id": f.svc.ID}},
		{"deploy_service", map[string]any{"service_id": f.svc.ID}},
		{"rollback_service", map[string]any{"service_id": f.svc.ID, "deployment_id": f.deployment.ID}},
	} {
		if text, isErr, _ := callTool(t, h, f.viewer, c.tool, c.args); !isErr || !strings.Contains(text, "403") && !strings.Contains(text, "requires role") {
			t.Fatalf("viewer %s = %v %s", c.tool, isErr, text)
		}
	}

	// Another team's resources are invisible.
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"get_service", map[string]any{"service_id": f.svc.ID}},
		{"list_services", map[string]any{"project_id": f.project}},
		{"get_build_logs", map[string]any{"deployment_id": f.deployment.ID}},
		{"list_env_names", map[string]any{"service_id": f.svc.ID}},
		{"set_env", map[string]any{"service_id": f.svc.ID, "vars": map[string]string{"X_VAR": "1"}}},
		{"scale_service", map[string]any{"service_id": f.svc.ID, "replicas": 3}},
	} {
		text, isErr, _ := callTool(t, h, f.outsider, c.tool, c.args)
		if !isErr || !strings.Contains(text, "not a member") || strings.Contains(text, f.envSecret) {
			t.Fatalf("outsider %s = %v %s", c.tool, isErr, text)
		}
	}
	if text, _, _ := callTool(t, h, f.outsider, "list_projects", map[string]any{}); strings.Contains(text, f.project) {
		t.Fatalf("outsider sees the project: %s", text)
	}
	if text, isErr, _ := callTool(t, h, f.outsider, "list_projects", map[string]any{"team_id": f.team}); !isErr {
		t.Fatalf("outsider list_projects(team) = %s", text)
	}
	// Nodes are for administrators.
	if text, isErr, _ := callTool(t, h, f.owner, "list_nodes", map[string]any{}); !isErr || !strings.Contains(text, "administrator") {
		t.Fatalf("owner list_nodes = %v %s", isErr, text)
	}
	if text, isErr, _ := callTool(t, h, f.adminToken, "list_nodes", map[string]any{}); isErr {
		t.Fatalf("admin list_nodes = %s", text)
	}
}

// No tool output carries a secret the REST API would hand to a developer.
func TestMCPNoSecretLeak(t *testing.T) {
	s, h := newTestServer(t)
	f := newMCPFixture(t, s, h)
	// The REST API gives a developer the webhook secret; MCP must not.
	if w := do(t, h, "GET", "/api/v1/services/"+f.svc.ID, f.owner, nil); !strings.Contains(w.Body.String(), f.webhookSecret) {
		t.Fatalf("fixture: REST does not return the webhook secret: %s", w.Body)
	}
	calls := []struct {
		tool string
		args map[string]any
	}{
		{"list_projects", map[string]any{}},
		{"list_services", map[string]any{"project_id": f.project}},
		{"get_service", map[string]any{"service_id": f.svc.ID}},
		{"list_deployments", map[string]any{"service_id": f.svc.ID}},
		{"get_deployment", map[string]any{"deployment_id": f.deployment.ID}},
		{"get_build_logs", map[string]any{"deployment_id": f.deployment.ID}},
		{"list_env_names", map[string]any{"service_id": f.svc.ID}},
		{"list_databases", map[string]any{"project_id": f.project}},
	}
	for _, c := range calls {
		text, isErr, _ := callTool(t, h, f.owner, c.tool, c.args)
		if isErr {
			t.Fatalf("%s: %s", c.tool, text)
		}
		for _, secret := range []string{f.webhookSecret, f.envSecret, "hunter22pass", f.owner} {
			if strings.Contains(text, secret) {
				t.Errorf("%s leaks %q: %s", c.tool, secret, text)
			}
		}
	}
}

func TestMCPDeployAudited(t *testing.T) {
	s := newTestServerBus(t)
	h := s.routes()
	f := newMCPFixture(t, s, h)
	ctx := context.Background()
	text, isErr, _ := callTool(t, h, f.owner, "deploy_service", map[string]any{"service_id": f.svc.ID})
	if isErr || !strings.Contains(text, `"status": "queued"`) {
		t.Fatalf("deploy_service = %v %s", isErr, text)
	}
	var n int
	if err := s.store.Q(ctx).QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action='mcp.deploy_service' AND team_id=$1`, f.team).Scan(&n); err != nil || n != 1 {
		t.Fatalf("deploy audit rows = %d %v", n, err)
	}
	if text, isErr, _ := callTool(t, h, f.owner, "rollback_service", map[string]any{"service_id": f.svc.ID}); !isErr || !strings.Contains(text, "no previous deployment") {
		t.Fatalf("rollback without history = %v %s", isErr, text)
	}
}

func TestRateWindow(t *testing.T) {
	l := &rateWindow{window: time.Minute, hits: map[string][]time.Time{}}
	for i := 0; i < 3; i++ {
		if !l.take("a", 3) {
			t.Fatalf("take %d refused", i)
		}
	}
	if l.take("a", 3) {
		t.Fatal("4th take allowed")
	}
	if !l.take("b", 3) {
		t.Fatal("keys are not independent")
	}
}

func TestRedactor(t *testing.T) {
	r := newRedactor("supersecretvalue", "postgres://u:dbpass123@h/db", "short")
	for in, mustNot := range map[string]string{
		"value=supersecretvalue here":                                                    "supersecretvalue",
		"dial postgres://u:dbpass123@h/db failed":                                        "dbpass123",
		"auth failed for password dbpass123":                                             "dbpass123",
		"git clone https://x-access-token:ghs_abcdefghijklmnopqrstuvwxyz@github.com/a/b": "ghs_abc",
		"Authorization: Bearer abcdefghijklmnop":                                         "abcdefghijklmnop",
		"export DB_PASSWORD=hunter2":                                                     "hunter2",
		`{"api_key": "k-123456"}`:                                                        "k-123456",
		"token paas_abcdefghijklmnopqrstuvwxyz used":                                     "paas_abcdefghijk",
		"AWS AKIAABCDEFGHIJKLMNOP":                                                       "AKIAABCDEFGHIJKLMNOP",
	} {
		if got := r.String(in); strings.Contains(got, mustNot) {
			t.Errorf("redact(%q) = %q", in, got)
		}
	}
	// Ordinary diagnostics survive.
	for _, keep := range []string{"SyntaxError: Unexpected token: }", "listening on port 8080", "short read"} {
		if got := r.String(keep); got != keep {
			t.Errorf("redact(%q) = %q, want unchanged", keep, got)
		}
	}
}
