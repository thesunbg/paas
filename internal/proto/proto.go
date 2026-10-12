// Package proto defines the messages exchanged over the NATS bus between the
// control plane, agents and edge proxies. Payloads are JSON.
package proto

import (
	"fmt"

	"github.com/thesunbg/paas/internal/model"
)

// Subjects.
const (
	SubjRegister   = "paas.agent.register"
	SubjHeartbeat  = "paas.agent.heartbeat"
	SubjRoutes     = "paas.proxy.routes"     // server -> proxies (full table)
	SubjRoutesGet  = "paas.proxy.routes.get" // proxy -> server (request current table)
	SubjProxyStats = "paas.proxy.stats"      // proxy -> server
	SubjWake       = "paas.proxy.wake"       // proxy -> server (request/reply)
	SubjDNS        = "paas.dns"              // server -> agents (full table)
	SubjDNSGet     = "paas.dns.get"
	SubjMesh       = "paas.mesh" // server -> agents (WireGuard peers)
	SubjMeshGet    = "paas.mesh.get"
)

// AgentSubject returns the subject an agent listens on for a command.
func AgentSubject(nodeID, cmd string) string {
	return fmt.Sprintf("paas.agent.%s.%s", nodeID, cmd)
}

// DeployLogSubject is where build/deploy log lines for a deployment are published.
func DeployLogSubject(deploymentID string) string {
	return "paas.logs.deploy." + deploymentID
}

// Agent commands.
const (
	CmdRun         = "run"
	CmdStop        = "stop"
	CmdBuild       = "build"
	CmdExec        = "exec"
	CmdLogs        = "logs"
	CmdImageGC     = "imagegc"
	CmdRemoveFiles = "rmfiles"
	CmdStats       = "stats"
	CmdPush        = "push"
	CmdJob         = "job"
	CmdTransfer    = "transfer"
)

type RegisterRequest struct {
	Token        string            `json:"token"`
	Name         string            `json:"name"`
	Address      string            `json:"address"`
	Roles        []string          `json:"roles"`
	CPUCores     int               `json:"cpu_cores"`
	MemoryMB     int               `json:"memory_mb"`
	Capabilities map[string]string `json:"capabilities"`
	Version      string            `json:"version"`
	WGPublicKey  string            `json:"wg_public_key"`
	WGPort       int               `json:"wg_port"`
}

type RegisterReply struct {
	NodeID         string `json:"node_id"`
	NodeIndex      int    `json:"node_index"`
	ClusterBase    string `json:"cluster_base"` // first two octets, e.g. "10.100"
	SharedNetIndex int    `json:"shared_net_index"`
	AllowLocalGit  bool   `json:"allow_local_git"`
	Error          string `json:"error,omitempty"`
}

type NodeUsage struct {
	MemoryUsedMB      int     `json:"memory_used_mb"`
	MemoryAvailableMB int     `json:"memory_available_mb"`
	CPUPercent        float64 `json:"cpu_percent"`
	Load1             float64 `json:"load1"`
	PSIMemorySome10   float64 `json:"psi_memory_some10"`
	DiskUsedGB        float64 `json:"disk_used_gb"`
	DiskTotalGB       float64 `json:"disk_total_gb"`
}

type ContainerStatus struct {
	ContainerID string `json:"container_id"`
	InstanceID  string `json:"instance_id"`
	ServiceID   string `json:"service_id"`
	Running     bool   `json:"running"`
	ExitCode    int    `json:"exit_code"`
	Restarts    int    `json:"restarts"`
	IP          string `json:"ip"`
	MemoryMB    int    `json:"memory_mb"`
	CPUMillis   int    `json:"cpu_millis"`
	Evicted     bool   `json:"evicted,omitempty"`
}

type Heartbeat struct {
	NodeID     string            `json:"node_id"`
	Token      string            `json:"token"`
	Usage      NodeUsage         `json:"usage"`
	Containers []ContainerStatus `json:"containers"`
}

type RunRequest struct {
	InstanceID        string            `json:"instance_id"`
	ServiceID         string            `json:"service_id"`
	ContainerID       string            `json:"container_id"`
	NetIndex          int               `json:"net_index"`
	Hostname          string            `json:"hostname"`
	Image             string            `json:"image"`
	PullImage         bool              `json:"pull_image"`
	Env               map[string]string `json:"env"`
	Command           []string          `json:"command,omitempty"`
	Port              int               `json:"port"`
	Resources         model.Resources   `json:"resources"`
	QoS               string            `json:"qos"`
	RuntimeClass      string            `json:"runtime_class"`
	Volumes           []model.Volume    `json:"volumes,omitempty"`
	HealthCheck       model.HealthCheck `json:"health_check"`
	LogSubject        string            `json:"log_subject,omitempty"`
	PlainHTTPRegistry string            `json:"plain_http_registry,omitempty"` // internal registry host:port
	Once              bool              `json:"once,omitempty"`                // run to completion, never restart (jobs)
	DNSSearch         string            `json:"dns_search,omitempty"`          // e.g. shop.internal: compose-style short names resolve
	TimeoutSec        int               `json:"timeout_sec,omitempty"`
}

