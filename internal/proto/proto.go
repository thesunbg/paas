// Package proto defines the messages exchanged over the NATS bus between the
// control plane, agents and edge proxies. Payloads are JSON.
package proto

import (
	"fmt"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/model"
)

// Subjects.
const (
	// SubjRegister and SubjHeartbeat are the legacy subjects: the sender is named in the payload.
	// Legacy: remove when no node has nkey IS NULL (plan 261017-cluster-trust).
	SubjRegister         = "paas.agent.register"
	SubjHeartbeat        = "paas.agent.heartbeat"
	SubjNodeRegisterAll  = "paas.node.*.register"
	SubjNodeHeartbeatAll = "paas.node.*.heartbeat"
	SubjCPKick           = "paas.cp.kick"          // control plane -> control planes: {nkey} to disconnect
	SubjRoutes           = "paas.proxy.routes"     // server -> proxies (full table)
	SubjRoutesGet        = "paas.proxy.routes.get" // proxy -> server (request current table)
	SubjProxyStats       = "paas.proxy.stats"      // proxy -> server
	SubjWake             = "paas.proxy.wake"       // proxy -> server (request/reply)
	SubjDNS              = "paas.dns"              // server -> agents (full table)
	SubjDNSGet           = "paas.dns.get"
	SubjMesh             = "paas.mesh" // server -> agents (WireGuard peers)
	SubjMeshGet          = "paas.mesh.get"
	SubjBinary           = "paas.agent.binary" // agent -> server: which paasd binary nodes should run
)

// DB HA timing, shared by the agent (self-fence) and the control plane (failover decision).
// The proof that the old primary is fenced before the standby is promoted lives next to the
// control plane's haPrimaryGone / haWitnessDown in internal/server/dbaas_failover.go.
const (
	// LeaseTTL: the agent treats its lease as expired this long after it SENT the last
	// heartbeat that was acked (not after the ack arrived).
	LeaseTTL = 20 * time.Second
	// FenceCheckEvery is the period of the agent's fence loop.
	FenceCheckEvery = 5 * time.Second
	// WalSenderTimeout is wal_sender_timeout and wal_receiver_timeout of HA servers: a
	// walsender notices a standby that stopped answering after this long.
	WalSenderTimeout = 10 * time.Second
)

// SubjNodeRegister and SubjNodeHeartbeat carry the node id in the subject: NATS permissions
// stop a node publishing under another id, so the server trusts the subject, not the payload.
func SubjNodeRegister(id string) string  { return "paas.node." + id + ".register" }
func SubjNodeHeartbeat(id string) string { return "paas.node." + id + ".heartbeat" }

// NodeIDFromSubject extracts <id> from paas.node.<id>.<verb>.
func NodeIDFromSubject(subject string) string {
	parts := strings.Split(subject, ".")
	if len(parts) != 4 || parts[0] != "paas" || parts[1] != "node" {
		return ""
	}
	return parts[2]
}

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
	CmdPITRPrepare = "pitrprepare"
	CmdStats       = "stats"
	CmdPush        = "push"
	CmdJob         = "job"
	CmdTransfer    = "transfer"
	CmdDetect      = "detect"
	CmdArchive     = "archive"  // tar.gz a directory under the data dir (volume backups, static sites)
	CmdExtract     = "extract"  // unpack such an archive into a directory
	CmdTermOpen    = "termopen" // interactive shell in a container (see TermOpenRequest)
	// Volume moves between nodes (see VolumeRecvRequest).
	CmdVolumeRecv   = "volrecv"
	CmdVolumeSend   = "volsend"
	CmdVolumeDigest = "voldigest"
	CmdVolumeCommit = "volcommit"
	// CmdRefreshImage pulls an image even when it is already on the node (agents without it do
	// not answer: the caller sees no responders and must not go on).
	CmdRefreshImage = "refreshimage"
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

