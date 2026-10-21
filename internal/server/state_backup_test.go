package server

import (
	"fmt"
	"testing"
)

func TestKeepState(t *testing.T) {
	// 4 backups a day for 40 days, newest first.
	var names []string
	for d := 40; d >= 1; d-- {
		for h := 23; h >= 0; h -= 6 {
			names = append(names, fmt.Sprintf("state-202610%02d-%02d0000.pgstate", d, h))
		}
	}
	keep := keepState(names)
	for i := 0; i < stateKeepHourly; i++ {
		if !keep[names[i]] {
			t.Fatalf("recent backup %s dropped", names[i])
		}
	}
	days := map[string]bool{}
	for n := range keep {
		days[n[6:14]] = true
	}
	if len(days) != stateKeepDaily {
		t.Fatalf("kept %d days, want %d", len(days), stateKeepDaily)
	}
	if keep[names[len(names)-1]] {
		t.Fatal("the oldest backup (day 1) should be gone")
	}
}
