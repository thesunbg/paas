package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thesunbg/paas/internal/proto"
	rt "github.com/thesunbg/paas/internal/runtime"
)

// Self-fencing of HA postgres. The control plane only promotes a standby after the primary's
// node has been silent for longer than proto.LeaseTTL plus a margin (see haPrimaryGone), so
// an agent that has not had a heartbeat acked for LeaseTTL knows it may be replaced. It then
// stops its HA postgres, unless that postgres is a standby or still has a streaming standby
// (then it cannot be replaced: the standby is the witness that it is alive).
//
// Exec auth, verified: Runtime.Exec runs with the container's own env (spec.Process.Env,
// which holds POSTGRES_USER / POSTGRES_PASSWORD from the run request), so the script can read
// both, and the official image trusts local and 127.0.0.1 connections anyway (the CP's own
// "psql -U <user>" execs rely on that). PGPASSWORD covers the case where it does not.
const fenceProbe = `PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d postgres -Atc "SELECT pg_is_in_recovery(), (SELECT count(*) FROM pg_stat_replication WHERE state='streaming')"`

// fencer holds the lease and what the fence loop needs from the runtime, so the loop is
// testable with a fake node.
type fencer struct {
	mu    sync.Mutex
	until time.Time // lease end (monotonic clock)
	seen  bool      // an ack was ever received: an old control plane never acks, never fence then

	list    func() []string                                      // running, unfenced HA postgres containers
	probe   func(ctx context.Context, id string) (string, error) // fenceProbe output
	fence   func(ctx context.Context, id string) error
	unfence func(ctx context.Context, id string) error
}

func newFencer(r *rt.Runtime) *fencer {
	return &fencer{
		list: r.HAPostgresRunning,
		probe: func(ctx context.Context, id string) (string, error) {
			rep := r.Exec(ctx, proto.ExecRequest{ContainerID: id, Cmd: []string{"sh", "-c", fenceProbe}, TimeoutSec: 3})
			switch {
			case rep.Error != "":
				return "", fmt.Errorf("%s", rep.Error)
			case rep.ExitCode != 0:
				return "", fmt.Errorf("exit code %d: %s", rep.ExitCode, strings.TrimSpace(rep.Stderr))
			}
			return rep.Stdout, nil
		},
		fence:   func(ctx context.Context, id string) error { return r.Fence(ctx, id, 2*time.Second) },
		unfence: r.Unfence,
	}
}

// grant records an acked heartbeat sent at sentAt: the lease runs from the send, never later.
func (f *fencer) grant(sentAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if until := sentAt.Add(proto.LeaseTTL); until.After(f.until) {
		f.until = until
	}
	f.seen = true
}

func (f *fencer) expired(now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen && now.After(f.until)
}

// agentShouldFence: a postgres that cannot be probed is fenced; a standby never is; a
// primary is fenced unless a standby is streaming from it.
func agentShouldFence(execErr error, inRecovery bool, streaming int) bool {
	switch {
	case execErr != nil:
		return true
	case inRecovery:
		return false
	default:
		return streaming == 0
	}
}

func parseProbe(out string) (inRecovery bool, streaming int, err error) {
	rec, n, ok := strings.Cut(strings.TrimSpace(out), "|")
	if !ok || (rec != "t" && rec != "f") {
		return false, 0, fmt.Errorf("unexpected probe output %q", out)
	}
	streaming, err = strconv.Atoi(n)
	return rec == "t", streaming, err
}

// step is one pass of the fence loop; it returns the containers it fenced.
func (f *fencer) step(ctx context.Context, now time.Time) []string {
	if !f.expired(now) {
		return nil
	}
	var fenced []string
	for _, id := range f.list() {
		out, err := f.probe(ctx, id)
		var rec bool
		var streaming int
		if err == nil {
			rec, streaming, err = parseProbe(out)
		}
		if !agentShouldFence(err, rec, streaming) {
			continue
		}
		slog.Warn("control plane lease expired: fencing HA postgres", "container", id, "probe_err", err, "in_recovery", rec, "streaming", streaming)
		if err := f.fence(ctx, id); err != nil {
			slog.Error("fence", "container", id, "err", err)
			continue
		}
		fenced = append(fenced, id)
	}
	return fenced
}

func (f *fencer) applyUnfence(ctx context.Context, ids []string) {
	for _, id := range ids {
		if err := f.unfence(ctx, id); err != nil {
			slog.Warn("unfence", "container", id, "err", err)
		} else {
			slog.Info("unfenced HA postgres", "container", id)
		}
	}
}

func (a *Agent) fenceLoop(ctx context.Context) {
	t := time.NewTicker(proto.FenceCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.fz.step(ctx, time.Now())
		}
	}
}
