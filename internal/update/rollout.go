package update

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// A rollout upgrades a cluster one machine at a time:
//
//  1. a backup of the platform state;
//  2. the control planes, those that are not the leader first and the leader last, so
//     leadership moves to an already updated control plane, which resumes the rollout
//     from the persisted state;
//  3. every other node (worker, build, edge), one at a time, so that apps and edge
//     traffic keep running on the others.
//
// Each step tells the machine to replace its binary and restart, then waits until it
// reports the new version and is online. A machine that does not within the step
// timeout is rolled back to its previous binary and the rollout stops (failed).
//
// The rollout is a plain value persisted after every Tick: Tick reads only the
// rollout and what the Cluster observes now, never memory of earlier ticks, so any
// control plane can continue it after the leader changed.

// Step kinds.
const (
	KindBackup       = "backup"
	KindControlPlane = "control_plane"
	KindNode         = "node"
)

// Rollout statuses.
const (
	StatusRunning = "running"
	StatusPaused  = "paused"
	StatusFailed  = "failed"
	StatusAborted = "aborted"
	StatusDone    = "done"
)

// Step statuses.
const (
	StepPending     = "pending"
	StepStarting    = "starting" // decided and persisted; the update command is (re)sent from here
	StepUpdating    = "updating" // command accepted; waiting for the new version
	StepRollingBack = "rolling_back"
	StepDone        = "done"
	StepSkipped     = "skipped"
	StepFailed      = "failed"
	StepRolledBack  = "rolled_back"
)

type Step struct {
	Kind         string     `json:"kind"`
	ID           string     `json:"id,omitempty"` // control plane name or node id
	Name         string     `json:"name"`
	Roles        []string   `json:"roles,omitempty"`
	Leader       bool       `json:"leader,omitempty"` // leader when the rollout was planned
	FromVersion  string     `json:"from_version,omitempty"`
	Status       string     `json:"status"`
	Message      string     `json:"message,omitempty"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	SentAt       *time.Time `json:"sent_at,omitempty"`
	RollbackAt   *time.Time `json:"rollback_at,omitempty"`
	RollbackSent bool       `json:"rollback_sent,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
}

