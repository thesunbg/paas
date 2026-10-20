package databases

import (
	"errors"
	"slices"
	"strings"
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
	if img, _ := ImageFor(MariaDB, "11.8"); img != "docker.io/library/mariadb:11.8" {
		t.Fatalf("mariadb %s", img)
	}
	if img, _ := ImageFor(MySQL, "8.4"); img != "docker.io/library/mysql:8.4" {
		t.Fatalf("mysql %s", img)
	}
	if _, err := ImageFor(MySQL, "11.8"); err == nil {
		t.Fatal("mysql accepted a MariaDB version")
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
		{MariaDB, "10.6", "", "eol"},
		{MySQL, "8.4", StatusOK, ""},
		{MySQL, "10.6", "", "bad"},
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

// The MySQL family shares SQL and differs in binaries, image env variables and server flags.
func TestMySQLFamily(t *testing.T) {
	c := Credentials{DBName: "app", Username: "u", Password: "p", RootPass: "r"}
	preset, _ := PresetByName("small")
	for _, tc := range []struct {
		engine, version              string
		client, dump, server, prefix string
		perfSchemaOff                bool
	}{
		{MariaDB, "11.8", "mariadb", "mariadb-dump", "mariadbd", "MARIADB", true},
		{MySQL, "8.4", "mysql", "mysqldump", "mysqld", "MYSQL", false},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			// Shared server: only the root password. Dedicated: also the app database and user.
			sh, err := ServerSpec(tc.engine, tc.version, preset, true, c)
			if err != nil {
				t.Fatal(err)
			}
			if sh.Env[tc.prefix+"_ROOT_PASSWORD"] != "r" || len(sh.Env) != 1 {
				t.Errorf("shared env %v", sh.Env)
			}
			if sh.Command[1] != tc.server {
				t.Errorf("server %q, want %q", sh.Command[1], tc.server)
			}
			if has := slices.Contains(sh.Command, "--performance-schema=OFF"); has != tc.perfSchemaOff {
				t.Errorf("performance-schema flag present=%v", has)
			}
			if sh.Language != tc.engine {
				t.Errorf("language %q", sh.Language)
			}
			ded, _ := ServerSpec(tc.engine, tc.version, preset, false, c)
			if ded.Env[tc.prefix+"_ROOT_PASSWORD"] != "r" || ded.Env[tc.prefix+"_DATABASE"] != "app" ||
				ded.Env[tc.prefix+"_USER"] != "u" || ded.Env[tc.prefix+"_PASSWORD"] != "p" {
				t.Errorf("dedicated env %v", ded.Env)
			}
			for k := range ded.Env {
				if !strings.HasPrefix(k, tc.prefix+"_") {
					t.Errorf("env %s has the other family's prefix", k)
				}
			}
			// Every client command runs the family's own client and logs in as root.
			create, _ := CreateLogical(tc.engine, c, 10)
			drop, _ := DropLogical(tc.engine, c)
			ro, _ := SetReadOnly(tc.engine, true, c, true)
			rw, _ := SetReadOnly(tc.engine, true, c, false)
			user, _ := CreateUser(tc.engine, true, c, "x", "y", "readonly")
			dropU, _ := DropUser(tc.engine, true, c, "x")
			pw, _ := SetPassword(tc.engine, true, c, "x", "z")
			stats, _ := Stats(tc.engine, true, c)
			restore, _ := Restore(tc.engine, true, c)
			ver := VersionCommand(tc.engine, true, c)
			for name, cmd := range map[string]Command{"create": create, "drop": drop, "ro": ro, "rw": rw, "user": user, "dropuser": dropU, "pw": pw, "stats": stats, "restore": restore, "version": ver} {
				if len(cmd.Cmd) < 2 || cmd.Cmd[0] != tc.client || cmd.Cmd[1] != "-uroot" || cmd.Env[0] != "MYSQL_PWD=r" {
					t.Errorf("%s: %v %v", name, cmd.Cmd, cmd.Env)
				}
			}
			if !strings.Contains(ver.Cmd[len(ver.Cmd)-1], "SELECT VERSION()") {
				t.Errorf("version query %v", ver.Cmd)
			}
			if !strings.Contains(ro.Stdin, "REVOKE INSERT, UPDATE") || !strings.Contains(rw.Stdin, "GRANT ALL PRIVILEGES") {
				t.Errorf("read-only SQL: %q / %q", ro.Stdin, rw.Stdin)
			}
			if !strings.Contains(create.Stdin, "CREATE USER IF NOT EXISTS 'u'@'%' IDENTIFIED BY 'p' WITH MAX_USER_CONNECTIONS 10") {
				t.Errorf("create SQL: %q", create.Stdin)
			}
			bak, ext, _ := Backup(tc.engine, true, c)
			if bak.Cmd[0] != tc.dump || ext != "sql" {
				t.Errorf("backup %v %s", bak.Cmd, ext)
			}
			if got := URL(tc.engine, Credentials{Host: "h", Port: 3306, Username: "u", Password: "p", DBName: "app"}); got != "mysql://u:p@h:3306/app" {
				t.Errorf("url %s", got)
			}
			// Import: the family's dump tool, and the MariaDB-only certificate flag only for it.
			imp, err := ImportFrom(tc.engine, true, c, "mysql://a:b@old:3306/src")
			if err != nil {
				t.Fatal(err)
			}
			script := imp.Cmd[2]
			if !strings.Contains(script, "D="+tc.dump+";") || !strings.Contains(script, tc.client+" -uroot 'app'") {
				t.Errorf("import script uses the wrong tools:\n%s", script)
			}
			other := "mysqldump"
			if tc.engine == MySQL {
				other = "mariadb-dump"
			}
			if strings.Contains(script, other) {
				t.Errorf("import script mentions %s:\n%s", other, script)
			}
			if has := strings.Contains(script, "skip-ssl-verify-server-cert"); has && tc.engine == MySQL {
				t.Errorf("MySQL import carries the MariaDB certificate flag")
			}
		})
	}
}
