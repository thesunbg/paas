package scheduler

import (
	"errors"
	"testing"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/store"
)

func node(id string, memMB int) store.Node {
	return store.Node{ID: id, Name: id, Status: "online", Roles: []string{"worker"}, CPUCores: 4, MemoryMB: memMB,
		ReservedMB: 1024, OvercommitMemory: 1.5, OvercommitCPU: 4}
}

func TestOvercommitAndGuaranteed(t *testing.T) {
	nodes := []store.Node{node("a", 5120)} // 4096 schedulable, 6144 with overcommit
	committed := map[string][2]int{"a": {5000, 0}}
	_, err := Pick(nodes, committed, Request{Resources: model.Resources{MemoryRequestMB: 512}, QoS: model.QoSBurstable})
	if err != nil {
		t.Fatalf("burstable should fit with overcommit: %v", err)
	}
	_, err = Pick(nodes, committed, Request{Resources: model.Resources{MemoryRequestMB: 512}, QoS: model.QoSGuaranteed})
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("guaranteed must not be overcommitted, got %v", err)
	}
}

func TestSpreadAndPin(t *testing.T) {
	nodes := []store.Node{node("a", 8192), node("b", 8192)}
	n, err := Pick(nodes, nil, Request{Spread: map[string]int{"a": 1}})
	if err != nil || n.ID != "b" {
		t.Fatalf("expected spread to b, got %v %v", n, err)
	}
	n, err = Pick(nodes, nil, Request{PinnedNode: "a", Spread: map[string]int{"a": 1}})
	if err != nil || n.ID != "a" {
		t.Fatalf("pinned must win, got %v %v", n, err)
	}
	nodes[1].Status = "cordoned"
	n, _ = Pick(nodes, nil, Request{Spread: map[string]int{"a": 1}})
	if n.ID != "a" {
		t.Fatal("cordoned node must be skipped")
	}
}