// JobReply is returned when a one-off container finishes.
type JobReply struct {
	ContainerID string `json:"container_id"`
	ExitCode    int    `json:"exit_code"`
	Output      string `json:"output"`
	Error       string `json:"error,omitempty"`
}

type RunReply struct {
	ContainerID string `json:"container_id"`
	IP          string `json:"ip"`
	Error       string `json:"error,omitempty"`
}

type StopRequest struct {
	ContainerID string `json:"container_id"`
	GraceSec    int    `json:"grace_sec"`
}

type GenericReply struct {
	Error string `json:"error,omitempty"`
}

type BuildRequest struct {
	DeploymentID string            `json:"deployment_id"`
	ServiceID    string            `json:"service_id"`
	ServiceType  string            `json:"service_type"`
	Repo         string            `json:"repo"`
	Branch       string            `json:"branch"`
	CommitSHA    string            `json:"commit_sha,omitempty"`
	GitToken     string            `json:"git_token,omitempty"`
	RootDir      string            `json:"root_dir,omitempty"`
	Dockerfile   string            `json:"dockerfile,omitempty"`
	Build        model.BuildConfig `json:"build"`
	ImageName    string            `json:"image_name"`
	BuildEnv     map[string]string `json:"build_env,omitempty"`
	LogSubject   string            `json:"log_subject"`
}

type BuildReply struct {
	Image         string `json:"image,omitempty"`
	StaticPath    string `json:"static_path,omitempty"`
	CommitSHA     string `json:"commit_sha,omitempty"`
	CommitMessage string `json:"commit_message,omitempty"`
	Detected      string `json:"detected,omitempty"`
	Port          int    `json:"port,omitempty"`
	Error         string `json:"error,omitempty"`
}

type ExecRequest struct {
	ContainerID string   `json:"container_id"`
	Cmd         []string `json:"cmd"`
	Env         []string `json:"env,omitempty"`
	Stdin       string   `json:"stdin,omitempty"`
	StdinFile   string   `json:"stdin_file,omitempty"`  // host path read as stdin
	StdoutFile  string   `json:"stdout_file,omitempty"` // host path stdout is written to
	TimeoutSec  int      `json:"timeout_sec,omitempty"`
}

type ExecReply struct {
	ExitCode    int    `json:"exit_code"`
	Stdout      string `json:"stdout"`
	Stderr      string `json:"stderr"`
	StdoutBytes int64  `json:"stdout_bytes"`
	Error       string `json:"error,omitempty"`
}

type LogsRequest struct {
	ContainerID string `json:"container_id"`
	Tail        int    `json:"tail"`
	Follow      bool   `json:"follow"`
	Subject     string `json:"subject"` // where to publish lines when following
	StopSubject string `json:"stop_subject"`
}

type LogsReply struct {
	Lines []string `json:"lines"`
	Error string   `json:"error,omitempty"`
}

type ImageGCRequest struct {
	Keep   []string `json:"keep"` // images still referenced
	Prefix string   `json:"prefix"`
}

type RemoveFilesRequest struct {
	Paths []string `json:"paths"`
}

type ProxyStats struct {
	ProxyID string                 `json:"proxy_id"`
	Hits    map[string]ServiceHits `json:"hits"` // service id -> hits
}

type ServiceHits struct {
	Requests    int64 `json:"requests"`
	LastRequest int64 `json:"last_request"` // unix seconds
	Errors5xx   int64 `json:"errors_5xx"`
}

type WakeRequest struct {
	ServiceID string `json:"service_id"`
}

type WakeReply struct {
	Upstreams []string `json:"upstreams"`
	Error     string   `json:"error,omitempty"`
}

// MeshPeer is one node in the WireGuard mesh.
type MeshPeer struct {
	NodeID    string `json:"node_id"`
	NodeIndex int    `json:"node_index"`
	PublicKey string `json:"public_key"`
	Endpoint  string `json:"endpoint"` // host:port
}

type MeshTable struct {
	Peers []MeshPeer `json:"peers"`
}

// PushRequest asks a build node to copy a local image to the internal registry.
type PushRequest struct {
	Image    string `json:"image"`
	Target   string `json:"target"` // registry reference
	Insecure bool   `json:"insecure"`
}

// TransferRequest uploads (PUT) or downloads (GET) a file under the agent data dir using a presigned URL.
type TransferRequest struct {
	Method string `json:"method"`
	URL    string `json:"url"`
	Path   string `json:"path"`
}
