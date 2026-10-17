package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
)

// A running job the node no longer reports is failed by the heartbeat; one it still runs,
// or any job when the agent predates Heartbeat.Jobs (nil), is left alone.
func TestHeartbeatFailsLostJobs(t *testing.T) {
	f := newLockFixture(t)
	s := f.s
	ctx := context.Background()
	svc := f.seedService(t, "job", model.InstRunning)

	newRun := func(age string) string {
		jr, err := s.store.CreateJobRun(ctx, svc.ID, f.nodeB.ID, "job-run", "schedule")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.store.Q(ctx).Exec(ctx, `UPDATE job_runs SET started_at = now() - $2::interval WHERE id=$1`, jr.ID, age); err != nil {
			t.Fatal(err)
		}
		return jr.ID
	}
	beat := func(jobs []string) {
		hb, _ := json.Marshal(proto.Heartbeat{NodeID: f.nodeB.ID, Token: s.ClusterToken, Jobs: jobs})
		s.onHeartbeat(&nats.Msg{Data: hb})
	}
	status := func(id string) string {
		jr, err := s.store.JobRunByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return jr.Status
	}

	id := newRun("1 minute")
	fresh := newRun("1 second")
	beat(nil)
	if got := status(id); got != "running" {
		t.Errorf("nil Jobs: status = %s, want running", got)
	}
	beat([]string{id})
	if got := status(id); got != "running" {
		t.Errorf("job reported alive: status = %s, want running", got)
	}
	beat([]string{})
	if got := status(id); got != "failed" {
		t.Errorf("job not reported: status = %s, want failed", got)
	}
	if got := status(fresh); got != "running" {
		t.Errorf("job inside the grace window: status = %s, want running", got)
	}
	if jr, _ := s.store.JobRunByID(ctx, id); jr.ExitCode == nil || *jr.ExitCode != -1 || jr.FinishedAt == nil {
		t.Errorf("lost job row = %+v", jr)
	}
}
