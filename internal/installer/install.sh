#!/bin/sh
# paas installer.
#
#   Control plane + first node (all-in-one):
#     curl -fsSL https://raw.githubusercontent.com/thesunbg/paas/main/internal/installer/install.sh | sudo sh -s -- server --domain apps.example.com --email you@example.com
#
#   Join another node to an existing cluster:
#     curl -fsSL https://<control-plane>/install.sh | sudo sh -s -- join --server nats://<control-plane>:4222 --token <cluster-token> --roles worker
#
# Options:
#   --domain DOMAIN      base domain for generated app URLs (wildcard DNS *.DOMAIN -> edge nodes)
#   --email EMAIL        enables automatic HTTPS (Let's Encrypt) on the edge proxy
#   --https              enables automatic HTTPS without an email
#   --dashboard-domain D dashboard hostname (default paas.DOMAIN; may equal DOMAIN)
#   --roles ROLES        comma separated: worker,build,edge (join; default worker)
#   --server URL         NATS URL of the control plane (join)
#   --token TOKEN        cluster token (join)
#   --binary URL|PATH    paasd binary to install (default: latest GitHub release)
#   --mirror HOST        Docker Hub pull-through mirror (e.g. mirror.gcr.io)
#   --no-zram            do not configure compressed swap in RAM
set -eu

MODE="${1:-server}"; [ $# -gt 0 ] && shift
DOMAIN=""; DASH=""; EMAIL=""; HTTPS=0; ROLES=""; SERVER=""; TOKEN=""; BINARY=""; MIRROR=""; ZRAM=1
while [ $# -gt 0 ]; do
  case "$1" in
    --domain) DOMAIN="$2"; shift 2;;
    --email) EMAIL="$2"; shift 2;;
    --https) HTTPS=1; shift;;
    --dashboard-domain) DASH="$2"; shift 2;;
    --roles) ROLES="$2"; shift 2;;
    --server) SERVER="$2"; shift 2;;
    --token) TOKEN="$2"; shift 2;;
    --binary) BINARY="$2"; shift 2;;
    --mirror) MIRROR="$2"; shift 2;;
    --no-zram) ZRAM=0; shift;;
    *) echo "unknown option $1" >&2; exit 2;;
  esac
done

CONTAINERD_VERSION="${CONTAINERD_VERSION:-2.1.4}"
RUNC_VERSION="${RUNC_VERSION:-1.3.0}"
CRUN_VERSION="${CRUN_VERSION:-1.24}"
CNI_VERSION="${CNI_VERSION:-1.8.0}"
BUILDKIT_VERSION="${BUILDKIT_VERSION:-0.25.1}"
PAAS_REPO="${PAAS_REPO:-thesunbg/paas}"

trap 'rc=$?; if [ $rc -ne 0 ]; then printf "\033[1;31merror:\033[0m installation failed (exit %s); rerun with sh -x to see where\n" "$rc" >&2; fi' EXIT
log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "run as root (sudo)"
[ "$(uname -s)" = Linux ] || die "Linux is required"
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64;;
  aarch64|arm64) ARCH=arm64;;
  *) die "unsupported architecture $(uname -m)";;
esac
case "$MODE" in server|join) ;; *) die "mode must be 'server' or 'join'";; esac
if [ "$MODE" = join ]; then
  [ -n "$SERVER" ] && [ -n "$TOKEN" ] || die "join needs --server and --token"
  [ -n "$ROLES" ] || ROLES=worker
else
  [ -n "$ROLES" ] || ROLES=worker,build
fi

fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }

install_packages() {
  if command -v apt-get >/dev/null; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq curl ca-certificates git nftables iptables tar >/dev/null
    if [ "$MODE" = server ]; then apt-get install -y -qq postgresql >/dev/null; fi
  elif command -v dnf >/dev/null; then
    dnf install -y -q curl ca-certificates git nftables iptables tar
    if [ "$MODE" = server ]; then dnf install -y -q postgresql-server; postgresql-setup --initdb || true; systemctl enable --now postgresql; fi
  else
    die "unsupported distribution (need apt or dnf)"
  fi
}

