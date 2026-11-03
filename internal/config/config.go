// Package config loads process configuration from environment variables.
package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type Server struct {
	DatabaseURL     string   // PAAS_DATABASE_URL
	HTTPAddr        string   // PAAS_HTTP_ADDR, API + dashboard
	NATSAddr        string   // PAAS_NATS_ADDR, listener for agents and proxies
	NATSAdvertise   string   // PAAS_NATS_ADVERTISE, host:port nodes dial for it (default: public URL host + NATSAddr port)
	NATSLegacy      bool     // PAAS_NATS_LEGACY_TOKEN=1: accept the shared cluster token as a full-permission NATS credential (cutover only)
	DataDir         string   // PAAS_DATA_DIR
	MasterKey       string   // PAAS_MASTER_KEY (hex, optional; otherwise <data>/master.key)
	BaseDomain      string   // PAAS_BASE_DOMAIN, generated app domains are <svc>-<project>.<base>
	DashboardDomain string   // PAAS_DASHBOARD_DOMAIN, served by the proxy to the API
	PublicURL       string   // PAAS_PUBLIC_URL, used in webhook URLs and links
	ClusterToken    string   // PAAS_CLUSTER_TOKEN (optional; generated and stored otherwise)
	AllowLocalGit   bool     // PAAS_ALLOW_LOCAL_GIT, allow file:// repos (dev / testing)
	ImageRetention  int      // PAAS_IMAGE_RETENTION, deployments kept per service for rollback
	ClusterBase     string   // PAAS_CLUSTER_BASE, first two octets of the container address space (default 10.100)
	S3              S3       // off-site backups
	DBHost          string   // PAAS_DB_HOST, public host for external database access (default db.<base>)
	StudioRepo      string   // PAAS_STUDIO_REPO, git repository of the DB studio (dbweb)
	StudioBranch    string   // PAAS_STUDIO_BRANCH
	DBPortMin       int      // PAAS_DB_PORT_MIN, first public port for databases (default 20000)
	DBPortMax       int      // PAAS_DB_PORT_MAX (default 20999)
	MetricsToken    string   // PAAS_METRICS_TOKEN, bearer token for GET /metrics (Prometheus); admins can always use their own token
	PublicIPs       []string // PAAS_PUBLIC_IPS, comma separated public IPs of the edge (setup checklist DNS check; default: node addresses)

	// High availability: several control planes share PostgreSQL and form a NATS cluster.
	ServerName    string   // PAAS_SERVER_NAME (unique per control plane)
	ClusterAddr   string   // PAAS_NATS_CLUSTER_ADDR, e.g. 0.0.0.0:6222
	ClusterRoutes []string // PAAS_NATS_ROUTES, comma separated nats://host:6222

	// Updates of paas itself (Settings → System → Updates).
	UpdateURL             string // PAAS_UPDATE_URL, "latest release" endpoint (GitHub API JSON)
	UpdateCheck           bool   // PAAS_UPDATE_CHECK=false disables the periodic check for new releases
	UpdateAllowUnverified bool   // PAAS_UPDATE_ALLOW_UNVERIFIED=true installs binaries without a published checksum (unsafe)
}

// S3 configures off-site backup storage (any S3-compatible service).
type S3 struct {
	Endpoint    string // PAAS_S3_ENDPOINT
	Region      string // PAAS_S3_REGION
	Bucket      string // PAAS_S3_BUCKET
	AccessKey   string // PAAS_S3_ACCESS_KEY
	SecretKey   string // PAAS_S3_SECRET_KEY
	Prefix      string // PAAS_S3_PREFIX
	VirtualHost bool   // PAAS_S3_VIRTUAL_HOST
}

