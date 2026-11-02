package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/thesunbg/paas/internal/databases"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/resources"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/templates"
)

// Team quotas are budgets an instance administrator gives a team: how many
// projects, services and databases it may have and how much memory, CPU, disk,
// preview deployments and custom domains they may take in total. Unlike the
// sign-up limits (limits.go), which only bind non-administrators, a quota binds
// every change to the team, whoever makes it (administrators raise the quota
// instead), so bulk installs are held to it too.
//
// Accounting follows the scheduler: memory and CPU are the requests each
// instance reserves on a node (guaranteed: the limit, burstable: the request,
// best-effort: nothing), times the instances a service may run (replicas, or
// the autoscaling maximum; one for a cron job, none for a static site). Since
// best-effort instances reserve nothing, max_memory_limit_mb also caps the sum
// of memory limits: what the team may use if every instance bursts. Stopped and
// sleeping services keep counting: they can start again at any time. Disk is
// the storage quota of each database (twice with a high-availability standby);
// service volumes have no size and are not counted.
//
// Only changes that add usage are checked: creating, scaling or resizing up.
// Restarts, rescheduling, waking and starting stopped services never are, and
// anything that lowers usage is allowed even over quota. The checks do not lock,
// so two concurrent requests may overshoot by one change (like the limits).

const settingTeamQuota = "team_quota_default"

// TeamQuota is a team's budget; 0 means unlimited.
type TeamQuota struct {
	MaxProjects      int `json:"max_projects"`
	MaxServices      int `json:"max_services"`
	MaxDatabases     int `json:"max_databases"`
	MaxMemoryMB      int `json:"max_memory_mb"`       // sum of memory requests (scheduler accounting)
	MaxMemoryLimitMB int `json:"max_memory_limit_mb"` // sum of memory limits (worst case)
	MaxCPUMillis     int `json:"max_cpu_millis"`      // sum of CPU requests
	MaxDiskMB        int `json:"max_disk_mb"`         // database storage
	MaxPreviews      int `json:"max_previews"`        // PR preview deployments
	MaxDomains       int `json:"max_domains"`         // custom domains
}

// TeamUsage is what a team uses, in the same units as TeamQuota.
type TeamUsage struct {
	Projects      int `json:"projects"`
	Services      int `json:"services"`
	Databases     int `json:"databases"`
	MemoryMB      int `json:"memory_mb"`
	MemoryLimitMB int `json:"memory_limit_mb"`
	CPUMillis     int `json:"cpu_millis"`
	DiskMB        int `json:"disk_mb"`
	Previews      int `json:"previews"`
	Domains       int `json:"domains"`
}

func (u TeamUsage) plus(o TeamUsage) TeamUsage {
	return TeamUsage{u.Projects + o.Projects, u.Services + o.Services, u.Databases + o.Databases, u.MemoryMB + o.MemoryMB,
		u.MemoryLimitMB + o.MemoryLimitMB, u.CPUMillis + o.CPUMillis, u.DiskMB + o.DiskMB, u.Previews + o.Previews, u.Domains + o.Domains}
}

func (u TeamUsage) minus(o TeamUsage) TeamUsage {
	return u.plus(o.times(-1))
}

func (u TeamUsage) times(n int) TeamUsage {
	return TeamUsage{u.Projects * n, u.Services * n, u.Databases * n, u.MemoryMB * n, u.MemoryLimitMB * n, u.CPUMillis * n, u.DiskMB * n, u.Previews * n, u.Domains * n}
}

type quotaDim struct {
	name       string
	used, add  int
	limit      int
	format     func(int) string
	resizeable bool
}

func (q TeamQuota) dims(used, add TeamUsage) []quotaDim {
	n := strconv.Itoa
	return []quotaDim{
		{"projects", used.Projects, add.Projects, q.MaxProjects, n, false},
		{"services", used.Services, add.Services, q.MaxServices, n, false},
		{"databases", used.Databases, add.Databases, q.MaxDatabases, n, false},
		{"memory", used.MemoryMB, add.MemoryMB, q.MaxMemoryMB, mibStr, true},
		{"memory limits", used.MemoryLimitMB, add.MemoryLimitMB, q.MaxMemoryLimitMB, mibStr, true},
		{"CPU", used.CPUMillis, add.CPUMillis, q.MaxCPUMillis, cpuStr, true},
		{"disk", used.DiskMB, add.DiskMB, q.MaxDiskMB, mibStr, true},
		{"preview deployments", used.Previews, add.Previews, q.MaxPreviews, n, false},
		{"custom domains", used.Domains, add.Domains, q.MaxDomains, n, false},
	}
}

func (q TeamQuota) unlimited() bool { return q == TeamQuota{} }

