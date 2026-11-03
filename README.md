# paas

**English** · [Tiếng Việt](README.vi.md)

A fast, lightweight, self-hosted PaaS, similar to Coolify, Heroku and Vercel. Push to GitHub and your app is live on its own domain with HTTPS in seconds. It handles frontends (static sites, React, Vue, Next.js…), backends (Node.js, Go, Python, PHP, Ruby, Rust, Java, Deno) and managed databases (PostgreSQL, MySQL/MariaDB, Redis, MongoDB). Servers can be added to the cluster with one command.

- **One binary** (`paasd`, about 37 MB) contains the control plane, node agent, edge proxy and an embedded NATS bus. Running everything on one node uses about 40 MB of RAM.
- **No Docker daemon.** Containers run on containerd with crun or runc. gVisor is used for sandboxed workloads. `standard` (namespaces, seccomp, dropped capabilities, root inside the container, no user namespace) is not a boundary for untrusted code: a kernel or runc CVE can escape to the host. Only `sandboxed` (gVisor) is, and a node without gVisor refuses to start sandboxed services.
- **Fast.** Builds use BuildKit with layer and cache-mount caching. Static sites are served directly by the proxy, with no container. Rollouts have zero downtime. Measured in the dev environment:
  - a cached Go redeploy goes live in about 2.4 s;
  - a static site goes live in about 0.9 s;
  - a sleeping app wakes up in about 1.7 s.
- **Dense.** These features let one node run many apps:
  - shared database servers;
  - request/limit QoS classes with controlled overcommit;
  - sleep mode with wake-on-request;
  - runtime tuning per language (heap and worker counts derived from the memory limit);
  - eviction under memory pressure;
  - right-sizing suggestions;
  - zram.
- **Clustered.** Nodes join over an encrypted WireGuard mesh. The address plan gives each node its own /16. Projects are isolated across nodes with nftables. An internal registry distributes images to the nodes that need them.

See [docs/PLAN.md](docs/PLAN.md) for the design and roadmap, and [docs/API.md](docs/API.md) for the REST API.

## Features

