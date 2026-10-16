package agent

import "testing"

func TestPlainSegment(t *testing.T) {
	for p, want := range map[string]bool{"svc": true, "dep-1": true, "": false, ".": false, "..": false, `a\b`: false} {
		if got := plainSegment(p); got != want {
			t.Errorf("plainSegment(%q) = %v, want %v", p, got, want)
		}
	}
}