install_runtime() {
  tmp=$(mktemp -d)
  if ! command -v containerd >/dev/null || ! containerd --version | grep -q " v2\."; then
    log "installing containerd $CONTAINERD_VERSION"
    fetch "https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}/containerd-${CONTAINERD_VERSION}-linux-${ARCH}.tar.gz" "$tmp/c.tgz"
    tar -xzf "$tmp/c.tgz" -C /usr/local
    fetch "https://raw.githubusercontent.com/containerd/containerd/v${CONTAINERD_VERSION}/containerd.service" /etc/systemd/system/containerd.service
  fi
  if ! command -v runc >/dev/null; then
    log "installing runc $RUNC_VERSION"
    fetch "https://github.com/opencontainers/runc/releases/download/v${RUNC_VERSION}/runc.${ARCH}" "$tmp/runc"
    install -m 0755 "$tmp/runc" /usr/local/sbin/runc
  fi
  if ! command -v crun >/dev/null; then
    log "installing crun $CRUN_VERSION (fast OCI runtime)"
    fetch "https://github.com/containers/crun/releases/download/${CRUN_VERSION}/crun-${CRUN_VERSION}-linux-${ARCH}" "$tmp/crun"
    install -m 0755 "$tmp/crun" /usr/local/bin/crun
  fi
  if [ ! -x /opt/cni/bin/bridge ]; then
    log "installing CNI plugins $CNI_VERSION"
    mkdir -p /opt/cni/bin
    fetch "https://github.com/containernetworking/plugins/releases/download/v${CNI_VERSION}/cni-plugins-linux-${ARCH}-v${CNI_VERSION}.tgz" "$tmp/cni.tgz"
    tar -xzf "$tmp/cni.tgz" -C /opt/cni/bin
  fi
  case ",$ROLES," in *,build,*)
    if ! command -v buildkitd >/dev/null; then
      log "installing BuildKit $BUILDKIT_VERSION"
      fetch "https://github.com/moby/buildkit/releases/download/v${BUILDKIT_VERSION}/buildkit-v${BUILDKIT_VERSION}.linux-${ARCH}.tar.gz" "$tmp/bk.tgz"
      tar -xzf "$tmp/bk.tgz" -C /usr/local
    fi
    mkdir -p /etc/buildkit
    cat > /etc/buildkit/buildkitd.toml <<EOF
[worker.oci]
  enabled = false
[worker.containerd]
  enabled = true
  namespace = "paas"
  # RUN steps get their own firewalled bridge (pn1022); paas-agent writes the
  # conflist after registration. Without it buildkitd would use the host network.
  networkMode = "cni"
  cniConfigPath = "/etc/buildkit/cni.conflist"
  cniBinaryPath = "/opt/cni/bin"
  gc = true
  [[worker.containerd.gcpolicy]]
    keepBytes = "25GB"
    keepDuration = "720h"
EOF
    if [ -n "$MIRROR" ]; then
      printf '[registry."docker.io"]\n  mirrors = ["%s"]\n' "$MIRROR" >> /etc/buildkit/buildkitd.toml
    fi
    cat > /etc/systemd/system/buildkit.service <<EOF
[Unit]
Description=BuildKit (paas builds)
After=containerd.service
Requires=containerd.service
# Fails until paas-agent has written cni.conflist; keep retrying.
StartLimitIntervalSec=0

[Service]
ExecStart=/usr/local/bin/buildkitd --config /etc/buildkit/buildkitd.toml
Restart=always
RestartSec=5
# Keep builds from starving running apps.
MemoryHigh=75%
CPUWeight=50