// BinaryInfo names the paasd binary of the control plane, so standalone agents
// can update themselves to it.
type BinaryInfo struct {
	Arch   string `json:"arch"`
	SHA256 string `json:"sha256"`
	URL    string `json:"url"`
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
	// Traffic on the physical network interfaces: counters since boot, and the
	// rate since the previous heartbeat (bytes per second).
	NetRxBytes uint64  `json:"net_rx_bytes,omitempty"`
	NetTxBytes uint64  `json:"net_tx_bytes,omitempty"`
	NetRxBps   float64 `json:"net_rx_bps,omitempty"`
	NetTxBps   float64 `json:"net_tx_bps,omitempty"`
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
	Fenced      bool   `json:"fenced,omitempty"` // an HA postgres the agent stopped because its lease expired
	// ImageDigest is the digest of the platform manifest (linux/<arch>) of the image the container
	// runs, not of the multi-arch index. Empty from agents that do not report it.
	ImageDigest string `json:"image_digest,omitempty"`
	// Bytes received and sent by the container since it started (its network namespace).
	NetRxBytes uint64 `json:"net_rx_bytes,omitempty"`
	NetTxBytes uint64 `json:"net_tx_bytes,omitempty"`
}

type Heartbeat struct {
	NodeID     string            `json:"node_id"`
	Token      string            `json:"token"`
	Usage      NodeUsage         `json:"usage"`
	Containers []ContainerStatus `json:"containers"`
	// Jobs are the job run IDs whose CmdJob request this agent is handling right now. nil means
	// an agent that predates the field (do nothing); the agent always sends a non-nil slice.
	Jobs []string `json:"jobs"`
}

