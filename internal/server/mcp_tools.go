package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/store"
)

// MCP tools. Every tool reaches data through mcpCall.api (the REST API, same
// token, same RBAC). Tools never return credentials: no token creation, no
// environment variable values, no database passwords, no join tokens, no 2FA.

type mcpParam struct {
	Name     string
	Type     string // id | string | integer | boolean | string_map | string_list
	Desc     string
	Required bool
	Min, Max int // integer bounds
}

type mcpTool struct {
	Name        string
	Title       string
	Description string
	Params      []mcpParam
	ReadOnly    bool
	Destructive bool
	Idempotent  bool
	OpenWorld   bool
	Write       bool // changes the platform: audited
	Run         func(c *mcpCall, a mcpArgs) (any, error)
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (t *mcpTool) describe() map[string]any {
	props := map[string]any{}
	required := []string{}
	for _, p := range t.Params {
		var sch map[string]any
		switch p.Type {
		case "id":
			sch = map[string]any{"type": "string", "pattern": idRe.String()}
		case "integer":
			sch = map[string]any{"type": "integer", "minimum": p.Min, "maximum": p.Max}
		case "string_map":
			sch = map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}}
		case "string_list":
			sch = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
		default:
			sch = map[string]any{"type": p.Type}
		}
		sch["description"] = p.Desc
		props[p.Name] = sch
		if p.Required {
			required = append(required, p.Name)
		}
	}
	schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	ann := map[string]any{"title": t.Title, "readOnlyHint": t.ReadOnly, "openWorldHint": t.OpenWorld}
	if !t.ReadOnly {
		ann["destructiveHint"] = t.Destructive
		ann["idempotentHint"] = t.Idempotent
	}
	return map[string]any{"name": t.Name, "title": t.Title, "description": t.Description, "inputSchema": schema, "annotations": ann}
}

type mcpArgs map[string]any

func (a mcpArgs) str(k string) string {
	v, _ := a[k].(string)
	return v
}

func (a mcpArgs) int(k string, def int) int {
	if v, ok := a[k].(int); ok {
		return v
	}
	return def
}

func (a mcpArgs) bool(k string, def bool) bool {
	if v, ok := a[k].(bool); ok {
		return v
	}
	return def
}

// validate checks arguments against the tool's parameters and normalizes
// numbers to int, maps to map[string]string and lists to []string.
func (t *mcpTool) validate(raw json.RawMessage) (mcpArgs, error) {
	in := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&in); err != nil {
			return nil, errors.New("arguments must be a JSON object")
		}
	}
	known := map[string]mcpParam{}
	for _, p := range t.Params {
		known[p.Name] = p
	}
	for k := range in {
		if _, ok := known[k]; !ok {
			return nil, fmt.Errorf("unknown argument %q", k)
		}
	}
	out := mcpArgs{}
	for _, p := range t.Params {
		v, ok := in[p.Name]
		if !ok || v == nil {
			if p.Required {
				return nil, fmt.Errorf("%s is required", p.Name)
			}
			continue
		}
		switch p.Type {
		case "id", "string":
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be a string", p.Name)
			}
			if p.Type == "id" && !idRe.MatchString(s) {
				return nil, fmt.Errorf("%s is not a valid id", p.Name)
			}
			if p.Required && s == "" {
				return nil, fmt.Errorf("%s is required", p.Name)
			}
			out[p.Name] = s
		case "integer":
			n, ok := v.(json.Number)
			i, err := strconv.Atoi(string(n))
			if !ok || err != nil {
				return nil, fmt.Errorf("%s must be an integer", p.Name)
			}
			if i < p.Min || i > p.Max {
				return nil, fmt.Errorf("%s must be between %d and %d", p.Name, p.Min, p.Max)
			}
			out[p.Name] = i
		case "boolean":
			b, ok := v.(bool)
			if !ok {
				return nil, fmt.Errorf("%s must be a boolean", p.Name)
			}
			out[p.Name] = b
		case "string_map":
			m, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s must be an object of strings", p.Name)
			}
			sm := map[string]string{}
			for k, x := range m {
				s, ok := x.(string)
				if !ok {
					return nil, fmt.Errorf("%s.%s must be a string", p.Name, k)
				}
				sm[k] = s
			}
			out[p.Name] = sm
		case "string_list":
			l, ok := v.([]any)
			if !ok {
				return nil, fmt.Errorf("%s must be an array of strings", p.Name)
			}
			sl := []string{}
			for _, x := range l {
				s, ok := x.(string)
				if !ok {
					return nil, fmt.Errorf("%s must be an array of strings", p.Name)
				}
				sl = append(sl, s)
			}
			out[p.Name] = sl
		}
	}
	return out, nil
}

