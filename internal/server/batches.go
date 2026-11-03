package server

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/resources"
	"github.com/thesunbg/paas/internal/scheduler"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/templates"
	"github.com/thesunbg/paas/internal/util"
)

// Batches install one template many times at once: a class of 30 learners each
// with their own n8n, a set of customers. Two modes:
//
//   - instances: all copies in one project (<name>-01, -02…). Fits apps with a
//     login of their own (n8n, WordPress…): learners only get a link.
//   - accounts: one dashboard account per learner (by email, with a first
//     password) owning a project with their copy. They see and manage only
//     theirs.
//
// Copies are created in the background a few at a time: each group starts only
// once the previous copies are up (or failed), so a class of 30 does not boot 30
// apps, 30 databases and 30 first-run migrations at once. With sleep (the
// default) every copy goes to sleep as soon as it is up and wakes on its first
// request, so only the copies in use hold memory. Deleting a batch removes
// everything it created (in accounts mode, the accounts too).

const (
	batchMax         = 200
	batchParallelMax = 10
	batchParallelDef = 2
	batchIdleDef     = 30              // minutes without a request before a copy sleeps
	batchStaleAfter  = 2 * time.Minute // a creating batch whose runner went quiet this long is resumed
)

var (
	batchCopyTimeout = 15 * time.Minute // a copy that is not up by then no longer holds the next ones back
	batchPoll        = 2 * time.Second  // tests shorten both
)

type batchLearner struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type batchInput struct {
	Name      string                       `json:"name"`
	Template  string                       `json:"template"`
	Mode      string                       `json:"mode"` // instances | accounts
	Count     int                          `json:"count"`
	Learners  []batchLearner               `json:"learners"`   // accounts mode (count is their number)
	TeamID    string                       `json:"team_id"`    // instances mode: team of the new project
	ProjectID string                       `json:"project_id"` // instances mode: an existing project instead
	Prefix    string                       `json:"prefix"`     // name of the copies (default: the template's)
	Env       map[string]map[string]string `json:"env"`
	Force     bool                         `json:"force"` // create even when capacity looks short
	// Pacing. Parallel copies start at the same time (default 2, at most 10).
	// Sleep (default on) puts each copy to sleep once it is up and after
	// IdleMinutes (default 30) without a request; it wakes on the next request.
	Parallel    int   `json:"parallel"`
	Sleep       *bool `json:"sleep"`
	IdleMinutes int   `json:"idle_minutes"`
}

// pacing returns the batch's start concurrency and sleep policy, with defaults applied.
func (in *batchInput) pacing() (int, model.SleepPolicy) {
	parallel := in.Parallel
	if parallel <= 0 {
		parallel = batchParallelDef
	}
	if parallel > batchParallelMax {
		parallel = batchParallelMax
	}
	sl := model.SleepPolicy{Enabled: in.Sleep == nil || *in.Sleep}
	if sl.Enabled {
		sl.IdleMinutes = in.IdleMinutes
		if sl.IdleMinutes <= 0 {
			sl.IdleMinutes = batchIdleDef
		}
	}
	return parallel, sl
}

// batchEstimate is what N copies need against what the cluster has free.
type batchEstimate struct {
	PerCopyRequestMB int  `json:"per_copy_request_mb"` // reserved by the scheduler
	PerCopyLimitMB   int  `json:"per_copy_limit_mb"`   // worst case
	NeedRequestMB    int  `json:"need_request_mb"`     // reserved once created (sleep: databases + the copies starting at once)
	NeedLimitMB      int  `json:"need_limit_mb"`       // worst case with every copy awake and busy
	StartupPeakMB    int  `json:"startup_peak_mb"`     // the copies starting at once at their limit, on top of the rest
	FreeScheduleMB   int  `json:"free_schedulable_mb"` // capacity minus committed, online workers
	FreeMemoryMB     int  `json:"free_memory_mb"`      // physical memory available now (0: not reported yet)
	Parallel         int  `json:"parallel"`
	Sleep            bool `json:"sleep"`
	MaxAwake         int  `json:"max_awake,omitempty"` // sleep: copies that can be awake at the same time
	Fits             bool `json:"fits"`
	// Blocked: even force is refused, because the memory the copies reserve plus their
	// start-up peak is more than the nodes physically have free: creating them would
	// push the node out of memory (the kernel then kills apps, or the control plane's
	// database). Turn sleep on, create fewer copies, or add a node.
	Blocked bool   `json:"blocked"`
	Warning string `json:"warning,omitempty"`
}

