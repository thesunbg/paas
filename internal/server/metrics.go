package server

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/store"
)

// traffic aggregates proxy statistics per service in 10 s buckets for one hour.
type traffic struct {
	mu      sync.Mutex
	buckets map[string]map[int64][2]int64 // service -> bucket start (unix/10) -> [requests, 5xx]
}

func newTraffic() *traffic { return &traffic{buckets: map[string]map[int64][2]int64{}} }

func (t *traffic) add(serviceID string, requests, errors int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.buckets[serviceID]
	if b == nil {
		b = map[int64][2]int64{}
		t.buckets[serviceID] = b
	}
	k := time.Now().Unix() / 10
	v := b[k]
	b[k] = [2]int64{v[0] + requests, v[1] + errors}
	for old := range b {
		if old < k-360 {
			delete(b, old)
		}
	}
}

// window sums requests and errors over the last d.
func (t *traffic) window(serviceID string, d time.Duration) (int64, int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	from := time.Now().Add(-d).Unix() / 10
	var req, errs int64
	for k, v := range t.buckets[serviceID] {
		if k >= from {
			req += v[0]
			errs += v[1]
		}
	}
	return req, errs
}

type scaleState struct {
	lowSince  time.Time
	lastScale time.Time
}

// metricsLoop records one sample per minute and evaluates autoscaling every 30 s.
func (s *Server) metricsLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	tick := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		tick++
		services, err := s.store.AllServices(ctx)
		if err != nil {
			continue
		}
		instances, _ := s.store.AllInstances(ctx)
		byService := map[string][]store.Instance{}
		for _, in := range instances {
			if in.Status == model.InstRunning {
				byService[in.ServiceID] = append(byService[in.ServiceID], in)
			}
		}
		for i := range services {
			svc := &services[i]
			if svc.Spec.Autoscale.Enabled && svc.Status == model.StatusRunning {
				s.autoscale(ctx, svc, byService[svc.ID])
			}
		}
		if tick%2 == 0 {
			s.recordMetrics(ctx, services, byService)
		}
	}
}

