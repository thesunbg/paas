// Package model holds the types shared between the control plane, agents and proxies.
package model

import "time"

// Service types.
const (
	TypeWeb      = "web"
	TypeWorker   = "worker"
	TypeStatic   = "static"
	TypeDatabase = "database"
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
}

// Route is one hostname served by the edge proxy.
type Route struct {
	Host       string   `json:"host"`
	ServiceID  string   `json:"service_id"`
	Kind       string   `json:"kind"` // upstream | static | sleeping | dashboard | unavailable
	Upstreams  []string `json:"upstreams,omitempty"`
	StaticRoot string   `json:"static_root,omitempty"`
	SPA        bool     `json:"spa,omitempty"`
}

type RouteTable struct {
	Version   int64     `json:"version"`
	Routes    []Route   `json:"routes"`
	Generated time.Time `json:"generated"`
}

// DNSTable maps internal names (svc.project.internal) to container IPs.
type DNSTable struct {
	Records map[string][]string `json:"records"`
}
