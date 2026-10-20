package databases

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCheckUpgrade(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		engine, from, to string
		want             string // "" = ok, "eol", or a substring of the error
	}{
		{Postgres, "16", "17", ""},
		{Postgres, "14", "18", ""},
		{Postgres, "17", "16", "not newer"},
		{Postgres, "17", "17", "already"},
		{Postgres, "17", "17@sha256:abc", "not available"},
		{Postgres, "17", "19", "not available"},
		{Postgres, "13", "17", "not in the catalog"},
		{MariaDB, "10.11", "11.4", ""},
		{MariaDB, "11.4", "11.8", ""},
		{MariaDB, "11.8", "11.4", "not newer"},
		{Mongo, "7.0", "8.0", ""},
		{Mongo, "8.0", "7.0", "not newer"},
		{MySQL, "8.4", "8.4", "already"},
	}
	for _, tc := range tests {
		_, err := CheckUpgrade(tc.engine, tc.from, tc.to, now)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s %s->%s: %v", tc.engine, tc.from, tc.to, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s %s->%s: err %v, want %q", tc.engine, tc.from, tc.to, err, tc.want)
		}
	}
	// A target past its end of life is refused with ErrEOL (Postgres 15 ends 2027-11-11).
	var eolErr *ErrEOL
	if _, err := CheckUpgrade(Postgres, "14", "15", time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC)); !errors.As(err, &eolErr) {
		t.Errorf("EOL target: %v", err)
	}
}

func TestCompareInventory(t *testing.T) {
	const a = "T|public.a\nT|public.we|ird\nC|public.a|5\nC|public.we|ird|7\n"
	tests := []struct {
		name, dst, want string
	}{
		{"same", a, ""},
		{"order and blank lines do not matter", "\nT|public.we|ird\nC|public.we|ird|7\nT|public.a\nC|public.a|5\n", ""},
		{"row count differs", strings.Replace(a, "|5", "|4", 1), "5 rows on the old server and 4"},
		{"table missing", "T|public.a\nC|public.a|5\n", "missing on the new server [public.we|ird]"},
		{"table extra", a + "T|public.z\n", "unexpected on it [public.z]"},
		{"count missing", "T|public.a\nT|public.we|ird\nC|public.a|5\n", "was not counted"},
	}
	for _, tc := range tests {
		err := CompareInventory(a, tc.dst)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: err %v, want %q", tc.name, err, tc.want)
		}
	}
	if err := CompareInventory("", ""); err != nil {
		t.Errorf("empty databases: %v", err)
	}
	// More tables than are counted: the list must match, the counts only where both sides have one.
	var src, dst strings.Builder
	for i := 0; i <= InventoryMax; i++ {
		src.WriteString("T|t" + string(rune('A'+i%26)) + string(rune('a'+i/26)) + "\n")
	}
	dst.WriteString(src.String())
	src.WriteString("C|tAa|1\n")
	if err := CompareInventory(src.String(), dst.String()); err != nil {
		t.Errorf("uncounted table beyond the limit: %v", err)
	}
}

func TestMissingExtensions(t *testing.T) {
	if got := MissingExtensions("pg_trgm\npostgis\n", "pg_trgm\nhstore\n"); len(got) != 1 || got[0] != "postgis" {
		t.Errorf("got %v", got)
	}
	if got := MissingExtensions("", "pg_trgm"); len(got) != 0 {
		t.Errorf("got %v", got)
	}
}

func TestRestoreForeign(t *testing.T) {
	c := Credentials{DBName: "d", Username: "u", Password: "p", RootPass: "r"}
	pg, _ := Restore(Postgres, true, c, true)
	if !strings.Contains(strings.Join(pg.Cmd, " "), "--no-acl") || !strings.Contains(strings.Join(pg.Cmd, " "), "--role=u") {
		t.Errorf("foreign postgres restore: %v", pg.Cmd)
	}
	pg, _ = Restore(Postgres, true, c, false)
	if strings.Contains(strings.Join(pg.Cmd, " "), "--no-acl") {
		t.Errorf("a restore into the same server keeps ACLs: %v", pg.Cmd)
	}
	m, _ := Restore(Mongo, false, c, true)
	j := strings.Join(m.Cmd, " ")
	if !strings.Contains(j, "--nsExclude=admin.*") || strings.Contains(j, "--drop") {
		t.Errorf("foreign mongo restore must skip admin.* and not drop: %v", m.Cmd)
	}
	m, _ = Restore(Mongo, false, c, false)
	if !strings.Contains(strings.Join(m.Cmd, " "), "--drop") {
		t.Errorf("same-server mongo restore: %v", m.Cmd)
	}
}