[Install]
WantedBy=multi-user.target
EOF
  ;; esac
  if [ -n "$MIRROR" ]; then
    mkdir -p /etc/containerd/certs.d/docker.io
    printf 'server = "https://registry-1.docker.io"\n\n[host."https://%s"]\n  capabilities = ["pull", "resolve"]\n' "$MIRROR" > /etc/containerd/certs.d/docker.io/hosts.toml
  fi
  if [ ! -f /etc/containerd/config.toml ]; then
    mkdir -p /etc/containerd
    cat > /etc/containerd/config.toml <<'EOF'
version = 3
[plugins.'io.containerd.cri.v1.images'.registry]
  config_path = "/etc/containerd/certs.d"
EOF
  fi
  rm -rf "$tmp"
}

tune_kernel() {
  cat > /etc/sysctl.d/90-paas.conf <<'EOF'
net.ipv4.ip_forward = 1
net.ipv4.conf.all.route_localnet = 0
net.core.somaxconn = 4096
net.ipv4.ip_local_port_range = 10240 65535
fs.inotify.max_user_instances = 8192
vm.swappiness = 100
vm.overcommit_memory = 1
EOF
  sysctl --system >/dev/null 2>&1 || true
  if [ "$ZRAM" = 1 ] && [ ! -e /etc/systemd/system/paas-zram.service ] && modprobe zram 2>/dev/null; then
    log "enabling zram (compressed swap in RAM, 50% of memory)"
    cat > /usr/local/sbin/paas-zram <<'EOF'
#!/bin/sh
set -e
[ -e /sys/block/zram0 ] || modprobe zram
swapoff /dev/zram0 2>/dev/null || true
echo 1 > /sys/block/zram0/reset
echo zstd > /sys/block/zram0/comp_algorithm 2>/dev/null || echo lz4 > /sys/block/zram0/comp_algorithm
mem_kb=$(awk '/MemTotal/{print $2}' /proc/meminfo)
echo $((mem_kb * 512)) > /sys/block/zram0/disksize
mkswap /dev/zram0 >/dev/null
swapon -p 100 /dev/zram0
EOF
    chmod +x /usr/local/sbin/paas-zram
    cat > /etc/systemd/system/paas-zram.service <<'EOF'
[Unit]
Description=paas compressed swap (zram)

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/paas-zram
ExecStop=/sbin/swapoff /dev/zram0

[Install]
WantedBy=multi-user.target
EOF
    systemctl enable --now paas-zram.service >/dev/null 2>&1 || true
  fi
}

install_paasd() {
  log "installing paasd"
  if [ -z "$BINARY" ]; then
    BINARY="https://github.com/${PAAS_REPO}/releases/latest/download/paasd-linux-${ARCH}"
  fi
  # A control plane serves its own binary at <url>/download/paasd-linux-<arch>.
  case "$BINARY" in */download/paasd) BINARY="$BINARY-linux-$ARCH";; esac
  case "$BINARY" in
    http*) fetch "$BINARY" /tmp/paasd.new ;;
    *) cp "$BINARY" /tmp/paasd.new ;;
  esac
  install -m 0755 /tmp/paasd.new /usr/local/bin/paasd
  rm -f /tmp/paasd.new
  mkdir -p /etc/paas /var/lib/paas
  chmod 700 /var/lib/paas
}

setup_postgres() {
  log "configuring PostgreSQL for the control plane"
  systemctl enable --now postgresql >/dev/null 2>&1 || true
  PGPASS=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
  su - postgres -c "psql -tc \"SELECT 1 FROM pg_roles WHERE rolname='paas'\"" | grep -q 1 || \
    su - postgres -c "psql -c \"CREATE ROLE paas LOGIN PASSWORD '$PGPASS'\"" >/dev/null
  su - postgres -c "psql -c \"ALTER ROLE paas PASSWORD '$PGPASS'\"" >/dev/null
  su - postgres -c "psql -tc \"SELECT 1 FROM pg_database WHERE datname='paas'\"" | grep -q 1 || \
    su - postgres -c "createdb -O paas paas"
  echo "PAAS_DATABASE_URL=postgres://paas:${PGPASS}@127.0.0.1:5432/paas?sslmode=disable" > /etc/paas/db.env
  chmod 600 /etc/paas/db.env
}

