// Package templates holds one-click application templates and Docker Compose import.
package templates

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"

	"github.com/thesunbg/paas/internal/model"
)

// Service is a service created by a template.
type Service struct {
	Name    string            `json:"name"`
	Image   string            `json:"image"`
	Type    string            `json:"type"` // web | worker
	Port    int               `json:"port,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Volumes []model.Volume    `json:"volumes,omitempty"`
	Command []string          `json:"command,omitempty"`
	Size    string            `json:"size,omitempty"`
	QoS     string            `json:"qos,omitempty"` // "": burstable
	Health  string            `json:"health_path,omitempty"`
	// HealthTimeout is how long the first start may take (seconds; default 180).
	// JVM apps that set themselves up on first boot need more on small servers.
	HealthTimeout int `json:"health_timeout,omitempty"`
	// Build is set for services built from the imported repository.
	Build *Build `json:"build,omitempty"`
	// Private services get no public domain; other services reach them by name.
	Private bool `json:"private,omitempty"`
	// Links are environment variables that receive a database's URL (env -> database name).
	Links map[string]string `json:"links,omitempty"`
	// Inputs are variables the user fills in before the services are created.
	Inputs []EnvInput `json:"inputs,omitempty"`
	// DependsOn names services (of the same plan) to start first.
	DependsOn []string `json:"depends_on,omitempty"`
	// KeepAwake: the app does work without requests (schedules, polling triggers), so it
	// does not get sleep unless the installer asks for it explicitly.
	KeepAwake bool `json:"keep_awake,omitempty"`
	// VolumeBackupHours: how often the volumes go off-site (0: the platform default, daily).
	VolumeBackupHours int `json:"volume_backup_hours,omitempty"`
	// Restricted is added to Env when the install is restricted (a class of learners on one
	// server): it turns off what lets a user run code on the host or read its environment.
	Restricted map[string]string `json:"restricted,omitempty"`
}

// KeepAwake reports whether one of the template's services must not sleep by default.
func (t *Template) KeepAwake() bool {
	for _, s := range t.Services {
		if s.KeepAwake {
			return true
		}
	}
	return false
}

// Build locates a service's Dockerfile in a repository.
type Build struct {
	RootDir    string `json:"root_dir"`
	Dockerfile string `json:"dockerfile,omitempty"` // relative to RootDir; empty = Dockerfile
	// Content replaces the repository's Dockerfile (an upstream that no longer builds).
	Content string `json:"-"`
}

// Database is a managed database created by a template.
type Database struct {
	Name    string `json:"name"`
	Engine  string `json:"engine"`
	Mode    string `json:"mode,omitempty"`
	Version string `json:"version,omitempty"`
	// BackupHours: how often it is dumped (0: the platform default, daily).
	BackupHours int `json:"backup_hours,omitempty"`
}

type Template struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Website     string `json:"website,omitempty"`
	// Repo is a public git repository the services with a Build section are built
	// from (apps without a published image).
	Repo      string     `json:"repo,omitempty"`
	Branch    string     `json:"branch,omitempty"`
	Services  []Service  `json:"services"`
	Databases []Database `json:"databases,omitempty"`
}

// Env values may reference ${db.<name>.host|port|user|password|name|url}, ${url}, ${host},
// ${random} (48 hex characters) and ${random:N} (N hex characters, for keys of a fixed length).
var All = []Template{
	{
		ID: "wordpress", Title: "WordPress", Category: "CMS",
		Description: "The world's most popular CMS, with a MariaDB database on the shared server.",
		Databases:   []Database{{Name: "wordpress", Engine: "mariadb"}},
		Services: []Service{{
			Name: "wordpress", Image: "docker.io/library/wordpress:6-apache", Type: "web", Port: 80, Size: "small",
			Env: map[string]string{
				"WORDPRESS_DB_HOST": "${db.wordpress.host}:${db.wordpress.port}", "WORDPRESS_DB_USER": "${db.wordpress.user}",
				"WORDPRESS_DB_PASSWORD": "${db.wordpress.password}", "WORDPRESS_DB_NAME": "${db.wordpress.name}",
			},
			Volumes: []model.Volume{{Name: "html", Path: "/var/www/html"}},
		}},
	},
	{
		ID: "n8n", Title: "n8n", Category: "Automation",
		Description: "Workflow automation (Zapier alternative) backed by PostgreSQL.",
		// Workflows and credentials live in the database: dumped every 6 hours, not daily.
		Databases: []Database{{Name: "n8n", Engine: "postgres", BackupHours: 6}},
		Services: []Service{{
			// Pinned: :latest would jump major versions (breaking changes) on a redeploy.
			// medium: n8n 2.x peaks at ~460 MB just starting (main process + its task runner),
			// so the 512 MB of small gets it OOM-killed on a first boot.
			Name: "n8n", Image: "docker.io/n8nio/n8n:2.47.3", Type: "web", Port: 5678, Size: "medium",
			Env: map[string]string{
				"DB_TYPE": "postgresdb", "DB_POSTGRESDB_HOST": "${db.n8n.host}", "DB_POSTGRESDB_PORT": "${db.n8n.port}",
				"DB_POSTGRESDB_DATABASE": "${db.n8n.name}", "DB_POSTGRESDB_USER": "${db.n8n.user}", "DB_POSTGRESDB_PASSWORD": "${db.n8n.password}",
				"N8N_HOST": "${host}", "WEBHOOK_URL": "${url}/", "N8N_PORT": "5678", "N8N_LISTEN_ADDRESS": "0.0.0.0", "N8N_ENCRYPTION_KEY": "${random}",
				// Behind the edge proxy: the client IP comes from X-Forwarded-For (login rate limits).
				"N8N_PROXY_HOPS": "1",
				// Keep the execution history bounded (14 days) so the database does not grow forever.
				"EXECUTIONS_DATA_PRUNE": "true", "EXECUTIONS_DATA_MAX_AGE": "336",
			},
			Volumes: []model.Volume{{Name: "data", Path: "/home/node/.n8n"}},
			// Schedule and polling triggers stop while n8n sleeps.
			KeepAwake:         true,
			VolumeBackupHours: 12,
			// A class on one server: no shell commands or host file triggers, no access to the
			// container's env (it holds the database password), files only in one folder.
			Restricted: map[string]string{
				"NODES_EXCLUDE":                `["n8n-nodes-base.executeCommand","n8n-nodes-base.localFileTrigger"]`,
				"N8N_BLOCK_ENV_ACCESS_IN_NODE": "true",
				"N8N_RESTRICT_FILE_ACCESS_TO":  "/home/node/.n8n-files",
			},
		}},
	},
	{
		ID: "uptime-kuma", Title: "Uptime Kuma", Category: "Monitoring",
		Description: "Self-hosted uptime monitoring with status pages.",
		Services: []Service{{
			Name: "uptime", Image: "docker.io/louislam/uptime-kuma:1", Type: "web", Port: 3001, Size: "micro",
			Volumes: []model.Volume{{Name: "data", Path: "/app/data"}},
		}},
	},
	{
		ID: "umami", Title: "Umami", Category: "Analytics",
		Description: "Privacy-friendly web analytics (Google Analytics alternative).",
		Databases:   []Database{{Name: "umami", Engine: "postgres"}},
		Services: []Service{{
			Name: "umami", Image: "ghcr.io/umami-software/umami:postgresql-latest", Type: "web", Port: 3000, Size: "small",
			Env:    map[string]string{"DATABASE_URL": "${db.umami.url}", "APP_SECRET": "${random}"},
			Health: "/api/heartbeat",
		}},
	},
	{
		ID: "gitea", Title: "Gitea", Category: "Development",
		Description: "Lightweight self-hosted Git service.",
		Databases:   []Database{{Name: "gitea", Engine: "postgres"}},
		Services: []Service{{
			Name: "gitea", Image: "docker.io/gitea/gitea:1.22", Type: "web", Port: 3000, Size: "small",
			Env: map[string]string{
				"GITEA__database__DB_TYPE": "postgres", "GITEA__database__HOST": "${db.gitea.host}:${db.gitea.port}",
				"GITEA__database__NAME": "${db.gitea.name}", "GITEA__database__USER": "${db.gitea.user}", "GITEA__database__PASSWD": "${db.gitea.password}",
				"GITEA__server__ROOT_URL": "${url}/", "GITEA__server__HTTP_PORT": "3000",
			},
			Volumes: []model.Volume{{Name: "data", Path: "/data"}},
		}},
	},
	{
		ID: "metabase", Title: "Metabase", Category: "Analytics",
		Description: "Business intelligence dashboards on top of your databases.",
		Databases:   []Database{{Name: "metabase", Engine: "postgres"}},
		Services: []Service{{
			Name: "metabase", Image: "docker.io/metabase/metabase:latest", Type: "web", Port: 3000, Size: "medium",
			Env: map[string]string{
				"MB_DB_TYPE": "postgres", "MB_DB_HOST": "${db.metabase.host}", "MB_DB_PORT": "${db.metabase.port}",
				"MB_DB_DBNAME": "${db.metabase.name}", "MB_DB_USER": "${db.metabase.user}", "MB_DB_PASS": "${db.metabase.password}",
			},
			Health: "/api/health",
		}},
	},
	{
		ID: "redis-insight", Title: "Redis + Redis Insight", Category: "Databases",
		Description: "A dedicated Redis with the Redis Insight web UI.",
		Databases:   []Database{{Name: "cache", Engine: "redis", Mode: "dedicated"}},
		Services: []Service{{
			Name: "redis-insight", Image: "docker.io/redis/redisinsight:latest", Type: "web", Port: 5540, Size: "small",
			Volumes: []model.Volume{{Name: "data", Path: "/data"}},
		}},
	},
}

func randomHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

func ByID(id string) (*Template, bool) {
	for i := range All {
		if All[i].ID == id {
			return &All[i], true
		}
	}
	return nil, false
}

// DBValues are the connection details a template can reference.
type DBValues struct {
	ID                                    string `json:"-"`
	Host, Port, User, Password, Name, URL string
}

var placeholder = regexp.MustCompile(`\$\{([a-z0-9_.:-]+)\}`)

// Expand substitutes placeholders in a template value.
func Expand(v string, dbs map[string]DBValues, publicURL, host string) string {
	return ExpandWith(v, dbs, nil, publicURL, host)
}

// ExpandWith also resolves ${svc.<name>.url|host} against the public URLs of
// the other services created together.
func ExpandWith(v string, dbs map[string]DBValues, services map[string]string, publicURL, host string) string {
	return placeholder.ReplaceAllStringFunc(v, func(m string) string {
		key := m[2 : len(m)-1]
		switch key {
		case "url":
			return publicURL
		case "host":
			return host
		case "random":
			return randomHex(48)
		}
		if n, ok := strings.CutPrefix(key, "random:"); ok {
			if l, err := strconv.Atoi(n); err == nil && l > 0 && l <= 128 {
				return randomHex(l)
			}
			return m
		}
		if strings.HasPrefix(key, "svc.") {
			parts := strings.Split(key, ".")
			if u, ok := services[parts[1]]; ok && len(parts) == 3 {
				switch parts[2] {
				case "url":
					return u
				case "host":
					return strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
				}
			}
			return m
		}
		if strings.HasPrefix(key, "db.") {
			parts := strings.Split(key, ".")
			if len(parts) == 3 {
				d, ok := dbs[parts[1]]
				if !ok {
					return m
				}
				switch parts[2] {
				case "host":
					return d.Host
				case "port":
					return d.Port
				case "user":
					return d.User
				case "password":
					return d.Password
				case "name":
					return d.Name
				case "url":
					return d.URL
				}
			}
		}
		return m
	})
}
