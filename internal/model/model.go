// Package model holds the types shared between the control plane, agents and proxies.
package model

import "time"

// Service types.
const (
	TypeWeb      = "web"
	TypeWorker   = "worker"
	TypeStatic   = "static"
	TypeDatabase = "database"
	TypeCron     = "cron"
)

// QoS classes, ordered from most to least protected.
const (
	QoSGuaranteed = "guaranteed"
	QoSBurstable  = "burstable"
	QoSBestEffort = "best-effort"
)

// Runtime classes.
const (
	RuntimeStandard  = "standard"  // crun (or runc)
	RuntimeSandboxed = "sandboxed" // gVisor runsc
)

// Service statuses.
const (
	StatusCreated   = "created"
	StatusDeploying = "deploying"
	StatusRunning   = "running"
	StatusSleeping  = "sleeping"
	StatusFailed    = "failed"
	StatusStopped   = "stopped"
)

// Deployment statuses.
const (
	DeployQueued     = "queued"
	DeployBuilding   = "building"
	DeployDeploying  = "deploying"
	DeployActive     = "active"
	DeploySuperseded = "superseded"
	DeployFailed     = "failed"
	DeployCancelled  = "cancelled"
)

// Instance statuses.
const (
	InstStarting = "starting"
	InstRunning  = "running"
	InstStopping = "stopping"
	InstStopped  = "stopped"
	InstFailed   = "failed"
	InstLost     = "lost"
)

type ServiceSource struct {
	Kind       string `json:"kind"` // git | image
	Repo       string `json:"repo,omitempty"`
	Branch     string `json:"branch,omitempty"`
	RootDir    string `json:"root_dir,omitempty"`
	Dockerfile string `json:"dockerfile,omitempty"`
	Image      string `json:"image,omitempty"`
	AutoDeploy bool   `json:"auto_deploy"`
	// InstallationID selects the GitHub App installation used for clones, statuses and comments.
	InstallationID int64 `json:"installation_id,omitempty"`
}

type BuildConfig struct {
	Builder    string `json:"builder,omitempty"` // auto | dockerfile
	InstallCmd string `json:"install_cmd,omitempty"`
	BuildCmd   string `json:"build_cmd,omitempty"`
	StartCmd   string `json:"start_cmd,omitempty"`
	OutputDir  string `json:"output_dir,omitempty"`
}

type Resources struct {
	CPURequestMillis int `json:"cpu_request_millis"`
	CPULimitMillis   int `json:"cpu_limit_millis"`
	MemoryRequestMB  int `json:"memory_request_mb"`
	MemoryLimitMB    int `json:"memory_limit_mb"`
}

type HealthCheck struct {
	Type       string `json:"type,omitempty"` // http | tcp | none
	Path       string `json:"path,omitempty"`
	TimeoutSec int    `json:"timeout_sec,omitempty"`
}

type Volume struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type SleepPolicy struct {
	Enabled     bool `json:"enabled"`
	IdleMinutes int  `json:"idle_minutes,omitempty"`
}

type ServiceSpec struct {
	Port         int               `json:"port,omitempty"`
	Replicas     int               `json:"replicas"`
	Resources    Resources         `json:"resources"`
	QoS          string            `json:"qos,omitempty"`
	RuntimeClass string            `json:"runtime_class,omitempty"`
	HealthCheck  HealthCheck       `json:"health_check"`
	Sleep        SleepPolicy       `json:"sleep"`
	Volumes      []Volume          `json:"volumes,omitempty"`
	Command      []string          `json:"command,omitempty"`
	Image        string            `json:"image,omitempty"` // fixed image (databases)
	SPA          bool              `json:"spa,omitempty"`
	Env          map[string]string `json:"env,omitempty"`      // non-secret env baked by the system (databases)
	Language     string            `json:"language,omitempty"` // detected language, used for runtime tuning
	DNSAliases   []string          `json:"dns_aliases,omitempty"`
	HAPostgres   bool              `json:"ha_postgres,omitempty"` // server of an HA database: the agent fences it when it loses the control plane
	// DependsOn names services of the project (by slug) this one talks to. They
	// start first, and it restarts after they are redeployed: programs such as
	// nginx resolve their upstreams once at startup and would keep a stale IP.
	DependsOn []string `json:"depends_on,omitempty"`
	// VolumeBackupHours is how often the volumes are backed up off-site
	// (0: every 24 h when off-site storage is configured, negative: never).
	VolumeBackupHours int `json:"volume_backup_hours,omitempty"`
	// Role marks services the platform runs for a project ("studio": the DB
	// studio). They are hidden from service lists and limits.
	Role          string    `json:"role,omitempty"`
	Previews      bool      `json:"previews,omitempty"` // deploy a preview for every pull request
	Autoscale     Autoscale `json:"autoscale"`
	Schedule      string    `json:"schedule,omitempty"` // cron jobs: 5-field cron expression
	JobTimeoutSec int       `json:"job_timeout_sec,omitempty"`
}

// Autoscale adjusts replicas between Min and Max from CPU and request rate.
type Autoscale struct {
	Enabled          bool `json:"enabled"`
	Min              int  `json:"min,omitempty"`
	Max              int  `json:"max,omitempty"`
	TargetCPUPercent int  `json:"target_cpu_percent,omitempty"` // of the CPU request (or limit when no request)
	TargetRPS        int  `json:"target_rps,omitempty"`         // requests per second per replica
}

// Route is one hostname served by the edge proxy.
type Route struct {
	Host       string   `json:"host"`
	ServiceID  string   `json:"service_id"`
	Kind       string   `json:"kind"` // upstream | static | sleeping | dashboard | unavailable
	Upstreams  []string `json:"upstreams,omitempty"`
	StaticRoot string   `json:"static_root,omitempty"`
	// StaticOrigin serves StaticRoot over the mesh for proxies on other nodes.
	StaticOrigin string `json:"static_origin,omitempty"`
	SPA          bool   `json:"spa,omitempty"`
	// Gate admits only browsers holding a ticket from the control plane (the DB studio).
	Gate *RouteGate `json:"gate,omitempty"`
}

// RouteGate protects a route that has no login of its own. The control plane
// hands the user a short-lived ticket (signed with Key) in the URL; the edge
// trades it for a session cookie and adds Header: Value to forwarded requests,
// which the upstream requires, so nothing else on its network can use it.
type RouteGate struct {
	Key    string `json:"key"`
	Header string `json:"header"`
	Value  string `json:"value"`
}

type RouteTable struct {
	Version   int64     `json:"version"`
	Routes    []Route   `json:"routes"`
	Generated time.Time `json:"generated"`
	// TCP exposes managed databases on DBHost:<port> through every edge proxy.
	TCP    []TCPRoute `json:"tcp,omitempty"`
	DBHost string     `json:"db_host,omitempty"`
}

// TCPRoute forwards one public port to a database server.
type TCPRoute struct {
	Port       int      `json:"port"`
	DatabaseID string   `json:"database_id"`
	Upstreams  []string `json:"upstreams"`
	// Mode: "postgres" answers SSLRequest and terminates TLS, "tls" terminates
	// TLS from the first byte (redis, mongo), "plain" passes bytes through (mysql).
	Mode  string   `json:"mode"`
	Allow []string `json:"allow"` // client CIDRs; empty allows nobody
}

// DNSTable maps internal names (svc.project.internal) to container IPs.
type DNSTable struct {
	Records map[string][]string `json:"records"`
}