type Agent struct {
	NATSURL        string   // PAAS_NATS_URL
	ClusterToken   string   // PAAS_CLUSTER_TOKEN, legacy nodes only (remove when no node has nkey IS NULL)
	JoinURL        string   // PAAS_JOIN_URL, https control plane URL used to enroll on first start
	JoinToken      string   // PAAS_JOIN_TOKEN, one-time token for the enroll (removed from the env file afterwards)
	IdentityDir    string   // PAAS_IDENTITY_DIR, holds node.json, seed.nk and ca.pem
	NodeID         string   // set in-process only (embedded agent): the node row id, skips enroll
	NodeName       string   // PAAS_NODE_NAME
	NodeAddress    string   // PAAS_NODE_ADDRESS
	Roles          []string // PAAS_ROLES (comma separated)
	DataDir        string   // PAAS_DATA_DIR
	ContainerdSock string   // PAAS_CONTAINERD_SOCK
	Namespace      string   // PAAS_CONTAINERD_NAMESPACE
	OCIRuntime     string   // PAAS_OCI_RUNTIME: crun | runc
	BuildkitAddr   string   // PAAS_BUILDKIT_ADDR
	CNIBinDir      string   // PAAS_CNI_BIN_DIR
	SubnetPrefix   string   // PAAS_SUBNET_PREFIX, first octet pair base (default 10.200)
	EvictPSI       float64  // PAAS_EVICT_PSI, memory PSI some avg10 threshold for eviction
	EvictMinFreeMB int      // PAAS_EVICT_MIN_FREE_MB
	WGPort         int      // PAAS_WG_PORT, UDP port of the WireGuard mesh
	Mesh           bool     // PAAS_MESH, connect to other nodes over WireGuard
}