write_units() {
  if [ "$MODE" = server ]; then
    [ -n "$DOMAIN" ] || DOMAIN="$(hostname -I 2>/dev/null | awk '{print $1}').sslip.io"
    SCHEME=http; [ -n "$EMAIL" ] && HTTPS=1; [ "$HTTPS" = 1 ] && SCHEME=https
    TLS=false; [ "$HTTPS" = 1 ] && TLS=true
    [ -n "$DASH" ] || DASH="paas.${DOMAIN}"
    cat > /etc/paas/paasd.env <<EOF
PAAS_DATA_DIR=/var/lib/paas
PAAS_BASE_DOMAIN=${DOMAIN}
PAAS_DASHBOARD_DOMAIN=${DASH}
PAAS_PUBLIC_URL=${SCHEME}://${DASH}
PAAS_ACME_EMAIL=${EMAIL}
PAAS_TLS=${TLS}
PAAS_ROLES=${ROLES}
PAAS_PROXY_HTTP_ADDR=:80
PAAS_PROXY_HTTPS_ADDR=:443
EOF
    ARGS=all
  else
    cat > /etc/paas/paasd.env <<EOF
PAAS_DATA_DIR=/var/lib/paas
PAAS_NATS_URL=${SERVER}
PAAS_CLUSTER_TOKEN=${TOKEN}
PAAS_ROLES=${ROLES}
PAAS_ACME_EMAIL=${EMAIL}
EOF
    ARGS=agent
  fi
  chmod 600 /etc/paas/paasd.env
  AFTER="containerd.service"
  case ",$ROLES," in *,build,*) AFTER="$AFTER buildkit.service";; esac
  if [ "$MODE" = server ]; then AFTER="$AFTER postgresql.service"; fi
  cat > /etc/systemd/system/paasd.service <<EOF
[Unit]
Description=paas (${ARGS})
After=network-online.target ${AFTER}
Wants=network-online.target ${AFTER}

[Service]
EnvironmentFile=/etc/paas/paasd.env
EnvironmentFile=-/etc/paas/db.env
ExecStart=/usr/local/bin/paasd ${ARGS}
Restart=always
RestartSec=2
LimitNOFILE=1048576
# Containers keep running when paasd restarts.
KillMode=process
OOMScoreAdjust=-900

[Install]
WantedBy=multi-user.target
EOF
  if [ "$MODE" = join ]; then
    case ",$ROLES," in *,edge,*)
      cat > /etc/systemd/system/paasd-proxy.service <<EOF
[Unit]
Description=paas edge proxy
After=network-online.target

[Service]
EnvironmentFile=/etc/paas/paasd.env
ExecStart=/usr/local/bin/paasd proxy
Restart=always
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF
      systemctl enable paasd-proxy.service >/dev/null 2>&1;;
    esac
  fi
}

log "installing paas ($MODE, roles: $ROLES)"
install_packages
install_runtime
tune_kernel
install_paasd
if [ "$MODE" = server ]; then setup_postgres; fi
write_units
systemctl daemon-reload
systemctl enable --now containerd >/dev/null
case ",$ROLES," in *,build,*) systemctl enable --now buildkit >/dev/null;; esac
systemctl enable paasd >/dev/null
systemctl restart paasd
if [ -e /etc/systemd/system/paasd-proxy.service ]; then systemctl restart paasd-proxy; fi

if [ "$MODE" = server ]; then
  log "done. Open ${SCHEME}://${DASH} (or http://$(hostname -I | awk '{print $1}'):8080) to create the administrator account."
  log "DNS: point *.${DOMAIN} and ${DASH} to this server."
else
  log "done. This node registers itself with the control plane within a few seconds (see: journalctl -u paasd -f)."
fi
