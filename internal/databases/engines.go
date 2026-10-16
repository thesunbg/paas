// Package databases describes managed database engines: how to run a server,
// create logical databases on a shared server, back up and restore.
package databases

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/thesunbg/paas/internal/model"
)

const (
	Postgres = "postgres"
	MySQL    = "mysql"
	Redis    = "redis"
	Mongo    = "mongo"
)

// Engine describes one database engine.
type Engine struct {
	Name        string `json:"name"`
	Image       string `json:"image"`
	Port        int    `json:"port"`
	DataPath    string `json:"data_path"`
	SharedOK    bool   `json:"shared"`      // supports many logical databases on one server
	DefaultEnv  string `json:"default_env"` // env var name injected into linked services
	MinMemoryMB int    `json:"min_memory_mb"`
	// Versions offered for new databases (newest first) and the default one.
	Versions       []string `json:"versions"`
	DefaultVersion string   `json:"default_version"`
	// LegacyVersion is what Image runs, for databases created before versions existed.
	LegacyVersion string `json:"legacy_version"`
}

var Engines = map[string]Engine{
	Postgres: {Name: Postgres, Image: "docker.io/library/postgres:17-alpine", Port: 5432, DataPath: "/var/lib/postgresql/data", SharedOK: true, DefaultEnv: "DATABASE_URL", MinMemoryMB: 128,
		Versions: []string{"18", "17", "16", "15", "14"}, DefaultVersion: "17", LegacyVersion: "17"},
	MySQL: {Name: MySQL, Image: "docker.io/library/mariadb:11", Port: 3306, DataPath: "/var/lib/mysql", SharedOK: true, DefaultEnv: "DATABASE_URL", MinMemoryMB: 192,
		Versions: []string{"11.8", "11.4", "10.11", "10.6"}, DefaultVersion: "11.8", LegacyVersion: "11"},
	Redis: {Name: Redis, Image: "docker.io/library/redis:7-alpine", Port: 6379, DataPath: "/data", DefaultEnv: "REDIS_URL", MinMemoryMB: 32,
		Versions: []string{"8", "7.4", "7.2"}, DefaultVersion: "7.4", LegacyVersion: "7"},
	Mongo: {Name: Mongo, Image: "docker.io/library/mongo:7", Port: 27017, DataPath: "/data/db", DefaultEnv: "MONGODB_URL", MinMemoryMB: 256,
		Versions: []string{"8.0", "7.0", "6.0"}, DefaultVersion: "7.0", LegacyVersion: "7"},
}

// ImageFor returns the image of an engine version ("" = the legacy image).
func ImageFor(engine, version string) (string, error) {
	e, ok := Engines[engine]
	if !ok {
		return "", fmt.Errorf("unknown engine %q", engine)
	}
	if version == "" || version == e.LegacyVersion && !contains(e.Versions, version) {
		return e.Image, nil
	}
	if !contains(e.Versions, version) {
		return "", fmt.Errorf("%s %s is not available (choose one of %s)", engine, version, strings.Join(e.Versions, ", "))
	}
	switch engine {
	case Postgres:
		return "docker.io/library/postgres:" + version + "-alpine", nil
	case MySQL:
		return "docker.io/library/mariadb:" + version, nil
	case Redis:
		return "docker.io/library/redis:" + version + "-alpine", nil
	default:
		return "docker.io/library/mongo:" + version, nil
	}
}

