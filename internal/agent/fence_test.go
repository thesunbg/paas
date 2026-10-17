package agent

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/proto"
)

func TestAgentShouldFence(t *testing.T) {
	tests := []struct {
		name       string
		execErr    error
		inRecovery bool
		streaming  int
		want       bool
	}{
		{"exec failed", errors.New("boom"), false, 1, true},
		{"exec failed on a standby", errors.New("boom"), true, 0, true},
		{"standby", nil, true, 0, false},
		{"primary with a streaming standby", nil, false, 1, false},
		{"primary without standby", nil, false, 0, true},
	}
	for _, tc := range tests {
		if got := agentShouldFence(tc.execErr, tc.inRecovery, tc.streaming); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// fakeNode is a node with HA postgres containers and a scripted psql.
type fakeNode struct {
	running  []string
	probe    map[string]string // container -> probe output
	probeErr error
	fenced   []string
	started  []string
}

func (n *fakeNode) fencer() *fencer {
	return &fencer{
		list: func() []string {
			return slices.DeleteFunc(slices.Clone(n.running), func(id string) bool { return slices.Contains(n.fenced, id) })
		},
		probe: func(_ context.Context, id string) (string, error) {
			return n.probe[id], n.probeErr
		},
		fence:   func(_ context.Context, id string) error { n.fenced = append(n.fenced, id); return nil },
		unfence: func(_ context.Context, id string) error { n.started = append(n.started, id); return nil },
	}
}

func TestFenceLoopStep(t *testing.T) {
	t0 := time.Now()
	expired := t0.Add(proto.LeaseTTL + time.Second)
	fresh := t0.Add(proto.LeaseTTL - time.Second)

	t.Run("lease expired, primary without standby is fenced", func(t *testing.T) {
		n := &fakeNode{running: []string{"pg"}, probe: map[string]string{"pg": "f|0\n"}}
		f := n.fencer()
		f.grant(t0)
		if got := f.step(context.Background(), expired); !slices.Equal(got, []string{"pg"}) || !slices.Equal(n.fenced, []string{"pg"}) {
			t.Fatalf("fenced = %v / %v", got, n.fenced)
		}
		// Fenced containers are not listed any more: a second pass does nothing.
		if got := f.step(context.Background(), expired); len(got) != 0 {
			t.Errorf("fenced again: %v", got)
		}
	})
	t.Run("a streaming standby keeps the primary up", func(t *testing.T) {
		n := &fakeNode{running: []string{"pg"}, probe: map[string]string{"pg": "f|1\n"}}
		f := n.fencer()
		f.grant(t0)
		if got := f.step(context.Background(), expired); len(got) != 0 {
			t.Fatalf("fenced %v", got)
		}
	})
	t.Run("a standby is never fenced", func(t *testing.T) {
		n := &fakeNode{running: []string{"pg"}, probe: map[string]string{"pg": "t|0\n"}}
		f := n.fencer()
		f.grant(t0)
		if got := f.step(context.Background(), expired); len(got) != 0 {
			t.Fatalf("fenced %v", got)
		}
	})
	t.Run("unusable probe fences", func(t *testing.T) {
		for name, n := range map[string]*fakeNode{
			"exec error": {running: []string{"pg"}, probeErr: errors.New("timeout")},
			"garbage":    {running: []string{"pg"}, probe: map[string]string{"pg": "oops"}},
		} {
			f := n.fencer()
			f.grant(t0)
			if got := f.step(context.Background(), expired); len(got) != 1 {
				t.Errorf("%s: fenced %v", name, got)
			}
		}
	})
	t.Run("lease still valid", func(t *testing.T) {
		n := &fakeNode{running: []string{"pg"}, probe: map[string]string{"pg": "f|0\n"}}
		f := n.fencer()
		f.grant(t0)
		if got := f.step(context.Background(), fresh); len(got) != 0 {
			t.Fatalf("fenced %v", got)
		}
	})
	t.Run("never acked (old control plane): no fencing", func(t *testing.T) {
		n := &fakeNode{running: []string{"pg"}, probe: map[string]string{"pg": "f|0\n"}}
		f := n.fencer()
		if got := f.step(context.Background(), t0.Add(time.Hour)); len(got) != 0 {
			t.Fatalf("fenced %v", got)
		}
	})
	t.Run("an older ack never shortens the lease", func(t *testing.T) {
		f := (&fakeNode{}).fencer()
		f.grant(t0.Add(10 * time.Second))
		f.grant(t0) // a slow ack of an earlier heartbeat arrives late
		if f.expired(t0.Add(proto.LeaseTTL + 5*time.Second)) {
			t.Error("lease expired too early")
		}
	})
	t.Run("unfence starts the container again", func(t *testing.T) {
		n := &fakeNode{running: []string{"pg"}, probe: map[string]string{"pg": "f|0\n"}}
		f := n.fencer()
		f.grant(t0)
		f.step(context.Background(), expired)
		f.applyUnfence(context.Background(), []string{"pg"})
		if !slices.Equal(n.started, []string{"pg"}) {
			t.Errorf("started = %v", n.started)
		}
	})
}