func findMCPTool(name string) *mcpTool {
	for i := range mcpTools {
		if mcpTools[i].Name == name {
			return &mcpTools[i]
		}
	}
	return nil
}

var (
	pService    = mcpParam{Name: "service_id", Type: "id", Required: true, Desc: "Service id (from list_services)"}
	pProject    = mcpParam{Name: "project_id", Type: "id", Required: true, Desc: "Project id (from list_projects)"}
	pDeployment = mcpParam{Name: "deployment_id", Type: "id", Required: true, Desc: "Deployment id (from list_deployments or get_service)"}
)

var mcpTools []mcpTool

func init() {
	mcpTools = []mcpTool{
		{Name: "list_projects", Title: "List projects", ReadOnly: true, Run: mcpListProjects,
			Description: "List the teams you belong to and their projects, with service and database counts.",
			Params:      []mcpParam{{Name: "team_id", Type: "id", Desc: "Only this team"}}},
		{Name: "list_services", Title: "List services", ReadOnly: true, Run: mcpListServices,
			Description: "List the services (web apps, workers, static sites, cron jobs) of a project with status, URL and running instances.",
			Params:      []mcpParam{pProject}},
		{Name: "get_service", Title: "Get service", ReadOnly: true, Run: mcpGetService,
			Description: "Details of a service: status, source, size, instances, current deployment, recent deployments, domains and linked databases.",
			Params:      []mcpParam{pService}},
		{Name: "list_deployments", Title: "List deployments", ReadOnly: true, Run: mcpListDeployments,
			Description: "Recent deployments of a service, newest first.",
			Params:      []mcpParam{pService, {Name: "limit", Type: "integer", Min: 1, Max: 50, Desc: "How many (default 20)"}}},
		{Name: "get_deployment", Title: "Get deployment", ReadOnly: true, Run: mcpGetDeployment,
			Description: "Status, commit, timing and error of one deployment.",
			Params:      []mcpParam{pDeployment}},
		{Name: "get_build_logs", Title: "Get build logs", ReadOnly: true, Run: mcpBuildLogs,
			Description: "The last lines of a deployment's build and rollout log. Secrets are redacted.",
			Params:      []mcpParam{pDeployment, {Name: "tail", Type: "integer", Min: 1, Max: 2000, Desc: "Number of lines (default 200)"}}},
		{Name: "get_runtime_logs", Title: "Get runtime logs", ReadOnly: true, Run: mcpRuntimeLogs,
			Description: "The last lines of output of every running instance of a service. Secrets are redacted.",
			Params:      []mcpParam{pService, {Name: "tail", Type: "integer", Min: 1, Max: 1000, Desc: "Lines per instance (default 200)"}}},
		{Name: "get_metrics", Title: "Get metrics", ReadOnly: true, Run: mcpMetrics,
			Description: "Summary of recent CPU, memory, request rate and 5xx errors of a service.",
			Params:      []mcpParam{pService, {Name: "hours", Type: "integer", Min: 1, Max: 48, Desc: "Window in hours (default 1)"}}},
		{Name: "list_env_names", Title: "List environment variable names", ReadOnly: true, Run: mcpListEnv,
			Description: "Names of a service's environment variables. Values are never returned.",
			Params:      []mcpParam{pService}},
		{Name: "list_databases", Title: "List databases", ReadOnly: true, Run: mcpListDatabases,
			Description: "Managed databases of a project: engine, version, mode, size and status. Credentials are not returned.",
			Params:      []mcpParam{pProject}},
		{Name: "list_nodes", Title: "List nodes", ReadOnly: true, Run: mcpListNodes,
			Description: "Cluster nodes with capacity and usage (platform administrators only)."},
		{Name: "deploy_service", Title: "Deploy service", Write: true, Run: mcpDeploy,
			Description: "Queue a new build and deployment of a service from its source (latest commit of its branch, or a given commit). Requires the developer role.",
			Params:      []mcpParam{pService, {Name: "commit_sha", Type: "string", Desc: "Deploy this commit (hex) instead of the branch head"}}},
		{Name: "rollback_service", Title: "Roll back service", Write: true, Run: mcpRollback,
			Description: "Redeploy an earlier successful deployment's artifact (no rebuild). Without deployment_id, the deployment before the current one is used. Requires the developer role.",
			Params:      []mcpParam{pService, {Name: "deployment_id", Type: "id", Desc: "Deployment to roll back to"}}},
		{Name: "restart_service", Title: "Restart service", Write: true, Idempotent: true, Run: mcpRestart,
			Description: "Rolling restart of a service's instances with the current deployment. Requires the developer role.",
			Params:      []mcpParam{pService}},
		{Name: "scale_service", Title: "Scale service", Write: true, Idempotent: true, Run: mcpScale,
			Description: "Set the number of replicas of a service. Requires the developer role.",
			Params:      []mcpParam{pService, {Name: "replicas", Type: "integer", Required: true, Min: 1, Max: 50, Desc: "Replica count"}}},
		{Name: "set_env", Title: "Set environment variables", Write: true, Destructive: true, Run: mcpSetEnv,
			Description: "Set and/or remove environment variables of a service, then restart it (unless restart is false). Values are stored encrypted and never echoed back. Requires the developer role.",
			Params: []mcpParam{pService,
				{Name: "vars", Type: "string_map", Desc: "Variables to set, NAME: value"},
				{Name: "unset", Type: "string_list", Desc: "Variable names to remove"},
				{Name: "restart", Type: "boolean", Desc: "Restart the service to apply (default true)"}}},
		{Name: "diagnose_deployment", Title: "Diagnose deployment", OpenWorld: true, Run: mcpDiagnose,
			Description: "Ask the platform's configured AI model to explain a failed deployment from its redacted logs. Suggestions only; nothing is changed. Requires the developer role and an AI provider set up by an administrator.",
			Params:      []mcpParam{pDeployment, {Name: "refresh", Type: "boolean", Desc: "Ignore the cached diagnosis"}}},
	}
}

