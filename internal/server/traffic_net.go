package server

import (
	"context"
	"time"

	"github.com/thesunbg/paas/internal/store"
)

// Network traffic. Nodes report byte counters (per container, and for their
// physical interfaces); once a minute the leader turns the growth since the
// previous minute into the per-minute metrics and adds it to daily totals.

const trafficDailyKeep = 90 // days

type netCounters struct{ rx, tx int64 }

// netDelta is the traffic since the previous sample of key. A counter that went
// down started again (container or node restarted): all of it is new. Without a
// previous sample (a new leader) nothing is counted, except for an instance that
// started within the last few minutes, whose whole counter is recent.
func (s *Server) netDelta(key string, cur netCounters, fresh bool) netCounters {
	if s.netLast == nil {
		s.netLast = map[string]netCounters{}
	}
	last, ok := s.netLast[key]
	s.netLast[key] = cur
	switch {
	case !ok && fresh:
		return cur
	case !ok:
		return netCounters{}
	case cur.rx < last.rx || cur.tx < last.tx:
		return cur
	}
	return netCounters{cur.rx - last.rx, cur.tx - last.tx}
}

// instanceTraffic sums the minute's traffic of a service's instances.
func (s *Server) instanceTraffic(ins []store.Instance, seen map[string]bool) netCounters {
	var t netCounters
	for _, in := range ins {
		seen[in.ID] = true
		d := s.netDelta(in.ID, netCounters{in.NetRxBytes, in.NetTxBytes}, time.Since(in.CreatedAt) < 3*time.Minute)
		t.rx += d.rx
		t.tx += d.tx
	}
	return t
}

// forgetNet drops the samples of instances and nodes that are gone.
func (s *Server) forgetNet(seen map[string]bool) {
	for k := range s.netLast {
		if !seen[k] {
			delete(s.netLast, k)
		}
	}
}

func (s *Server) addDailyTraffic(ctx context.Context, table, col, id string, day time.Time, t netCounters) {
	if t.rx == 0 && t.tx == 0 {
		return
	}
	_, _ = s.store.Q(ctx).Exec(ctx, `INSERT INTO `+table+` (`+col+`, day, rx_bytes, tx_bytes) VALUES ($1,$2,$3,$4)
		ON CONFLICT (`+col+`, day) DO UPDATE SET rx_bytes = `+table+`.rx_bytes + EXCLUDED.rx_bytes, tx_bytes = `+table+`.tx_bytes + EXCLUDED.tx_bytes`,
		id, day, t.rx, t.tx)
}

type trafficDay struct {
	Day string `json:"day"`
	Rx  int64  `json:"rx_bytes"`
	Tx  int64  `json:"tx_bytes"`
}

type trafficSummary struct {
	Daily  []trafficDay `json:"daily"` // the last 30 days, oldest first
	Rx30d  int64        `json:"rx_30d"`
	Tx30d  int64        `json:"tx_30d"`
	RxDay  int64        `json:"rx_today"`
	TxDay  int64        `json:"tx_today"`
	Since  string       `json:"since,omitempty"` // first day with data, when less than 30
	TZNote string       `json:"tz"`
}

func (s *Server) trafficSummary(ctx context.Context, table, col, id string) trafficSummary {
	out := trafficSummary{Daily: []trafficDay{}, TZNote: "UTC"}
	rows, err := s.store.Q(ctx).Query(ctx, `SELECT to_char(day,'YYYY-MM-DD'), rx_bytes, tx_bytes FROM `+table+`
		WHERE `+col+`=$1 AND day > (now() at time zone 'utc')::date - 30 ORDER BY day`, id)
	if err != nil {
		return out
	}
	defer rows.Close()
	today := time.Now().UTC().Format("2006-01-02")
	for rows.Next() {
		var d trafficDay
		if rows.Scan(&d.Day, &d.Rx, &d.Tx) != nil {
			continue
		}
		out.Daily = append(out.Daily, d)
		out.Rx30d += d.Rx
		out.Tx30d += d.Tx
		if d.Day == today {
			out.RxDay, out.TxDay = d.Rx, d.Tx
		}
	}
	if len(out.Daily) > 0 && len(out.Daily) < 30 {
		out.Since = out.Daily[0].Day
	}
	return out
}
