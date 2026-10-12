# REST API

Base URL: `http://<control-plane>:8080/api/v1` (the dashboard domain proxies to it too).

Authentication, any of:
- `Authorization: Bearer <token>` — a session token from `/auth/login` or an API token (`paas_…`) from `/tokens`.
- Cookie `paas_session` (set by login/register; used by the dashboard). Cookie-authenticated non-GET requests must be same-origin.
- `?access_token=<token>` query parameter (for `EventSource` streams).

Errors are `{"error": "message"}` with a 4xx/5xx status.

## Auth and users

| Method | Path | Body | Response |
|---|---|---|---|
| GET | `/setup` | | `{needs_setup, allow_signup, base_domain}` (no auth) |
| POST | `/auth/register` | `{email, password, name}` | `{user, token}`. The first user becomes administrator; later signups need `allow_signup` |
| POST | `/auth/login` | `{email, password}` | `{user, token}` |
| POST | `/auth/logout` | | |
| GET | `/me` | | `{user, teams: [{id, name, slug, role}]}` |
| POST | `/me/password` | `{current, new}` | |
| GET/POST | `/tokens` | `{name}` | POST returns `{token, info}` once |
| DELETE | `/tokens/{id}` | | |
| GET | `/overview` | | `{projects, services, running, sleeping, databases, nodes?}` |

## Teams

| Method | Path | Body |
|---|---|---|
| GET/POST | `/teams` | `{name}` |
| GET/POST | `/teams/{team}/members` | `{email, role}` (role: admin, developer, viewer). Unknown emails get an account; the response contains `temporary_password` |
| DELETE | `/teams/{team}/members/{user}` | |
| GET/POST | `/teams/{team}/projects` | `{name}`. GET items include `services`, `running`, `databases` counts |

Roles: `viewer` (read) < `developer` (deploy, configure) < `admin` (delete, restore, members) < `owner`.

## Projects

| Method | Path | Notes |
|---|---|---|
| GET | `/projects/{id}` | `{project, services: [service + url, instances, memory_mb], databases}` |
| DELETE | `/projects/{id}` | deletes all services and databases |
| POST | `/projects/{id}/services` | create a service (below) |
| POST | `/projects/{id}/databases` | create a database (below) |

## Services

Create body:

```json
{
  "name": "api",
  "type": "web",                       // web | worker | static
  "source": {"repo": "https://github.com/me/app", "branch": "main", "root_dir": "", "dockerfile": ""},
  // or "source": {"image": "ghcr.io/me/app:1.2"}
  "build": {"builder": "", "install_cmd": "", "build_cmd": "", "start_cmd": "", "output_dir": ""},
  "size": "small",                     // nano | micro | small | medium | large | xlarge
  "qos": "burstable",                  // guaranteed | burstable | best-effort
  "port": 8080,
  "replicas": 1,
  "sleep": {"enabled": true, "idle_minutes": 15},
  "health_check": {"type": "http", "path": "/healthz", "timeout_sec": 60},
  "runtime_class": "standard",         // standard (crun/runc) | sandboxed (gVisor)
  "volumes": [{"name": "data", "path": "/data"}],
  "env": {"KEY": "value"},
  "git_token": "ghp_…",                // private repos + GitHub commit statuses
  "domain": "app.example.com",
  "auto_deploy": true,                 // deploy on push webhooks
  "deploy": true                       // queue the first deployment immediately
}
```

| Method | Path | Notes |
|---|---|---|
| GET | `/services/{id}` | `{service, project, domains, urls, instances, deployments, databases, webhook_url, webhook_secret, internal_host, recommendation, tuning_env}` |
| PATCH | `/services/{id}` | same fields as create (partial). Runtime changes trigger a rolling restart |
| DELETE | `/services/{id}` | |
| POST | `/services/{id}/deploy` | `{commit_sha?}` → deployment (202) |
| POST | `/services/{id}/restart` | rolls the current artifact again |
| POST | `/services/{id}/stop`, `/start` | |
| POST | `/services/{id}/scale` | `{replicas}` |
| GET | `/services/{id}/deployments` | latest 50 |
| GET/PUT | `/services/{id}/env` | GET → `{vars, linked}`. PUT `{vars: {K: V}, replace?: bool, restart?: bool}` |
| DELETE | `/services/{id}/env/{key}` | |
| GET/POST | `/services/{id}/domains` | `{hostname}` |
| DELETE | `/services/{id}/domains/{domainId}` | |
| GET | `/services/{id}/logs?tail=300` | `[{instance, lines: []}]` runtime logs |
| GET | `/services/{id}/logs/stream` | Server-Sent Events, `event: log` |
| POST | `/services/{id}/exec` | `{command}` → `{exit_code, stdout, stderr}` |

Service `status`: `created`, `deploying`, `running`, `sleeping`, `stopped`, `failed`.

## Deployments

| Method | Path | Notes |
|---|---|---|
| GET | `/deployments/{id}` | `{deployment, service}` |
| GET | `/deployments/{id}/logs?after=<id>` | `[{id, ts, line}]` build + deploy log |
| GET | `/deployments/{id}/logs/stream` | SSE: `event: log` lines, final `event: done` with the status |
| POST | `/deployments/{id}/rollback` | redeploys that artifact |
| POST | `/deployments/{id}/cancel` | queued only |

Deployment `status`: `queued`, `building`, `deploying`, `active`, `superseded`, `failed`, `cancelled`.

