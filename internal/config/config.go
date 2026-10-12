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
	DatabaseURL     string // PAAS_DATABASE_URL
	HTTPAddr        string // PAAS_HTTP_ADDR, API + dashboard
	NATSAddr        string // PAAS_NATS_ADDR, listener for agents and proxies
	DataDir         string // PAAS_DATA_DIR
	MasterKey       string // PAAS_MASTER_KEY (hex, optional; otherwise <data>/master.key)
	BaseDomain      string // PAAS_BASE_DOMAIN, generated app domains are <svc>-<project>.<base>
	DashboardDomain string // PAAS_DASHBOARD_DOMAIN, served by the proxy to the API
	PublicURL       string // PAAS_PUBLIC_URL, used in webhook URLs and links
	ClusterToken    string // PAAS_CLUSTER_TOKEN (optional; generated and stored otherwise)
	AllowLocalGit   bool   // PAAS_ALLOW_LOCAL_GIT, allow file:// repos (dev / testing)
	ImageRetention  int    // PAAS_IMAGE_RETENTION, deployments kept per service for rollback
}

type Agent struct {
	NATSURL        string   // PAAS_NATS_URL
	ClusterToken   string   // PAAS_CLUSTER_TOKEN
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
}

type Proxy struct {
	NATSURL           string // PAAS_NATS_URL
	ClusterToken      string // PAAS_CLUSTER_TOKEN
	HTTPAddr          string // PAAS_PROXY_HTTP_ADDR
	HTTPSAddr         string // PAAS_PROXY_HTTPS_ADDR
	ACMEEmail         string // PAAS_ACME_EMAIL, enables automatic HTTPS when set
	ACMECA            string // PAAS_ACME_CA, optional custom directory (e.g. staging)
	DataDir           string // PAAS_DATA_DIR
	DashboardUpstream string // PAAS_DASHBOARD_UPSTREAM, e.g. 127.0.0.1:8080
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
		DataDir:         data,
		MasterKey:       env("PAAS_MASTER_KEY", ""),
		BaseDomain:      base,
		DashboardDomain: env("PAAS_DASHBOARD_DOMAIN", "paas."+base),
		PublicURL:       strings.TrimRight(env("PAAS_PUBLIC_URL", "http://paas."+base), "/"),
		ClusterToken:    env("PAAS_CLUSTER_TOKEN", ""),
		AllowLocalGit:   envBool("PAAS_ALLOW_LOCAL_GIT", false),
		ImageRetention:  envInt("PAAS_IMAGE_RETENTION", 3),
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
		NodeName:       env("PAAS_NODE_NAME", host),
		NodeAddress:    env("PAAS_NODE_ADDRESS", ""),
		Roles:          roles,
		DataDir:        data,
		ContainerdSock: env("PAAS_CONTAINERD_SOCK", "/run/containerd/containerd.sock"),
		Namespace:      env("PAAS_CONTAINERD_NAMESPACE", "paas"),
		OCIRuntime:     env("PAAS_OCI_RUNTIME", "crun"),
		BuildkitAddr:   env("PAAS_BUILDKIT_ADDR", "unix:///run/buildkit/buildkitd.sock"),
		CNIBinDir:      env("PAAS_CNI_BIN_DIR", "/opt/cni/bin"),
		SubnetPrefix:   env("PAAS_SUBNET_PREFIX", "10.200"),
		EvictPSI:       envFloat("PAAS_EVICT_PSI", 40),
		EvictMinFreeMB: envInt("PAAS_EVICT_MIN_FREE_MB", 256),
	}
}

func LoadProxy() Proxy {
	data := env("PAAS_DATA_DIR", "/var/lib/paas")
	return Proxy{
		NATSURL:           env("PAAS_NATS_URL", "nats://127.0.0.1:4222"),
		ClusterToken:      env("PAAS_CLUSTER_TOKEN", ""),
		HTTPAddr:          env("PAAS_PROXY_HTTP_ADDR", ":80"),
		HTTPSAddr:         env("PAAS_PROXY_HTTPS_ADDR", ":443"),
		ACMEEmail:         env("PAAS_ACME_EMAIL", ""),
		ACMECA:            env("PAAS_ACME_CA", ""),
		DataDir:           filepath.Join(data, "proxy"),
		DashboardUpstream: env("PAAS_DASHBOARD_UPSTREAM", "127.0.0.1:8080"),
	}
}