// HeartbeatAck answers a heartbeat once the control plane has recorded it. The agent's
// lease on its HA postgres containers is renewed by it.
type HeartbeatAck struct {
	// Unfence lists fenced containers that may start again: their instance still exists
	// and no failover is pending.
	Unfence []string `json:"unfence,omitempty"`
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
	HAPostgres        bool              `json:"ha_postgres,omitempty"` // postgres of an HA database: the agent self-fences it without a lease
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

type RefreshImageRequest struct {
	Image             string `json:"image"`
	PlainHTTPRegistry string `json:"plain_http_registry,omitempty"`
}

type RefreshImageReply struct {
	Digest string `json:"digest,omitempty"` // platform manifest digest of what the tag points at now
	Error  string `json:"error,omitempty"`
}

type GenericReply struct {
	Error string `json:"error,omitempty"`
}

type BuildRequest struct {
	DeploymentID string            `json:"deployment_id"`
	ServiceID    string            `json:"service_id"`
	ProjectID    string            `json:"project_id"`
	ServiceType  string            `json:"service_type"`
	Repo         string            `json:"repo"`
	Branch       string            `json:"branch"`
	CommitSHA    string            `json:"commit_sha,omitempty"`
	GitToken     string            `json:"git_token,omitempty"`
	GitUser      string            `json:"git_user,omitempty"` // HTTP basic user sent with GitToken (default x-access-token)
	RootDir      string            `json:"root_dir,omitempty"`
	Dockerfile   string            `json:"dockerfile,omitempty"`
	Build        model.BuildConfig `json:"build"`
	ImageName    string            `json:"image_name"`
	BuildEnv     map[string]string `json:"build_env,omitempty"`
	LogSubject   string            `json:"log_subject"`
}

type BuildReply struct {
	Image         string  `json:"image,omitempty"`
	StaticPath    string  `json:"static_path,omitempty"`
	CommitSHA     string  `json:"commit_sha,omitempty"`
	CommitMessage string  `json:"commit_message,omitempty"`
	Detected      string  `json:"detected,omitempty"`
	Port          int     `json:"port,omitempty"`
	Prisma        *Prisma `json:"prisma,omitempty"`
	Error         string  `json:"error,omitempty"`
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
	// PartURLs (PUT only) upload the file as a multipart upload, PartSize bytes per
	// part (the last one is shorter); the reply carries each part's ETag.
	PartURLs []string `json:"part_urls,omitempty"`
	PartSize int64    `json:"part_size,omitempty"`
}

type TransferReply struct {
	Error string   `json:"error,omitempty"`
	ETags []string `json:"etags,omitempty"`
}

// DetectRequest asks a build node to inspect a repository without building it.
type DetectRequest struct {
	Repo       string `json:"repo"`
	Branch     string `json:"branch"`
	RootDir    string `json:"root_dir,omitempty"`
	GitToken   string `json:"git_token,omitempty"`
	GitUser    string `json:"git_user,omitempty"`
	Dockerfile string `json:"dockerfile,omitempty"`
	// ComposePath reads this compose file instead of searching for one.
	ComposePath string `json:"compose_path,omitempty"`
}

// EnvHint is a variable of an example env file (see templates.ExampleEnv).
type EnvHint struct {
	Key   string `json:"key"`
	Kind  string `json:"kind"`
	Value string `json:"value,omitempty"`
	Hint  string `json:"hint,omitempty"`
}

// ComposeFile is a docker-compose file found in a repository.
type ComposeFile struct {
	Path     string   `json:"path"` // relative to the repository root
	Content  string   `json:"content"`
	Services []string `json:"services"`
	// Files are the env files it refers to (or their committed examples), by repository path.
	Files map[string]string `json:"files,omitempty"`
}

type DetectReply struct {
	Language      string  `json:"language"`
	Framework     string  `json:"framework,omitempty"`
	Static        bool    `json:"static"`
	Port          int     `json:"port,omitempty"`
	SuggestedType string  `json:"suggested_type"` // web | static | worker
	Dockerfile    bool    `json:"dockerfile"`
	CommitSHA     string  `json:"commit_sha,omitempty"`
	CommitMessage string  `json:"commit_message,omitempty"`
	Summary       string  `json:"summary"`
	Prisma        *Prisma `json:"prisma,omitempty"`
	// Compose is set when the repository has a docker-compose file describing several services.
	Compose *ComposeFile `json:"compose,omitempty"`
	// Env lists the variables of the example env file next to the app (.env.example).
	Env     []EnvHint `json:"env,omitempty"`
	EnvFile string    `json:"env_file,omitempty"`
	Error   string    `json:"error,omitempty"`
}

// ArchiveRequest packs Dir into File (both relative to the agent data dir).
// With PauseContainer set, that container is frozen while the archive is made,
// so the copy of its volumes is consistent.
type ArchiveRequest struct {
	Dir            string `json:"dir"`
	File           string `json:"file"`
	PauseContainer string `json:"pause_container,omitempty"`
}

type ArchiveReply struct {
	Size  int64  `json:"size"`
	Error string `json:"error,omitempty"`
}

// ExtractRequest unpacks File into Dir, which must be empty or missing unless Replace is set.
type ExtractRequest struct {
	File    string `json:"file"`
	Dir     string `json:"dir"`
	Replace bool   `json:"replace,omitempty"`
}

// VolumeRecvRequest opens a one-shot receiver for the volumes of a service on the node's
// WireGuard mesh address (never anywhere else). It accepts one connection, from From (the
// sender's mesh address), that starts with Token, and unpacks the tar stream it carries into
// Dir (relative to the data dir). Cancel ends a receive of the same ID and removes Dir.
type VolumeRecvRequest struct {
	ID     string `json:"id"`
	Token  string `json:"token,omitempty"`
	From   string `json:"from,omitempty"`
	Dir    string `json:"dir"`
	Cancel bool   `json:"cancel,omitempty"`
}

type VolumeRecvReply struct {
	Port  int    `json:"port,omitempty"`
	Error string `json:"error,omitempty"`
}

// VolumeSendRequest streams Dir to the receiver at Addr (a mesh address) and waits for its answer.
type VolumeSendRequest struct {
	ID    string `json:"id"`
	Addr  string `json:"addr"`
	Token string `json:"token"`
	Dir   string `json:"dir"`
}

// VolumeSum describes a directory tree: bytes of file content, entries, and a SHA-256 over every
// entry's name, type, permissions, owner, size, link target and content.
type VolumeSum struct {
	Bytes  int64  `json:"bytes"`
	Files  int64  `json:"files"`
	Digest string `json:"digest"`
}

type VolumeSendReply struct {
	Sent     VolumeSum `json:"sent"`
	Received VolumeSum `json:"received"` // what the receiver says it unpacked
	Error    string    `json:"error,omitempty"`
}

type VolumeDigestRequest struct {
	Dir string `json:"dir"`
}

type VolumeDigestReply struct {
	Sum   VolumeSum `json:"sum"`
	Error string    `json:"error,omitempty"`
}

// VolumeCommitRequest renames a received tree (From) to the volumes directory of a service (To),
// replacing what is there.
type VolumeCommitRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// TermOpenRequest starts an interactive process with a terminal in a running
// container. Once open, the session's bytes travel on plain NATS subjects:
// keystrokes on TermInSubject (agent side), output on TermOutSubject (node
// side), and TermCtl messages on TermCtlSubject. The agent ends a session that
// receives nothing (not even a ping) for TermIdleTimeout.
type TermOpenRequest struct {
	SessionID   string   `json:"session_id"`
	ContainerID string   `json:"container_id"`
	Cmd         []string `json:"cmd,omitempty"` // default: bash if the image has it, else sh
	Cols        int      `json:"cols"`
	Rows        int      `json:"rows"`
}

// TermCtl is a control message: a resize, a keep-alive ping or a close (to the
// agent), or the exit of the process (from the agent, on TermOutSubject's
// sibling TermExitSubject).
type TermCtl struct {
	Cols  int    `json:"cols,omitempty"`
	Rows  int    `json:"rows,omitempty"`
	Ping  bool   `json:"ping,omitempty"`
	Close bool   `json:"close,omitempty"`
	Exit  *int   `json:"exit,omitempty"`
	Error string `json:"error,omitempty"`
}

const TermIdleTimeout = 90 * time.Second

func TermInSubject(nodeID, session string) string { return AgentSubject(nodeID, "term."+session+".in") }
func TermCtlSubject(nodeID, session string) string {
	return AgentSubject(nodeID, "term."+session+".ctl")
}
func TermOutSubject(nodeID, session string) string {
	return fmt.Sprintf("paas.node.%s.term.%s.out", nodeID, session)
}
func TermExitSubject(nodeID, session string) string {
	return fmt.Sprintf("paas.node.%s.term.%s.exit", nodeID, session)
}

// PITRPrepareRequest lays out a PostgreSQL data directory for point-in-time
// recovery: it extracts a base backup and copies the archived WAL next to it.
// Paths are relative to the agent data directory.
type PITRPrepareRequest struct {
	BaseBackup string `json:"base_backup"` // gzipped tar from pg_basebackup -Ft -z
	WALSource  string `json:"wal_source"`  // archive directory of the source server
	DataDir    string `json:"data_dir"`    // PGDATA of the new server
	WALDest    string `json:"wal_dest"`    // archive directory of the new server
	TargetTime string `json:"target_time"` // RFC 3339; empty = end of the archive
}

// Prisma describes the database a Prisma app expects, so the platform can provide it.
type Prisma struct {
	Provider   string `json:"provider"`            // sqlite | postgresql | mysql | ...
	URLEnv     string `json:"url_env,omitempty"`   // env var named in url = env("...")
	Migrations bool   `json:"migrations"`          // prisma/migrations exists
	SeedFile   string `json:"seed_file,omitempty"` // SQLite file committed in the repository (initial data)
}