// Shapes returned to agents: the useful fields, never credentials.

func mcpServiceView(svc *store.Service) map[string]any {
	src := map[string]any{"kind": svc.Source.Kind}
	if svc.Source.Repo != "" {
		src["repo"] = newRedactor().String(svc.Source.Repo)
		src["branch"] = svc.Source.Branch
		src["auto_deploy"] = svc.Source.AutoDeploy
	}
	if svc.Source.Image != "" {
		src["image"] = svc.Source.Image
	}
	v := map[string]any{
		"id": svc.ID, "name": svc.Name, "slug": svc.Slug, "type": svc.Type, "status": svc.Status, "project_id": svc.ProjectID,
		"source": src, "replicas": svc.Spec.Replicas, "port": svc.Spec.Port,
		"memory_limit_mb": svc.Spec.Resources.MemoryLimitMB, "cpu_limit_millis": svc.Spec.Resources.CPULimitMillis,
		"current_deployment_id": svc.CurrentDeploymentID,
	}
	if svc.Spec.Language != "" {
		v["language"] = svc.Spec.Language
	}
	if svc.Spec.Autoscale.Enabled {
		v["autoscale"] = map[string]any{"min": svc.Spec.Autoscale.Min, "max": svc.Spec.Autoscale.Max}
	}
	if svc.Spec.Schedule != "" {
		v["schedule"] = svc.Spec.Schedule
	}
	if svc.ParentID != nil {
		v["preview_of"] = *svc.ParentID
	}
	return v
}

func mcpDeploymentView(d *store.Deployment) map[string]any {
	r := newRedactor()
	v := map[string]any{
		"id": d.ID, "service_id": d.ServiceID, "status": d.Status, "trigger": d.Trigger,
		"commit_sha": d.CommitSHA, "commit_message": r.String(d.CommitMessage), "branch": d.Branch,
		"created_at": d.CreatedAt, "started_at": d.StartedAt, "finished_at": d.FinishedAt,
		"can_rollback_to": (d.Status == model.DeployActive || d.Status == model.DeploySuperseded) && (d.Image != "" || d.StaticPath != ""),
	}
	if d.Detected != "" {
		v["detected_stack"] = d.Detected
	}
	if d.Error != "" {
		v["error"] = r.String(d.Error)
	}
	return v
}