func (s *Server) recordMetrics(ctx context.Context, services []store.Service, byService map[string][]store.Instance) {
	now := time.Now().UTC().Truncate(time.Minute)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	seen := map[string]bool{}
	for _, svc := range services {
		ins := byService[svc.ID]
		req, errs := s.traffic.window(svc.ID, time.Minute)
		net := s.instanceTraffic(ins, seen)
		if len(ins) == 0 && req == 0 {
			continue
		}
		mem, cpu, restarts := 0, 0, 0
		for _, in := range ins {
			mem += in.MemoryUsageMB
			cpu += in.CPUUsageMillis
			restarts += in.Restarts
		}
		_, _ = s.store.Q(ctx).Exec(ctx, `INSERT INTO service_metrics (service_id, ts, instances, memory_mb, cpu_millis, requests, errors_5xx, net_rx_bytes, net_tx_bytes, restarts)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT DO NOTHING`, svc.ID, now, len(ins), mem, cpu, req, errs, net.rx, net.tx, restarts)
		s.addDailyTraffic(ctx, "service_traffic_daily", "service_id", svc.ID, day, net)
	}
	committed, _ := s.store.NodeCommitted(ctx)
	if nodes, err := s.store.Nodes(ctx); err == nil {
		for _, n := range nodes {
			if n.Status == "offline" {
				continue
			}
			key := "node:" + n.ID
			seen[key] = true
			net := s.netDelta(key, netCounters{int64(n.Usage.NetRxBytes), int64(n.Usage.NetTxBytes)}, false)
			_, _ = s.store.Q(ctx).Exec(ctx, `INSERT INTO node_metrics (node_id, ts, cpu_percent, memory_used_mb, memory_committed_mb, net_rx_bytes, net_tx_bytes)
				VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, n.ID, now, n.Usage.CPUPercent, n.Usage.MemoryUsedMB, committed[n.ID][0], net.rx, net.tx)
			s.addDailyTraffic(ctx, "node_traffic_daily", "node_id", n.ID, day, net)
		}
	}
	s.forgetNet(seen)
	_, _ = s.store.Q(ctx).Exec(ctx, `DELETE FROM service_metrics WHERE ts < now() - interval '48 hours'`)
	_, _ = s.store.Q(ctx).Exec(ctx, `DELETE FROM node_metrics WHERE ts < now() - interval '48 hours'`)
	if now.Minute() == 0 {
		_, _ = s.store.Q(ctx).Exec(ctx, `DELETE FROM service_traffic_daily WHERE day < (now() at time zone 'utc')::date - $1::int`, trafficDailyKeep)
		_, _ = s.store.Q(ctx).Exec(ctx, `DELETE FROM node_traffic_daily WHERE day < (now() at time zone 'utc')::date - $1::int`, trafficDailyKeep)
	}
}

// desiredReplicas computes the autoscaling target from CPU and request-rate ratios.
func desiredReplicas(a model.Autoscale, current int, avgCPUPercent, rpsPerReplica float64) int {
	ratio := 0.0
	if a.TargetCPUPercent > 0 {
		ratio = math.Max(ratio, avgCPUPercent/float64(a.TargetCPUPercent))
	}
	if a.TargetRPS > 0 {
		ratio = math.Max(ratio, rpsPerReplica/float64(a.TargetRPS))
	}
	desired := current
	switch {
	case ratio > 1.1:
		desired = int(math.Ceil(float64(current) * ratio))
	case ratio < 0.5:
		desired = int(math.Ceil(float64(current) * ratio * 1.25)) // keep headroom when shrinking
	}
	lo, hi := max(a.Min, 1), max(a.Max, max(a.Min, 1))
	return min(max(desired, lo), hi)
}

func (s *Server) autoscale(ctx context.Context, svc *store.Service, ins []store.Instance) {
	a := svc.Spec.Autoscale
	cur := svc.Spec.Replicas
	if cur <= 0 || len(ins) == 0 {
		return
	}
	cpuRef := svc.Spec.Resources.CPURequestMillis
	if cpuRef <= 0 {
		cpuRef = max(svc.Spec.Resources.CPULimitMillis, 1000)
	}
	totalCPU := 0
	for _, in := range ins {
		totalCPU += in.CPUUsageMillis
	}
	avgCPU := float64(totalCPU) / float64(len(ins)) * 100 / float64(cpuRef)
	req, _ := s.traffic.window(svc.ID, time.Minute)
	rps := float64(req) / 60 / float64(len(ins))
	desired := desiredReplicas(a, cur, avgCPU, rps)

	s.scaleMu.Lock()
	st := s.scale[svc.ID]
	if st == nil {
		st = &scaleState{}
		s.scale[svc.ID] = st
	}
	apply := false
	switch {
	case desired > cur:
		apply = time.Since(st.lastScale) > 30*time.Second
		st.lowSince = time.Time{}
	case desired < cur:
		if st.lowSince.IsZero() {
			st.lowSince = time.Now()
		}
		// Scale down slowly: after 5 minutes of low load, one replica at a time.
		if time.Since(st.lowSince) > 5*time.Minute && time.Since(st.lastScale) > 2*time.Minute {
			desired, apply = cur-1, true
		}
	default:
		st.lowSince = time.Time{}
	}
	if apply {
		st.lastScale = time.Now()
	}
	s.scaleMu.Unlock()
	if !apply || desired == cur {
		return
	}
	slog.Info("autoscaling", "service", svc.Slug, "from", cur, "to", desired, "cpu_percent", int(avgCPU), "rps_per_replica", fmt.Sprintf("%.1f", rps))
	if err := s.scaleService(ctx, svc, desired); err != nil {
		slog.Warn("autoscale", "service", svc.Slug, "err", err)
		return
	}
	s.notify(ctx, s.teamOfService(ctx, svc), "autoscale", fmt.Sprintf("%s scaled from %d to %d replicas (CPU %d%%, %.1f req/s per replica)", svc.Slug, cur, desired, int(avgCPU), rps), nil)
}

type metricPoint struct {
	TS        time.Time `json:"ts"`
	Instances int       `json:"instances"`
	MemoryMB  int       `json:"memory_mb"`
	CPUMillis int       `json:"cpu_millis"`
	Requests  int       `json:"requests"`
	Errors5xx int       `json:"errors_5xx"`
	NetRx     int64     `json:"net_rx_bytes"` // received during the minute
	NetTx     int64     `json:"net_tx_bytes"` // sent during the minute
}

func (s *Server) handleServiceMetrics(w http.ResponseWriter, r *http.Request) {
	svc, _ := s.serviceFor(w, r, "viewer")
	if svc == nil {
		return
	}
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	if hours <= 0 || hours > 48 {
		hours = 6
	}
	rows, err := s.store.Q(r.Context()).Query(r.Context(), `SELECT ts, instances, memory_mb, cpu_millis, requests, errors_5xx, net_rx_bytes, net_tx_bytes FROM service_metrics
		WHERE service_id=$1 AND ts > now() - make_interval(hours => $2) ORDER BY ts`, svc.ID, hours)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	defer rows.Close()
	points := []metricPoint{}
	for rows.Next() {
		var p metricPoint
		if err := rows.Scan(&p.TS, &p.Instances, &p.MemoryMB, &p.CPUMillis, &p.Requests, &p.Errors5xx, &p.NetRx, &p.NetTx); err == nil {
			points = append(points, p)
		}
	}
	req, errs := s.traffic.window(svc.ID, 5*time.Minute)
	writeJSON(w, 200, map[string]any{"points": points, "rps_5m": float64(req) / 300, "errors_5m": errs,
		"traffic": s.trafficSummary(r.Context(), "service_traffic_daily", "service_id", svc.ID)})
}

func (s *Server) handleNodeMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	rows, err := s.store.Q(r.Context()).Query(r.Context(), `SELECT ts, cpu_percent, memory_used_mb, memory_committed_mb, net_rx_bytes, net_tx_bytes FROM node_metrics
		WHERE node_id=$1 AND ts > now() - interval '24 hours' ORDER BY ts`, r.PathValue("id"))
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	defer rows.Close()
	type point struct {
		TS        time.Time `json:"ts"`
		CPU       float64   `json:"cpu_percent"`
		Used      int       `json:"memory_used_mb"`
		Committed int       `json:"memory_committed_mb"`
		NetRx     int64     `json:"net_rx_bytes"`
		NetTx     int64     `json:"net_tx_bytes"`
	}
	out := []point{}
	for rows.Next() {
		var p point
		if err := rows.Scan(&p.TS, &p.CPU, &p.Used, &p.Committed, &p.NetRx, &p.NetTx); err == nil {
			out = append(out, p)
		}
	}
	writeJSON(w, 200, out)
}