func (s *Server) estimateBatch(ctx context.Context, tpl *templates.Template, n, parallel int, sleep bool) batchEstimate {
	freeSchedule, freeMemory := 0, 0
	committed, _ := s.store.NodeCommitted(ctx)
	nodes, _ := s.store.Nodes(ctx)
	for i := range nodes {
		node := &nodes[i]
		if node.Status != "online" || !hasRole(node.Roles, "worker") {
			continue
		}
		capMB, _ := scheduler.Capacity(node, model.QoSBurstable)
		if free := capMB - committed[node.ID][0]; free > 0 {
			freeSchedule += free
		}
		freeMemory += node.Usage.MemoryAvailableMB
	}
	return computeBatchEstimate(tpl, n, parallel, sleep, freeSchedule, freeMemory)
}

// computeBatchEstimate is the arithmetic of estimateBatch, given what the workers have free.
func computeBatchEstimate(tpl *templates.Template, n, parallel int, sleep bool, freeSchedule, freeMemory int) batchEstimate {
	e := batchEstimate{Parallel: parallel, Sleep: sleep, FreeScheduleMB: freeSchedule, FreeMemoryMB: freeMemory}
	svcReq, svcLim, dbReq, dbLim := 0, 0, 0, 0
	for _, ts := range tpl.Services {
		size, ok := resources.SizeByName(firstNonEmpty(ts.Size, "small"))
		if !ok {
			size, _ = resources.SizeByName("small")
		}
		svcReq += size.MemoryRequestMB
		svcLim += size.MemoryLimitMB
	}
	for _, d := range tpl.Databases {
		if d.Mode == "dedicated" {
			dbReq += 96 // a micro server
			dbLim += 256
		}
	}
	e.PerCopyRequestMB, e.PerCopyLimitMB = svcReq+dbReq, svcLim+dbLim
	starting := min(parallel, n)
	e.StartupPeakMB = starting * (svcLim - svcReq)
	if sleep {
		// Asleep, a copy holds only its databases; the ones starting hold their services too.
		e.NeedRequestMB = dbReq*n + svcReq*starting
	} else {
		e.NeedRequestMB = e.PerCopyRequestMB * n
	}
	e.NeedLimitMB = e.PerCopyLimitMB * n
	e.Fits = e.NeedRequestMB <= e.FreeScheduleMB
	if sleep && svcReq > 0 {
		e.MaxAwake = min(n, max(0, (e.FreeScheduleMB-dbReq*n)/svcReq))
	}
	// Physical memory is only known once the agents report it; without it, keep to the reservation check.
	e.Blocked = e.FreeMemoryMB > 0 && e.NeedRequestMB+e.StartupPeakMB > e.FreeMemoryMB
	switch {
	case e.Blocked:
		e.Warning = fmt.Sprintf("%d copies reserve %s and need up to %s more while %d start at once, but the nodes have only %s of memory free: this would run them out of memory. Turn sleep on, create fewer copies, or add a node",
			n, mbStr(e.NeedRequestMB), mbStr(e.StartupPeakMB), starting, mbStr(e.FreeMemoryMB))
	case !e.Fits:
		e.Warning = fmt.Sprintf("%d copies reserve %s but only %s is free on the worker nodes: some would not start", n, mbStr(e.NeedRequestMB), mbStr(e.FreeScheduleMB))
	case sleep && e.MaxAwake < n:
		e.Warning = fmt.Sprintf("The copies sleep when unused; about %d of the %d can be awake at the same time, a copy that cannot wake says so", e.MaxAwake, n)
	case !sleep && e.NeedLimitMB > e.FreeMemoryMB:
		e.Warning = fmt.Sprintf("They fit, but if every copy is busy at once they may use up to %s and the nodes have %s free: fine for a class, tight for heavy use", mbStr(e.NeedLimitMB), mbStr(e.FreeMemoryMB))
	}
	return e
}

