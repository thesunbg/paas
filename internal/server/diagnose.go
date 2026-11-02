package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
)

// AI diagnosis of failed deployments: off until an administrator configures a
// provider. Logs are redacted before they leave the platform, the model only
// suggests (nothing is applied), and the answer is cached per deployment.

const settingAI = "ai_provider"

const (
	aiProviderAnthropic = "anthropic"
	aiProviderOpenAI    = "openai" // any OpenAI-compatible chat completions endpoint
	aiDefaultModel      = "claude-sonnet-5-5"
	aiDefaultAnthropic  = "https://api.anthropic.com"
	aiMaxPromptBytes    = 32 << 10 // logs and facts sent to the model
	aiMaxAnswerBytes    = 1 << 20
	aiMaxTokens         = 1024
	aiBuildLogLines     = 200
	aiRuntimeLogLines   = 80
	aiMaxLineLen        = 400
	aiPerUserPerHour    = 30
)

// aiTimeout bounds one model call; a variable so tests can shorten it.
var aiTimeout = 60 * time.Second

var aiClient = &http.Client{}

var diagnoseLimiter = &rateWindow{window: time.Hour, hits: map[string][]time.Time{}}

type aiConfig struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key"`
}

// aiSettings returns the configured provider, nil when AI is off.
func (s *Server) aiSettings(ctx context.Context) (*aiConfig, error) {
	enc, err := s.store.GetSetting(ctx, settingAI)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	plain, err := s.box.Decrypt(enc)
	if err != nil {
		return nil, fmt.Errorf("cannot read AI settings: %w", err)
	}
	var c aiConfig
	if err := json.Unmarshal([]byte(plain), &c); err != nil {
		return nil, err
	}
	if c.Provider == "" {
		return nil, nil
	}
	return &c, nil
}

func aiView(c *aiConfig) map[string]any {
	if c == nil {
		return map[string]any{"enabled": false, "provider": "", "model": "", "base_url": "", "has_key": false, "default_model": aiDefaultModel}
	}
	return map[string]any{"enabled": true, "provider": c.Provider, "model": c.Model, "base_url": c.BaseURL, "has_key": c.APIKey != "", "default_model": aiDefaultModel}
}

// GET /api/v1/admin/ai
func (s *Server) handleGetAI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	c, err := s.aiSettings(r.Context())
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, aiView(c))
}

// PUT /api/v1/admin/ai {provider, model, base_url, api_key}
// An empty provider turns AI off; an empty api_key keeps the stored one.
func (s *Server) handlePutAI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in aiConfig
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	ctx := r.Context()
	u := currentUser(r)
	in.Provider, in.Model, in.BaseURL = strings.TrimSpace(in.Provider), strings.TrimSpace(in.Model), strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	in.APIKey = strings.TrimSpace(in.APIKey)
	if in.Provider == "" {
		if err := s.store.DeleteSetting(ctx, settingAI); err != nil && !errors.Is(err, store.ErrNotFound) {
			writeError(w, 500, err.Error())
			return
		}
		s.store.Audit(ctx, &u.ID, nil, "ai.disable", "")
		writeJSON(w, 200, aiView(nil))
		return
	}
	old, _ := s.aiSettings(ctx)
	if in.APIKey == "" && old != nil && old.Provider == in.Provider {
		in.APIKey = old.APIKey
	}
	switch in.Provider {
	case aiProviderAnthropic:
		in.Model = firstNonEmpty(in.Model, aiDefaultModel)
		if in.BaseURL == aiDefaultAnthropic {
			in.BaseURL = ""
		}
		if in.APIKey == "" {
			writeError(w, 400, "an Anthropic API key is required")
			return
		}
	case aiProviderOpenAI:
		if in.BaseURL == "" || in.Model == "" {
			writeError(w, 400, "an OpenAI-compatible provider needs base_url (e.g. http://ollama:11434/v1) and model")
			return
		}
	default:
		writeError(w, 400, "provider must be anthropic or openai")
		return
	}
	if in.BaseURL != "" {
		if pu, err := url.Parse(in.BaseURL); err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" || pu.User != nil {
			writeError(w, 400, "base_url must be an http(s) URL without credentials")
			return
		}
	}
	b, _ := json.Marshal(in)
	if err := s.store.SetSetting(ctx, settingAI, s.box.MustEncrypt(string(b))); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	s.store.Audit(ctx, &u.ID, nil, "ai.update", in.Provider+" "+in.Model)
	writeJSON(w, 200, aiView(&in))
}

