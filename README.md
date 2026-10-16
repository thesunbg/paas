# paas

A fast, lightweight, self-hosted PaaS, similar to Coolify, Heroku and Vercel. Push to GitHub and your app is live on its own domain with HTTPS in seconds. It handles frontends (static sites, React, Vue, Next.js…), backends (Node.js, Go, Python, PHP, Ruby, Rust, Java, Deno) and managed databases (PostgreSQL, MySQL/MariaDB, Redis, MongoDB). Servers can be added to the cluster with one command.

- **One binary** (`paasd`, about 37 MB) contains the control plane, node agent, edge proxy and an embedded NATS bus. Running everything on one node uses about 40 MB of RAM.
- **No Docker daemon.** Containers run on containerd with crun or runc. gVisor is used for sandboxed workloads.
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
| Build | Language detection for Node (npm/pnpm/yarn/bun, Next/Nuxt/SvelteKit/Remix/Astro/Vite/CRA/Angular/Nest/Express), Go, Python (Django/FastAPI/Flask), PHP (Laravel via FrankenPHP), Ruby (Rails), Rust, Java (Maven/Gradle), Deno and static HTML. Your own Dockerfile always wins. Build-time secrets never land in image layers |
| Run | Web services, workers, static sites (SPA fallback, gzip, immutable asset caching), cron jobs, PR preview deployments, persistent volumes |
| Operate | Zero-downtime rolling deploys, instant rollback, environment variables encrypted at rest, custom domains with automatic HTTPS (Let's Encrypt), live build and runtime logs, web console (exec), scaling, autoscaling (CPU / requests per second), metrics history, crash restarts and self-healing |
| Databases | Shared or dedicated PostgreSQL / MariaDB, plus Redis and MongoDB; linking injects `DATABASE_URL`; scheduled backups and restore; off-site copies to S3/R2/B2/MinIO; one-click upgrade from shared to dedicated |
| Density | QoS classes (guaranteed / burstable / best-effort), per-node overcommit, sleep mode, memory-pressure eviction (PSI), language runtime tuning, right-sizing, zram, image and build-cache GC |
| Cluster | `join` with one command, WireGuard mesh, internal DNS (`<service>.<project>.internal`), cross-node project isolation, internal image registry, cordon / drain, node health and rescheduling |
| Security | Sessions + API tokens, TOTP two-factor auth, team RBAC (owner/admin/developer/viewer), audit log, seccomp, no-new-privileges, per-IP rate limiting at the edge |
| Notify | Webhook, Slack, Discord and Telegram channels for deploys, crashes, node outages, failed backups and jobs |
| Interfaces | Web dashboard (embedded Svelte app), `paas` CLI, REST API |

## Install

**1. Control plane + first node** (Ubuntu/Debian or Fedora/RHEL, as root):

```bash
curl -fsSL https://raw.githubusercontent.com/thesunbg/paas/main/internal/installer/install.sh \
  | sudo sh -s -- server --domain apps.example.com --email you@example.com
```

Point DNS for `*.apps.example.com` and `paas.apps.example.com` at the server. Then open `https://paas.apps.example.com` and create the administrator account. The script installs:
- containerd 2, crun, runc, the CNI plugins and BuildKit;
- PostgreSQL for the control-plane state;
- zram and kernel tuning;
- a systemd unit for `paasd`.

**2. Add servers when load grows.** Copy the join command from *Nodes → Add node* (or `paas join-command`):

```bash
curl -fsSL https://paas.apps.example.com/install.sh | sudo sh -s -- join \
  --server nats://<control-plane>:4222 --token <cluster-token> --roles worker
```

Roles: `worker` (runs apps), `build` (runs BuildKit) and `edge` (runs the HTTPS proxy). Nodes need to reach each other on UDP 51820 (WireGuard) and the control plane on TCP 4222.

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

## High availability

Run several control planes against the same (highly available) PostgreSQL:

```bash
# on cp1 and cp2 (and cp3), in /etc/paas/paasd.env
PAAS_SERVER_NAME=cp1
PAAS_DATABASE_URL=postgres://paas:…@db.internal:5432/paas
PAAS_MASTER_KEY=<same hex key on every control plane>
PAAS_CLUSTER_TOKEN=<same token on every control plane>
PAAS_NATS_CLUSTER_ADDR=0.0.0.0:6222
PAAS_NATS_ROUTES=nats://cp2:6222,nats://cp3:6222
```

Agents and proxies list every control plane: `PAAS_NATS_URL=nats://cp1:4222,nats://cp2:4222,nats://cp3:4222`. Every control plane serves the API and the bus. One elected leader (a PostgreSQL advisory lock) runs the deploy queue, reconciler, cron, autoscaler and maintenance. When the leader dies, another one takes over within seconds, and running apps are never affected.

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
| `PAAS_S3_ENDPOINT`, `PAAS_S3_BUCKET`, `PAAS_S3_ACCESS_KEY`, `PAAS_S3_SECRET_KEY`, `PAAS_S3_REGION`, `PAAS_S3_PREFIX` | | off-site database backups |
| `PAAS_EVICT_PSI` / `PAAS_EVICT_MIN_FREE_MB` | `40` / `256` | memory-pressure eviction thresholds |
| `PAAS_SERVER_NAME`, `PAAS_NATS_CLUSTER_ADDR`, `PAAS_NATS_ROUTES`, `PAAS_MASTER_KEY`, `PAAS_CLUSTER_TOKEN` | | high availability (see above) |

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
