// Package scheduler places instances on nodes using resource requests,
// per-node overcommit ratios, pinning and replica anti-affinity.
package scheduler

import (
	"errors"
	"fmt"
	"sort"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/store"
)

// Request describes what needs to be placed.
type Request struct {
	Resources    model.Resources
	QoS          string
	RuntimeClass string
	PinnedNode   string          // must run here (volumes)
	Spread       map[string]int  // node id -> replicas of this service already there
	Exclude      map[string]bool // nodes to avoid (recently evicted)
	Role         string          // worker | build
}

var ErrNoCapacity = errors.New("no node has enough free capacity")

// Capacity is the schedulable memory (MB) and CPU (millis) of a node for a QoS class.
func Capacity(n *store.Node, qos string) (mem int, cpu int) {
	base := n.MemoryMB - n.ReservedMB
	if base < 0 {
		base = 0
	}
	over := n.OvercommitMemory
	if over < 1 || qos == model.QoSGuaranteed {
		over = 1 // guaranteed workloads are never overcommitted
	}
	cpuOver := n.OvercommitCPU
	if cpuOver < 1 {
		cpuOver = 1
	}
	return int(float64(base) * over), int(float64(n.CPUCores*1000) * cpuOver)
}

// Pick chooses the best node, or returns ErrNoCapacity.
// committed maps node id -> [memory MB, cpu millis] already requested.
func Pick(nodes []store.Node, committed map[string][2]int, req Request) (*store.Node, error) {
	role := req.Role
	if role == "" {
		role = "worker"
	}
	type cand struct {
		node  *store.Node
		free  float64
		count int
	}
	var cands []cand
	reasons := map[string]string{}
	for i := range nodes {
		n := &nodes[i]
		if req.PinnedNode != "" && n.ID != req.PinnedNode {
			continue
		}
		if n.Status != "online" {
			reasons[n.Name] = "node is " + n.Status
			continue
		}
		if !n.HasRole(role) {
			reasons[n.Name] = "missing role " + role
			continue
		}
		if req.Exclude[n.ID] && req.PinnedNode == "" {
			reasons[n.Name] = "temporarily excluded"
			continue
		}
		if req.RuntimeClass == model.RuntimeSandboxed && n.Capabilities["runtime.sandboxed"] != "true" {
			reasons[n.Name] = "gVisor not installed"
			continue
		}
		memCap, cpuCap := Capacity(n, req.QoS)
		used := committed[n.ID]
		if used[0]+req.Resources.MemoryRequestMB > memCap {
			reasons[n.Name] = fmt.Sprintf("memory: %d/%d MB committed", used[0], memCap)
			continue
		}
		if used[1]+req.Resources.CPURequestMillis > cpuCap {
			reasons[n.Name] = fmt.Sprintf("cpu: %d/%d m committed", used[1], cpuCap)
			continue
		}
		free := 1.0
		if memCap > 0 {
			free = float64(memCap-used[0]-req.Resources.MemoryRequestMB) / float64(memCap)
		}
		cands = append(cands, cand{node: n, free: free, count: req.Spread[n.ID]})
	}
	if len(cands) == 0 {
		if req.PinnedNode != "" && len(reasons) == 0 {
			return nil, fmt.Errorf("%w: pinned node not found", ErrNoCapacity)
		}
		return nil, fmt.Errorf("%w %v", ErrNoCapacity, reasons)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].count != cands[j].count {
			return cands[i].count < cands[j].count // anti-affinity between replicas
		}
		return cands[i].free > cands[j].free // then the least loaded node
	})
	return cands[0].node, nil
}