// diagnosis is what the model is asked to produce.
type diagnosis struct {
	Summary        string   `json:"summary"`
	LikelyCause    string   `json:"likely_cause"`
	SuggestedFixes []string `json:"suggested_fixes"`
}

type diagnosisOut struct {
	diagnosis
	DeploymentID string    `json:"deployment_id"`
	Provider     string    `json:"provider"`
	Model        string    `json:"model"`
	CreatedAt    time.Time `json:"created_at"`
	Cached       bool      `json:"cached"`
}

func (s *Server) cachedDiagnosis(ctx context.Context, deploymentID string) (*diagnosisOut, error) {
	out := diagnosisOut{DeploymentID: deploymentID, Cached: true}
	err := s.store.Q(ctx).QueryRow(ctx, `SELECT result, provider, model, created_at FROM deployment_diagnoses WHERE deployment_id=$1`, deploymentID).
		Scan(&out.diagnosis, &out.Provider, &out.Model, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GET /api/v1/deployments/{id}/diagnosis: the cached diagnosis, if any.
func (s *Server) handleGetDiagnosis(w http.ResponseWriter, r *http.Request) {
	d, _ := s.deploymentFor(w, r, "viewer")
	if d == nil {
		return
	}
	c, err := s.aiSettings(r.Context())
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	cached, err := s.cachedDiagnosis(r.Context(), d.ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"enabled": c != nil, "diagnosis": cached})
}

// POST /api/v1/deployments/{id}/diagnose {refresh}
func (s *Server) handleDiagnose(w http.ResponseWriter, r *http.Request) {
	d, svc := s.deploymentFor(w, r, "developer")
	if d == nil {
		return
	}
	var in struct {
		Refresh bool `json:"refresh"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	ctx := r.Context()
	cfg, err := s.aiSettings(ctx)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if cfg == nil {
		writeError(w, 400, "AI diagnosis is not configured; an administrator can set up a provider in Settings → AI")
		return
	}
	switch d.Status {
	case model.DeployQueued, model.DeployBuilding, model.DeployDeploying:
		writeError(w, 409, "the deployment is still in progress")
		return
	}
	if !in.Refresh {
		if cached, err := s.cachedDiagnosis(ctx, d.ID); err != nil {
			writeError(w, 500, err.Error())
			return
		} else if cached != nil {
			writeJSON(w, 200, cached)
			return
		}
	}
	u := currentUser(r)
	if !diagnoseLimiter.take(u.ID, aiPerUserPerHour) {
		writeError(w, 429, fmt.Sprintf("at most %d diagnoses per hour", aiPerUserPerHour))
		return
	}
	proj, err := s.store.ProjectByID(ctx, svc.ProjectID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	prompt := s.diagnosisPrompt(ctx, svc, d)
	callCtx, cancel := context.WithTimeout(ctx, aiTimeout)
	defer cancel()
	text, err := cfg.complete(callCtx, diagnoseSystemPrompt, prompt)
	if err != nil {
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			writeError(w, 504, "the AI provider did not answer in time")
			return
		}
		writeError(w, 502, "AI provider: "+err.Error())
		return
	}
	out := &diagnosisOut{diagnosis: parseDiagnosis(text), DeploymentID: d.ID, Provider: cfg.Provider, Model: cfg.Model, CreatedAt: time.Now().UTC()}
	if _, err := s.store.Q(ctx).Exec(ctx, `INSERT INTO deployment_diagnoses (deployment_id, result, provider, model, created_by) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (deployment_id) DO UPDATE SET result=EXCLUDED.result, provider=EXCLUDED.provider, model=EXCLUDED.model, created_by=EXCLUDED.created_by, created_at=now()`,
		d.ID, out.diagnosis, cfg.Provider, cfg.Model, u.ID); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	s.store.Audit(ctx, &u.ID, &proj.TeamID, "deployment.diagnose", svc.Slug+" "+shortID(d.ID))
	writeJSON(w, 200, out)
}

const diagnoseSystemPrompt = `You diagnose failed deployments on a self-hosted platform-as-a-service that builds apps from git (buildpack-style auto-detection or a Dockerfile) and runs them as containers behind a proxy.
You get facts about the service and deployment, the tail of the build/rollout log and, when available, the app's own output.
Find the most likely root cause. Be specific and concise; quote the decisive log line when there is one. Do not invent facts that are not in the input; say so when the logs are inconclusive.
Values shown as [REDACTED] were removed on purpose; never ask for them or guess them.
Suggest fixes the user can apply (code, configuration, port, start command, environment variable names, service size). Never suggest disabling security features.
Reply with only a JSON object, no markdown: {"summary": "one or two sentences", "likely_cause": "the root cause", "suggested_fixes": ["short concrete step", "..."]} with at most 5 fixes.`

// diagnosisPrompt gathers what the model needs, redacted and size-bounded.
func (s *Server) diagnosisPrompt(ctx context.Context, svc *store.Service, d *store.Deployment) string {
	red := s.serviceRedactor(ctx, svc)
	var facts strings.Builder
	fmt.Fprintf(&facts, "Service: %s (type %s)\n", svc.Slug, svc.Type)
	fmt.Fprintf(&facts, "Source: %s", svc.Source.Kind)
	if svc.Source.Kind == "image" {
		fmt.Fprintf(&facts, " %s", red.String(svc.Source.Image))
	} else if svc.Source.Dockerfile != "" || svc.Build.Builder == "dockerfile" {
		fmt.Fprintf(&facts, " (Dockerfile build)")
	}
	facts.WriteString("\n")
	if stack := firstNonEmpty(d.Detected, svc.Spec.Language); stack != "" {
		fmt.Fprintf(&facts, "Detected stack: %s\n", stack)
	}
	if svc.Build.BuildCmd != "" || svc.Build.StartCmd != "" {
		fmt.Fprintf(&facts, "Build command: %q, start command: %q\n", red.String(svc.Build.BuildCmd), red.String(svc.Build.StartCmd))
	}
	if svc.Source.RootDir != "" {
		fmt.Fprintf(&facts, "Root directory: %s\n", svc.Source.RootDir)
	}
	if svc.Spec.Port > 0 {
		fmt.Fprintf(&facts, "Expected port: %d (the app must listen on $PORT)\n", svc.Spec.Port)
	}
	if hc := svc.Spec.HealthCheck; hc.Type != "" {
		fmt.Fprintf(&facts, "Health check: %s %s\n", hc.Type, hc.Path)
	}
	if res := svc.Spec.Resources; res.MemoryLimitMB > 0 {
		fmt.Fprintf(&facts, "Memory limit: %d MB, CPU limit: %d millicores\n", res.MemoryLimitMB, res.CPULimitMillis)
	}
	fmt.Fprintf(&facts, "Deployment: status %s, trigger %s", d.Status, d.Trigger)
	if d.CommitSHA != "" {
		fmt.Fprintf(&facts, ", commit %s %q", shortSHA(d.CommitSHA), red.String(truncate(d.CommitMessage, 200)))
	}
	facts.WriteString("\n")
	if d.Error != "" {
		fmt.Fprintf(&facts, "Error reported by the platform: %s\n", red.String(truncate(d.Error, 2000)))
	}
	oom := strings.Contains(strings.ToLower(d.Error), "out of memory") || strings.Contains(d.Error, "OOM")
	var runtime []string
	for _, in := range s.instancesOf(ctx, svc.ID) {
		if in.DeploymentID == nil || *in.DeploymentID != d.ID {
			continue
		}
		fmt.Fprintf(&facts, "Instance %s: status %s, restarts %d, memory peak %d MB\n", shortID(in.ID), in.Status, in.Restarts, in.MemoryPeakMB)
		if lim := svc.Spec.Resources.MemoryLimitMB; lim > 0 && in.MemoryPeakMB >= lim*95/100 {
			oom = true
		}
		if s.nc != nil && len(runtime) == 0 && in.ContainerID != "" {
			var rep proto.LogsReply
			if err := s.requestAgent(ctx, in.NodeID, proto.CmdLogs, proto.LogsRequest{ContainerID: in.ContainerID, Tail: aiRuntimeLogLines}, &rep, 5*time.Second); err == nil {
				runtime = rep.Lines
			}
		}
	}
	if oom {
		facts.WriteString("Out-of-memory: the app was killed at (or ran very close to) its memory limit.\n")
	}
	build, _ := s.store.DeploymentLogTail(ctx, d.ID, aiBuildLogLines)
	clip := func(lines []string) []string {
		out := make([]string, len(lines))
		for i, l := range lines {
			out[i] = truncate(red.String(l), aiMaxLineLen)
		}
		return out
	}
	build, runtime = clip(build), clip(runtime)
	// Keep the end of each log (where failures are) within the budget.
	budget := aiMaxPromptBytes - facts.Len() - 200
	runtimeText := tailWithin(runtime, budget/3)
	buildText := tailWithin(build, budget-len(runtimeText))
	var b strings.Builder
	b.WriteString(facts.String())
	b.WriteString("\n== Build and rollout log (last lines) ==\n")
	b.WriteString(firstNonEmpty(buildText, "(empty)\n"))
	if len(runtime) > 0 {
		b.WriteString("\n== App output (last lines) ==\n")
		b.WriteString(runtimeText)
	}
	return b.String()
}

// tailWithin joins the last lines that fit in max bytes.
func tailWithin(lines []string, max int) string {
	size, start := 0, len(lines)
	for start > 0 && size+len(lines[start-1])+1 <= max {
		start--
		size += len(lines[start]) + 1
	}
	if start == len(lines) {
		return ""
	}
	return strings.Join(lines[start:], "\n") + "\n"
}

// parseDiagnosis reads the model's JSON answer, tolerating code fences and
// surrounding prose; unparseable answers become the summary.
func parseDiagnosis(text string) diagnosis {
	var d diagnosis
	if i, j := strings.Index(text, "{"), strings.LastIndex(text, "}"); i >= 0 && j > i {
		if json.Unmarshal([]byte(text[i:j+1]), &d) != nil {
			d = diagnosis{}
		}
	}
	if d.Summary == "" && d.LikelyCause == "" && len(d.SuggestedFixes) == 0 {
		d.Summary = strings.TrimSpace(text)
	}
	d.Summary = truncate(strings.TrimSpace(d.Summary), 2000)
	d.LikelyCause = truncate(strings.TrimSpace(d.LikelyCause), 2000)
	fixes := []string{}
	for _, f := range d.SuggestedFixes {
		if f = strings.TrimSpace(f); f != "" && len(fixes) < 8 {
			fixes = append(fixes, truncate(f, 1000))
		}
	}
	d.SuggestedFixes = fixes
	return d
}

// complete sends one system + user message and returns the model's text.
func (c *aiConfig) complete(ctx context.Context, system, user string) (string, error) {
	var (
		endpoint string
		body     any
		headers  = map[string]string{"Content-Type": "application/json"}
	)
	switch c.Provider {
	case aiProviderAnthropic:
		endpoint = firstNonEmpty(c.BaseURL, aiDefaultAnthropic) + "/v1/messages"
		headers["x-api-key"] = c.APIKey
		headers["anthropic-version"] = "2023-06-01"
		body = map[string]any{
			"model": firstNonEmpty(c.Model, aiDefaultModel), "max_tokens": aiMaxTokens, "system": system,
			"messages": []map[string]any{{"role": "user", "content": user}},
		}
	case aiProviderOpenAI:
		endpoint = c.BaseURL + "/chat/completions"
		if c.APIKey != "" {
			headers["Authorization"] = "Bearer " + c.APIKey
		}
		body = map[string]any{
			"model": c.Model, "max_tokens": aiMaxTokens, "temperature": 0,
			"messages": []map[string]any{{"role": "system", "content": system}, {"role": "user", "content": user}},
		}
	default:
		return "", fmt.Errorf("unknown provider %q", c.Provider)
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := aiClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL is known to the admin; keep the message short
		}
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, aiMaxAnswerBytes))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error json.RawMessage `json:"error"`
		}
		msg := strings.TrimSpace(string(data))
		if json.Unmarshal(data, &e) == nil && len(e.Error) > 0 {
			var obj struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(e.Error, &obj) == nil && obj.Message != "" {
				msg = obj.Message
			} else {
				msg = string(e.Error)
			}
		}
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, newRedactor(c.APIKey).String(truncate(msg, 300)))
	}
	if c.Provider == aiProviderAnthropic {
		var out struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			return "", fmt.Errorf("unexpected answer: %w", err)
		}
		var text strings.Builder
		for _, c := range out.Content {
			if c.Type == "text" {
				text.WriteString(c.Text)
			}
		}
		if text.Len() == 0 {
			return "", errors.New("the model returned no text")
		}
		return text.String(), nil
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("unexpected answer: %w", err)
	}
	if len(out.Choices) == 0 || out.Choices[0].Message.Content == "" {
		return "", errors.New("the model returned no text")
	}
	return out.Choices[0].Message.Content, nil
}