func mcpListProjects(c *mcpCall, a mcpArgs) (any, error) {
	var teams []store.Team
	if err := c.api(http.MethodGet, "/teams", nil, &teams); err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, t := range teams {
		if id := a.str("team_id"); id != "" && t.ID != id {
			continue
		}
		var projs []struct {
			store.Project
			Services  int `json:"services"`
			Databases int `json:"databases"`
			Running   int `json:"running"`
		}
		entry := map[string]any{"id": t.ID, "name": t.Name, "role": t.Role}
		if err := c.api(http.MethodGet, "/teams/"+seg(t.ID)+"/projects", nil, &projs); err != nil {
			entry["error"] = err.Error()
		} else {
			list := []map[string]any{}
			for _, p := range projs {
				list = append(list, map[string]any{"id": p.ID, "name": p.Name, "slug": p.Slug, "services": p.Services, "running": p.Running, "databases": p.Databases})
			}
			entry["projects"] = list
		}
		out = append(out, entry)
	}
	if id := a.str("team_id"); id != "" && len(out) == 0 {
		return nil, errors.New("you are not a member of this team (HTTP 403)")
	}
	return map[string]any{"teams": out}, nil
}

type projectDetailOut struct {
	Project  store.Project `json:"project"`
	Services []struct {
		store.Service
		URL       string `json:"url"`
		Instances int    `json:"instances"`
	} `json:"services"`
	Databases []store.Database `json:"databases"`
}

func mcpListServices(c *mcpCall, a mcpArgs) (any, error) {
	var d projectDetailOut
	if err := c.api(http.MethodGet, "/projects/"+seg(a.str("project_id")), nil, &d); err != nil {
		return nil, err
	}
	list := []map[string]any{}
	for i := range d.Services {
		v := mcpServiceView(&d.Services[i].Service)
		v["url"] = d.Services[i].URL
		v["running_instances"] = d.Services[i].Instances
		list = append(list, v)
	}
	return map[string]any{"project": map[string]any{"id": d.Project.ID, "name": d.Project.Name, "slug": d.Project.Slug}, "services": list}, nil
}

func mcpListDatabases(c *mcpCall, a mcpArgs) (any, error) {
	var d projectDetailOut
	if err := c.api(http.MethodGet, "/projects/"+seg(a.str("project_id")), nil, &d); err != nil {
		return nil, err
	}
	list := []map[string]any{}
	for _, db := range d.Databases {
		v := map[string]any{
			"id": db.ID, "name": db.Name, "engine": db.Engine, "version": db.Version, "mode": db.Mode, "size": db.SizePreset,
			"status": db.Status, "db_name": db.DBName, "last_backup_at": db.LastBackupAt,
			"ha": db.Options.HA, "pitr": db.Options.PITR, "pooler": db.Options.Pooler, "public": db.Options.PublicPort > 0,
		}
		if db.Error != "" {
			v["error"] = newRedactor().String(db.Error)
		}
		list = append(list, v)
	}
	return map[string]any{"databases": list}, nil
}

