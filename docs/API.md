# REST API

Base URL: `http://<control-plane>:8080/api/v1` (the dashboard domain proxies to it too).

Authentication, any of:
- `Authorization: Bearer <token>` — a session token from `/auth/login` or an API token (`paas_…`) from `/tokens`.
- Cookie `paas_session` (set by login/register; used by the dashboard). Cookie-authenticated non-GET requests must be same-origin.
- `?access_token=<token>` query parameter, only on the `…/logs/stream` routes (for `EventSource`); other routes ignore it.

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
| GET/POST | `/services/{id}/volume-backups` | off-site archives of the service's volumes (`volume_backup_hours` in the spec: 0 = daily, negative = off); POST backs up now |
| POST | `/services/{id}/recover` | `{backup_id?, allow_empty}` admin, only while the service's node is offline: moves it to another node with its volumes from a backup (default the newest) |
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
Engines: `postgres`, `mysql` (MySQL 8.4), `mariadb`, `redis`, `mongo`. `shared` mode is available for postgres, mysql and mariadb. Before the split, `mysql` ran MariaDB: upgrading paasd renames those databases (and their shared server, `shared-mysql*` → `shared-mariadb*`) to `mariadb` in one migration. paasd restarts as a single binary on upgrade; do not run the old and new binaries side by side.

MySQL 8.4 authenticates with `caching_sha2_password` (`mysql_native_password` is off by default in 8.4 and the platform does not enable it). Clients that cannot use `caching_sha2_password` over a connection without TLS (for example an older PHP mysqlnd) may fail to connect to a plain `mysql://` URL: update the driver rather than weakening the server. MariaDB is unaffected.

| Method | Path | Notes |
|---|---|---|
| GET | `/databases/{id}` | `{database, version, version_status, server_version, image_digest?, image_pinned, connection: {host, port, database, username, password, url}, server, links}`. `server_version` is what the server reports (cached 10 min). `image_digest` is the digest of the platform manifest (linux/<arch>) the server runs; `image_pinned: false` means the server still follows its tag (its node does not report digests) |
| DELETE | `/databases/{id}` | |
| POST | `/databases/{id}/links` | `{service_id, env_name?, restart?}` |
| DELETE | `/databases/{id}/links/{serviceId}` | |
| GET/POST | `/databases/{id}/backups` | POST runs a backup now |
| POST | `/databases/{id}/backups/{backupId}/restore` | |
| POST | `/databases/{id}/backups/verify` | restores the newest backup into a scratch database, checks it, drops it (also runs weekly by itself) |
| POST | `/databases/{id}/recover` | `{allow_empty}` admin, only while the server's node is offline: a new server on another node from the newest off-site dump. Shared servers: platform admin, recreates the server and restores every database on it. 202 |
| POST | `/databases/{id}/upgrade` | `{size?}` shared → dedicated |
| POST | `/databases/{id}/version` | `{version, confirm_downtime}` → 202, status `upgrading`. One-way major upgrade, see below |
| POST | `/databases/{id}/update-minor` | `{confirm_shared?}` → 202, status `upgrading`. See below |
| POST | `/databases/{id}/retry` | retry failed provisioning |

### Image pinning and minor updates

A database server runs a tag (`postgres:17`) that each node freezes at its first pull. Once a node reports the digest of the manifest a server runs, the service is pinned to `<tag>@sha256:<digest>`: redeploys, reschedules to another node and new standbys all run that exact image. A node that does not report digests (an older agent) leaves the server on its tag (`image_pinned: false`).

`POST /databases/{id}/update-minor` moves the server to the newest build of the tag it already runs (17 → 17.x); it never changes the major version. In order: a backup is taken and pinned (retention keeps it until the database is deleted), the node pulls the tag again and, only if that succeeds, the server is stopped and started on the new image (the server is unavailable for the restart: a stateful service is stopped before it is started), then the new digest is written to the service and the cached `server_version` is dropped. A failed pull aborts before anything is stopped. Errors of the background job are in `database.error`.

- Dedicated: team `admin` (same as `upgrade`). Shared: platform administrators only (it restarts every tenant's database on that server), and `{"confirm_shared": true}` is required (400 otherwise); 409 while another database on that server is `upgrading`.
- 409 for a database with HA on (turn HA off first) or one that is not `ready`.
- There is no rollback: the pinned backup is the only way back (restore it into a new database of the same version). Updating a server whose node runs an agent older than this feature fails with "agent is too old" before touching anything.
- Audit event `database.update_minor`.

### Major upgrade

`POST /databases/{id}/version {"version": "18", "confirm_downtime": true}` moves a database to a newer version of its engine (PostgreSQL, MariaDB, MySQL, MongoDB). It is **one way**: there is no rollback, the old database is deleted at the end. It is a dump and restore into a new server, so it needs downtime for the services that use the database.

1. The services linked to the database are stopped and the database is set read-only (Mongo has no read-only switch: only the stop).
2. A backup is taken (after the stop) and pinned: retention keeps it until the database is deleted.
3. The target is created: shared database, the shared server of the new version plus the logical database; dedicated database, a new server `<name>-v<version>` (the old one keeps running).
4. The backup is restored into it (PostgreSQL without ACLs, which the extra users get again from the replay below; Mongo without `admin.*`) and the extra users are created again.
5. Verification: same tables (collections) and the same row counts (the 200 biggest tables), and every extra user can log in. A PostgreSQL database that uses an extension the new image lacks is refused before anything is touched when the target shared server already exists, otherwise right after the target is created (before the restore).
6. The flip: `server_service_id` and `version` change in one compare-and-swap. The linked services start again, one by one, with the new `DATABASE_URL`.
7. The old logical database (shared) or the old server and its volume (dedicated) is removed at once.

A failure before the flip deletes the target, allows writes again and starts the linked services again: the database stays on its old server, `ready`, with the reason in `error`. `databases.move_state` records progress, so a restart of paasd does the same (before the flip) or finishes steps 6-7 (after it). While a move runs the database is `upgrading` and refuses HA, PITR, import, resize, manual and scheduled backups, restore, update-minor and delete, and the HA monitor skips it.

- Team `admin` (same as `upgrade`). Audit event `database.version`.
- 400: `confirm_downtime` missing, or a `version` that is not an exact catalog name newer than the current one (only upgrades, in catalog order). 409: target past its end of life, Redis (no restore), HA, PITR or the pooler on (turn them off, upgrade, turn them on), or the database is not `ready`.
- Extra caveat: a PostgreSQL database loses grants made by hand to roles other than its own extra users.

Database `status`: `provisioning`, `ready`, `failed`, `upgrading`, `deleting`.

## Cluster (administrators)

| Method | Path | Notes |
|---|---|---|
| GET | `/nodes` | `{nodes: [node + legacy (still on the shared cluster token), committed_memory_mb, capacity_memory_mb, instances, used_by_apps_mb, overcommit_ratio], sleeping_services}` |
| PATCH | `/nodes/{id}` | `{status: online|cordoned, overcommit_memory, overcommit_cpu, reserved_mb, labels}` |
| DELETE | `/nodes/{id}` | offline/cordoned nodes only. 409 with `stuck` when services or databases still keep data there; `?force=true` removes it anyway (they restart empty) |
| GET | `/nodes/{id}/stuck` | `[{kind: service|database|shared-database, id, name, project, backup}]` data that lives only on this node, with its newest off-site backup |
| POST | `/cluster/join-tokens` | `{roles: [worker,build,edge]}` (default `worker`) -> `{token, expires_at, command}`. Single-use token. 409 when `PAAS_PUBLIC_URL` is not https |
| POST | `/cluster/enroll` | no login, the join token is the credential. `{token, name, nkey}` -> node identity, bus URL and CA. Called by the agent |
| GET | `/sizes` | instance size presets |

## Webhooks

`POST /webhooks/github/{serviceId}` — configure it in GitHub (content type `application/json`, secret = the service `webhook_secret`, event `push`). Pushes to the service branch deploy automatically; `root_dir` limits deploys to changes under that folder.

## Databases as a service

| Method | Path | Notes |
|---|---|---|
| POST | `/api/v1/databases/{id}/public` | `{enabled, allowed_cidrs}`: external access on `db.<base domain>:<port>` (ports `PAAS_DB_PORT_MIN`–`MAX`, default 20000–20999). PostgreSQL TLS via SSLRequest (`sslmode=require`), Redis `rediss://`, MongoDB `tls=true` when HTTPS is on |
| POST | `/api/v1/projects/{id}/databases` | `version` picks the engine version, matched exactly against `/api/v1/sizes` → `engines.<engine>.versions` (unknown → 400). A version past its end of life → 409; one within 180 days of it is created and the 201 body carries `warning`. Shared databases of one version share a server |
| GET | `/api/v1/databases/{id}/metrics?hours=24` | size and connections per minute (7 days), quota, read-only flag |
| PUT | `/api/v1/databases/{id}/quota` | administrators: `{quota_mb}` (0 = default: shared 1 GB, dedicated by size). Over quota PostgreSQL is read-only, MySQL and MariaDB lose write privileges |
| POST | `/api/v1/databases/{id}/pooler` | `{enabled}` PgBouncer (transaction mode); linked services get `<ENV>_POOLED` |
| PUT | `/api/v1/databases/{id}/size` | `{size}` resize a dedicated server in place |
| GET/POST | `/api/v1/databases/{id}/users` | extra users `{name, role: readonly\|readwrite}` (PostgreSQL, MySQL, MariaDB) |
| DELETE | `/api/v1/databases/{id}/users/{user}` | |
| POST | `/api/v1/databases/{id}/users/{user}/password` | rotate |
| GET/POST | `/api/v1/databases/{id}/pitr` | `{enabled, retention_days}` WAL archiving + daily base backups (dedicated PostgreSQL) |
| POST | `/api/v1/databases/{id}/pitr/restore` | `{target_time?, name?}` restore into a new database |
| GET/POST | `/api/v1/databases/{id}/ha` | `{enabled}` hot standby on another node, automatic failover ~45–60 s after the primary's node stops responding |
| POST | `/api/v1/databases/{id}/ha/switchover` | planned switchover to the standby |

## Accounts (administrators)

| Method | Path | Notes |
|---|---|---|
| GET | `/api/v1/users` | All accounts with team counts |
| POST | `/api/v1/users` | `{email, name?, password?, is_admin?}`; without a password one is generated and returned once |
| PATCH | `/api/v1/users/{id}` | `{name?, is_admin?, password?, reset_password?, disable_2fa?}` |
| DELETE | `/api/v1/users/{id}` | |

## Sign in with GitHub

Uses the GitHub App's user authorization (no separate OAuth app). `GET /api/v1/auth/github` starts it;
GitHub returns to the app callback URL. Accounts are matched by linked GitHub id, then by verified email;
otherwise a new account is created when sign-up is open. Accounts with 2FA finish with
`POST /api/v1/auth/github/2fa {pending, code}`. Signed-in users link with `GET /api/v1/me/github/link`
and unlink with `DELETE /api/v1/me/github`.

## Sign-up and team limits (administrators)

| Method | Path | Notes |
|---|---|---|
| GET | `/api/v1/admin/settings` | `{allow_signup, limits}` |
| GET/PUT | `/api/v1/admin/storage` | off-site S3 storage `{endpoint, region, bucket, access_key, secret_key, prefix, virtual_host}`; PUT tests it first, an empty endpoint turns it off. `PAAS_S3_*` env wins (`from_env`) |
| GET/POST | `/api/v1/admin/state-backups` | platform state (control-plane database) backups: list `{last, backups, offsite_error, every}` / back up now |
| GET | `/api/v1/admin/state-backups/{name}` | download a sealed backup (restore with `paasd restore-state --file`) |
| PUT | `/api/v1/admin/settings` | `{allow_signup?, limits?: {max_projects, max_services, max_databases, max_memory_mb, max_replicas, max_signups_per_ip_per_day}}` (0 = unlimited) |

With sign-up on, anyone can register at `/#/signup` and gets a personal team. Limits apply to every
non-administrator; requests over a limit return 403, too many sign-ups from one IP return 429.

## GitHub App (Vercel-style integration)

One GitHub App per installation of paas, created from a manifest; teams install it on
their GitHub accounts/organizations and pick repositories from a list.

| Method | Path | Notes |
|---|---|---|
| GET | `/api/v1/github/app` | Status of the app (`configured`, `slug`, `html_url`) |
| POST | `/api/v1/github/app/manifest` | Admin. `{organization?}` → `{action_url, manifest}`; the browser POSTs `manifest` as a form field to `action_url` |
| GET | `/api/v1/github/app/callback` | GitHub redirects here after creating the app (stores its keys encrypted) |
| DELETE | `/api/v1/github/app` | Admin. Forget the app |
| GET | `/api/v1/teams/{team}/github` | Connected installations and a signed `install_url` |
| GET | `/api/v1/github/installed` | GitHub redirects here after installing; the OAuth code proves the user can access the installation |
| GET | `/api/v1/teams/{team}/github/repos` | Repositories granted to the team's installations |
| GET | `/api/v1/teams/{team}/github/branches?installation_id=&repo=owner/name` | Branches |
| DELETE | `/api/v1/teams/{team}/github/installations/{id}` | Team admin. Unlink |
| POST | `/api/v1/webhooks/github-app` | App webhook (push, pull_request, installation), HMAC-verified |
| POST | `/api/v1/projects/{id}/detect` | `{repo, branch, root_dir?, installation_id? \| git_token?}` → detected language, framework, suggested type and port |

A service created with `source.installation_id` clones with short-lived installation tokens and
receives pushes and pull requests through the app webhook (no per-repository webhook needed).

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
- `GET /sizes` also returns `database_sizes` (presets with `Name`, `MemoryMB`, `CPU`, `MaxConns`) and `engines`; each engine lists `versions` as `{name, eol, status}` (`status` is `ok`, `ending` or `eol`; no `eol` date means none announced). `GET /databases/{id}` returns `version_status` and `version_eol`. The `database.eol` notification event fires once per database and status (`ending`, then `eol`).
- Backups have `s3_key` when copied off-site (`PAAS_S3_*` settings); restoring on another node downloads from S3.
- SSE deployment streams send `id:` per line and honor `Last-Event-ID`.
- Viewers can read env var names (values masked).
