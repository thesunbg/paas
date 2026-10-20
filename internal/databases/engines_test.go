package databases

import (
	"errors"
	"testing"
	"time"
)

func TestParseStats(t *testing.T) {
	for in, want := range map[string][2]int64{
		"8123456 3\n":            {8123456, 3},
		"8123456\t3\n":           {8123456, 3},
		"Warning: x\n1048576 12": {1048576, 12},
	} {
		size, conns, err := ParseStats(in)
		if err != nil || size != want[0] || int64(conns) != want[1] {
			t.Errorf("%q: got %d %d %v", in, size, conns, err)
		}
	}
	if _, _, err := ParseStats(""); err == nil {
		t.Error("empty output must fail")
	}
}

func TestVersions(t *testing.T) {
	if img, _ := ImageFor(Postgres, "16"); img != "docker.io/library/postgres:16" {
		t.Fatalf("16 %s", img)
	}
	if img, _ := ImageFor(MySQL, "11.8"); img != "docker.io/library/mariadb:11.8" {
		t.Fatalf("mysql %s", img)
	}
	for _, v := range []string{"", "9.6", "17@sha256:abc", "../x", "17-alpine", "1"} {
		if _, err := ImageFor(Postgres, v); err == nil {
			t.Fatalf("version %q accepted", v)
		}
	}
	if v := ParseVersion("mongod version v7.0.14\\nBuild Info"); v != "7.0.14" {
		t.Fatalf("parse %q", v)
	}
	if v := ParseVersion("Redis server v=7.4.1 sha=00000000"); v != "7.4.1" {
		t.Fatalf("parse %q", v)
	}
}

func TestStatus(t *testing.T) {
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	v := Version{"x", day(2026, 11, 12)}
	for _, c := range []struct {
		now  time.Time
		want string
	}{
		{day(2026, 11, 12).Add(-time.Nanosecond), StatusEnding},
		{day(2026, 11, 12), StatusEOL},
		{day(2026, 11, 13), StatusEOL},
		{v.EOL.Add(-EndingWindow), StatusOK},                       // exactly 180 days left
		{v.EOL.Add(-EndingWindow + time.Nanosecond), StatusEnding}, // just under
		{day(2020, 1, 1), StatusOK},
	} {
		if got := Status(v, c.now); got != c.want {
			t.Errorf("now=%v: got %s, want %s", c.now, got, c.want)
		}
	}
	if got := Status(Version{Name: "8"}, day(2100, 1, 1)); got != StatusOK {
		t.Errorf("no EOL date: got %s", got)
	}
}

func TestCheckCreate(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		engine, version, status string
		err                     string // "" ok, "eol", "bad"
	}{
		{MySQL, "10.6", "", "eol"},
		{Mongo, "6.0", "", "eol"},
		{Postgres, "14", StatusEnding, ""},
		{Postgres, "17", StatusOK, ""},
		{Redis, "8", StatusOK, ""},
		{Postgres, "17@sha256:0123", "", "bad"},
		{Postgres, "../x", "", "bad"},
		{"nope", "1", "", "bad"},
	} {
		st, err := CheckCreate(c.engine, c.version, now)
		var eol *ErrEOL
		switch {
		case c.err == "eol" && !errors.As(err, &eol):
			t.Errorf("%s %s: want ErrEOL, got %v", c.engine, c.version, err)
		case c.err == "bad" && (err == nil || errors.As(err, &eol)):
			t.Errorf("%s %s: want lookup error, got %v", c.engine, c.version, err)
		case c.err == "" && (err != nil || st != c.status):
			t.Errorf("%s %s: got %s, %v; want %s", c.engine, c.version, st, err, c.status)
		}
	}
}

// A new database without an explicit version gets the default, and a template, compose
// import or prisma flow that omits one must not hit an EOL gate.
func TestDefaultVersionsNotEOL(t *testing.T) {
	for name, e := range Engines {
		if _, err := CheckCreate(name, e.DefaultVersion, time.Now()); err != nil {
			t.Errorf("%s default %s: %v", name, e.DefaultVersion, err)
		}
	}
}