func mcpGetService(c *mcpCall, a mcpArgs) (any, error) {
	var d struct {
		Service   store.Service  `json:"service"`
		Domains   []store.Domain `json:"domains"`
		URLs      []string       `json:"urls"`
		Instances []struct {
			store.Instance
			NodeName string `json:"node_name"`
		} `json:"instances"`
		Deployments []store.Deployment `json:"deployments"`
		Databases   []struct {
			store.DatabaseLink
			Name   string `json:"name"`
			Engine string `json:"engine"`
		} `json:"databases"`
		InternalHost string     `json:"internal_host"`
		NextRun      *time.Time `json:"next_run"`
	}
	if err := c.api(http.MethodGet, "/services/"+seg(a.str("service_id")), nil, &d); err != nil {
		return nil, err
	}
	out := mcpServiceView(&d.Service)
	out["urls"] = d.URLs
	out["internal_host"] = d.InternalHost
	hosts := []string{}
	for _, dom := range d.Domains {
		hosts = append(hosts, dom.Hostname)
	}
	out["domains"] = hosts
	insts := []map[string]any{}
	for _, in := range d.Instances {
		insts = append(insts, map[string]any{
			"id": in.ID, "node": in.NodeName, "status": in.Status, "deployment_id": in.DeploymentID, "restarts": in.Restarts,
			"memory_usage_mb": in.MemoryUsageMB, "memory_peak_mb": in.MemoryPeakMB, "cpu_usage_millis": in.CPUUsageMillis,
		})
	}
	out["instances"] = insts
	recent := []map[string]any{}
	for i := range d.Deployments {
		dv := mcpDeploymentView(&d.Deployments[i])
		if d.Service.CurrentDeploymentID != nil && d.Deployments[i].ID == *d.Service.CurrentDeploymentID {
			out["current_deployment"] = dv
		}
		if len(recent) < 5 {
			recent = append(recent, dv)
		}
	}
	out["recent_deployments"] = recent
	dbs := []map[string]any{}
	for _, l := range d.Databases {
		dbs = append(dbs, map[string]any{"database_id": l.DatabaseID, "name": l.Name, "engine": l.Engine, "env_name": l.EnvName})
	}
	out["linked_databases"] = dbs
	if d.NextRun != nil {
		out["next_run"] = d.NextRun
	}
	return out, nil
}

func mcpListDeployments(c *mcpCall, a mcpArgs) (any, error) {
	var deps []store.Deployment
	if err := c.api(http.MethodGet, "/services/"+seg(a.str("service_id"))+"/deployments", nil, &deps); err != nil {
		return nil, err
	}
	n := a.int("limit", 20)
	list := []map[string]any{}
	for i := range deps {
		if len(list) >= n {
			break
		}
		list = append(list, mcpDeploymentView(&deps[i]))
	}
	return map[string]any{"deployments": list}, nil
}

type deploymentOut struct {
	Deployment store.Deployment `json:"deployment"`
	Service    store.Service    `json:"service"`
}

func mcpGetDeployment(c *mcpCall, a mcpArgs) (any, error) {
	var d deploymentOut
	if err := c.api(http.MethodGet, "/deployments/"+seg(a.str("deployment_id")), nil, &d); err != nil {
		return nil, err
	}
	v := mcpDeploymentView(&d.Deployment)
	v["service"] = map[string]any{"id": d.Service.ID, "name": d.Service.Name, "slug": d.Service.Slug}
	v["is_current"] = d.Service.CurrentDeploymentID != nil && *d.Service.CurrentDeploymentID == d.Deployment.ID
	return v, nil
}

func mcpBuildLogs(c *mcpCall, a mcpArgs) (any, error) {
	// Authorize (viewer, as GET /deployments/{id}/logs) through the API, then
	// read the tail directly: the API endpoint pages from the start.
	var d deploymentOut
	if err := c.api(http.MethodGet, "/deployments/"+seg(a.str("deployment_id")), nil, &d); err != nil {
		return nil, err
	}
	ctx := c.r.Context()
	lines, err := c.s.store.DeploymentLogTail(ctx, d.Deployment.ID, a.int("tail", 200))
	if err != nil {
		return nil, err
	}
	svc, err := c.s.store.ServiceByID(ctx, d.Deployment.ServiceID)
	if err != nil {
		return nil, err
	}
	if lines == nil {
		lines = []string{}
	}
	return map[string]any{"deployment_id": d.Deployment.ID, "status": d.Deployment.Status, "lines": c.s.serviceRedactor(ctx, svc).Lines(lines)}, nil
}

func mcpRuntimeLogs(c *mcpCall, a mcpArgs) (any, error) {
	id := a.str("service_id")
	var out []struct {
		Instance string   `json:"instance"`
		Lines    []string `json:"lines"`
		Error    string   `json:"error,omitempty"`
	}
	if err := c.api(http.MethodGet, "/services/"+seg(id)+"/logs?tail="+strconv.Itoa(a.int("tail", 200)), nil, &out); err != nil {
		return nil, err
	}
	svc, err := c.s.store.ServiceByID(c.r.Context(), id)
	if err != nil {
		return nil, err
	}
	red := c.s.serviceRedactor(c.r.Context(), svc)
	list := []map[string]any{}
	for _, il := range out {
		e := map[string]any{"instance": il.Instance, "lines": red.Lines(il.Lines)}
		if il.Error != "" {
			e["error"] = red.String(il.Error)
		}
		list = append(list, e)
	}
	return map[string]any{"instances": list}, nil
}