type Rollout struct {
	ID          string            `json:"id"`
	Version     string            `json:"version"`
	Status      string            `json:"status"`
	Message     string            `json:"message"`
	Steps       []Step            `json:"steps"`
	Release     *Release          `json:"release,omitempty"`
	Checksums   map[string]string `json:"checksums"` // arch -> SHA-256 of paasd-linux-<arch>
	RequestedBy string            `json:"requested_by"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	FinishedAt  *time.Time        `json:"finished_at,omitempty"`
}

// Machine is a control plane or node as known when planning.
type Machine struct {
	ID     string
	Name   string
	Roles  []string
	Leader bool
}

// Observation is what the cluster reports about a step's machine right now.
type Observation struct {
	Found    bool   // still part of the cluster
	Online   bool   // heard from recently
	Cordoned bool   // nodes: an administrator took it out of scheduling
	Version  string // version it last registered with
}

// Cluster is what a rollout acts on. The control plane implements it over the bus and
// the database; tests use fakes.
type Cluster interface {
	Backup(ctx context.Context) (string, error)
	Observe(ctx context.Context, st Step) Observation
	// Start tells the machine to install the rollout's version and restart. An error
	// wrapping ErrRejected fails the step at once; any other is retried until the timeout.
	Start(ctx context.Context, st Step) error
	Rollback(ctx context.Context, st Step) error
	Now() time.Time
}

// Plan orders the steps: backup, non-leader control planes, the leader, other nodes.
// Within each group machines go by name so the order is predictable.
func Plan(cps, nodes []Machine) []Step {
	cps = append([]Machine(nil), cps...)
	sort.SliceStable(cps, func(i, j int) bool {
		if cps[i].Leader != cps[j].Leader {
			return !cps[i].Leader
		}
		return cps[i].Name < cps[j].Name
	})
	nodes = append([]Machine(nil), nodes...)
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	steps := []Step{{Kind: KindBackup, Name: "platform state backup", Status: StepPending}}
	for _, m := range cps {
		steps = append(steps, Step{Kind: KindControlPlane, ID: m.ID, Name: m.Name, Leader: m.Leader, Status: StepPending})
	}
	for _, m := range nodes {
		steps = append(steps, Step{Kind: KindNode, ID: m.ID, Name: m.Name, Roles: m.Roles, Status: StepPending})
	}
	return steps
}

// Active: the rollout still has work to do (it may be paused).
func (r *Rollout) Active() bool { return r.Status == StatusRunning || r.Status == StatusPaused }

// HoldsNodes: nodes must not follow the control plane's binary on their own (agent
// self-update) while this rollout decides who runs what: in progress, or stopped by a
// failure that left a node rolled back.
func (r *Rollout) HoldsNodes() bool { return r.Active() || r.Status == StatusFailed }

// Current is the index of the step in progress or next, -1 when none is left.
func (r *Rollout) Current() int {
	for i, st := range r.Steps {
		switch st.Status {
		case StepPending, StepStarting, StepUpdating, StepRollingBack:
			return i
		}
	}
	return -1
}

func (r *Rollout) Pause() error {
	if r.Status != StatusRunning {
		return fmt.Errorf("the update is %s, not running", r.Status)
	}
	r.Status, r.Message = StatusPaused, "paused: the step in progress finishes, no further machine is started"
	return nil
}

func (r *Rollout) Resume() error {
	if r.Status != StatusPaused {
		return fmt.Errorf("the update is %s, not paused", r.Status)
	}
	r.Status, r.Message = StatusRunning, ""
	return nil
}

// Abort stops the rollout for good. A machine being updated right now is not
// interrupted: it comes back with the new version or its local guard rolls it back.
func (r *Rollout) Abort(now time.Time) error {
	if !r.Active() {
		return fmt.Errorf("the update is %s, not in progress", r.Status)
	}
	r.finish(now, StatusAborted, "aborted by an administrator")
	return nil
}

func (r *Rollout) finish(now time.Time, status, msg string) {
	r.Status, r.Message, r.FinishedAt = status, msg, &now
}

func (st *Step) end(now time.Time, status, msg string) {
	st.Status, st.Message, st.FinishedAt = status, msg, &now
}

func (st *Step) label() string {
	if st.Kind == KindControlPlane {
		return "control plane " + st.Name
	}
	return "node " + st.Name
}

// Tick advances the rollout by at most one action and reports whether it changed
// (the caller persists it then). timeout bounds each wait: for the machine to accept
// the command, to come back with the new version, to come back after a rollback.
func (r *Rollout) Tick(ctx context.Context, c Cluster, timeout time.Duration) bool {
	if r.Status != StatusRunning {
		return false
	}
	now := c.Now()
	i := r.Current()
	if i < 0 {
		r.finish(now, StatusDone, "every machine runs "+r.Version)
		return true
	}
	st := &r.Steps[i]
	if st.Kind == KindBackup {
		name, err := c.Backup(ctx)
		if err != nil {
			st.end(now, StepFailed, err.Error())
			r.finish(now, StatusFailed, "the state backup failed, nothing was updated: "+err.Error())
			return true
		}
		st.end(now, StepDone, name)
		return true
	}
	obs := c.Observe(ctx, *st)
	updated := obs.Online && Same(obs.Version, r.Version)
	switch st.Status {
	case StepPending:
		switch {
		case !obs.Found:
			st.end(now, StepSkipped, "no longer part of the cluster")
		case updated:
			st.end(now, StepDone, "already runs "+r.Version)
		case !obs.Online:
			st.end(now, StepSkipped, "offline: not updated; start the update again once it is back")
		case obs.Cordoned:
			st.end(now, StepSkipped, "cordoned: not updated; start the update again to include it")
		default:
			st.FromVersion, st.Status, st.StartedAt = obs.Version, StepStarting, &now
		}
		return true
	case StepStarting:
		if updated {
			st.end(now, StepDone, "updated from "+orUnknown(st.FromVersion))
			return true
		}
		err := c.Start(ctx, *st)
		if err == nil {
			st.Status, st.SentAt, st.Message = StepUpdating, &now, "installing and restarting"
			return true
		}
		if errors.Is(err, ErrRejected) || now.Sub(*st.StartedAt) > timeout {
			st.end(now, StepFailed, err.Error())
			r.finish(now, StatusFailed, fmt.Sprintf("failed at %s, which was not changed: %v", st.label(), err))
			return true
		}
		msg := "not reachable yet, retrying: " + err.Error()
		changed := st.Message != msg
		st.Message = msg
		return changed
	case StepUpdating:
		if updated {
			st.end(now, StepDone, "updated from "+orUnknown(st.FromVersion))
			return true
		}
		if now.Sub(*st.SentAt) <= timeout {
			return false
		}
		st.Status, st.RollbackAt = StepRollingBack, &now
		st.Message = fmt.Sprintf("did not come back healthy with %s within %s; rolling back", r.Version, timeout)
		st.RollbackSent = c.Rollback(ctx, *st) == nil
		return true
	case StepRollingBack:
		if obs.Online && !Same(obs.Version, r.Version) {
			st.end(now, StepRolledBack, "rolled back to "+orUnknown(obs.Version))
			r.finish(now, StatusFailed, fmt.Sprintf("failed at %s, rolled back to %s", st.label(), orUnknown(obs.Version)))
			return true
		}
		if now.Sub(*st.RollbackAt) > timeout {
			st.end(now, StepFailed, "did not come back after the rollback either")
			r.finish(now, StatusFailed, fmt.Sprintf("failed at %s, which did not come back after the rollback; check the machine (paasd.prev next to the binary is the previous version)", st.label()))
			return true
		}
		if !st.RollbackSent {
			// Not reachable (it may crash-loop): its local guard restores paasd.prev on its own.
			st.RollbackSent = c.Rollback(ctx, *st) == nil
			return st.RollbackSent
		}
		return false
	}
	return false
}

func orUnknown(v string) string {
	if v == "" {
		return "an unknown version"
	}
	return v
}
