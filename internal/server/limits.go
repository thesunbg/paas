package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/thesunbg/paas/internal/store"
)

// Public sign-up lets anyone create an account, so teams of non-administrators
// get resource limits. Zero means unlimited. Administrators are never limited.

const settingTeamLimits = "team_limits"

type TeamLimits struct {
	MaxProjects    int `json:"max_projects"`
	MaxServices    int `json:"max_services"`
	MaxDatabases   int `json:"max_databases"`
	MaxMemoryMB    int `json:"max_memory_mb"` // per instance
	MaxReplicas    int `json:"max_replicas"`  // per service, including autoscaling
	MaxPerIPPerDay int `json:"max_signups_per_ip_per_day"`
	MaxTeams       int `json:"max_teams"` // owned per user, including the personal one
}

var defaultTeamLimits = TeamLimits{MaxProjects: 3, MaxServices: 5, MaxDatabases: 2, MaxMemoryMB: 512, MaxReplicas: 2, MaxPerIPPerDay: 3, MaxTeams: 3}

func (s *Server) teamLimits(ctx context.Context) TeamLimits {
	l := defaultTeamLimits
	if v, err := s.store.GetSetting(ctx, settingTeamLimits); err == nil {
		_ = json.Unmarshal([]byte(v), &l)
	}
	return l
}

// limited reports whether the caller is subject to team limits.
func limited(r *http.Request) bool {
	u := currentUser(r)
	return u == nil || !u.IsAdmin
}

type quotaError struct{ msg string }

func (e quotaError) Error() string { return e.msg }

func (s *Server) countTeam(ctx context.Context, teamID, what string) (int, error) {
	q := map[string]string{
		"projects":  `SELECT count(*) FROM projects WHERE team_id=$1`,
		"services":  `SELECT count(*) FROM services sv JOIN projects p ON p.id=sv.project_id WHERE p.team_id=$1 AND sv.parent_id IS NULL AND sv.type <> 'database' AND COALESCE(sv.spec->>'role', '') = ''`,
		"databases": `SELECT count(*) FROM databases d JOIN projects p ON p.id=d.project_id WHERE p.team_id=$1`,
	}[what]
	var n int
	err := s.store.Q(ctx).QueryRow(ctx, q, teamID).Scan(&n)
	return n, err
}

// checkCounts verifies that a team can add the given number of resources.
func (s *Server) checkCounts(r *http.Request, teamID string, projects, services, dbs int) error {
	if !limited(r) {
		return nil
	}
	l := s.teamLimits(r.Context())
	for _, c := range []struct {
		what  string
		add   int
		limit int
	}{{"projects", projects, l.MaxProjects}, {"services", services, l.MaxServices}, {"databases", dbs, l.MaxDatabases}} {
		if c.add == 0 || c.limit == 0 {
			continue
		}
		n, err := s.countTeam(r.Context(), teamID, c.what)
		if err != nil {
			return err
		}
		if n+c.add > c.limit {
			return quotaError{fmt.Sprintf("your team can have at most %d %s; ask an administrator for more", c.limit, c.what)}
		}
	}
	return nil
}

// checkServiceSize verifies the memory and replica limits of a service spec.
func (s *Server) checkServiceSize(r *http.Request, svc *store.Service) error {
	if !limited(r) {
		return nil
	}
	l := s.teamLimits(r.Context())
	if l.MaxMemoryMB > 0 && svc.Spec.Resources.MemoryLimitMB > l.MaxMemoryMB {
		return quotaError{fmt.Sprintf("memory is limited to %d MB per instance", l.MaxMemoryMB)}
	}
	if l.MaxReplicas > 0 {
		n := svc.Spec.Replicas
		if svc.Spec.Autoscale.Enabled && svc.Spec.Autoscale.Max > n {
			n = svc.Spec.Autoscale.Max
		}
		if n > l.MaxReplicas {
			return quotaError{fmt.Sprintf("services are limited to %d replicas", l.MaxReplicas)}
		}
	}
	return nil
}

// GET /api/v1/admin/settings
func (s *Server) handleGetSystemSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	signup, _ := s.store.GetSetting(r.Context(), settingAllowSignup)
	writeJSON(w, 200, map[string]any{"allow_signup": signup == "true", "limits": s.teamLimits(r.Context()), "quota": s.defaultQuota(r.Context())})
}

// PUT /api/v1/admin/settings
func (s *Server) handlePutSystemSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in struct {
		AllowSignup *bool       `json:"allow_signup"`
		Limits      *TeamLimits `json:"limits"`
		Quota       *TeamQuota  `json:"quota"` // default team quota
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	ctx := r.Context()
	if in.Limits != nil {
		l := *in.Limits
		for _, v := range []int{l.MaxProjects, l.MaxServices, l.MaxDatabases, l.MaxMemoryMB, l.MaxReplicas, l.MaxPerIPPerDay, l.MaxTeams} {
			if v < 0 {
				writeError(w, 400, "limits cannot be negative")
				return
			}
		}
		b, _ := json.Marshal(l)
		if err := s.store.SetSetting(ctx, settingTeamLimits, string(b)); err != nil {
			writeError(w, 500, err.Error())
			return
		}
	}
	if in.Quota != nil {
		if !in.Quota.valid() {
			writeError(w, 400, "quotas cannot be negative")
			return
		}
		b, _ := json.Marshal(in.Quota)
		if err := s.store.SetSetting(ctx, settingTeamQuota, string(b)); err != nil {
			writeError(w, 500, err.Error())
			return
		}
	}
	if in.AllowSignup != nil {
		v := "false"
		if *in.AllowSignup {
			v = "true"
		}
		if err := s.store.SetSetting(ctx, settingAllowSignup, v); err != nil {
			writeError(w, 500, err.Error())
			return
		}
	}
	u := currentUser(r)
	s.store.Audit(ctx, &u.ID, nil, "settings.update", "system")
	s.handleGetSystemSettings(w, r)
}