## Databases

Create body: `{"name": "main", "engine": "postgres", "mode": "shared", "size": "micro", "link_services": ["<service id>"], "env_name": "DATABASE_URL", "backup_schedule_hours": 24}`.
Engines: `postgres`, `mysql` (MariaDB), `redis`, `mongo`. `shared` mode is available for postgres and mysql.

| Method | Path | Notes |
|---|---|---|
| GET | `/databases/{id}` | `{database, connection: {host, port, database, username, password, url}, server, links}` |
| DELETE | `/databases/{id}` | |
| POST | `/databases/{id}/links` | `{service_id, env_name?, restart?}` |
| DELETE | `/databases/{id}/links/{serviceId}` | |
| GET/POST | `/databases/{id}/backups` | POST runs a backup now |
| POST | `/databases/{id}/backups/{backupId}/restore` | |
| POST | `/databases/{id}/upgrade` | `{size?}` shared → dedicated |
| POST | `/databases/{id}/retry` | retry failed provisioning |

Database `status`: `provisioning`, `ready`, `failed`, `upgrading`, `deleting`.

## Cluster (administrators)

| Method | Path | Notes |
|---|---|---|
| GET | `/nodes` | `{nodes: [node + committed_memory_mb, capacity_memory_mb, instances, used_by_apps_mb, overcommit_ratio], sleeping_services}` |
| PATCH | `/nodes/{id}` | `{status: online|cordoned, overcommit_memory, overcommit_cpu, reserved_mb, labels}` |
| DELETE | `/nodes/{id}` | offline/cordoned nodes only |
| GET | `/cluster/join` | `{nats_url, token, command}` |
| GET | `/sizes` | instance size presets |

## Webhooks

`POST /webhooks/github/{serviceId}` — configure it in GitHub (content type `application/json`, secret = the service `webhook_secret`, event `push`). Pushes to the service branch deploy automatically; `root_dir` limits deploys to changes under that folder.

## Advanced features

### Service fields (create / PATCH)
- `"type": "cron"` with `"schedule": "*/15 * * * *"` (UTC, 5 fields or `@daily`/`@hourly`…), `"job_timeout_sec": 3600`, `"command": [...]`. Cron services get no domain and no replicas.
- `"previews": true` — every pull request targeting the service branch gets its own service (`<slug>-pr-<n>`, URL `<slug>-pr-<n>-<project>.<base>`), sleeping when idle, deleted when the PR closes. Needs the GitHub webhook with the `pull_request` event.
- `"autoscale": {"enabled": true, "min": 1, "max": 5, "target_cpu_percent": 70, "target_rps": 50}`.
- `GET /services/{id}` additionally returns `previews` (list of services with `parent_id`/`preview_pr`) and `next_run` (cron).
- Services have `parent_id` and `preview_pr` (null for normal services).

| Method | Path | Notes |
|---|---|---|
| POST | `/services/{id}/run` | start a cron job now → job run (202) |
| GET | `/services/{id}/runs` | `[{id, status: running|succeeded|failed, exit_code, output, trigger: schedule|manual, started_at, finished_at}]` |
| GET | `/services/{id}/metrics?hours=6` | `{points: [{ts, instances, memory_mb, cpu_millis, requests, errors_5xx}], rps_5m, errors_5m}` (one point per minute) |
| GET | `/nodes/{id}/metrics` | `[{ts, cpu_percent, memory_used_mb, memory_committed_mb}]` (24 h) |

### Templates and Docker Compose
| Method | Path | Notes |
|---|---|---|
| GET | `/templates` | `[{id, title, description, category, services, databases}]` |
| POST | `/projects/{id}/templates/{templateId}` | `{domain?}` → `{services, databases}`; databases are provisioned first |
| POST | `/projects/{id}/compose` | `{compose: "<yaml>"}` → `{services, warnings}`. Image services only; named volumes kept; services reach each other by compose name |

### Notifications (per team)
| Method | Path | Notes |
|---|---|---|
| GET | `/teams/{team}/notifications` | `{channels: [{id, name, kind, events}], events: [all event names]}` |
| POST | `/teams/{team}/notifications` | `{name, kind: webhook|slack|discord|telegram, target, events}`; telegram target is `<bot token>:<chat id>` |
| DELETE | `/teams/{team}/notifications/{id}` | |
| POST | `/teams/{team}/notifications/{id}/test` | sends a test message |

Events: `deploy.succeeded`, `deploy.failed`, `instance.crashloop`, `instance.evicted`, `node.offline`, `backup.failed`, `job.failed`, `autoscale`.

### Two-factor authentication
| Method | Path | Notes |
|---|---|---|
| POST | `/me/2fa/setup` | `{secret, otpauth_url}` (not active until enabled) |
| POST | `/me/2fa/enable` | `{code}` |
| POST | `/me/2fa/disable` | `{code, password}` |

When enabled, `/auth/login` answers `401 {"error": "two-factor code required", "two_factor": true}` until the body includes `"code"`. `user.totp_enabled` tells whether it is on.

### Other
- `GET /sizes` also returns `database_sizes` (presets with `Name`, `MemoryMB`, `CPU`, `MaxConns`) and `engines`.
- Backups have `s3_key` when copied off-site (`PAAS_S3_*` settings); restoring on another node downloads from S3.
- SSE deployment streams send `id:` per line and honor `Last-Event-ID`.
- Viewers can read env var names (values masked).