// fits returns a quotaError naming the first dimension the change would push over the quota.
func (q TeamQuota) fits(used, add TeamUsage) error {
	for _, d := range q.dims(used, add) {
		if d.add <= 0 || d.limit <= 0 || d.used+d.add <= d.limit {
			continue
		}
		msg := fmt.Sprintf("quota exceeded: %s %s of %s used", d.name, d.format(d.used), d.format(d.limit))
		if d.resizeable {
			msg += fmt.Sprintf(", this needs %s more", d.format(d.add))
		}
		return quotaError{msg + "; ask an administrator to raise the team's quota"}
	}
	return nil
}

// mibStr formats MiB: "512 MiB", "3.5 GiB", "4 GiB".
func mibStr(mb int) string {
	if mb < 1024 {
		return fmt.Sprintf("%d MiB", mb)
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(mb)/1024), ".0") + " GiB"
}

func cpuStr(m int) string {
	return strconv.FormatFloat(float64(m)/1000, 'f', -1, 64) + " CPU"
}

// defaultQuota is the instance-wide quota of teams without an override.
func (s *Server) defaultQuota(ctx context.Context) TeamQuota {
	var q TeamQuota
	if v, err := s.store.GetSetting(ctx, settingTeamQuota); err == nil {
		_ = json.Unmarshal([]byte(v), &q)
	}
	return q
}

// teamQuota is the default with the team's override applied key by key.
func (s *Server) teamQuota(ctx context.Context, teamID string) TeamQuota {
	q := s.defaultQuota(ctx)
	if raw, err := s.store.TeamQuota(ctx, teamID); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &q)
	}
	return q
}

// serviceUsage is what one service reserves (see the accounting rules above).
func serviceUsage(typ string, spec model.ServiceSpec) TeamUsage {
	n := spec.Replicas
	switch {
	case typ == model.TypeStatic:
		n = 0
	case typ == model.TypeCron:
		n = 1
	case spec.Autoscale.Enabled && spec.Autoscale.Max > n:
		n = spec.Autoscale.Max
	}
	r := spec.Resources
	return TeamUsage{MemoryMB: r.MemoryRequestMB * n, MemoryLimitMB: r.MemoryLimitMB * n, CPUMillis: r.CPURequestMillis * n}
}

// dbUsage is what a database adds: itself, its storage quota and, when
// dedicated, its server (and HA standby), sized like databases.ServerSpec.
func dbUsage(db *store.Database) TeamUsage {
	u := TeamUsage{Databases: 1, DiskMB: quotaMB(db)}
	copies := 1
	if db.Options.HA {
		copies = 2
	}
	if db.Mode == "dedicated" {
		if p, ok := databases.PresetByName(db.SizePreset); ok {
			mem := max(p.MemoryMB, databases.Engines[db.Engine].MinMemoryMB)
			u.MemoryMB, u.MemoryLimitMB, u.CPUMillis = mem*copies, mem*copies, p.CPU/4*copies
		}
	}
	u.DiskMB *= copies
	return u
}

// newDBUsage is dbUsage for a database about to be created, with createDatabase's defaults.
func newDBUsage(in CreateDatabaseInput) TeamUsage {
	eng, ok := databases.Engines[in.Engine]
	if !ok {
		return TeamUsage{Databases: 1}
	}
	db := &store.Database{Engine: in.Engine, Mode: in.Mode, SizePreset: firstNonEmpty(in.Size, "micro")}
	if db.Mode == "" {
		db.Mode = "shared"
		if !eng.SharedOK {
			db.Mode = "dedicated"
		}
	}
	return dbUsage(db)
}

// planUsage is what installing a template / compose plan adds (databases reused through existing are free).
func planUsage(svcs []templates.Service, tdbs []templates.Database, existing map[string]string, domain string) TeamUsage {
	var u TeamUsage
	for _, ts := range svcs {
		typ := model.TypeWeb
		if ts.Type == "worker" || ts.Port == 0 {
			typ = model.TypeWorker
		}
		spec := model.ServiceSpec{Port: ts.Port, Replicas: 1}
		size, ok := resources.SizeByName(firstNonEmpty(ts.Size, "small"))
		if !ok {
			size, _ = resources.SizeByName("small")
		}
		resources.Apply(&spec, size, model.QoSBurstable)
		_ = resources.Normalize(typ, &spec)
		u = u.plus(serviceUsage(typ, spec))
		u.Services++
	}
	for _, d := range tdbs {
		if existing[d.Name] == "" {
			u = u.plus(newDBUsage(CreateDatabaseInput{Engine: d.Engine, Mode: d.Mode}))
		}
	}
	if domain != "" {
		u.Domains++
	}
	return u
}

