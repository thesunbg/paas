package databases

import "testing"

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
	if img, _ := ImageFor(Postgres, ""); img != "docker.io/library/postgres:17-alpine" {
		t.Fatalf("legacy %s", img)
	}
	if img, _ := ImageFor(Postgres, "16"); img != "docker.io/library/postgres:16-alpine" {
		t.Fatalf("16 %s", img)
	}
	if img, _ := ImageFor(MySQL, "11"); img != "docker.io/library/mariadb:11" {
		t.Fatalf("legacy mysql %s", img)
	}
	if _, err := ImageFor(Postgres, "9.6"); err == nil {
		t.Fatal("unknown version accepted")
	}
	if v := ParseVersion("mongod version v7.0.14\\nBuild Info"); v != "7.0.14" {
		t.Fatalf("parse %q", v)
	}
	if v := ParseVersion("Redis server v=7.4.1 sha=00000000"); v != "7.4.1" {
		t.Fatalf("parse %q", v)
	}
}