type Proxy struct {
	NATSURL           string // PAAS_NATS_URL
	ClusterToken      string // PAAS_CLUSTER_TOKEN, legacy nodes only (remove when no node has nkey IS NULL)
	IdentityDir       string // PAAS_IDENTITY_DIR, written by the agent on the same host
	HTTPAddr          string // PAAS_PROXY_HTTP_ADDR
	HTTPSAddr         string // PAAS_PROXY_HTTPS_ADDR
	ACMEEmail         string // PAAS_ACME_EMAIL, enables automatic HTTPS when set
	TLS               bool   // PAAS_TLS, enables automatic HTTPS without an email
	ACMECA            string // PAAS_ACME_CA, optional custom directory (e.g. staging)
	DataDir           string // PAAS_DATA_DIR
	DashboardUpstream string // PAAS_DASHBOARD_UPSTREAM, e.g. 127.0.0.1:8080
	RateLimit         int    // PAAS_PROXY_RATE_LIMIT, requests per second per client IP (0 = off)
	RateBurst         int    // PAAS_PROXY_RATE_BURST
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(env(key, ""))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(env(key, "")); err == nil {
		return n
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if f, err := strconv.ParseFloat(env(key, ""), 64); err == nil {
		return f
	}
	return def
}

func LoadServer() Server {
	data := env("PAAS_DATA_DIR", "/var/lib/paas")
	base := env("PAAS_BASE_DOMAIN", "localhost")
	return Server{
		DatabaseURL:     env("PAAS_DATABASE_URL", "postgres://paas:paas@127.0.0.1:5432/paas?sslmode=disable"),
		HTTPAddr:        env("PAAS_HTTP_ADDR", ":8080"),
		NATSAddr:        env("PAAS_NATS_ADDR", "0.0.0.0:4222"),
		NATSAdvertise:   env("PAAS_NATS_ADVERTISE", ""),
		NATSLegacy:      envBool("PAAS_NATS_LEGACY_TOKEN", false),
		DataDir:         data,
		MasterKey:       env("PAAS_MASTER_KEY", ""),
		BaseDomain:      base,
		DashboardDomain: env("PAAS_DASHBOARD_DOMAIN", "paas."+base),
		PublicURL:       strings.TrimRight(env("PAAS_PUBLIC_URL", "http://paas."+base), "/"),
		ClusterToken:    env("PAAS_CLUSTER_TOKEN", ""),
		MetricsToken:    env("PAAS_METRICS_TOKEN", ""),
		PublicIPs:       splitList(env("PAAS_PUBLIC_IPS", "")),
		AllowLocalGit:   envBool("PAAS_ALLOW_LOCAL_GIT", false),
		ImageRetention:  envInt("PAAS_IMAGE_RETENTION", 3),
		DBHost:          env("PAAS_DB_HOST", "db."+base),
		StudioRepo:      env("PAAS_STUDIO_REPO", "https://github.com/thesunbg/dbweb.git"),
		StudioBranch:    env("PAAS_STUDIO_BRANCH", "main"),
		DBPortMin:       envInt("PAAS_DB_PORT_MIN", 20000),
		DBPortMax:       envInt("PAAS_DB_PORT_MAX", 20999),
		ClusterBase:     env("PAAS_CLUSTER_BASE", "10.100"),
		ServerName:      env("PAAS_SERVER_NAME", hostname()),
		ClusterAddr:     env("PAAS_NATS_CLUSTER_ADDR", ""),
		ClusterRoutes:   splitList(env("PAAS_NATS_ROUTES", "")),
		UpdateURL:       env("PAAS_UPDATE_URL", "https://api.github.com/repos/thesunbg/paas/releases/latest"),
		UpdateCheck:     envBool("PAAS_UPDATE_CHECK", true),

		UpdateAllowUnverified: envBool("PAAS_UPDATE_ALLOW_UNVERIFIED", false),
		S3: S3{
			Endpoint: env("PAAS_S3_ENDPOINT", ""), Region: env("PAAS_S3_REGION", "auto"), Bucket: env("PAAS_S3_BUCKET", ""),
			AccessKey: env("PAAS_S3_ACCESS_KEY", ""), SecretKey: env("PAAS_S3_SECRET_KEY", ""),
			Prefix: env("PAAS_S3_PREFIX", "paas-backups"), VirtualHost: envBool("PAAS_S3_VIRTUAL_HOST", false),
		},
	}
}

func LoadAgent() Agent {
	host, _ := os.Hostname()
	roles := strings.Split(env("PAAS_ROLES", "worker,build"), ",")
	for i := range roles {
		roles[i] = strings.TrimSpace(roles[i])
	}
	data := env("PAAS_DATA_DIR", "/var/lib/paas")
	_ = runtime.NumCPU
	return Agent{
		NATSURL:        env("PAAS_NATS_URL", "nats://127.0.0.1:4222"),
		ClusterToken:   env("PAAS_CLUSTER_TOKEN", ""),
		JoinURL:        env("PAAS_JOIN_URL", ""),
		JoinToken:      env("PAAS_JOIN_TOKEN", ""),
		IdentityDir:    env("PAAS_IDENTITY_DIR", filepath.Join(data, "identity")),
		NodeName:       env("PAAS_NODE_NAME", host),
		NodeAddress:    env("PAAS_NODE_ADDRESS", ""),
		Roles:          roles,
		DataDir:        data,
		ContainerdSock: env("PAAS_CONTAINERD_SOCK", "/run/containerd/containerd.sock"),
		Namespace:      env("PAAS_CONTAINERD_NAMESPACE", "paas"),
		OCIRuntime:     env("PAAS_OCI_RUNTIME", "crun"),
		BuildkitAddr:   env("PAAS_BUILDKIT_ADDR", "unix:///run/buildkit/buildkitd.sock"),
		CNIBinDir:      env("PAAS_CNI_BIN_DIR", "/opt/cni/bin"),
		SubnetPrefix:   env("PAAS_SUBNET_PREFIX", "10.100"),
		WGPort:         envInt("PAAS_WG_PORT", 51820),
		Mesh:           envBool("PAAS_MESH", true),
		EvictPSI:       envFloat("PAAS_EVICT_PSI", 40),
		EvictMinFreeMB: envInt("PAAS_EVICT_MIN_FREE_MB", 256),
	}
}

func LoadProxy() Proxy {
	data := env("PAAS_DATA_DIR", "/var/lib/paas")
	return Proxy{
		NATSURL:           env("PAAS_NATS_URL", "nats://127.0.0.1:4222"),
		ClusterToken:      env("PAAS_CLUSTER_TOKEN", ""),
		IdentityDir:       env("PAAS_IDENTITY_DIR", filepath.Join(data, "identity")),
		HTTPAddr:          env("PAAS_PROXY_HTTP_ADDR", ":80"),
		HTTPSAddr:         env("PAAS_PROXY_HTTPS_ADDR", ":443"),
		ACMEEmail:         env("PAAS_ACME_EMAIL", ""),
		TLS:               envBool("PAAS_TLS", false),
		ACMECA:            env("PAAS_ACME_CA", ""),
		DataDir:           filepath.Join(data, "proxy"),
		DashboardUpstream: env("PAAS_DASHBOARD_UPSTREAM", "127.0.0.1:8080"),
		RateLimit:         envInt("PAAS_PROXY_RATE_LIMIT", 0),
		RateBurst:         envInt("PAAS_PROXY_RATE_BURST", 0),
	}
}

func hostname() string {
	h, _ := os.Hostname()
	if h == "" {
		h = "paas"
	}
	return h
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