// GET /api/v1/batches/estimate?template=&count=
func (s *Server) handleBatchEstimate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	tpl, ok := templates.ByID(r.URL.Query().Get("template"))
	if !ok {
		writeError(w, 404, "unknown template")
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("count"))
	if n < 1 || n > batchMax {
		writeError(w, 400, fmt.Sprintf("count must be 1 to %d", batchMax))
		return
	}
	in := batchInput{IdleMinutes: 0}
	in.Parallel, _ = strconv.Atoi(r.URL.Query().Get("parallel"))
	if v := r.URL.Query().Get("sleep"); v != "" {
		b := v == "true" || v == "1"
		in.Sleep = &b
	}
	parallel, sl := in.pacing()
	writeJSON(w, 200, s.estimateBatch(r.Context(), tpl, n, parallel, sl.Enabled))
}

// POST /api/v1/batches (platform administrators)
func (s *Server) handleCreateBatch(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in batchInput
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	ctx := r.Context()
	tpl, ok := templates.ByID(in.Template)
	if !ok {
		writeError(w, 404, "unknown template")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		writeError(w, 400, "name is required (e.g. the class)")
		return
	}
	labels, emails := []string{}, []string{}
	switch in.Mode {
	case "instances":
		if in.Count < 1 || in.Count > batchMax {
			writeError(w, 400, fmt.Sprintf("count must be 1 to %d", batchMax))
			return
		}
		prefix := util.Slugify(firstNonEmpty(in.Prefix, tpl.Services[0].Name))
		width := len(strconv.Itoa(in.Count))
		if width < 2 {
			width = 2
		}
		for i := 1; i <= in.Count; i++ {
			labels = append(labels, fmt.Sprintf("%s-%0*d", prefix, width, i))
			emails = append(emails, "")
		}
	case "accounts":
		if len(in.Learners) < 1 || len(in.Learners) > batchMax {
			writeError(w, 400, fmt.Sprintf("add 1 to %d learners (name and email)", batchMax))
			return
		}
		seen := map[string]bool{}
		for i, l := range in.Learners {
			addr, err := mail.ParseAddress(strings.TrimSpace(l.Email))
			if err != nil {
				writeError(w, 400, fmt.Sprintf("line %d: invalid email %q", i+1, l.Email))
				return
			}
			e := strings.ToLower(addr.Address)
			if seen[e] {
				writeError(w, 400, fmt.Sprintf("line %d: %s appears twice", i+1, e))
				return
			}
			seen[e] = true
			if _, err := s.store.UserByEmail(ctx, e); err == nil {
				writeError(w, 409, fmt.Sprintf("line %d: an account with %s already exists", i+1, e))
				return
			}
			labels = append(labels, firstNonEmpty(strings.TrimSpace(l.Name), strings.SplitN(e, "@", 2)[0]))
			emails = append(emails, e)
		}
	default:
		writeError(w, 400, "mode must be instances or accounts")
		return
	}
	for _, ts := range tpl.Services {
		for _, input := range ts.Inputs {
			if input.Required && strings.TrimSpace(in.Env[ts.Name][input.Key]) == "" {
				writeError(w, 400, fmt.Sprintf("%s is required (the same value is used for every copy)", input.Key))
				return
			}
		}
	}
	if in.Parallel < 0 || in.Parallel > batchParallelMax {
		writeError(w, 400, fmt.Sprintf("parallel must be 1 to %d", batchParallelMax))
		return
	}
	if in.IdleMinutes < 0 || in.IdleMinutes > 24*60 {
		writeError(w, 400, "idle_minutes must be 0 to 1440")
		return
	}
	parallel, sleep := in.pacing()
	est := s.estimateBatch(ctx, tpl, len(labels), parallel, sleep.Enabled)
	if est.Blocked {
		writeJSON(w, 409, map[string]any{"error": est.Warning, "estimate": est})
		return
	}
	if !est.Fits && !in.Force {
		writeJSON(w, 409, map[string]any{"error": est.Warning, "estimate": est})
		return
	}

	// Quotas are checked for the whole batch up front: copies are created in the
	// background, and a class half installed is worse than a clear refusal.
	perCopy := planUsage(tpl.Services, tpl.Databases, nil, "")
	if in.Mode == "accounts" {
		// Each learner gets a new team (with the default quota) holding one project and one copy.
		if err := s.defaultQuota(ctx).fits(TeamUsage{}, perCopy.plus(TeamUsage{Projects: 1})); err != nil {
			writeError(w, 403, "each learner's team: "+err.Error())
			return
		}
	}
	u := currentUser(r)
	var projectID *string
	if in.Mode == "instances" {
		var p *store.Project
		var err error
		need := perCopy.times(len(labels))
		if in.ProjectID != "" {
			if p, err = s.store.ProjectByID(ctx, in.ProjectID); err != nil {
				writeError(w, 404, "project not found")
				return
			}
			if err := s.checkQuota(ctx, p.TeamID, need); err != nil {
				writeLimitError(w, err)
				return
			}
		} else {
			team := firstNonEmpty(in.TeamID, s.firstTeam(ctx, u.ID))
			if team == "" {
				writeError(w, 400, "team_id is required")
				return
			}
			if err := s.checkQuota(ctx, team, need.plus(TeamUsage{Projects: 1})); err != nil {
				writeLimitError(w, err)
				return
			}
			if p, err = s.store.CreateProject(ctx, team, in.Name, s.uniqueProjectSlug(ctx, in.Name)); err != nil {
				writeError(w, 500, err.Error())
				return
			}
		}
		projectID = &p.ID
	}
	var id string
	// The input is kept (encrypted: it holds the template inputs) so another control
	// plane can finish the batch if this one stops half way.
	optsJSON, _ := json.Marshal(in)
	if err := s.store.Q(ctx).QueryRow(ctx, `INSERT INTO batches (name, template, mode, project_id, created_by, options_enc) VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		in.Name, tpl.ID, in.Mode, projectID, u.ID, s.box.MustEncrypt(string(optsJSON))).Scan(&id); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	for i := range labels {
		if _, err := s.store.Q(ctx).Exec(ctx, `INSERT INTO batch_items (batch_id, idx, label, email) VALUES ($1,$2,$3,$4)`, id, i, labels[i], emails[i]); err != nil {
			writeError(w, 500, err.Error())
			return
		}
	}
	s.store.Audit(ctx, &u.ID, nil, "batch.create", fmt.Sprintf("%s: %d × %s (%s)", in.Name, len(labels), tpl.ID, in.Mode))
	go s.runBatch(context.WithoutCancel(ctx), id, tpl, in)
	writeJSON(w, 202, map[string]any{"id": id, "items": len(labels), "estimate": est})
}

func (s *Server) firstTeam(ctx context.Context, userID string) string {
	teams, err := s.store.TeamsForUser(ctx, userID)
	if err != nil || len(teams) == 0 {
		return ""
	}
	return teams[0].ID
}

// runBatch creates the copies that are not created yet, `parallel` at a time: a
// slot is freed when its copy is up (asleep with sleep on), failed, or past
// batchCopyTimeout. It stops when the batch is being deleted, and keeps the batch's
// heartbeat fresh so that a stale "creating" batch can be told apart and resumed.
func (s *Server) runBatch(ctx context.Context, id string, tpl *templates.Template, in batchInput) {
	parallel, sleep := in.pacing()
	stopBeat := make(chan struct{})
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopBeat:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				_, _ = s.store.Q(ctx).Exec(ctx, `UPDATE batches SET heartbeat_at=now() WHERE id=$1`, id)
			}
		}
	}()
	defer close(stopBeat)

	rows, err := s.store.Q(ctx).Query(ctx, `SELECT idx, label, email, user_id, project_id FROM batch_items
		WHERE batch_id=$1 AND service_id IS NULL AND error='' ORDER BY idx`, id)
	if err != nil {
		return
	}
	type item struct {
		idx           int
		label, email  string
		user, project *string
	}
	var items []item
	for rows.Next() {
		var it item
		if rows.Scan(&it.idx, &it.label, &it.email, &it.user, &it.project) == nil {
			items = append(items, it)
		}
	}
	rows.Close()
	var shared *store.Project
	if in.Mode == "instances" {
		var pid string
		_ = s.store.Q(ctx).QueryRow(ctx, `SELECT project_id FROM batches WHERE id=$1`, id).Scan(&pid)
		if shared, err = s.store.ProjectByID(ctx, pid); err != nil {
			_, _ = s.store.Q(ctx).Exec(ctx, `UPDATE batch_items SET error='the project is gone' WHERE batch_id=$1 AND service_id IS NULL AND error=''`, id)
			_, _ = s.store.Q(ctx).Exec(ctx, `UPDATE batches SET status='ready' WHERE id=$1 AND status='creating'`, id)
			return
		}
	}
	slots := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for _, it := range items {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		if !s.batchCreating(ctx, id) {
			<-slots
			break
		}
		fail := func(err error) {
			slog.Warn("batch item", "batch", id, "item", it.label, "err", err)
			_, _ = s.store.Q(ctx).Exec(ctx, `UPDATE batch_items SET error=$3 WHERE batch_id=$1 AND idx=$2`, id, it.idx, err.Error())
			<-slots
		}
		p, name := shared, it.label
		if in.Mode == "accounts" {
			if it.user != nil && it.project != nil { // resumed: the account was made before the stop
				if p, err = s.store.ProjectByID(ctx, *it.project); err != nil {
					fail(errors.New("the learner's project is gone"))
					continue
				}
			} else {
				uid, pass, proj, err := s.createLearner(ctx, it.label, it.email, in.Name)
				if err != nil {
					fail(err)
					continue
				}
				_, _ = s.store.Q(ctx).Exec(ctx, `UPDATE batch_items SET user_id=$3, project_id=$4, password_enc=$5 WHERE batch_id=$1 AND idx=$2`,
					id, it.idx, uid, proj.ID, s.box.MustEncrypt(pass))
				p = proj
			}
			name = ""
		}
		svcs, dbs, err := s.installTemplate(ctx, p, tpl, name, "", in.Env, sleep)
		if err != nil {
			fail(err)
			continue
		}
		dbIDs := []string{}
		for _, d := range dbs {
			dbIDs = append(dbIDs, d.ID)
		}
		var svcID *string
		if len(svcs) > 0 {
			svcID = &svcs[0].ID
		}
		_, _ = s.store.Q(ctx).Exec(ctx, `UPDATE batch_items SET service_id=$3, database_ids=$4, project_id=COALESCE(project_id, $5) WHERE batch_id=$1 AND idx=$2`,
			id, it.idx, svcID, dbIDs, p.ID)
		wg.Add(1)
		go func(svcs []*store.Service) {
			defer wg.Done()
			defer func() { <-slots }()
			s.settleCopy(ctx, id, svcs, sleep.Enabled, batchCopyTimeout)
		}(svcs)
	}
	wg.Wait()
	_, _ = s.store.Q(ctx).Exec(ctx, `UPDATE batches SET status='ready' WHERE id=$1 AND status='creating'`, id)
}

// batchCreating reports whether the batch is still being created (not deleted meanwhile).
func (s *Server) batchCreating(ctx context.Context, id string) bool {
	var status string
	err := s.store.Q(ctx).QueryRow(ctx, `SELECT status FROM batches WHERE id=$1`, id).Scan(&status)
	return err == nil && status == "creating"
}

// settleCopy waits until every service of a copy has come up, failed or stopped,
// or the timeout passes. With sleep it then puts the copy's sleep-enabled services
// to sleep right away: a copy nobody opened yet holds no memory, and the next
// copies start into the room it leaves.
func (s *Server) settleCopy(ctx context.Context, batchID string, svcs []*store.Service, sleep bool, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for _, svc := range svcs {
		for time.Now().Before(deadline) && ctx.Err() == nil {
			cur, err := s.store.ServiceByID(ctx, svc.ID)
			if err != nil || copySettled(cur) {
				break
			}
			if !s.batchCreating(ctx, batchID) {
				return
			}
			select {
			case <-ctx.Done():
			case <-time.After(batchPoll):
			}
		}
	}
	if !sleep {
		return
	}
	for _, svc := range svcs {
		cur, err := s.store.ServiceByID(ctx, svc.ID)
		if err != nil || !cur.Spec.Sleep.Enabled || cur.Type != model.TypeWeb || cur.Status != model.StatusRunning {
			continue
		}
		if unlock, ok := s.tryLock(cur.ID); ok {
			s.putToSleep(ctx, cur.ID, unlock)
		}
	}
}

// copySettled: the service no longer needs its start-up room.
func copySettled(svc *store.Service) bool {
	switch svc.Status {
	case model.StatusRunning:
		return svc.CurrentDeploymentID != nil
	case model.StatusFailed, model.StatusStopped, model.StatusSleeping:
		return true
	}
	return false
}

// resumeBatches finishes batches whose runner stopped (its control plane restarted
// or lost leadership). A "creating" batch with a stale heartbeat is claimed by
// moving the heartbeat in the same statement, so only one control plane resumes it.
func (s *Server) resumeBatches(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		s.resumeStaleBatches(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) resumeStaleBatches(ctx context.Context) {
	rows, err := s.store.Q(ctx).Query(ctx, `UPDATE batches SET heartbeat_at=now()
		WHERE status='creating' AND heartbeat_at < now() - make_interval(secs => $1)
		RETURNING id, template, options_enc`, batchStaleAfter.Seconds())
	if err != nil {
		return
	}
	type stale struct{ id, template, opts string }
	var list []stale
	for rows.Next() {
		var b stale
		if rows.Scan(&b.id, &b.template, &b.opts) == nil {
			list = append(list, b)
		}
	}
	rows.Close()
	for _, b := range list {
		tpl, ok := templates.ByID(b.template)
		var in batchInput
		var plain string
		if b.opts != "" {
			plain, err = s.box.Decrypt(b.opts)
		}
		if !ok || b.opts == "" || err != nil || json.Unmarshal([]byte(plain), &in) != nil {
			// Created before batches kept their input: nothing to resume with.
			_, _ = s.store.Q(ctx).Exec(ctx, `UPDATE batch_items SET error='interrupted by a control plane restart' WHERE batch_id=$1 AND service_id IS NULL AND error=''`, b.id)
			_, _ = s.store.Q(ctx).Exec(ctx, `UPDATE batches SET status='ready' WHERE id=$1 AND status='creating'`, b.id)
			continue
		}
		slog.Info("resuming batch", "batch", b.id)
		go s.runBatch(ctx, b.id, tpl, in)
	}
}

// createLearner makes a dashboard account with its own team and a project in it.
func (s *Server) createLearner(ctx context.Context, name, email, project string) (string, string, *store.Project, error) {
	pass := util.RandomPassword(12)
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err != nil {
		return "", "", nil, err
	}
	u, err := s.store.CreateUser(ctx, email, name, string(hash), false)
	if err != nil {
		return "", "", nil, fmt.Errorf("account %s: %w", email, err)
	}
	team, err := s.store.CreateTeam(ctx, name+"'s team", util.Slugify(name)+"-"+util.RandomToken(2), u.ID, false)
	if err != nil {
		return u.ID, "", nil, err
	}
	p, err := s.store.CreateProject(ctx, team.ID, project, s.uniqueProjectSlug(ctx, project+" "+name))
	if err != nil {
		return u.ID, "", nil, err
	}
	return u.ID, pass, p, nil
}

type batchItemView struct {
	Index     int     `json:"index"`
	Label     string  `json:"label"`
	Email     string  `json:"email,omitempty"`
	Password  string  `json:"password,omitempty"` // first dashboard password (accounts mode)
	ProjectID *string `json:"project_id"`
	ServiceID *string `json:"service_id"`
	Status    string  `json:"status"` // the service's, or creating / failed
	URL       string  `json:"url,omitempty"`
	Error     string  `json:"error,omitempty"`
}

type batchView struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Template  string          `json:"template"`
	Mode      string          `json:"mode"`
	ProjectID *string         `json:"project_id"`
	Status    string          `json:"status"`
	CreatedAt time.Time       `json:"created_at"`
	Items     []batchItemView `json:"items,omitempty"`
	Counts    map[string]int  `json:"counts"`
}

func (s *Server) loadBatch(ctx context.Context, id string, withItems bool) (*batchView, error) {
	var b batchView
	err := s.store.Q(ctx).QueryRow(ctx, `SELECT id, name, template, mode, project_id, status, created_at FROM batches WHERE id=$1`, id).
		Scan(&b.ID, &b.Name, &b.Template, &b.Mode, &b.ProjectID, &b.Status, &b.CreatedAt)
	if err != nil {
		return nil, err
	}
	rows, err := s.store.Q(ctx).Query(ctx, `SELECT i.idx, i.label, i.email, i.password_enc, i.project_id, i.service_id, i.error, COALESCE(sv.status,''),
			COALESCE((SELECT d.hostname FROM domains d WHERE d.service_id=i.service_id ORDER BY d.is_generated, d.created_at LIMIT 1), '')
		FROM batch_items i LEFT JOIN services sv ON sv.id=i.service_id WHERE i.batch_id=$1 ORDER BY i.idx`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	b.Counts = map[string]int{}
	for rows.Next() {
		var it batchItemView
		var passEnc, svcStatus, host string
		if err := rows.Scan(&it.Index, &it.Label, &it.Email, &passEnc, &it.ProjectID, &it.ServiceID, &it.Error, &svcStatus, &host); err != nil {
			return nil, err
		}
		switch {
		case it.Error != "":
			it.Status = "failed"
		case it.ServiceID == nil:
			it.Status = "creating"
		default:
			it.Status = svcStatus
		}
		if host != "" {
			it.URL = s.urlFor(host)
		}
		if passEnc != "" {
			it.Password, _ = s.box.Decrypt(passEnc)
		}
		b.Counts[it.Status]++
		if withItems {
			b.Items = append(b.Items, it)
		}
	}
	return &b, rows.Err()
}

// GET /api/v1/batches
func (s *Server) handleListBatches(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	rows, err := s.store.Q(r.Context()).Query(r.Context(), `SELECT id FROM batches ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	out := []*batchView{}
	for _, id := range ids {
		if b, err := s.loadBatch(r.Context(), id, false); err == nil {
			out = append(out, b)
		}
	}
	writeJSON(w, 200, out)
}

// GET /api/v1/batches/{id}
func (s *Server) handleGetBatch(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	b, err := s.loadBatch(r.Context(), r.PathValue("id"), true)
	if err != nil {
		writeError(w, 404, "batch not found")
		return
	}
	writeJSON(w, 200, b)
}

// GET /api/v1/batches/{id}/csv: the hand-out list (links, and accounts with their first password).
func (s *Server) handleBatchCSV(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	b, err := s.loadBatch(r.Context(), r.PathValue("id"), true)
	if err != nil {
		writeError(w, 404, "batch not found")
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.csv"`, util.Slugify(b.Name)))
	_, _ = w.Write([]byte("\xEF\xBB\xBF")) // BOM, so Excel reads UTF-8 names
	cw := csv.NewWriter(w)
	head := []string{"#", "name", "app_url", "status"}
	if b.Mode == "accounts" {
		head = append(head, "email", "dashboard_url", "first_password")
	}
	_ = cw.Write(head)
	for _, it := range b.Items {
		row := []string{strconv.Itoa(it.Index + 1), it.Label, it.URL, it.Status}
		if b.Mode == "accounts" {
			row = append(row, it.Email, s.cfg.PublicURL, it.Password)
		}
		_ = cw.Write(row)
	}
	cw.Flush()
}

// DELETE /api/v1/batches/{id}: removes every copy (and in accounts mode the
// learners' accounts and projects), then the batch.
func (s *Server) handleDeleteBatch(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	tag, err := s.store.Q(ctx).Exec(ctx, `UPDATE batches SET status='deleting' WHERE id=$1 AND status<>'deleting'`, id)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, 409, "the batch does not exist or is already being deleted")
		return
	}
	u := currentUser(r)
	s.store.Audit(ctx, &u.ID, nil, "batch.delete", id)
	go s.deleteBatch(context.WithoutCancel(ctx), id)
	writeJSON(w, 202, map[string]string{"status": "deleting"})
}