| Area | What you get |
|---|---|
| Deploy | Git repositories (GitHub webhooks, auto-deploy, commit statuses), Docker images, Docker Compose import, one-click templates (WordPress, n8n, Uptime Kuma, Umami, Gitea, Metabase…) |
| Git hosts | Private repositories from GitLab (gitlab.com or self-hosted), Gitea / Forgejo (codeberg.org or self-hosted) and Bitbucket Cloud with a per-team access token (encrypted, sent only to its own host); push-to-deploy webhooks with token / HMAC verification, MR/PR previews, commit statuses; `paas git-credentials`, `paas webhook` |
| Build | Language detection for Node (npm/pnpm/yarn/bun, Next/Nuxt/SvelteKit/Remix/Astro/Vite/CRA/Angular/Nest/Express), Go, Python (Django/FastAPI/Flask), PHP (Laravel via FrankenPHP), Ruby (Rails), Rust, Java (Maven/Gradle), Deno and static HTML. Your own Dockerfile always wins. Build-time secrets never land in image layers. Built and pulled images are scanned for known vulnerabilities (Trivy) |
| Run | Web services, workers, static sites (SPA fallback, gzip, immutable asset caching), cron jobs, PR preview deployments, persistent volumes |
| Operate | Zero-downtime rolling deploys, instant rollback, environment variables encrypted at rest, custom domains with automatic HTTPS (Let's Encrypt), live build and runtime logs, web console (exec), scaling, autoscaling (CPU / requests per second), metrics history, threshold alerts (CPU, memory, restarts, 5xx rate, instances) with "for" durations and cooldowns, HTTP(S) uptime checks with 24h / 7d uptime and TLS expiry warnings, Prometheus `/metrics` export, crash restarts and self-healing |
| Databases | Shared or dedicated PostgreSQL / MySQL / MariaDB, plus Redis and MongoDB; linking injects `DATABASE_URL`; scheduled backups and restore; off-site copies to S3/R2/B2/MinIO; one-click upgrade from shared to dedicated |
| Density | QoS classes (guaranteed / burstable / best-effort), per-node overcommit, sleep mode, memory-pressure eviction (PSI), language runtime tuning, right-sizing, zram, image and build-cache GC, per-team quotas (projects, services, databases, reserved memory and CPU, database storage, previews, custom domains) |
| Cluster | `join` with one command, WireGuard mesh, internal DNS (`<service>.<project>.internal`), cross-node project isolation, internal image registry, cordon / drain, node health and rescheduling, moving services with volumes between nodes (copy over the mesh, checksum, short downtime; `drain --move-volumes`), restoring a lost node's stateful services from their latest backups with the RPO recorded |
| Security | Sessions + API tokens, TOTP two-factor auth, single sign-on with OpenID Connect (Google Workspace, Entra ID, Keycloak, Authentik…: PKCE, safe account linking, just-in-time accounts, group → team role mapping, enforced SSO per email domain with an owner break-glass login), team RBAC (owner/admin/developer/viewer), audit log, seccomp, no-new-privileges, per-IP rate limiting at the edge, image vulnerability scanning with a per-project policy (off / warn / block at a severity threshold) |
| Notify | Webhook, Slack, Discord and Telegram channels for deploys, crashes, node outages, failed backups and jobs, alerts firing and resolved, uptime checks down and recovered, expiring TLS certificates |
| AI / MCP | MCP endpoint (`/mcp`) so AI agents such as Claude Code can inspect, deploy, roll back, restart and scale apps with an API token and its RBAC; optional "Explain this failure" on failed deployments (Anthropic or any OpenAI-compatible / self-hosted model, secrets redacted, off by default) |
| Interfaces | Web dashboard (embedded Svelte app, English and Vietnamese: EN/VI switch, follows the browser language), `paas` CLI, REST API, MCP server for AI agents ([docs/MCP.md](docs/MCP.md)) |

## Performance and capacity

How many apps fit on a server. Every table says which kind of number it holds:
- **Measured**: in the development environment (2 nodes, containerd + runc, BuildKit, PostgreSQL).
- **Estimated**: computed from paas's defaults (sizes, overcommit, memory reserved for the system) and typical memory use of each kind of app. Real numbers depend on the app and its traffic; the dashboard always shows actual use so you can adjust.

### Speed (measured)

| Operation | Time |
|---|---|
| Redeploy of a Go app with a warm build cache, until it serves traffic | about 2.4 s |
| A static site goes live | about 0.9 s |
| A sleeping app wakes on its first request | about 1.7 s |
| Failed requests during a rolling deploy | 0 |
| Control-plane leader dies, another takes over (HA) | about 1 s |

Why: no Docker daemon (the agent drives containerd, containers start with crun, about twice as fast as runc); multi-level build caches (BuildKit layers and cache mounts); an in-cluster registry; static sites served by the proxy with no container; traffic switches only after the health check passes.

### Memory used by the platform

| Component | RAM |
|---|---|
| `paasd` with every role on one node (control plane, agent, edge proxy, NATS) — measured | about 40 MB |
| containerd | about 20–30 MB |
| PostgreSQL holding the control-plane state | about 50–150 MB depending on the number of apps |
| Compared with a platform built on the Docker daemon | about 100 MB saved per node (no `dockerd`) |

Each node keeps **1 GB** for the operating system and the platform by default; the scheduler never places apps there (*Nodes → Settings*, `reserved_mb`).

### App sizes and how the scheduler counts

The **request** is the memory the scheduler reserves for an app; the **limit** is the most it may use.

| Size | Memory request | Memory limit | CPU request | CPU limit |
|---|---|---|---|---|
| nano | 48 MB | 128 MB | 0.025 vCPU | 0.25 vCPU |
| micro | 96 MB | 256 MB | 0.05 vCPU | 0.5 vCPU |
| small | 160 MB | 512 MB | 0.1 vCPU | 1 vCPU |
| medium | 384 MB | 1 GB | 0.25 vCPU | 2 vCPU |
| large | 768 MB | 2 GB | 0.5 vCPU | 4 vCPU |
| xlarge | 1.5 GB | 4 GB | 1 vCPU | 8 vCPU |

A node's capacity = (RAM − 1 GB reserved) × **1.5** (default overcommit for burstable apps). Most apps stay well under their limit most of the time; when real memory runs short, the agent evicts the least important workload and zram compresses memory.

### Shared databases (estimated)

| How it runs | RAM |
|---|---|
| One PostgreSQL server per app | about 100–250 MB per app |
| A logical database on a shared PostgreSQL server (the templates' default) | about 5–20 MB per app, plus one shared server of about 150–300 MB |
| Redis | about 10–50 MB |

30 n8n instances: 30 PostgreSQL servers take about 3–7.5 GB; 30 databases on one shared server about 0.3–0.9 GB.

### Example: how many n8n on one server?

The n8n template is **small** (request 160 MB, limit 512 MB) with a **shared** PostgreSQL database. Idle, an n8n typically uses about 150–250 MB (estimated).

| Server RAM | n8n always running (estimated) | Batch with sleep (the default) |
|---|---|---|
| 4 GB | about 12–15 | about 50–100 copies, about 10–12 awake at once |
| 8 GB | about 30–35 | 200 copies (one batch's maximum), about 25–30 awake at once |
| 16 GB | about 65–75 | several hundred copies (several batches), about 65 awake at once |
| 32 GB | about 140–150 | several hundred to over a thousand copies, about 140 awake at once |

- **Always running** is bounded by real memory (about 200 MB each), not by the scheduler: 8 GB leaves about 7 GB for apps, about 35 n8n; the scheduler would allow (8 − 1) × 1.5 / 0.16 ≈ 67, but packing past real memory makes them slow.
- **Sleep**: a copy nobody uses holds no memory (only its logical database, about 10 MB, and its data on disk). The limit becomes disk space and how many people use them at once — a class of 30 where 10 work at the same time holds memory for 10. A copy wakes in about 2 s.
- When you create a batch, paas computes this for your own server and shows how many copies can be awake together; a batch that would run the server out of memory is refused.

### Other apps (estimated, 8 GB server, always running)

| App | Template size | Typical real memory | Apps on 8 GB |
|---|---|---|---|
| Static site (built React/Vue, HTML) | no container | 0 | hundreds to thousands (disk-bound) |
| Go API | small | 10–30 MB | 100+ (scheduler-bound: about 67 at small, about 220 at nano) |
| Node.js / Express API | small | 60–120 MB | about 55–65 (the scheduler allows 67 small apps) |
| Python API (FastAPI, Django) | small | 80–250 MB | about 30–65 |
| WordPress (shared MariaDB) | small | 80–150 MB | about 45–65 |
| Next.js / Nuxt SSR | small | 150–300 MB | about 25–45 |
| Uptime Kuma | micro | 80–120 MB | about 60–85 |
| Metabase, Keycloak (Java) | medium | 400–700 MB | about 10–15 |

Sleep mode for rarely used apps (previews, side projects, demos) typically fits 2–3 times as many.

### Disk

| What | Typical size |
|---|---|
| An app's images (the last 3 kept for rollback, `PAAS_IMAGE_RETENTION`) | about 0.2–1 GB per app |
| A logical database | 1 GB quota by default (adjustable) |
| Build cache | garbage-collected automatically |

An 8 GB RAM / 160 GB SSD server usually runs out of memory before disk, unless apps store many files (Nextcloud, Jellyfin…).

### Check on your own server

- *Nodes*: reserved and real memory and the overcommit ratio of each node.
- Service → *Metrics*: CPU, memory and requests per second; *Settings* suggests a smaller size when an app uses much less than its limit.
- *Batches → New batch*: the estimate for the template and number of copies you pick, on your server.

```bash
ps -o rss=,comm= -C paasd | awk '{printf "%s: %.0f MB\n", $2, $1/1024}'
```

## Install

**1. Control plane + first node** (Ubuntu/Debian or Fedora/RHEL, as root):

```bash
curl -fsSL https://raw.githubusercontent.com/thesunbg/paas/main/internal/installer/install.sh \
  | sudo sh -s -- server --domain apps.example.com --email you@example.com
```

Or, from your laptop, without logging in to the server yourself (the `paas` CLI connects over SSH, confirms the host key, checks the machine, warns when DNS does not point at it yet, runs the same script and waits for the dashboard):

```bash
paas install --ssh root@203.0.113.10 --domain apps.example.com --email you@example.com
```

It authenticates with your SSH agent by default (`--identity ~/.ssh/key` or `--password` otherwise; a non-root user needs sudo). The host key is accepted when `~/.ssh/known_hosts` lists it, when it matches `--fingerprint SHA256:...`, or when you confirm it interactively (`--yes` trusts it on first use); a key that differs from known_hosts is refused. `--installer-url` runs another install.sh, and flags after `--` are passed to it.

Use `--https` instead of `--email` for automatic HTTPS without an email. Point DNS for `*.apps.example.com` and `paas.apps.example.com` at the server. Changing `--domain` later and re-running the script moves the generated app domains and the GitHub App webhook to the new domain. Then open `https://paas.apps.example.com` and create the administrator account: a setup wizard follows and checks DNS, HTTPS, ports, Git, off-site backups, notifications and a first deploy, with a how-to for each (re-open it from *Settings → System → Setup checklist*). Over SSH, `paas doctor` prints the same checklist. The script installs:
- containerd 2, crun, runc, the CNI plugins and BuildKit;
- PostgreSQL for the control-plane state;
- zram and kernel tuning;
- a systemd unit for `paasd`.

**2. Add servers when load grows.** Copy the join command from *Nodes → Add node* (or `paas join-command`):

```bash
curl -fsSL https://paas.apps.example.com/install.sh | sudo sh -s -- join \
  --url https://paas.apps.example.com --token <join-token> --roles worker
```

Or let the control plane do it: *Nodes → Add node → Install over SSH* (or `paas node add root@203.0.113.11 --roles worker`). Give the host, the SSH user (root or a sudo user) and a private key or password, and confirm the server's host key fingerprint. The control plane checks the machine (OS, architecture, systemd, memory, disk, free ports, an existing paasd, whether it reaches the control plane), runs the join command above with a fresh single-use token, streams the installer output and waits until the node is online. The key or password is used for that install only and never stored; attempts, their checklist and log are kept (`paas node installs`).

The token is single-use and short-lived, and it is only issued when `PAAS_PUBLIC_URL` is `https://`. The node trades it for its own identity (`POST /api/v1/cluster/enroll`) and receives the bus address and CA from the control plane.

Roles: `worker` (runs apps), `build` (runs BuildKit) and `edge` (runs the HTTPS proxy). Nodes need to reach each other on UDP 51820 (WireGuard) and the control plane on TCP 4222.

**Migrating an existing cluster** (nodes that still use the shared cluster token): set `PAAS_NATS_LEGACY_TOKEN=1` on the control plane, upgrade it, then run a fresh join command on each node. When no node shows `legacy` in the nodes API (`GET /nodes`), unset the flag.

**3. Deploy:**

```bash
paas login --server https://paas.apps.example.com
paas projects create shop && paas use shop
paas create api --repo https://github.com/me/api --size small
paas db create main --engine postgres --link api      # injects DATABASE_URL
paas env set api LOG_LEVEL=info                       # rolling restart
paas logs api -f
```

Then add the webhook URL shown on the service page to the GitHub repository (events: `push`, plus `pull_request` for previews). Every push deploys automatically.

## Connect GitHub

Open **Settings → GitHub → Create GitHub App** (administrator, once). GitHub creates a private
app for your server and sends you back. Then **Connect GitHub account**, choose the repositories
to grant, and in **New service** pick a repository from the list: language, type and port are
detected automatically, and pushes / pull requests deploy without manual webhooks or tokens.
The server's `PAAS_PUBLIC_URL` must be reachable from GitHub.

## Databases as a service

Managed PostgreSQL, MySQL, MariaDB, Redis and MongoDB, shared or dedicated, with:
external access on `db.<base domain>:<port>` (TLS and an IP allowlist; open TCP
20000–20999 in the firewall), usage metrics and storage quotas, PgBouncer pooling,
extra read-only / read-write users, in-place resize, point-in-time recovery and
high availability with automatic failover for dedicated PostgreSQL.

## High availability

Run several control planes against the same (highly available) PostgreSQL:

```bash
# on cp1 and cp2 (and cp3), in /etc/paas/paasd.env
PAAS_SERVER_NAME=cp1
PAAS_DATABASE_URL=postgres://paas:…@db.internal:5432/paas
PAAS_MASTER_KEY=<same hex key on every control plane>
PAAS_CLUSTER_TOKEN=<same token on every control plane; authenticates routes between control planes only>
PAAS_NATS_CLUSTER_ADDR=0.0.0.0:6222
PAAS_NATS_ROUTES=nats://cp2:6222,nats://cp3:6222
```

Nodes get the bus URL and the cluster CA when they enroll; point the dashboard hostname at every control plane, and the NATS client learns the other servers from the cluster. Bus and routes use TLS from a CA the control planes create and share through the database. Every control plane serves the API and the bus. One elected leader (a PostgreSQL advisory lock) runs the deploy queue, reconciler, cron, autoscaler and maintenance. When the leader dies, another one takes over within seconds, and running apps are never affected.

The installer sets up an extra control plane with `server --database-url … --master-key-file … --server-name cp2 --routes nats://cp1:6222`. Backups, restoring the platform state and recovering from a lost node are in [docs/DR.md](docs/DR.md).

## Upgrading

Settings → System → Updates shows the version of every control plane and node and the latest release (checked every 6 hours by the leader; `PAAS_UPDATE_CHECK=false` turns that off, `PAAS_UPDATE_URL` points it at a mirror). **Update** (or `paas update --yes [--version vX.Y.Z]`, progress with `paas update status`) runs a rolling upgrade:

1. a backup of the platform state;
2. the control planes, one at a time, the leader last so leadership moves to an updated one, which carries on;
3. every other node (worker, build, edge) one at a time; offline and cordoned nodes are skipped.

Each machine downloads the release binary, checks it against the release's `SHA256SUMS` (it refuses a missing or wrong checksum; `PAAS_UPDATE_ALLOW_UNVERIFIED=true` skips the check and is unsafe), swaps it in atomically keeping the old one as `/usr/local/bin/paasd.prev`, and restarts under systemd; containers keep running. A machine that does not come back healthy with the new version within 10 minutes is rolled back to `paasd.prev` and the update stops; a local systemd timer does the same if the new binary cannot even start. Updates can be paused, resumed and aborted, and any node can be rolled back by hand. Only binaries are rolled back, never the database: migrations are additive, so the previous version keeps working on the newer schema, and downgrades are refused. If a release's notes say a migration is not additive, the way back is the state backup taken in step 1 (`paasd restore-state`). Development builds (anything but a `vX.Y.Z` tag) are never told a release is newer.

### Protecting the control plane from running out of memory

`paasd` runs with `OOMScoreAdjust=-900`, and installs from this version on give the control plane's local PostgreSQL the same (its backends are reset to 0, so a runaway query can still be killed and PostgreSQL recovers). When a node runs out of memory the kernel then kills an app, never the platform. Re-running the install script applies it to an existing server; or by hand:

```bash
for u in postgresql.service postgresql@.service; do sudo mkdir -p /etc/systemd/system/$u.d; printf '[Service]\nOOMScoreAdjust=-900\nEnvironment=PG_OOM_ADJUST_FILE=/proc/self/oom_score_adj\nEnvironment=PG_OOM_ADJUST_VALUE=0\n' | sudo tee /etc/systemd/system/$u.d/paas-oom.conf >/dev/null; done
```

```bash
sudo systemctl daemon-reload && sudo systemctl restart postgresql paasd
```

Batches (one template for a whole class) start their copies two at a time and put each to sleep once it is up, so creating 30 copies no longer boots 30 apps at once, and only the copies in use hold memory. A batch that would run the nodes out of memory is refused, even with "Create anyway".

## Configuration

`paasd` is configured through environment variables (`/etc/paas/paasd.env`):

| Variable | Default | Meaning |
|---|---|---|
| `PAAS_DATABASE_URL` | `postgres://paas:paas@127.0.0.1:5432/paas` | control-plane state |
| `PAAS_BASE_DOMAIN` | `localhost` | apps get `<service>-<project>.<base>` |
| `PAAS_DASHBOARD_DOMAIN` | `paas.<base>` | dashboard hostname on the edge proxy |
| `PAAS_PUBLIC_URL` | `http://paas.<base>` | used in webhook URLs and links |
| `PAAS_ACME_EMAIL` | | enables automatic HTTPS |
| `PAAS_DATA_DIR` | `/var/lib/paas` | volumes, builds, backups, keys |
| `PAAS_HTTP_ADDR` / `PAAS_NATS_ADDR` | `:8080` / `0.0.0.0:4222` | API and cluster bus |
| `PAAS_PROXY_HTTP_ADDR` / `PAAS_PROXY_HTTPS_ADDR` | `:80` / `:443` | edge proxy |
| `PAAS_PROXY_RATE_LIMIT` / `PAAS_PROXY_RATE_BURST` | `0` (off) | requests per second per client IP |
| `PAAS_ROLES` | `worker,build` | node roles |
| `PAAS_OCI_RUNTIME` | `crun` | `crun` or `runc` |
| `PAAS_CLUSTER_BASE` | `10.100` | container address space (node *n* gets `10.(100+n).0.0/16`) |
| `PAAS_WG_PORT` / `PAAS_MESH` | `51820` / `true` | WireGuard mesh |
| `PAAS_IMAGE_RETENTION` | `3` | deployments kept per service for rollback |
| `PAAS_PUBLIC_IPS` | node addresses | public IPs DNS should point to (setup checklist), comma separated; set it behind NAT |
| `PAAS_METRICS_TOKEN` | | bearer token for the Prometheus endpoint `GET /metrics` (administrators can also use their own API token) |
| `PAAS_UPTIME_ALLOW_PRIVATE` | `false` | let uptime checks reach private addresses (trusted networks) |
| `PAAS_S3_ENDPOINT`, `PAAS_S3_BUCKET`, `PAAS_S3_ACCESS_KEY`, `PAAS_S3_SECRET_KEY`, `PAAS_S3_REGION`, `PAAS_S3_PREFIX` | | off-site database backups |
| `PAAS_EVICT_PSI` / `PAAS_EVICT_MIN_FREE_MB` | `40` / `256` | memory-pressure eviction thresholds |
| `PAAS_UPDATE_URL` | GitHub's latest release of thesunbg/paas | release endpoint for update checks (GitHub API JSON) |
| `PAAS_UPDATE_CHECK` | `true` | `false` stops the 6-hourly check for new releases |
| `PAAS_UPDATE_ALLOW_UNVERIFIED` | `false` | **unsafe**: install release binaries that have no published SHA-256 checksum |
| `PAAS_SERVER_NAME`, `PAAS_NATS_CLUSTER_ADDR`, `PAAS_NATS_ROUTES`, `PAAS_NATS_ADVERTISE`, `PAAS_MASTER_KEY`, `PAAS_CLUSTER_TOKEN` | | high availability (see above) |

## Development

```bash
make build            # bin/paasd and bin/paas
make web              # rebuild the dashboard into internal/webui/dist
make test             # unit tests
make test-integration # needs root, containerd, buildkitd (containerd worker) and CNI plugins
```

To run locally you need containerd, BuildKit (containerd worker, namespace `paas`), the CNI plugins in `/opt/cni/bin`, and PostgreSQL. Start everything with `PAAS_ALLOW_LOCAL_GIT=true sudo -E bin/paasd all`, then open http://127.0.0.1:8080. `examples/` holds tiny Node, Go, Python and static apps to try.

Layout:

```
cmd/paasd        server | agent | proxy | all
cmd/paas         CLI
internal/server  REST API, deploy pipeline, reconciler, scheduler glue, databases, previews, jobs, autoscaling
internal/agent   node agent (commands, heartbeats, eviction, static origin)
internal/runtime containerd runtime (crun/runc/gVisor), logs, exec, metrics
internal/network CNI bridges, WireGuard mesh, nftables isolation, internal DNS
internal/build   git + BuildKit pipeline;  internal/detect  language detection
internal/proxy   edge proxy (routing, TLS, static, wake-up, rate limit)
internal/...     store (PostgreSQL), scheduler, resources (QoS/tuning), databases, templates, cron, s3, totp, secrets
web/             dashboard source (Svelte 5)
```
