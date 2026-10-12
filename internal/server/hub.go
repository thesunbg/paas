package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
)

// LogHub persists deployment logs published on the bus and fans them out to live viewers.
type LogHub struct {
	store *store.Store
	nc    *nats.Conn

	mu   sync.Mutex
	subs map[string]map[chan []string]struct{}
}

func NewLogHub(st *store.Store, nc *nats.Conn) *LogHub {
	return &LogHub{store: st, nc: nc, subs: map[string]map[chan []string]struct{}{}}
}

func (h *LogHub) Start() error {
	decode := func(m *nats.Msg) (string, []string, bool) {
		var lines []string
		if err := json.Unmarshal(m.Data, &lines); err != nil {
			return "", nil, false
		}
		for i, l := range lines {
			if len(l) > 4000 {
				lines[i] = l[:4000] + "…"
			}
		}
		return strings.TrimPrefix(m.Subject, proto.DeployLogSubject("")), lines, true
	}
	// One control plane stores each batch…
	if _, err := h.nc.QueueSubscribe(proto.DeployLogSubject("*"), "log-store", func(m *nats.Msg) {
		if id, lines, ok := decode(m); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := h.store.AppendDeploymentLogs(ctx, id, lines); err != nil {
				slog.Warn("store deployment logs", "deployment", id, "err", err)
			}
			_ = h.nc.Publish("paas.logs.stored."+id, nil)
		}
	}); err != nil {
		return err
	}
	// …and every control plane wakes its live viewers once it is stored.
	_, err := h.nc.Subscribe("paas.logs.stored.*", func(m *nats.Msg) {
		h.deliver(strings.TrimPrefix(m.Subject, "paas.logs.stored."), nil)
	})
	return err
}

func (h *LogHub) deliver(id string, lines []string) {
	h.mu.Lock()
	for ch := range h.subs[id] {
		select {
		case ch <- lines:
		default: // slow viewer; drop
		}
	}
	h.mu.Unlock()
}

// Log writes control-plane lines into a deployment's log.
func (h *LogHub) Log(deploymentID string, lines ...string) {
	data, _ := json.Marshal(lines)
	if err := h.nc.Publish(proto.DeployLogSubject(deploymentID), data); err != nil {
		slog.Warn("publish log", "err", err)
	}
}

// Subscribe returns a channel of new lines for a deployment and a cancel func.
func (h *LogHub) Subscribe(id string) (chan []string, func()) {
	ch := make(chan []string, 256)
	h.mu.Lock()
	if h.subs[id] == nil {
		h.subs[id] = map[chan []string]struct{}{}
	}
	h.subs[id][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs[id], ch)
		if len(h.subs[id]) == 0 {
			delete(h.subs, id)
		}
		h.mu.Unlock()
	}
}

func respondJSON(m *nats.Msg, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return m.Respond(data)
}