func (s *Server) deleteBatch(ctx context.Context, id string) {
	var mode string
	var shared *string
	if err := s.store.Q(ctx).QueryRow(ctx, `SELECT mode, project_id FROM batches WHERE id=$1`, id).Scan(&mode, &shared); err != nil {
		return
	}
	rows, err := s.store.Q(ctx).Query(ctx, `SELECT user_id, project_id, service_id, database_ids FROM batch_items WHERE batch_id=$1`, id)
	if err != nil {
		return
	}
	type item struct {
		user, project, service *string
		dbs                    []string
	}
	var items []item
	for rows.Next() {
		var it item
		if rows.Scan(&it.user, &it.project, &it.service, &it.dbs) == nil {
			items = append(items, it)
		}
	}
	rows.Close()
	for _, it := range items {
		if mode == "accounts" && it.project != nil {
			s.deleteProjectContents(ctx, *it.project)
			if p, err := s.store.ProjectByID(ctx, *it.project); err == nil {
				_ = s.store.DeleteProject(ctx, p.ID)
				_, _ = s.store.Q(ctx).Exec(ctx, `DELETE FROM teams t WHERE t.id=$1 AND NOT t.is_system AND NOT EXISTS (SELECT 1 FROM projects WHERE team_id=t.id)`, p.TeamID)
			}
			if it.user != nil {
				_, _ = s.store.Q(ctx).Exec(ctx, `DELETE FROM users WHERE id=$1 AND NOT is_admin`, *it.user)
			}
			continue
		}
		if it.service != nil {
			if svc, err := s.store.ServiceByID(ctx, *it.service); err == nil {
				_ = s.deleteService(ctx, svc)
			}
		}
		for _, dbID := range it.dbs {
			if db, err := s.store.DatabaseByID(ctx, dbID); err == nil {
				_ = s.deleteDatabase(ctx, db)
			}
		}
	}
	// A project the batch created for itself goes too once it is empty.
	if mode == "instances" && shared != nil {
		svcs, _ := s.store.ServicesForProject(ctx, *shared)
		dbs, _ := s.store.DatabasesForProject(ctx, *shared)
		var name string
		_ = s.store.Q(ctx).QueryRow(ctx, `SELECT name FROM batches WHERE id=$1`, id).Scan(&name)
		if p, err := s.store.ProjectByID(ctx, *shared); err == nil && len(svcs) == 0 && len(dbs) == 0 && p.Name == name {
			_ = s.store.DeleteProject(ctx, p.ID)
		}
	}
	_, _ = s.store.Q(ctx).Exec(ctx, `DELETE FROM batches WHERE id=$1`, id)
	slog.Info("batch deleted", "batch", id, "items", len(items))
}

// deleteProjectContents removes a project's databases and services (as deleting the project does).
func (s *Server) deleteProjectContents(ctx context.Context, projectID string) {
	dbs, _ := s.store.DatabasesForProject(ctx, projectID)
	for i := range dbs {
		_ = s.deleteDatabase(ctx, &dbs[i])
	}
	svcs, _ := s.store.ServicesForProject(ctx, projectID)
	for i := range svcs {
		_ = s.deleteService(ctx, &svcs[i])
	}
}

func mbStr(v int) string {
	if v >= 1024 {
		return fmt.Sprintf("%.1f GB", float64(v)/1024)
	}
	return fmt.Sprintf("%d MB", v)
}