// DisplayVersion is the version shown for a database ("" = legacy).
func DisplayVersion(engine, version string) string {
	if version != "" {
		return version
	}
	return Engines[engine].LegacyVersion
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Preset is a sizing preset: container limit plus engine tuning derived from it.
type Preset struct {
	Name     string `json:"name"`
	MemoryMB int    `json:"memory_mb"`
	CPU      int    `json:"cpu_millis"`
	MaxConns int    `json:"max_connections"`
	QuotaMB  int    `json:"storage_quota_mb"` // default storage quota of a dedicated server
}

var Presets = []Preset{
	{"nano", 128, 250, 25, 1024},
	{"micro", 256, 500, 50, 2048},
	{"small", 512, 1000, 100, 5120},
	{"medium", 1024, 2000, 150, 10240},
	{"large", 2048, 4000, 200, 20480},
	{"xlarge", 4096, 8000, 300, 51200},
}

// SharedQuotaMB is the default storage quota of a logical database on a shared server.
const SharedQuotaMB = 1024

func PresetByName(name string) (Preset, bool) {
	for _, p := range Presets {
		if p.Name == name {
			return p, true
		}
	}
	return Preset{}, false
}

// Credentials identify the database a service connects to.
type Credentials struct {
	Host     string
	Port     int
	DBName   string
	Username string
	Password string
	RootPass string // shared servers: superuser password
}

// ServerSpec builds the service spec of a database server container.
// For dedicated servers creds are the app credentials; for shared servers only RootPass is used.
func ServerSpec(engine, version string, preset Preset, shared bool, c Credentials) (model.ServiceSpec, error) {
	e, ok := Engines[engine]
	if !ok {
		return model.ServiceSpec{}, fmt.Errorf("unknown engine %q", engine)
	}
	image, err := ImageFor(engine, version)
	if err != nil {
		return model.ServiceSpec{}, err
	}
	mem := preset.MemoryMB
	if mem < e.MinMemoryMB {
		mem = e.MinMemoryMB
	}
	spec := model.ServiceSpec{
		Port:     e.Port,
		Replicas: 1,
		Image:    image,
		Resources: model.Resources{
			MemoryLimitMB: mem, MemoryRequestMB: mem,
			CPULimitMillis: preset.CPU, CPURequestMillis: preset.CPU / 4,
		},
		QoS:         model.QoSGuaranteed,
		HealthCheck: model.HealthCheck{Type: "tcp", TimeoutSec: 180},
		Volumes:     []model.Volume{{Name: "data", Path: e.DataPath}},
		Env:         map[string]string{},
		Language:    engine,
	}
	switch engine {
	case Postgres:
		shm := mem / 4
		spec.Env["PGDATA"] = e.DataPath + "/pgdata"
		if shared {
			spec.Env["POSTGRES_USER"] = "postgres"
			spec.Env["POSTGRES_PASSWORD"] = c.RootPass
		} else {
			spec.Env["POSTGRES_USER"] = c.Username
			spec.Env["POSTGRES_PASSWORD"] = c.Password
			spec.Env["POSTGRES_DB"] = c.DBName
		}
		spec.Command = []string{"docker-entrypoint.sh", "postgres",
			"-c", "shared_buffers=" + strconv.Itoa(shm) + "MB",
			"-c", "effective_cache_size=" + strconv.Itoa(mem*3/4) + "MB",
			"-c", "work_mem=" + strconv.Itoa(max(1, mem/preset.MaxConns/2)) + "MB",
			"-c", "maintenance_work_mem=" + strconv.Itoa(max(16, mem/16)) + "MB",
			"-c", "max_connections=" + strconv.Itoa(preset.MaxConns),
		}
	case MySQL:
		spec.Env["MARIADB_ROOT_PASSWORD"] = firstNonEmpty(c.RootPass, c.Password)
		if !shared {
			spec.Env["MARIADB_DATABASE"] = c.DBName
			spec.Env["MARIADB_USER"] = c.Username
			spec.Env["MARIADB_PASSWORD"] = c.Password
		}
		spec.Command = []string{"docker-entrypoint.sh", "mariadbd",
			"--innodb-buffer-pool-size=" + strconv.Itoa(max(32, mem*45/100)) + "M",
			"--max-connections=" + strconv.Itoa(preset.MaxConns),
			"--performance-schema=OFF",
			"--skip-name-resolve",
		}
	case Redis:
		spec.Command = []string{"redis-server", "--requirepass", c.Password, "--appendonly", "yes",
			"--maxmemory", strconv.Itoa(max(16, mem*80/100)) + "mb", "--maxmemory-policy", "noeviction"}
	case Mongo:
		spec.Env["MONGO_INITDB_ROOT_USERNAME"] = c.Username
		spec.Env["MONGO_INITDB_ROOT_PASSWORD"] = c.Password
		cache := float64(mem) * 0.4 / 1024
		if cache < 0.25 {
			cache = 0.25
		}
		spec.Command = []string{"docker-entrypoint.sh", "mongod", "--wiredTigerCacheSizeGB", strconv.FormatFloat(cache, 'f', 2, 64)}
	}
	return spec, nil
}

// URL builds the connection string for a linked service.
func URL(engine string, c Credentials) string {
	user := url.UserPassword(c.Username, c.Password)
	host := c.Host + ":" + strconv.Itoa(c.Port)
	switch engine {
	case Postgres:
		return (&url.URL{Scheme: "postgres", User: user, Host: host, Path: "/" + c.DBName, RawQuery: "sslmode=disable"}).String()
	case MySQL:
		return (&url.URL{Scheme: "mysql", User: user, Host: host, Path: "/" + c.DBName}).String()
	case Redis:
		return (&url.URL{Scheme: "redis", User: url.UserPassword("default", c.Password), Host: host}).String()
	case Mongo:
		return (&url.URL{Scheme: "mongodb", User: user, Host: host, Path: "/" + c.DBName, RawQuery: "authSource=admin"}).String()
	}
	return ""
}

// Command is something to execute inside the database server container.
type Command struct {
	Cmd   []string
	Env   []string
	Stdin string
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func quoteLit(s string) string   { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
func quoteMy(s string) string    { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

// CreateLogical returns the command that creates a database and its owner on a shared server.
func CreateLogical(engine string, c Credentials, maxConns int) (Command, error) {
	switch engine {
	case Postgres:
		sql := fmt.Sprintf(`DO $$ BEGIN
IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = %[1]s) THEN
  CREATE ROLE %[2]s LOGIN PASSWORD %[3]s CONNECTION LIMIT %[4]d;
END IF; END $$;
SELECT 'CREATE DATABASE %[5]s OWNER %[2]s' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = %[6]s)\gexec
REVOKE ALL ON DATABASE %[5]s FROM PUBLIC;
GRANT ALL ON DATABASE %[5]s TO %[2]s;
`, quoteLit(c.Username), quoteIdent(c.Username), quoteLit(c.Password), maxConns, quoteIdent(c.DBName), quoteLit(c.DBName))
		return Command{Cmd: []string{"psql", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "postgres"}, Stdin: sql}, nil
	case MySQL:
		sql := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s;\nCREATE USER IF NOT EXISTS %s@'%%' IDENTIFIED BY %s WITH MAX_USER_CONNECTIONS %d;\nGRANT ALL PRIVILEGES ON %s.* TO %s@'%%';\nFLUSH PRIVILEGES;\n",
			quoteMy(c.DBName), quoteLit(c.Username), quoteLit(c.Password), maxConns, quoteMy(c.DBName), quoteLit(c.Username))
		return Command{Cmd: []string{"mariadb", "-uroot"}, Env: []string{"MYSQL_PWD=" + c.RootPass}, Stdin: sql}, nil
	}
	return Command{}, fmt.Errorf("%s does not support shared servers", engine)
}

// DropLogical removes a database and its owner from a shared server.
func DropLogical(engine string, c Credentials) (Command, error) {
	switch engine {
	case Postgres:
		sql := fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE);\nDROP ROLE IF EXISTS %s;\n", quoteIdent(c.DBName), quoteIdent(c.Username))
		return Command{Cmd: []string{"psql", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "postgres"}, Stdin: sql}, nil
	case MySQL:
		sql := fmt.Sprintf("DROP DATABASE IF EXISTS %s;\nDROP USER IF EXISTS %s@'%%';\n", quoteMy(c.DBName), quoteLit(c.Username))
		return Command{Cmd: []string{"mariadb", "-uroot"}, Env: []string{"MYSQL_PWD=" + c.RootPass}, Stdin: sql}, nil
	}
	return Command{}, fmt.Errorf("%s does not support shared servers", engine)
}

// Backup returns a command whose stdout is a full dump of one database.
func Backup(engine string, shared bool, c Credentials) (Command, string, error) {
	switch engine {
	case Postgres:
		user := c.Username
		if shared {
			user = "postgres"
		}
		return Command{Cmd: []string{"pg_dump", "-U", user, "-Fc", "-d", c.DBName}}, "dump", nil
	case MySQL:
		return Command{Cmd: []string{"mariadb-dump", "-uroot", "--single-transaction", "--routines", "--databases", c.DBName}, Env: []string{"MYSQL_PWD=" + firstNonEmpty(c.RootPass, c.Password)}}, "sql", nil
	case Redis:
		return Command{Cmd: []string{"sh", "-c", "redis-cli --no-auth-warning -a \"$REDIS_PASSWORD\" SAVE >/dev/null && cat /data/dump.rdb"}, Env: []string{"REDIS_PASSWORD=" + c.Password}}, "rdb", nil
	case Mongo:
		return Command{Cmd: []string{"mongodump", "--quiet", "--archive", "--gzip", "-u", c.Username, "-p", c.Password, "--authenticationDatabase", "admin"}}, "archive.gz", nil
	}
	return Command{}, "", fmt.Errorf("unknown engine %q", engine)
}

// Restore returns a command that reads a dump on stdin.
func Restore(engine string, shared bool, c Credentials) (Command, error) {
	switch engine {
	case Postgres:
		user := c.Username
		if shared {
			user = "postgres"
		}
		cmd := []string{"pg_restore", "-U", user, "--clean", "--if-exists", "--no-owner", "-d", c.DBName}
		if shared {
			cmd = append(cmd, "--role="+c.Username)
		}
		return Command{Cmd: cmd}, nil
	case MySQL:
		return Command{Cmd: []string{"mariadb", "-uroot"}, Env: []string{"MYSQL_PWD=" + firstNonEmpty(c.RootPass, c.Password)}}, nil
	case Mongo:
		return Command{Cmd: []string{"mongorestore", "--quiet", "--archive", "--gzip", "--drop", "-u", c.Username, "-p", c.Password, "--authenticationDatabase", "admin"}}, nil
	}
	return Command{}, fmt.Errorf("restore is not supported for %s yet", engine)
}

// Stats returns a command printing the database's size in bytes and its open connections.
func Stats(engine string, shared bool, c Credentials) (Command, error) {
	switch engine {
	case Postgres:
		user := c.Username
		if shared {
			user = "postgres"
		}
		q := fmt.Sprintf("SELECT pg_database_size(%[1]s), (SELECT count(*) FROM pg_stat_activity WHERE datname = %[1]s)", quoteLit(c.DBName))
		return Command{Cmd: []string{"psql", "-At", "-F", " ", "-U", user, "-d", "postgres", "-c", q}}, nil
	case MySQL:
		q := fmt.Sprintf("SELECT (SELECT COALESCE(SUM(data_length+index_length),0) FROM information_schema.tables WHERE table_schema=%[1]s), (SELECT COUNT(*) FROM information_schema.processlist WHERE db=%[1]s)", quoteLit(c.DBName))
		return Command{Cmd: []string{"mariadb", "-uroot", "-N", "-B", "-e", q}, Env: []string{"MYSQL_PWD=" + firstNonEmpty(c.RootPass, c.Password)}}, nil
	case Redis:
		return Command{Cmd: []string{"sh", "-c", `redis-cli --no-auth-warning -a "$REDIS_PASSWORD" INFO | awk -F: '/^used_memory:/{m=$2} /^connected_clients:/{c=$2} END{gsub(/\r/,"",m); gsub(/\r/,"",c); print m, c}'`}, Env: []string{"REDIS_PASSWORD=" + c.Password}}, nil
	case Mongo:
		js := `const t = db.getSiblingDB("admin").runCommand({listDatabases: 1}).totalSize; print(t + " " + db.serverStatus().connections.current)`
		return Command{Cmd: []string{"mongosh", "--quiet", "-u", c.Username, "-p", c.Password, "--authenticationDatabase", "admin", "--eval", js}}, nil
	}
	return Command{}, fmt.Errorf("unknown engine %q", engine)
}

// VersionCommand prints the exact server version.
func VersionCommand(engine string, shared bool, c Credentials) Command {
	switch engine {
	case Postgres:
		user := c.Username
		if shared {
			user = "postgres"
		}
		return Command{Cmd: []string{"psql", "-At", "-U", user, "-d", "postgres", "-c", "SHOW server_version"}}
	case MySQL:
		return Command{Cmd: []string{"mariadb", "-uroot", "-N", "-B", "-e", "SELECT VERSION()"}, Env: []string{"MYSQL_PWD=" + firstNonEmpty(c.RootPass, c.Password)}}
	case Redis:
		return Command{Cmd: []string{"redis-server", "--version"}}
	default:
		return Command{Cmd: []string{"mongod", "--version"}}
	}
}

// ParseVersion extracts the version number from VersionCommand's output.
func ParseVersion(out string) string {
	m := versionRe.FindString(out)
	return m
}

var versionRe = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)

// ParseStats reads the output of Stats.
func ParseStats(out string) (size int64, conns int, err error) {
	f := strings.Fields(out)
	if len(f) < 2 {
		return 0, 0, fmt.Errorf("unexpected stats output %q", strings.TrimSpace(out))
	}
	size, err = strconv.ParseInt(strings.SplitN(f[len(f)-2], ".", 2)[0], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	conns, err = strconv.Atoi(f[len(f)-1])
	return size, conns, err
}

// SetReadOnly turns writes off (or back on) for the database, used when it exceeds its quota.
func SetReadOnly(engine string, shared bool, c Credentials, on bool) (Command, error) {
	switch engine {
	case Postgres:
		user := c.Username
		if shared {
			user = "postgres"
		}
		sql := fmt.Sprintf("ALTER DATABASE %s SET default_transaction_read_only = off;\n", quoteIdent(c.DBName))
		if on {
			sql = fmt.Sprintf("ALTER DATABASE %s SET default_transaction_read_only = on;\nSELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE datname = %s AND pid <> pg_backend_pid();\n",
				quoteIdent(c.DBName), quoteLit(c.DBName))
		}
		return Command{Cmd: []string{"psql", "-v", "ON_ERROR_STOP=1", "-U", user, "-d", "postgres"}, Stdin: sql}, nil
	case MySQL:
		sql := fmt.Sprintf("GRANT ALL PRIVILEGES ON %s.* TO %s@'%%';\nFLUSH PRIVILEGES;\n", quoteMy(c.DBName), quoteLit(c.Username))
		if on {
			sql = fmt.Sprintf("REVOKE INSERT, UPDATE, CREATE, ALTER, INDEX ON %s.* FROM %s@'%%';\nFLUSH PRIVILEGES;\n", quoteMy(c.DBName), quoteLit(c.Username))
		}
		return Command{Cmd: []string{"mariadb", "-uroot"}, Env: []string{"MYSQL_PWD=" + firstNonEmpty(c.RootPass, c.Password)}, Stdin: sql}, nil
	}
	return Command{}, fmt.Errorf("read-only mode is not supported for %s", engine)
}

// Extra users: "readonly" can read every table, "readwrite" can also change rows.
// Neither can change the schema; the owner account keeps that.

// CreateUser returns the command adding a user with a role to the database.
func CreateUser(engine string, shared bool, c Credentials, name, password, role string) (Command, error) {
	if role != "readonly" && role != "readwrite" {
		return Command{}, fmt.Errorf("role must be readonly or readwrite")
	}
	switch engine {
	case Postgres:
		priv := "SELECT"
		if role == "readwrite" {
			priv = "SELECT, INSERT, UPDATE, DELETE"
		}
		seq := "SELECT"
		if role == "readwrite" {
			seq = "USAGE, SELECT, UPDATE"
		}
		u, owner, db := quoteIdent(name), quoteIdent(c.Username), quoteIdent(c.DBName)
		sql := fmt.Sprintf(`CREATE ROLE %[1]s LOGIN PASSWORD %[2]s CONNECTION LIMIT 20;
GRANT CONNECT ON DATABASE %[3]s TO %[1]s;
\c %[3]s
GRANT USAGE ON SCHEMA public TO %[1]s;
GRANT %[4]s ON ALL TABLES IN SCHEMA public TO %[1]s;
GRANT %[5]s ON ALL SEQUENCES IN SCHEMA public TO %[1]s;
ALTER DEFAULT PRIVILEGES FOR ROLE %[6]s IN SCHEMA public GRANT %[4]s ON TABLES TO %[1]s;
ALTER DEFAULT PRIVILEGES FOR ROLE %[6]s IN SCHEMA public GRANT %[5]s ON SEQUENCES TO %[1]s;
`, u, quoteLit(password), db, priv, seq, owner)
		admin := c.Username
		if shared {
			admin = "postgres"
		}
		return Command{Cmd: []string{"psql", "-v", "ON_ERROR_STOP=1", "-U", admin, "-d", "postgres"}, Stdin: sql}, nil
	case MySQL:
		priv := "SELECT"
		if role == "readwrite" {
			priv = "SELECT, INSERT, UPDATE, DELETE"
		}
		sql := fmt.Sprintf("CREATE USER %[1]s@'%%' IDENTIFIED BY %[2]s WITH MAX_USER_CONNECTIONS 20;\nGRANT %[3]s ON %[4]s.* TO %[1]s@'%%';\nFLUSH PRIVILEGES;\n",
			quoteLit(name), quoteLit(password), priv, quoteMy(c.DBName))
		return Command{Cmd: []string{"mariadb", "-uroot"}, Env: []string{"MYSQL_PWD=" + firstNonEmpty(c.RootPass, c.Password)}, Stdin: sql}, nil
	}
	return Command{}, fmt.Errorf("extra users are supported for PostgreSQL and MySQL")
}

// DropUser removes an extra user.
func DropUser(engine string, shared bool, c Credentials, name string) (Command, error) {
	switch engine {
	case Postgres:
		u, owner, db := quoteIdent(name), quoteIdent(c.Username), quoteIdent(c.DBName)
		sql := fmt.Sprintf(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = %[4]s;
\c %[3]s
ALTER DEFAULT PRIVILEGES FOR ROLE %[2]s IN SCHEMA public REVOKE ALL ON TABLES FROM %[1]s;
ALTER DEFAULT PRIVILEGES FOR ROLE %[2]s IN SCHEMA public REVOKE ALL ON SEQUENCES FROM %[1]s;
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM %[1]s;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM %[1]s;
REVOKE ALL ON SCHEMA public FROM %[1]s;
\c postgres
REVOKE ALL ON DATABASE %[3]s FROM %[1]s;
DROP ROLE IF EXISTS %[1]s;
`, u, owner, db, quoteLit(name))
		admin := c.Username
		if shared {
			admin = "postgres"
		}
		return Command{Cmd: []string{"psql", "-v", "ON_ERROR_STOP=1", "-U", admin, "-d", "postgres"}, Stdin: sql}, nil
	case MySQL:
		sql := fmt.Sprintf("DROP USER IF EXISTS %s@'%%';\n", quoteLit(name))
		return Command{Cmd: []string{"mariadb", "-uroot"}, Env: []string{"MYSQL_PWD=" + firstNonEmpty(c.RootPass, c.Password)}, Stdin: sql}, nil
	}
	return Command{}, fmt.Errorf("extra users are supported for PostgreSQL and MySQL")
}

// SetPassword changes a user's password.
func SetPassword(engine string, shared bool, c Credentials, name, password string) (Command, error) {
	switch engine {
	case Postgres:
		admin := c.Username
		if shared {
			admin = "postgres"
		}
		return Command{Cmd: []string{"psql", "-v", "ON_ERROR_STOP=1", "-U", admin, "-d", "postgres"},
			Stdin: fmt.Sprintf("ALTER ROLE %s PASSWORD %s;\n", quoteIdent(name), quoteLit(password))}, nil
	case MySQL:
		return Command{Cmd: []string{"mariadb", "-uroot"}, Env: []string{"MYSQL_PWD=" + firstNonEmpty(c.RootPass, c.Password)},
			Stdin: fmt.Sprintf("ALTER USER %s@'%%' IDENTIFIED BY %s;\nFLUSH PRIVILEGES;\n", quoteLit(name), quoteLit(password))}, nil
	}
	return Command{}, fmt.Errorf("not supported for %s", engine)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// ImportExitDump is the exit code of an import whose source could not be dumped.
const ImportExitDump = 3

// importFile holds the dump during an import, on the data volume (not the
// container's root filesystem, which can be small).
func importFile(engine string) string { return Engines[engine].DataPath + "/.paas-import" }

// ImportFrom returns a command, run inside the database server, that copies
// the database at sourceURL into this one (existing objects are replaced).
// The source credentials travel in the environment, never on a command line.
func ImportFrom(engine string, shared bool, c Credentials, sourceURL string) (Command, error) {
	u, err := url.Parse(sourceURL)
	if err != nil || u.Host == "" {
		return Command{}, fmt.Errorf("invalid source URL")
	}
	file := importFile(engine)
	switch engine {
	case Postgres:
		if u.Scheme != "postgres" && u.Scheme != "postgresql" {
			return Command{}, fmt.Errorf("the source must be a postgres:// URL")
		}
		user := c.Username
		role := ""
		if shared {
			user, role = "postgres", " --role="+shQuote(c.Username)
		}
		script := fmt.Sprintf(`F=%[1]s
pg_dump --no-owner --no-acl -Fc -d "$SRC_URL" -f "$F" || { rm -f "$F"; exit %[2]d; }
pg_restore -U %[3]s --clean --if-exists --no-owner --no-acl%[4]s -d %[5]s "$F"
rc=$?; rm -f "$F"; exit $rc`, shQuote(file), ImportExitDump, shQuote(user), role, shQuote(c.DBName))
		return Command{Cmd: []string{"sh", "-c", script}, Env: []string{"SRC_URL=" + sourceURL}}, nil
	case MySQL:
		if u.Scheme != "mysql" && u.Scheme != "mariadb" {
			return Command{}, fmt.Errorf("the source must be a mysql:// URL")
		}
		src := strings.TrimPrefix(u.Path, "/")
		if src == "" {
			return Command{}, fmt.Errorf("the source URL must name a database (mysql://user:pass@host:3306/name)")
		}
		pass, _ := u.User.Password()
		port := firstNonEmpty(u.Port(), "3306")
		// MariaDB 11.4+ clients verify the server certificate by default, which
		// fails against most MySQL servers; older clients lack the option.
		script := fmt.Sprintf(`F=%[1]s
SSL=""; mariadb-dump --help 2>/dev/null | grep -q ssl-verify-server-cert && SSL="--skip-ssl-verify-server-cert"
MYSQL_PWD="$SRC_PASS" mariadb-dump $SSL -h "$SRC_HOST" -P "$SRC_PORT" -u "$SRC_USER" --single-transaction --routines --triggers --no-tablespaces "$SRC_DB" > "$F" || { rm -f "$F"; exit %[2]d; }
MYSQL_PWD="$DST_PASS" mariadb -uroot %[3]s < "$F"
rc=$?; rm -f "$F"; exit $rc`, shQuote(file), ImportExitDump, shQuote(c.DBName))
		return Command{Cmd: []string{"sh", "-c", script}, Env: []string{
			"SRC_HOST=" + u.Hostname(), "SRC_PORT=" + port, "SRC_USER=" + u.User.Username(), "SRC_PASS=" + pass, "SRC_DB=" + src,
			"DST_PASS=" + firstNonEmpty(c.RootPass, c.Password),
		}}, nil
	case Mongo:
		if u.Scheme != "mongodb" && u.Scheme != "mongodb+srv" {
			return Command{}, fmt.Errorf("the source must be a mongodb:// URL")
		}
		src := strings.TrimPrefix(u.Path, "/")
		if src == "" {
			return Command{}, fmt.Errorf("the source URL must name a database (mongodb://user:pass@host:27017/name)")
		}
		script := fmt.Sprintf(`F=%[1]s
mongodump --quiet --uri "$SRC_URL" --archive="$F" --gzip || { rm -f "$F"; exit %[2]d; }
mongorestore --quiet --archive="$F" --gzip --drop -u "$DST_USER" -p "$DST_PASS" --authenticationDatabase admin --nsFrom "$SRC_DB.*" --nsTo %[3]s
rc=$?; rm -f "$F"; exit $rc`, shQuote(file), ImportExitDump, shQuote(c.DBName+".*"))
		return Command{Cmd: []string{"sh", "-c", script}, Env: []string{"SRC_URL=" + sourceURL, "SRC_DB=" + src, "DST_USER=" + c.Username, "DST_PASS=" + c.Password}}, nil
	}
	return Command{}, fmt.Errorf("importing is not supported for %s; restore a dump file instead", engine)
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }
