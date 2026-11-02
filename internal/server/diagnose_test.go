package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/store"
)

// fakeModel is an httptest server speaking the Anthropic Messages or the
// OpenAI chat completions API; it records the last request.
type fakeModel struct {
	srv   *httptest.Server
	hits  atomic.Int32
	last  atomic.Pointer[http.Request]
	body  atomic.Pointer[map[string]any]
	delay time.Duration
	reply string
}

func newFakeModel(t *testing.T, reply string) *fakeModel {
	t.Helper()
	m := &fakeModel{reply: reply}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.hits.Add(1)
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		m.last.Store(r.Clone(r.Context()))
		m.body.Store(&body)
		if m.delay > 0 {
			select {
			case <-time.After(m.delay):
			case <-r.Context().Done():
				return
			}
		}
		switch r.URL.Path {
		case "/v1/messages":
			writeJSON(w, 200, map[string]any{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "text", "text": m.reply}}})
		case "/v1/chat/completions":
			writeJSON(w, 200, map[string]any{"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": m.reply}}}})
		default:
			writeError(w, 404, "no such path "+r.URL.Path)
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

const fakeAnswer = "```json\n{\"summary\": \"The app cannot start: express is missing.\", \"likely_cause\": \"express is not in package.json dependencies\", \"suggested_fixes\": [\"npm install express --save\", \"redeploy\"]}\n```"

func TestDiagnoseDisabledByDefault(t *testing.T) {
	s, h := newTestServer(t)
	f := newMCPFixture(t, s, h)
	w := do(t, h, "POST", "/api/v1/deployments/"+f.deployment.ID+"/diagnose", f.owner, nil)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "not configured") {
		t.Fatalf("diagnose without provider = %d %s", w.Code, w.Body)
	}
	w = do(t, h, "GET", "/api/v1/deployments/"+f.deployment.ID+"/diagnosis", f.viewer, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("diagnosis status = %d %s", w.Code, w.Body)
	}
	w = do(t, h, "GET", "/api/v1/admin/ai", f.adminToken, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("admin ai = %d %s", w.Code, w.Body)
	}
}

func TestDiagnoseAnthropic(t *testing.T) {
	s, h := newTestServer(t)
	f := newMCPFixture(t, s, h)
	m := newFakeModel(t, fakeAnswer)
	const key = "sk-ant-test-key-0123456789abcdef"

	// Only administrators configure the provider; the key is never returned.
	if w := do(t, h, "PUT", "/api/v1/admin/ai", f.owner, map[string]string{"provider": "anthropic", "api_key": key, "base_url": m.srv.URL}); w.Code != 403 {
		t.Fatalf("non-admin put = %d", w.Code)
	}
	w := do(t, h, "PUT", "/api/v1/admin/ai", f.adminToken, map[string]string{"provider": "anthropic", "api_key": key, "base_url": m.srv.URL})
	if w.Code != 200 || strings.Contains(w.Body.String(), key) || !strings.Contains(w.Body.String(), `"model":"claude-sonnet-5-5"`) {
		t.Fatalf("put ai = %d %s", w.Code, w.Body)
	}
	if enc, _ := s.store.GetSetting(t.Context(), settingAI); strings.Contains(enc, key) {
		t.Fatal("API key stored in clear")
	}
	if w := do(t, h, "GET", "/api/v1/admin/ai", f.adminToken, nil); strings.Contains(w.Body.String(), key) || !strings.Contains(w.Body.String(), `"has_key":true`) {
		t.Fatalf("get ai = %s", w.Body)
	}

	// Viewers cannot spend tokens.
	if w := do(t, h, "POST", "/api/v1/deployments/"+f.deployment.ID+"/diagnose", f.viewer, nil); w.Code != 403 {
		t.Fatalf("viewer diagnose = %d", w.Code)
	}
	if w := do(t, h, "POST", "/api/v1/deployments/"+f.deployment.ID+"/diagnose", f.outsider, nil); w.Code != 403 {
		t.Fatalf("outsider diagnose = %d", w.Code)
	}

	w = do(t, h, "POST", "/api/v1/deployments/"+f.deployment.ID+"/diagnose", f.owner, nil)
	if w.Code != 200 {
		t.Fatalf("diagnose = %d %s", w.Code, w.Body)
	}
	var out diagnosisOut
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Summary, "express is missing") || out.LikelyCause == "" || len(out.SuggestedFixes) != 2 || out.Cached {
		t.Fatalf("diagnosis = %+v", out)
	}

	// Request shape.
	req, body := m.last.Load(), *m.body.Load()
	if req.URL.Path != "/v1/messages" || req.Header.Get("x-api-key") != key || req.Header.Get("anthropic-version") != "2023-06-01" {
		t.Fatalf("request = %s %v", req.URL.Path, req.Header)
	}
	if body["model"] != "claude-sonnet-5-5" || body["system"] == "" || body["max_tokens"] == nil {
		t.Fatalf("body = %v", body)
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", body["messages"])
	}
	prompt, _ := msgs[0].(map[string]any)["content"].(string)
	if !strings.Contains(prompt, "Cannot find module 'express'") || !strings.Contains(prompt, "container exited with code 1") || !strings.Contains(prompt, "Expected port: 8080") {
		t.Fatalf("prompt lacks context:\n%s", prompt)
	}
	for _, secret := range []string{f.envSecret, "hunter22pass", f.webhookSecret} {
		if strings.Contains(prompt, secret) {
			t.Fatalf("prompt leaks %q:\n%s", secret, prompt)
		}
	}
	if len(prompt) > aiMaxPromptBytes {
		t.Fatalf("prompt is %d bytes", len(prompt))
	}

	// Cached per deployment; refresh asks again.
	w = do(t, h, "POST", "/api/v1/deployments/"+f.deployment.ID+"/diagnose", f.owner, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"cached":true`) || m.hits.Load() != 1 {
		t.Fatalf("second diagnose = %d %s hits=%d", w.Code, w.Body, m.hits.Load())
	}
	if w := do(t, h, "GET", "/api/v1/deployments/"+f.deployment.ID+"/diagnosis", f.viewer, nil); !strings.Contains(w.Body.String(), "express is missing") {
		t.Fatalf("viewer reads cached diagnosis = %s", w.Body)
	}
	if w := do(t, h, "POST", "/api/v1/deployments/"+f.deployment.ID+"/diagnose", f.owner, map[string]bool{"refresh": true}); w.Code != 200 || m.hits.Load() != 2 {
		t.Fatalf("refresh = %d hits=%d", w.Code, m.hits.Load())
	}

	// The MCP tool reaches the same endpoint.
	text, isErr, _ := callTool(t, h, f.owner, "diagnose_deployment", map[string]any{"deployment_id": f.deployment.ID})
	if isErr || !strings.Contains(text, "express is missing") {
		t.Fatalf("mcp diagnose = %v %s", isErr, text)
	}
	if text, isErr, _ := callTool(t, h, f.viewer, "diagnose_deployment", map[string]any{"deployment_id": f.deployment.ID}); !isErr {
		t.Fatalf("viewer mcp diagnose = %s", text)
	}

	// Disabling keeps working and turns the feature off.
	if w := do(t, h, "PUT", "/api/v1/admin/ai", f.adminToken, map[string]string{"provider": ""}); w.Code != 200 {
		t.Fatalf("disable = %d", w.Code)
	}
	if w := do(t, h, "POST", "/api/v1/deployments/"+f.deployment.ID+"/diagnose", f.owner, map[string]bool{"refresh": true}); w.Code != 400 {
		t.Fatalf("diagnose after disable = %d", w.Code)
	}
}

func TestDiagnoseOpenAICompatible(t *testing.T) {
	s, h := newTestServer(t)
	f := newMCPFixture(t, s, h)
	m := newFakeModel(t, `{"summary":"s","likely_cause":"c","suggested_fixes":["a"]}`)
	if w := do(t, h, "PUT", "/api/v1/admin/ai", f.adminToken, map[string]string{"provider": "openai", "model": "llama3"}); w.Code != 400 {
		t.Fatalf("openai without base_url = %d", w.Code)
	}
	w := do(t, h, "PUT", "/api/v1/admin/ai", f.adminToken, map[string]string{"provider": "openai", "model": "llama3", "base_url": m.srv.URL + "/v1", "api_key": "local-key-123"})
	if w.Code != 200 {
		t.Fatalf("put = %d %s", w.Code, w.Body)
	}
	w = do(t, h, "POST", "/api/v1/deployments/"+f.deployment.ID+"/diagnose", f.owner, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"likely_cause":"c"`) {
		t.Fatalf("diagnose = %d %s", w.Code, w.Body)
	}
	req, body := m.last.Load(), *m.body.Load()
	if req.URL.Path != "/v1/chat/completions" || req.Header.Get("Authorization") != "Bearer local-key-123" || body["model"] != "llama3" {
		t.Fatalf("request = %s %v %v", req.URL.Path, req.Header, body)
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("messages = %v", msgs)
	}
	if strings.Contains(msgs[1].(map[string]any)["content"].(string), f.envSecret) {
		t.Fatal("prompt leaks the env secret")
	}
}