// teamUsage adds up what a team uses now.
func (s *Server) teamUsage(ctx context.Context, teamID string) (TeamUsage, error) {
	var u TeamUsage
	var err error
	if u.Projects, err = s.countTeam(ctx, teamID, "projects"); err != nil {
		return u, err
	}
	if u.Services, err = s.countTeam(ctx, teamID, "services"); err != nil {
		return u, err
	}
	specs, err := s.store.TeamServiceSpecs(ctx, teamID)
	if err != nil {
		return u, err
	}
	for _, sv := range specs {
		u = u.plus(serviceUsage(sv.Type, sv.Spec))
		if sv.IsPreview {
			u.Previews++
		}
	}
	dbs, err := s.store.TeamDatabases(ctx, teamID)
	if err != nil {
		return u, err
	}
	for i := range dbs {
		u.Databases++
		// Memory and CPU of dedicated servers are counted with the services above.
		u.DiskMB += dbUsage(&dbs[i]).DiskMB
	}
	u.Domains, err = s.store.TeamCustomDomains(ctx, teamID)
	return u, err
}

// checkQuota refuses a change adding `add` to a team's usage beyond its quota
// (a quotaError). The system team (shared services) has no quota.
func (s *Server) checkQuota(ctx context.Context, teamID string, add TeamUsage) error {
	if teamID == s.systemTeamID {
		return nil
	}
	q := s.teamQuota(ctx, teamID)
	if q.unlimited() {
		return nil
	}
	used, err := s.teamUsage(ctx, teamID)
	if err != nil {
		return err
	}
	return q.fits(used, add)
}

// writeLimitError answers a failed limit or quota check: 403 for a quota, 500 otherwise.
func writeLimitError(w http.ResponseWriter, err error) {
	var qe quotaError
	if errors.As(err, &qe) {
		writeError(w, http.StatusForbidden, qe.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

// GET /api/v1/teams/{team}/quota (members): usage against the quota.
func (s *Server) handleGetTeamQuota(w http.ResponseWriter, r *http.Request) {
	team := r.PathValue("team")
	if !s.requireTeam(w, r, team, "viewer") {
		return
	}
	ctx := r.Context()
	raw, err := s.store.TeamQuota(ctx, team)
	if err != nil {
		writeError(w, 404, "team not found")
		return
	}
	usage, err := s.teamUsage(ctx, team)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"quota": s.teamQuota(ctx, team), "default": s.defaultQuota(ctx), "override": raw, "usage": usage})
}

// quotaOverride is a partial TeamQuota: keys left out use the instance default.
type quotaOverride struct {
	MaxProjects      *int `json:"max_projects,omitempty"`
	MaxServices      *int `json:"max_services,omitempty"`
	MaxDatabases     *int `json:"max_databases,omitempty"`
	MaxMemoryMB      *int `json:"max_memory_mb,omitempty"`
	MaxMemoryLimitMB *int `json:"max_memory_limit_mb,omitempty"`
	MaxCPUMillis     *int `json:"max_cpu_millis,omitempty"`
	MaxDiskMB        *int `json:"max_disk_mb,omitempty"`
	MaxPreviews      *int `json:"max_previews,omitempty"`
	MaxDomains       *int `json:"max_domains,omitempty"`
}

func (o quotaOverride) valid() bool {
	for _, v := range []*int{o.MaxProjects, o.MaxServices, o.MaxDatabases, o.MaxMemoryMB, o.MaxMemoryLimitMB, o.MaxCPUMillis, o.MaxDiskMB, o.MaxPreviews, o.MaxDomains} {
		if v != nil && *v < 0 {
			return false
		}
	}
	return true
}

func (q TeamQuota) valid() bool {
	for _, d := range q.dims(TeamUsage{}, TeamUsage{}) {
		if d.limit < 0 {
			return false
		}
	}
	return true
}

// PUT /api/v1/teams/{team}/quota {quota: {max_…}} (instance administrators). The
// object replaces the team's override; {} goes back to the instance default.
// Lowering a quota below the current usage is allowed: nothing is stopped, the
// team just cannot grow until it is back under.
func (s *Server) handlePutTeamQuota(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	team := r.PathValue("team")
	ctx := r.Context()
	if _, err := s.store.TeamQuota(ctx, team); err != nil {
		writeError(w, 404, "team not found")
		return
	}
	var in struct {
		Quota quotaOverride `json:"quota"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if !in.Quota.valid() {
		writeError(w, 400, "quotas cannot be negative")
		return
	}
	raw, _ := json.Marshal(in.Quota)
	if err := s.store.SetTeamQuota(ctx, team, raw); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	u := currentUser(r)
	s.store.Audit(ctx, &u.ID, &team, "team.quota", string(raw))
	s.handleGetTeamQuota(w, r)
}
