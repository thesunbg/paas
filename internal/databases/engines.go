// Package databases describes managed database engines: how to run a server,
// create logical databases on a shared server, back up and restore.
package databases

import (
	"fmt"
	"net/url"
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
	Name        string
	Image       string
	Port        int
	DataPath    string
	SharedOK    bool   // supports many logical databases on one server
	DefaultEnv  string // env var name injected into linked services
	MinMemoryMB int
}

var Engines = map[string]Engine{
	Postgres: {Name: Postgres, Image: "docker.io/library/postgres:17-alpine", Port: 5432, DataPath: "/var/lib/postgresql/data", SharedOK: true, DefaultEnv: "DATABASE_URL", MinMemoryMB: 128},
	MySQL:    {Name: MySQL, Image: "docker.io/library/mariadb:11", Port: 3306, DataPath: "/var/lib/mysql", SharedOK: true, DefaultEnv: "DATABASE_URL", MinMemoryMB: 192},
	Redis:    {Name: Redis, Image: "docker.io/library/redis:7-alpine", Port: 6379, DataPath: "/data", DefaultEnv: "REDIS_URL", MinMemoryMB: 32},
	Mongo:    {Name: Mongo, Image: "docker.io/library/mongo:7", Port: 27017, DataPath: "/data/db", DefaultEnv: "MONGODB_URL", MinMemoryMB: 256},
}

// Preset is a sizing preset: container limit plus engine tuning derived from it.
type Preset struct {
	Name     string
	MemoryMB int
	CPU      int // millis
	MaxConns int
}

var Presets = []Preset{
	{"nano", 128, 250, 25},
	{"micro", 256, 500, 50},
	{"small", 512, 1000, 100},
	{"medium", 1024, 2000, 150},
	{"large", 2048, 4000, 200},
	{"xlarge", 4096, 8000, 300},
}

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
func ServerSpec(engine string, preset Preset, shared bool, c Credentials) (model.ServiceSpec, error) {
	e, ok := Engines[engine]
	if !ok {
		return model.ServiceSpec{}, fmt.Errorf("unknown engine %q", engine)
	}
	mem := preset.MemoryMB
	if mem < e.MinMemoryMB {
		mem = e.MinMemoryMB
	}
	spec := model.ServiceSpec{
		Port:     e.Port,
		Replicas: 1,
		Image:    e.Image,
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

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