func mcpMetrics(c *mcpCall, a mcpArgs) (any, error) {
	hours := a.int("hours", 1)
	var m struct {
		Points   []metricPoint `json:"points"`
		RPS5m    float64       `json:"rps_5m"`
		Errors5m int64         `json:"errors_5m"`
	}
	if err := c.api(http.MethodGet, "/services/"+seg(a.str("service_id"))+"/metrics?hours="+strconv.Itoa(hours), nil, &m); err != nil {
		return nil, err
	}
	out := map[string]any{"window_hours": hours, "samples": len(m.Points), "rps_last_5m": round2(m.RPS5m), "errors_5xx_last_5m": m.Errors5m}
	if len(m.Points) == 0 {
		return out, nil
	}
	var cpuSum, memSum, req, errs, cpuMax, memMax int
	for _, p := range m.Points {
		cpuSum += p.CPUMillis
		memSum += p.MemoryMB
		req += p.Requests
		errs += p.Errors5xx
		cpuMax = max(cpuMax, p.CPUMillis)
		memMax = max(memMax, p.MemoryMB)
	}
	n := len(m.Points)
	last := m.Points[n-1]
	out["latest"] = map[string]any{"at": last.TS, "instances": last.Instances, "cpu_millis": last.CPUMillis, "memory_mb": last.MemoryMB}
	out["cpu_millis"] = map[string]any{"avg": cpuSum / n, "max": cpuMax}
	out["memory_mb"] = map[string]any{"avg": memSum / n, "max": memMax}
	span := last.TS.Sub(m.Points[0].TS).Seconds() + 60 // samples are per minute
	out["requests"] = map[string]any{"total": req, "avg_rps": round2(float64(req) / span), "errors_5xx": errs}
	return out, nil
}

func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }

func mcpListEnv(c *mcpCall, a mcpArgs) (any, error) {
	var e struct {
		Vars   map[string]string `json:"vars"`
		Linked map[string]string `json:"linked"`
	}
	if err := c.api(http.MethodGet, "/services/"+seg(a.str("service_id"))+"/env", nil, &e); err != nil {
		return nil, err
	}
	return map[string]any{"names": sortedKeys(e.Vars), "from_linked_databases": sortedKeys(e.Linked)}, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func mcpListNodes(c *mcpCall, _ mcpArgs) (any, error) {
	var n struct {
		Nodes []struct {
			store.Node
			CommittedMemoryMB int `json:"committed_memory_mb"`
			CapacityMemoryMB  int `json:"capacity_memory_mb"`
			CommittedCPU      int `json:"committed_cpu_millis"`
			CapacityCPU       int `json:"capacity_cpu_millis"`
			Instances         int `json:"instances"`
			UsedByAppsMB      int `json:"used_by_apps_mb"`
		} `json:"nodes"`
		Sleeping int `json:"sleeping_services"`
	}
	if err := c.api(http.MethodGet, "/nodes", nil, &n); err != nil {
		return nil, err
	}
	list := []map[string]any{}
	for _, x := range n.Nodes {
		list = append(list, map[string]any{
			"id": x.ID, "name": x.Name, "status": x.Status, "roles": x.Roles, "address": x.Address, "version": x.Version,
			"cpu_cores": x.CPUCores, "memory_mb": x.MemoryMB, "instances": x.Instances, "used_by_apps_mb": x.UsedByAppsMB,
			"committed_memory_mb": x.CommittedMemoryMB, "capacity_memory_mb": x.CapacityMemoryMB,
			"committed_cpu_millis": x.CommittedCPU, "capacity_cpu_millis": x.CapacityCPU, "last_seen_at": x.LastSeenAt, "usage": x.Usage,
		})
	}
	return map[string]any{"nodes": list, "sleeping_services": n.Sleeping}, nil
}

func mcpDeploy(c *mcpCall, a mcpArgs) (any, error) {
	id := a.str("service_id")
	c.auditService(id)
	body := map[string]string{}
	if sha := a.str("commit_sha"); sha != "" {
		body["commit_sha"] = sha
		c.auditTarget += " @" + sha
	}
	var d store.Deployment
	if err := c.api(http.MethodPost, "/services/"+seg(id)+"/deploy", body, &d); err != nil {
		return nil, err
	}
	return map[string]any{"deployment": mcpDeploymentView(&d), "next": "Poll get_deployment until the status is active or failed; read get_build_logs on failure."}, nil
}

func mcpRollback(c *mcpCall, a mcpArgs) (any, error) {
	id := a.str("service_id")
	c.auditService(id)
	var deps []store.Deployment
	if err := c.api(http.MethodGet, "/services/"+seg(id)+"/deployments", nil, &deps); err != nil {
		return nil, err
	}
	target := a.str("deployment_id")
	if target != "" {
		found := false
		for _, d := range deps {
			found = found || d.ID == target
		}
		if !found {
			return nil, errors.New("deployment_id is not one of this service's recent deployments")
		}
	} else {
		// The newest usable deployment older than the active one.
		seenActive := false
		for _, d := range deps {
			if d.Status == model.DeployActive {
				seenActive = true
				continue
			}
			if seenActive && d.Status == model.DeploySuperseded && (d.Image != "" || d.StaticPath != "") {
				target = d.ID
				break
			}
		}
		if target == "" {
			return nil, errors.New("no previous deployment to roll back to")
		}
	}
	c.auditTarget += " to " + shortID(target)
	var nd store.Deployment
	if err := c.api(http.MethodPost, "/deployments/"+seg(target)+"/rollback", nil, &nd); err != nil {
		return nil, err
	}
	return map[string]any{"rolled_back_to": target, "deployment": mcpDeploymentView(&nd)}, nil
}

func mcpRestart(c *mcpCall, a mcpArgs) (any, error) {
	id := a.str("service_id")
	c.auditService(id)
	var d store.Deployment
	if err := c.api(http.MethodPost, "/services/"+seg(id)+"/restart", nil, &d); err != nil {
		return nil, err
	}
	return map[string]any{"deployment": mcpDeploymentView(&d)}, nil
}

func mcpScale(c *mcpCall, a mcpArgs) (any, error) {
	id := a.str("service_id")
	c.auditService(id)
	n := a.int("replicas", 1)
	c.auditTarget += fmt.Sprintf(" to %d", n)
	var out map[string]any
	if err := c.api(http.MethodPost, "/services/"+seg(id)+"/scale", map[string]int{"replicas": n}, &out); err != nil {
		return nil, err
	}
	return map[string]any{"replicas": n}, nil
}

func mcpSetEnv(c *mcpCall, a mcpArgs) (any, error) {
	id := a.str("service_id")
	c.auditService(id)
	vars, _ := a["vars"].(map[string]string)
	unset, _ := a["unset"].([]string)
	if len(vars) == 0 && len(unset) == 0 {
		return nil, errors.New("give vars to set and/or unset names to remove")
	}
	if vars == nil {
		vars = map[string]string{}
	}
	names := sortedKeys(vars)
	c.auditTarget += fmt.Sprintf(" set %v unset %v", names, unset) // names only, never values
	for _, k := range unset {
		if !envKeyRe.MatchString(k) {
			return nil, fmt.Errorf("invalid environment variable name %q", k)
		}
	}
	// Removals first; the PUT below (which also checks the role) restarts once to apply everything.
	for _, k := range unset {
		if err := c.api(http.MethodDelete, "/services/"+seg(id)+"/env/"+seg(k), nil, nil); err != nil {
			return nil, err
		}
	}
	restart := a.bool("restart", true)
	var out struct {
		Deployment *store.Deployment `json:"deployment"`
	}
	if err := c.api(http.MethodPut, "/services/"+seg(id)+"/env", map[string]any{"vars": vars, "restart": restart}, &out); err != nil {
		return nil, err
	}
	res := map[string]any{"status": "saved", "set": names, "unset": unset}
	if out.Deployment != nil {
		res["deployment_id"] = out.Deployment.ID
	}
	return res, nil
}

func mcpDiagnose(c *mcpCall, a mcpArgs) (any, error) {
	var out map[string]any
	err := c.api(http.MethodPost, "/deployments/"+seg(a.str("deployment_id"))+"/diagnose", map[string]bool{"refresh": a.bool("refresh", false)}, &out)
	return out, err
}