func TestDiagnoseTimeoutAndState(t *testing.T) {
	s, h := newTestServer(t)
	f := newMCPFixture(t, s, h)
	m := newFakeModel(t, fakeAnswer)
	m.delay = 5 * time.Second
	old := aiTimeout
	aiTimeout = 200 * time.Millisecond
	t.Cleanup(func() { aiTimeout = old })
	do(t, h, "PUT", "/api/v1/admin/ai", f.adminToken, map[string]string{"provider": "anthropic", "api_key": "k-123456789", "base_url": m.srv.URL})
	start := time.Now()
	w := do(t, h, "POST", "/api/v1/deployments/"+f.deployment.ID+"/diagnose", f.owner, nil)
	if w.Code != 504 || time.Since(start) > 3*time.Second {
		t.Fatalf("slow provider = %d after %s", w.Code, time.Since(start))
	}
	if c, _ := s.cachedDiagnosis(t.Context(), f.deployment.ID); c != nil {
		t.Fatal("a failed call was cached")
	}
	// A deployment still running cannot be diagnosed.
	d, err := s.store.CreateDeployment(t.Context(), &store.Deployment{ServiceID: f.svc.ID, Status: "building", Trigger: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	if w := do(t, h, "POST", "/api/v1/deployments/"+d.ID+"/diagnose", f.owner, nil); w.Code != 409 {
		t.Fatalf("in-progress = %d", w.Code)
	}
}

func TestParseDiagnosis(t *testing.T) {
	d := parseDiagnosis("Sure!\n```json\n{\"summary\":\"x\",\"likely_cause\":\"y\",\"suggested_fixes\":[\"a\",\" \",\"b\"]}\n```")
	if d.Summary != "x" || d.LikelyCause != "y" || len(d.SuggestedFixes) != 2 {
		t.Fatalf("parse = %+v", d)
	}
	d = parseDiagnosis("plain prose answer")
	if d.Summary != "plain prose answer" || d.SuggestedFixes == nil {
		t.Fatalf("prose = %+v", d)
	}
	if got := tailWithin([]string{"aaaa", "bbbb", "cccc"}, 10); got != "bbbb\ncccc\n" {
		t.Fatalf("tailWithin = %q", got)
	}
}
