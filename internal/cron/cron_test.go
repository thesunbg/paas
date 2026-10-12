package cron

import (
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestMatches(t *testing.T) {
	cases := []struct {
		expr string
		t    string
		want bool
	}{
		{"*/15 * * * *", "2026-10-12 10:30", true},
		{"*/15 * * * *", "2026-10-12 10:31", false},
		{"0 3 * * *", "2026-10-12 03:00", true},
		{"0 9-17 * * mon-fri", "2026-10-12 10:00", true},  // Monday
		{"0 9-17 * * mon-fri", "2026-10-11 10:00", false}, // Sunday
		{"30 2 1 * *", "2026-11-01 02:30", true},
		{"@daily", "2026-10-12 00:00", true},
		{"0 0 * * 7", "2026-10-11 00:00", true}, // 7 = Sunday
		{"5,10 * * jan *", "2026-01-05 08:10", true},
	}
	for _, c := range cases {
		s, err := Parse(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := s.Matches(at(c.t)); got != c.want {
			t.Errorf("%s at %s = %v, want %v", c.expr, c.t, got, c.want)
		}
	}
}

func TestNextAndErrors(t *testing.T) {
	s, _ := Parse("0 */6 * * *")
	if n := s.Next(at("2026-10-12 07:10")); !n.Equal(at("2026-10-12 12:00")) {
		t.Fatal(n)
	}
	for _, bad := range []string{"* * *", "61 * * * *", "*/0 * * * *", "a b c d e"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}
