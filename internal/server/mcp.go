package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/thesunbg/paas/internal/agent"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

// The Model Context Protocol endpoint (Streamable HTTP transport, JSON
// responses only). AI agents authenticate with an API token; every tool is
// executed by dispatching a request through the regular API mux with that same
// token, so the REST handlers' RBAC checks apply unchanged.

const mcpProtocolVersion = "2025-06-18"

// mcpVersions are the protocol revisions this server speaks, newest first.
var mcpVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

const (
	mcpMaxBody      = 1 << 20
	mcpMaxBatch     = 20
	mcpRatePerMin   = 120 // requests per token per minute
	mcpToolResultKB = 256 // tool output is cut beyond this
)

// JSON-RPC error codes.
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternalError  = -32603
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"` // set on responses to server requests
	Error   json.RawMessage `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// mcpLimiter counts MCP requests per API token over a sliding minute.
var mcpLimiter = &rateWindow{window: time.Minute, hits: map[string][]time.Time{}}

type rateWindow struct {
	mu     sync.Mutex
	window time.Duration
	hits   map[string][]time.Time
}

func (c *rateWindow) take(key string, max int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	var keep []time.Time
	for _, t := range c.hits[key] {
		if time.Since(t) < c.window {
			keep = append(keep, t)
		}
	}
	if len(keep) >= max {
		c.hits[key] = keep
		return false
	}
	c.hits[key] = append(keep, time.Now())
	if len(c.hits) > 100000 {
		c.hits = map[string][]time.Time{}
	}
	return true
}

// mcpSession is the context of one MCP HTTP request.
type mcpSession struct {
	s     *Server
	mux   http.Handler // the API mux, without the logging wrappers
	token string
	user  *store.User
	r     *http.Request
}

// handleMCP serves /mcp. api is the route mux the tools dispatch through.
func (s *Server) handleMCP(api http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			// No server-initiated stream (GET) and no sessions to end (DELETE).
			w.Header().Set("Allow", "POST")
			writeError(w, http.StatusMethodNotAllowed, "use POST")
			return
		}
		// Bearer tokens are never sent by browsers on their own, but a rebinding
		// page could still hold one: refuse foreign origins.
		if r.Header.Get("Origin") != "" && !sameOrigin(r) {
			writeError(w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		tok := ""
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			tok = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		}
		if !strings.HasPrefix(tok, "paas_") {
			w.Header().Set("WWW-Authenticate", `Bearer realm="paas"`)
			writeError(w, http.StatusUnauthorized, "an API token is required (Authorization: Bearer paas_...)")
			return
		}
		hash := util.HashToken(tok)
		user, err := s.store.UserByAPIToken(r.Context(), hash)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="paas", error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, "invalid or revoked API token")
			return
		}
		if !mcpLimiter.take(hash, mcpRatePerMin) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded; slow down")
			return
		}
		if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !slices.Contains(mcpVersions, v) {
			writeError(w, http.StatusBadRequest, "unsupported MCP-Protocol-Version "+v)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, mcpMaxBody+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(body) > mcpMaxBody {
			writeError(w, http.StatusRequestEntityTooLarge, "request too large")
			return
		}
		sess := &mcpSession{s: s, mux: api, token: tok, user: user, r: r}
		body = bytes.TrimSpace(body)
		if len(body) > 0 && body[0] == '[' {
			var batch []json.RawMessage
			if err := json.Unmarshal(body, &batch); err != nil {
				writeJSON(w, 200, rpcFail(nil, rpcParseError, "parse error"))
				return
			}
			if len(batch) == 0 || len(batch) > mcpMaxBatch {
				writeJSON(w, 200, rpcFail(nil, rpcInvalidRequest, fmt.Sprintf("a batch holds 1 to %d messages", mcpMaxBatch)))
				return
			}
			var out []*rpcResponse
			for _, m := range batch {
				if resp := sess.handle(m); resp != nil {
					out = append(out, resp)
				}
			}
			if len(out) == 0 {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			writeJSON(w, 200, out)
			return
		}
		resp := sess.handle(body)
		if resp == nil {
			w.WriteHeader(http.StatusAccepted) // notification or response
			return
		}
		writeJSON(w, 200, resp)
	}
}

func rpcFail(id json.RawMessage, code int, msg string) *rpcResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

func rpcOK(id json.RawMessage, result any) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

// handle processes one JSON-RPC message; nil means no reply (notifications and
// responses to server requests, which this server never sends).
func (m *mcpSession) handle(raw json.RawMessage) *rpcResponse {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return rpcFail(nil, rpcParseError, "parse error")
	}
	if req.Method == "" {
		if len(req.Result) > 0 || len(req.Error) > 0 || len(req.ID) == 0 {
			return nil // a response (this server sends no requests) or a malformed notification
		}
		return rpcFail(req.ID, rpcInvalidRequest, "method is required")
	}
	if req.JSONRPC != "2.0" {
		return rpcFail(req.ID, rpcInvalidRequest, `jsonrpc must be "2.0"`)
	}
	if len(req.ID) == 0 {
		return nil // notification (notifications/initialized, cancelled, ...)
	}
	if string(req.ID) == "null" || (req.ID[0] != '"' && !isJSONNumber(req.ID)) {
		return rpcFail(nil, rpcInvalidRequest, "id must be a string or a number")
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &p); err != nil {
				return rpcFail(req.ID, rpcInvalidParams, "invalid initialize params")
			}
		}
		version := mcpProtocolVersion
		if slices.Contains(mcpVersions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		return rpcOK(req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "paas", "title": "paas", "version": agent.Version},
			"instructions": "Tools to inspect and operate apps on this paas instance. Start with list_projects, then list_services / get_service. " +
				"IDs are UUIDs returned by the list tools. Write tools (deploy, rollback, restart, scale, set_env) need the developer role and are audited. " +
				"Secret values (environment variable values, passwords, tokens) are never returned.",
		})
	case "ping":
		return rpcOK(req.ID, map[string]any{})
	case "tools/list":
		tools := make([]map[string]any, 0, len(mcpTools))
		for _, t := range mcpTools {
			tools = append(tools, t.describe())
		}
		return rpcOK(req.ID, map[string]any{"tools": tools})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			return rpcFail(req.ID, rpcInvalidParams, "params.name is required")
		}
		tool := findMCPTool(p.Name)
		if tool == nil {
			return rpcFail(req.ID, rpcInvalidParams, "unknown tool: "+p.Name)
		}
		args, err := tool.validate(p.Arguments)
		if err != nil {
			return rpcFail(req.ID, rpcInvalidParams, "invalid arguments for "+tool.Name+": "+err.Error())
		}
		return rpcOK(req.ID, m.callTool(tool, args))
	default:
		return rpcFail(req.ID, rpcMethodNotFound, "method not found: "+req.Method)
	}
}

func isJSONNumber(b []byte) bool {
	var n json.Number
	return json.Unmarshal(b, &n) == nil
}

// callTool runs a tool and shapes its outcome as a CallToolResult. Failures
// the agent can act on (permission denied, not found, bad state) are tool
// errors (isError), not protocol errors.
func (m *mcpSession) callTool(tool *mcpTool, args mcpArgs) map[string]any {
	c := &mcpCall{mcpSession: m}
	ctx := m.r.Context()
	out, err := tool.Run(c, args)
	if tool.Write {
		target := c.auditTarget
		if err != nil {
			target += " (failed: " + truncate(err.Error(), 200) + ")"
		}
		m.s.store.Audit(context.WithoutCancel(ctx), &m.user.ID, c.auditTeam, "mcp."+tool.Name, strings.TrimSpace(target))
	}
	if err != nil {
		return map[string]any{"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true}
	}
	b, _ := json.MarshalIndent(out, "", " ")
	text := string(b)
	if len(text) > mcpToolResultKB<<10 {
		text = strings.ToValidUTF8(text[:mcpToolResultKB<<10], "") + "\n... (truncated)"
	}
	res := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": false}
	// structuredContent must be an object; lists come wrapped by the tools.
	if _, isMap := out.(map[string]any); isMap {
		res["structuredContent"] = out
	}
	return res
}

// mcpCall is one tool invocation.
type mcpCall struct {
	*mcpSession
	auditTeam   *string
	auditTarget string
}

// apiError is a non-2xx answer of the internal API.
type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s (HTTP %d)", e.Msg, e.Status)
}

// api performs a request against the REST API as the token's owner and
// decodes the JSON answer into out (when not nil).
func (c *mcpCall) api(method, path string, body, out any) error {
	var rd io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(c.r.Context(), method, "/api/v1"+path, rd)
	if err != nil {
		return err
	}
	req.Host = c.r.Host
	req.RemoteAddr = c.r.RemoteAddr
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	if xff := c.r.Header.Get("X-Forwarded-For"); xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	rec := httptest.NewRecorder()
	c.mux.ServeHTTP(rec, req)
	if rec.Code >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		if e.Error == "" {
			e.Error = http.StatusText(rec.Code)
		}
		return &apiError{Status: rec.Code, Msg: e.Error}
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return nil
}

// seg escapes one path segment.
func seg(id string) string { return url.PathEscape(id) }

// auditService records which service a write tool targets (for the audit log only).
func (c *mcpCall) auditService(id string) {
	c.auditTarget = id
	ctx := c.r.Context()
	svc, err := c.s.store.ServiceByID(ctx, id)
	if err != nil {
		return
	}
	proj, err := c.s.store.ProjectByID(ctx, svc.ProjectID)
	if err != nil {
		return
	}
	c.auditTeam = &proj.TeamID
	c.auditTarget = proj.Slug + "/" + svc.Slug
}

func (c *mcpCall) auditDeployment(id string) {
	c.auditTarget = id
	d, err := c.s.store.DeploymentByID(c.r.Context(), id)
	if err != nil {
		return
	}
	c.auditService(d.ServiceID)
	c.auditTarget += " deployment " + shortID(id)
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
