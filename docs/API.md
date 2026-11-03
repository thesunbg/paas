# REST API

Base URL: `http://<control-plane>:8080/api/v1` (the dashboard domain proxies to it too).

Authentication, any of:
- `Authorization: Bearer <token>` — a session token from `/auth/login` or an API token (`paas_…`) from `/tokens`.
- Cookie `paas_session` (set by login/register; used by the dashboard). Cookie-authenticated non-GET requests must be same-origin.
- `?access_token=<token>` query parameter, only on the `…/logs/stream` routes (for `EventSource`); other routes ignore it.

Errors are `{"error": "message"}` with a 4xx/5xx status.

AI agents can also use the [MCP endpoint](MCP.md) at `POST /mcp` (same API tokens, same permissions).

## Auth and users

| Method | Path | Body | Response |
|---|---|---|---|
| GET | `/setup` | | `{needs_setup, allow_signup, base_domain, github_login, sso: [{slug, name}]}` (no auth) |
| POST | `/auth/register` | `{email, password, name}` | `{user, token}`. The first user becomes administrator; later signups need `allow_signup` |
| POST | `/auth/login` | `{email, password}` | `{user, token}`. 403 `{error, sso}` when single sign-on is enforced for the email's domain |
| POST | `/auth/logout` | | |
| GET | `/me` | | `{user, teams: [{id, name, slug, role}], setup_pending?}`; `setup_pending` (administrators only) is true until the setup wizard is finished or skipped (an instance that already ran apps before the wizard existed starts with it skipped) |
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
| GET | `/teams/{team}/git-credentials` | viewer+. `{credentials: [{id, provider, host, username, name, created_at, updated_at}], providers}`; tokens are never returned |
| POST | `/teams/{team}/git-credentials` | admin+. `{provider: gitlab\|gitea\|bitbucket\|github, host?, username?, token, name?}` → 201. One credential per host (saving again replaces it). `host` defaults to gitlab.com / bitbucket.org / github.com and is required for gitea; https only, no path |
| DELETE | `/teams/{team}/git-credentials/{id}` | admin+ |

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
| GET | `/services/{id}` | `{service, project, domains, urls, instances, deployments, databases, webhook_url, webhook_secret, git_provider, internal_host, recommendation, tuning_env}`. `webhook_url` is the endpoint for the repository's provider |
| GET | `/services/{id}/webhook` | developer+. `{provider, url, secret, urls: {github, gitlab, gitea, bitbucket}}` |
| PATCH | `/services/{id}` | same fields as create (partial). Runtime changes trigger a rolling restart |
| DELETE | `/services/{id}` | |
| POST | `/services/{id}/deploy` | `{commit_sha?}` → deployment (202) |
| POST | `/services/{id}/restart` | rolls the current artifact again |
| POST | `/services/{id}/stop`, `/start` | |
| POST | `/services/{id}/scale` | `{replicas}` |
| GET/POST | `/services/{id}/volume-backups` | off-site archives of the service's volumes (`volume_backup_hours` in the spec: 0 = daily, negative = off); POST backs up now |
| POST | `/services/{id}/recover` | `{backup_id?, allow_empty, node?}` admin, only while the service's node is unreachable (offline, or no heartbeat for a minute; a cordoned node that still reports is not): restores it on another node (`node`: id or name, default the online worker with the most free memory) from a volume backup (default the newest off-site one), or with empty volumes. 202 `{node, backup, move, rpo_at}`: `rpo_at` is the backup's time, writes after it are lost. Runs in the background as a `restore` in `GET /services/{id}/move` |
| GET | `/services/{id}/move` | viewer. `{active, history: [move], movable, reason, node: {id, name, online}, targets: [{id, name, memory_available_mb}]}` (`targets` only for team admins). A move is `{id, kind: move|restore, from_node, to_node, phase: pending|stopping|copying|verifying|switching|starting|done|failed|aborted, prev_status, abort_requested, backup_id, rpo_at, bytes, files, digest, retain_days, error, requested_by, created_at, updated_at, finished_at}` |
| POST | `/services/{id}/move` | `{node?, retain_days?, confirm_downtime: true}` admin: moves a service with volumes to another online worker (default: most free memory). The service stops, its volumes stream from node to node over the WireGuard mesh (one-time token, mesh addresses only), are checked (bytes, entries, SHA-256 of the tree), it starts on the new node and routes follow; downtime ≈ copy + check + start. Any failure before the switch (or no start on the new node within 5 min) leaves it running on the old node. The old copy is removed `retain_days` (default 7, max 90) after success. 202 move; 409 when one is running, the node is offline (use `recover`), or for databases, platform and preview services |
| DELETE | `/services/{id}/move` | admin: aborts the move in progress (only before the switch; 409 after). 202; the service starts again on its node |
| GET | `/services/{id}/deployments` | latest 50 |
| GET/PUT | `/services/{id}/env` | GET → `{vars, linked}`. PUT `{vars: {K: V}, replace?: bool, restart?: bool}` |
| DELETE | `/services/{id}/env/{key}` | |
| GET/POST | `/services/{id}/domains` | `{hostname}` |
| DELETE | `/services/{id}/domains/{domainId}` | |
| GET | `/services/{id}/logs?tail=300` | `[{instance, lines: []}]` runtime logs |
| GET | `/services/{id}/logs/stream` | Server-Sent Events, `event: log` |
| POST | `/services/{id}/exec` | `{command}` → `{exit_code, stdout, stderr}` |
| GET | `/services/{id}/terminal?instance=&cols=&rows=` | WebSocket: interactive shell with a terminal in a running instance (developer). Binary frames are keystrokes/output, text frames JSON `{cols, rows}` (resize) or `{exit}`/`{error}`. Same-origin only; audited as `service.terminal` |

Service `status`: `created`, `deploying`, `running`, `sleeping`, `stopped`, `failed`.

## Deployments

| Method | Path | Notes |
|---|---|---|
| GET | `/deployments/{id}` | `{deployment, service}` |
| GET | `/deployments/{id}/logs?after=<id>` | `[{id, ts, line}]` build + deploy log |
| GET | `/deployments/{id}/logs/stream` | SSE: `event: log` lines, final `event: done` with the status |
| POST | `/deployments/{id}/rollback` | redeploys that artifact |
| POST | `/deployments/{id}/cancel` | queued only |
| POST | `/deployments/{id}/diagnose` | developer+. `{refresh?}` → `{summary, likely_cause, suggested_fixes[], provider, model, created_at, cached}`: asks the configured AI provider to explain the failure from the redacted log tail, app output and error. Suggestions only. Cached per deployment (`refresh` asks again); 400 when no provider is set, 409 while the deployment runs, 429 over 30 per user per hour, 504 when the model times out |
| GET | `/deployments/{id}/diagnosis` | `{enabled, diagnosis}`: whether AI diagnosis is configured and the cached answer (or `null`) |

Deployment `status`: `queued`, `building`, `deploying`, `active`, `superseded`, `failed`, `cancelled`.

### Vulnerability scans

| Method | Path | Notes |
|---|---|---|
| GET | `/deployments/{id}/scan` | viewer. The deployment's scan, 404 when it has none (static sites, rollbacks/restarts, policy `off`) |
| GET | `/services/{id}/scan` | viewer. `{scan: <newest scan or null>, policy: {mode, threshold}}` |
| GET | `/projects/{id}/scan-policy` | viewer. `{mode, threshold}`; default `{"mode":"warn","threshold":"CRITICAL"}` |
| PUT | `/projects/{id}/scan-policy` | team admin. `{mode: off|warn|block, threshold: CRITICAL|HIGH|MEDIUM|LOW}` |

A scan: `{deployment_id, service_id, image, status: scanned|unavailable, scanner, scanner_version, db_updated_at, os, critical, high, medium, low, unknown, findings: [{id, severity, pkg, installed, fixed, title, target, url}], policy_mode, threshold, blocked, message, scanned_at}`. `findings` holds the 200 most severe (fixable first within a severity); the counts cover everything, a CVE found twice in the same package version counting once.

How it runs: after a build, the build node exports the image from containerd to a local archive and runs `trivy image --input <archive> --format json --scanners vuln --skip-db-update --offline-scan`. For a Docker-image deployment a build node first pulls the image the way a worker would. Trivy never contacts a registry and nothing from the image is executed. Its DB is cached in `<data dir>/trivy` and refreshed (`trivy image --download-db-only`) when older than 12 hours, so a scan on a fresh cache adds only the export and the analysis; when a refresh fails the cached copy is used and retried an hour later. Scans on one node run one at a time.

Policy: `off` skips the scan. `warn` (default) records the result and deploys; findings at or above the threshold send `deploy.vulnerable`. `block` fails the deployment before any instance starts (log line `blocked by the vulnerability policy: N finding(s) at or above HIGH (...)`), sends `deploy.vulnerable`, and forgets the deployment's image so it cannot be rolled back to. When no scan can be made (Trivy not installed on the build node, an agent too old to scan, no online build node, scan error), `warn` deploys with `vulnerability scan skipped: <why>` and `block` fails closed with `vulnerability scan is required by the project policy (block) but could not run: <why>`. UNKNOWN severities never count towards the threshold. Rollbacks and restarts reuse an artifact and are not scanned again.

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
| GET | `/nodes` | `{nodes: [node + legacy (still on the shared cluster token), ssh_host_key (when installed over SSH), committed_memory_mb, capacity_memory_mb, instances, used_by_apps_mb, overcommit_ratio], sleeping_services}` |
| PATCH | `/nodes/{id}` | `{status: online|cordoned, overcommit_memory, overcommit_cpu, reserved_mb, labels}` |
| DELETE | `/nodes/{id}` | offline/cordoned nodes only. 409 with `stuck` when services or databases still keep data there; `?force=true` removes it anyway (they restart empty) |
| GET | `/nodes/{id}/stuck` | `[{kind: service|database|shared-database, id, name, project, backup}]` data that lives only on this node, with its newest off-site backup |
| POST | `/nodes/{id}/drain` | `{move_volumes?}` cordons the node and moves its stateless instances without downtime. With `move_volumes`, every service with volumes on it is also moved (as `POST /services/{id}/move`, one at a time); database servers stay. 202 `{status, moving, pinned_stateful, moving_stateful}` |
| POST | `/nodes/{id}/recover` | `{node?, allow_empty, confirm_data_loss: true}` node unreachable (as above): restores every service with volumes of the node on other nodes from its newest off-site volume backup (as `POST /services/{id}/recover`, one at a time). 202 `{started: [{service, id, move, node, rpo_at}], skipped: [{kind, name, id, reason}]}`; databases are skipped (they have `POST /databases/{id}/recover`), so are services without a backup unless `allow_empty` |
| POST | `/cluster/join-tokens` | `{roles: [worker,build,edge]}` (default `worker`) -> `{token, expires_at, command}`. Single-use token. 409 when `PAAS_PUBLIC_URL` is not https |
| POST | `/cluster/ssh-host-key` | `{host, port}` -> `{address, ips, key_type, fingerprint (SHA256:...), host_key (authorized_keys line), control_plane}`. Reads the SSH host key without logging in, for the administrator to confirm. 30 per 15 min per administrator |
| POST | `/cluster/ssh-installs` | `{host, port (22), user (root), auth: key|password, private_key?, passphrase?, password? (login password, or the sudo password with a key), roles, name?, fingerprint (required: the confirmed host key), reinstall?, preflight_only?, allow_control_plane?}` -> 202 attempt. Installs a node over SSH in the background: connect (host key must match `fingerprint`, checked before authentication), preflight, `install.sh join` with a fresh single-use join token, wait until the node is online (steps `connect, preflight, install, enroll, done`). Credentials are used for this attempt only and never stored; they are masked in the log. 400 when the host is this control plane unless `allow_control_plane`; 409 when `PAAS_PUBLIC_URL` is not https; 429 after 10 starts per 15 min per administrator or 5 running per control plane |
| GET | `/cluster/ssh-installs` | the 50 latest attempts: `{id, host, port, user, auth_method, roles, node_name, host_key, host_key_fingerprint, status: running|succeeded|failed, step, preflight: {checks: [{id, title, status: ok|warn|fail, detail}], hostname, os, arch, memory_mb, disk_free_mb, ips}, error, node_id, server_name, created_by_email, created_at, finished_at}` |
| GET | `/cluster/ssh-installs/{id}` | one attempt plus `log_tail` (last 30 lines) |
| GET | `/cluster/ssh-installs/{id}/logs` | `?after=ID` -> `[{id, ts, line}]` |
| GET | `/cluster/ssh-installs/{id}/logs/stream` | SSE: `log` events (with ids; `Last-Event-ID` resumes), then `done` with the final status |
| POST | `/cluster/ssh-installs/{id}/cancel` | stops a running attempt (on the control plane running it); its unused join token is revoked |
| POST | `/cluster/enroll` | no login, the join token is the credential. `{token, name, nkey}` -> node identity, bus URL and CA. Called by the agent |
| GET | `/sizes` | instance size presets |

## Webhooks

`POST /webhooks/github/{serviceId}` — configure it in GitHub (content type `application/json`, secret = the service `webhook_secret`, event `push`). Pushes to the service branch deploy automatically; `root_dir` limits deploys to changes under that folder.

The same push and preview behaviour for other hosts, all with the service `webhook_secret`:

| Path | Provider | Verification | Events |
|---|---|---|---|
| `POST /webhooks/gitlab/{serviceId}` | GitLab (gitlab.com or self-hosted) | `X-Gitlab-Token` equals the secret | `Push Hook`, `Merge Request Hook` |
| `POST /webhooks/gitea/{serviceId}` | Gitea / Forgejo | `X-Gitea-Signature` or `X-Forgejo-Signature`: hex HMAC-SHA256 of the body | `push`, `pull_request` |
| `POST /webhooks/bitbucket/{serviceId}` | Bitbucket Cloud | `X-Hub-Signature: sha256=<hex HMAC>` (the secret is mandatory) | `repo:push`, `pullrequest:created/updated/fulfilled/rejected` |

Pull / merge requests from forks never get previews. Bitbucket pushes carry no file lists, so `root_dir` does not filter them.

**Private repositories on other hosts.** A team stores one access token per git host (`/teams/{team}/git-credentials`, encrypted at rest). Builds of the team's services whose repository is `https://<that host>/…` clone with it; it is never sent to another host, over http, or for a URL that carries credentials. A service's own `git_token` takes precedence. The same credential reports commit statuses (GitLab, Gitea, Bitbucket Cloud, GitHub) and posts the preview URL on the pull / merge request; give the token write access to statuses for that (GitLab `api`, Gitea write:repository, Bitbucket Repositories: Write). Clone user: `oauth2` for GitLab, `x-token-auth` for Bitbucket access tokens (set `username` for an app password), `x-access-token` otherwise. Build nodes only clone from public addresses, so a self-hosted host must resolve to a public IP.

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

## Single sign-on (OpenID Connect)

Instance administrators configure providers; every provider uses the same redirect URI,
`<public URL>/api/v1/auth/oidc/callback`. Sign-in is an Authorization Code flow with PKCE (S256), a
single-use `state` bound to the browser by a cookie, and a `nonce`; the ID token's signature (JWKS,
refetched on key rotation), `iss`, `aud`, `azp`, `exp` and `nonce` are checked.

| Method | Path | Notes |
|---|---|---|
| GET | `/api/v1/auth/oidc/{slug}` | starts a sign-in (no auth); the callback redirects to `/#/` with a session cookie, or to `/#/login?sso_error=…` / `?sso_2fa=<pending>` |
| POST | `/api/v1/auth/2fa` | `{pending, code}` finishes an SSO or GitHub sign-in when the account has 2FA and the provider does not skip it |
| GET | `/api/v1/me/oidc` | providers linked to the signed-in account `[{provider_id, provider_slug, provider_name, email, created_at}]` |
| GET | `/api/v1/me/oidc/{slug}/link` | links the provider to the signed-in account (browser redirect) |
| DELETE | `/api/v1/me/oidc/{slug}` | unlinks it (refused when it is the only way in under enforced SSO) |
| GET | `/api/v1/admin/oidc` | `{providers, redirect_uri}`; the client secret is never returned (`has_secret`) |
| POST | `/api/v1/admin/oidc` | `{name, slug?, issuer, client_id, client_secret?, scopes?, enabled?, trusted_domains?, allowed_domains?, enforce_domains?, auto_provision?, default_team_id?, default_role?, groups_claim?, group_roles?: [{group, team_id, role}], skip_totp?}`; discovery is checked first. 201 |
| PATCH | `/api/v1/admin/oidc/{id}` | same fields, all optional; an empty `client_secret` keeps the stored one |
| DELETE | `/api/v1/admin/oidc/{id}` | removes the provider and its links |

Which account an identity signs in to: a linked identity (provider + `sub`) signs in to its account.
Otherwise the provider must report `email_verified: true`; an existing account with that email is
linked automatically only when the domain is in `trusted_domains` and the account is not an
administrator — anything else must be linked from a signed-in session. Unknown users get an account
only with `auto_provision` (added to `default_team_id` with `default_role`). `allowed_domains` limits
every sign-in to verified emails in those domains. `group_roles` sets team roles from the groups claim
at each sign-in (highest role wins; owners untouched; memberships are not removed). `skip_totp`
(default true) trusts the provider's MFA; turn it off to ask accounts with 2FA for their code.
`enforce_domains` refuses password and GitHub sign-in (and sign-up) for those domains, except for the
instance owner (the first administrator), whose password stays as a break-glass login; API tokens keep working.

## Sign-up and team limits (administrators)

| Method | Path | Notes |
|---|---|---|
| GET | `/api/v1/admin/settings` | `{allow_signup, limits}` |
| GET | `/api/v1/batches/estimate?template=&count=` | memory N copies reserve / may use vs what the workers have free |
| POST | `/api/v1/batches` | `{name, template, mode: instances|accounts, count (instances), learners: [{name, email}] (accounts), team_id?, project_id?, prefix?, env?, force?}` bulk install (platform admin). `instances`: N copies in one project (`<prefix>-01…`); `accounts`: one dashboard account (first password generated) + project per learner. 409 with `estimate` when capacity looks short, unless `force`. 202 |
| GET | `/api/v1/batches`, `/api/v1/batches/{id}` | batches with per-status counts; one batch with its items `{label, email, password, status, url, project_id, service_id, error}` |
| GET | `/api/v1/batches/{id}/csv` | hand-out list (UTF-8 CSV) |
| DELETE | `/api/v1/batches/{id}` | deletes every copy with its databases (accounts mode: the accounts, their teams and projects too). 202 |
| GET/PUT | `/api/v1/admin/storage` | off-site S3 storage `{endpoint, region, bucket, access_key, secret_key, prefix, virtual_host}`; PUT tests it first, an empty endpoint turns it off. `PAAS_S3_*` env wins (`from_env`) |
| GET/POST | `/api/v1/admin/state-backups` | platform state (control-plane database) backups: list `{last, backups, offsite_error, every}` / back up now |
| GET | `/api/v1/admin/state-backups/{name}` | download a sealed backup (restore with `paasd restore-state --file`) |
| GET/PUT | `/api/v1/admin/ai` | AI failure diagnosis provider `{provider, model, base_url, api_key}` (provider `anthropic`, `openai` or empty) → `{enabled, provider, model, base_url, has_key, default_model}`. `anthropic`: Messages API, key required, model defaults to `claude-sonnet-5-5`, `base_url` optional; `openai`: any OpenAI-compatible `/chat/completions` endpoint (`base_url` like `http://ollama:11434/v1`, model required, key optional). The key is stored encrypted and never returned; an empty `api_key` keeps the stored one; an empty provider turns AI off |
| PUT | `/api/v1/admin/settings` | `{allow_signup?, limits?: {max_projects, max_services, max_databases, max_memory_mb, max_replicas, max_signups_per_ip_per_day}, quota?: TeamQuota}` (0 = unlimited); `quota` is the default team quota |

## Setup checklist (administrators)

The first-run wizard in the dashboard (and `paas doctor`) is backed by these. Steps, in order:
`admin` (2FA on the caller's account), `dns` (`*.<base>` probed with a random label, and the dashboard
domain, resolved for A and AAAA and compared with `PAAS_PUBLIC_IPS`, else the addresses of edge nodes and
of this host's node; `records` lists what to create), `https` (TLS dial to the dashboard domain with SNI,
falling back to the local edge proxy: expiry, host name, trust), `ports` (80 / 443 listening on this host
and on edge nodes), `github`, `backups` (off-site S3 upload / download / delete), `notifications`
(channels in administrators' teams), `first_app` (a deployed service outside the system project).
Each step: `{id, title, howto, status: ok|warn|fail, summary, details?, records?: [{name, type, value}], info?, checked_at, skipped, completed}`.
Checks run concurrently, at most 8 s each, and results are cached for 30 s.

| Method | Path | Notes |
|---|---|---|
| GET | `/api/v1/admin/setup-checklist[?refresh=1]` | `{dismissed, finished, completed: [step], skipped: [step], updated_at, pending, steps: [...]}`; `refresh=1` runs every check again |
| POST | `/api/v1/admin/setup-checklist/{step}/check` | runs one check again (bypasses the cache) and returns the step |
| PUT | `/api/v1/admin/setup-checklist` | `{dismissed?, finished?, completed?: [step], skipped?: [step]}`; fields left out are kept. Stored per instance (settings table) |

With sign-up on, anyone can register at `/#/signup` and gets a personal team. Limits apply to every
non-administrator; requests over a limit return 403, too many sign-ups from one IP return 429.

## Updating paas (administrators)

| Method | Path | Notes |
|---|---|---|
| GET | `/api/v1/admin/update` | `{version (this control plane), development, check_enabled, allow_unverified, latest: {tag, name, notes, url, published_at, assets} \| null, checked_at, check_error, update_available, control_planes: [{name, version, arch, node_id, leader, online, started_at, last_seen_at}], nodes: [{id, name, version, arch, roles, status, online, pinned, control_plane}], rollout: Rollout \| null}` (`rollout` is the latest one, finished or not) |
| POST | `/api/v1/admin/update/check` | looks for the latest release now (`PAAS_UPDATE_URL`) and answers as GET |
| POST | `/api/v1/admin/update` | `{version?, confirm: true}` starts a rolling update to `version` (default: the latest release). 400 without `confirm`, for a downgrade, when the release lacks a `paasd-linux-<arch>` the cluster needs, or when it publishes no `SHA256SUMS` (unless `PAAS_UPDATE_ALLOW_UNVERIFIED=true`); 409 while another update is running or paused; 502 when the release cannot be fetched. 201 Rollout |
| POST | `/api/v1/admin/update/pause`, `/resume`, `/abort` | pause: the step in progress finishes, no further machine is started; resume a paused update; abort a running or paused one for good. 409 when the update is not in a state that allows it. 200 Rollout |
| POST | `/api/v1/nodes/{id}/rollback-binary` | puts the node's previous paasd binary (`paasd.prev`) back and restarts it; a control plane's own node rolls back that control plane. A node keeps that binary (`pinned`, it no longer follows the control plane's binary on its own) until an update reaches it. 202; 409 when there is no previous binary; 502 when the node does not answer |
| GET | `/download/release/{version}/paasd-linux-{amd64,arm64}` | no login: the binary of the version being rolled out, for nodes. The control plane fetches it from the release once, verifies it against the release's checksum and serves it from its data directory (the node verifies it again). 404 for any other version |

Rollout: `{id, version, status: running|paused|failed|aborted|done, message, steps: [{kind: backup|control_plane|node, id, name, roles, leader, from_version, status: pending|starting|updating|rolling_back|done|skipped|failed|rolled_back, message, started_at, finished_at}], checksums: {arch: sha256}, requested_by, created_at, updated_at, finished_at}`.
Order: a backup of the platform state, the control planes that are not the leader, the leader (leadership moves to an updated one, which continues from the database), then every other node one at a time (edge nodes too, so traffic keeps flowing through the others). Offline and cordoned nodes are skipped with a message. A machine that does not report the new version and come back online within 10 minutes is rolled back to `paasd.prev` and the update stops (`failed`, "failed at node X, rolled back to vY"). While an update is running, paused or failed, nodes do not follow the control plane's binary on their own (agent self-update). Rolling back restores binaries only: database migrations are additive, so the previous binary works on the newer schema; downgrades are refused.

## Team quotas

A quota is a team's budget, set by instance administrators: a default for every team
(`quota` in `/admin/settings`) and an optional per-team override. Unlike the limits above it binds
every change to the team, administrators' and bulk installs included.

`TeamQuota` (0 = unlimited): `max_projects`, `max_services`, `max_databases`, `max_memory_mb`
(sum of memory requests, as the scheduler reserves them), `max_memory_limit_mb` (sum of memory
limits: the worst case, which also covers best-effort instances that reserve nothing),
`max_cpu_millis` (sum of CPU requests), `max_disk_mb` (database storage quotas; twice with an HA
standby; service volumes are unsized and not counted), `max_previews`, `max_domains` (custom domains).
A service counts its replicas (the autoscaling maximum when higher; one for a cron job, none for a
static site); stopped and sleeping services keep counting.

| Method | Path | Notes |
|---|---|---|
| GET | `/api/v1/teams/{team}/quota` | team members: `{quota (effective), default, override, usage: {projects, services, databases, memory_mb, memory_limit_mb, cpu_millis, disk_mb, previews, domains}}` |
| PUT | `/api/v1/teams/{team}/quota` | administrators: `{quota: {max_…}}` replaces the team's override; keys left out use the default, `{}` resets |

Checked when a project, service, database, custom domain or preview is created, a service is scaled
or resized, a database is resized, upgraded to dedicated, given HA, a larger storage quota or
restored to a new database, and when a template, compose file or repository is imported. Batches
check all copies up front (accounts mode: one copy against the default quota of each learner's new
team). Over quota the request fails with 403, e.g. `quota exceeded: memory 3.5 GiB of 4 GiB used,
this needs 1 GiB more; …`. Changes that lower usage are always allowed, and restarts, rescheduling,
waking and starting stopped services are never checked. CLI: `paas quota`.

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
- `"previews": true` — every pull request targeting the service branch gets its own service (`<slug>-pr-<n>`, URL `<slug>-pr-<n>-<project>.<base>`), sleeping when idle, deleted when the PR closes. Needs the webhook with pull / merge request events (GitHub, GitLab, Gitea / Forgejo or Bitbucket).
- `"autoscale": {"enabled": true, "min": 1, "max": 5, "target_cpu_percent": 70, "target_rps": 50}`.
- `GET /services/{id}` additionally returns `previews` (list of services with `parent_id`/`preview_pr`) and `next_run` (cron).
- Services have `parent_id` and `preview_pr` (null for normal services).

| Method | Path | Notes |
|---|---|---|
| POST | `/services/{id}/run` | start a cron job now → job run (202) |
| GET | `/services/{id}/runs` | `[{id, status: running|succeeded|failed, exit_code, output, trigger: schedule|manual, started_at, finished_at}]` |
| GET | `/services/{id}/metrics?hours=6` | `{points: [{ts, instances, memory_mb, cpu_millis, requests, errors_5xx, net_rx_bytes, net_tx_bytes}], rps_5m, errors_5m, traffic}` (one point per minute; `net_*` = bytes received/sent by the containers during that minute). `traffic`: `{daily: [{day, rx_bytes, tx_bytes}], rx_30d, tx_30d, rx_today, tx_today, since?, tz}` (UTC days, kept 90 days) |
| GET | `/nodes/{id}/metrics` | `[{ts, cpu_percent, memory_used_mb, memory_committed_mb, net_rx_bytes, net_tx_bytes}]` (24 h; traffic of the physical interfaces). `GET /nodes` also carries `usage.net_rx_bps`/`net_tx_bps` (now) and `traffic` (as above) per node |

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

Events: `deploy.succeeded`, `deploy.failed`, `deploy.vulnerable`, `instance.crashloop`, `instance.evicted`, `node.offline`, `backup.failed`, `job.failed`, `autoscale`, `database.quota`, `database.failover`, `database.eol`, `volume.move`, `alert.firing`, `alert.resolved`, `uptime.down`, `uptime.up`, `uptime.cert`, `update.available` (a newer paas release, once per version; administrators' teams), `update.failed` (a rolling update stopped). Webhook payloads of alert and uptime events carry `data` (rule / check, project, service, value or status). Channels created before the alert / uptime events existed are not subscribed to them; an alert rule or uptime check that lists `channel_ids` sends to those channels whatever their events.

### Alert rules (per project)
Developers and above create, change and delete; viewers read. The leader evaluates enabled rules every minute against the per-minute service metrics; a rule fires when its condition held for `for_seconds`, notifies `alert.firing` once, then `alert.resolved` when the condition stops holding. A new firing within `cooldown_seconds` of the previous notification is not notified (nor is its resolve). States are kept per rule and service and survive a leader change. Only running services are judged.

| Method | Path | Notes |
|---|---|---|
| GET | `/projects/{id}/alerts[?service={serviceId}]` | `{rules: [{id, service_id, name, metric, comparator, threshold, for_seconds, cooldown_seconds, channel_ids, enabled, states: [{service_id, service_name, state: ok|pending|firing, value, pending_since, firing_since, resolved_at, last_notified_at, notified}]}], metrics: {name: description}, comparators}`. With `service`, the service's rules and the project-wide ones |
| POST | `/projects/{id}/alerts` | `{metric, comparator: > >= < <=, threshold, service_id?, name?, for_seconds? (300), cooldown_seconds? (1800), channel_ids? ([]: the team's channels subscribed to the event), enabled?}`. No `service_id`: every web and worker service of the project. At most 50 per project |
| PATCH | `/alerts/{id}` | any field above but `service_id`; a changed condition or disabling resets the state |
| DELETE | `/alerts/{id}` | |

Metrics: `cpu_percent` (of the CPU limit, else the request, else one core; average per instance), `memory_percent` (of the memory limit, else the request), `restarts` (container restarts during the last 5 minutes), `error_rate` (5xx share of the requests of the last 5 minutes, in %; no traffic = no data), `instances` (running instances). The edge proxies report request and 5xx counts but no response times, so there is no latency metric.

### Uptime checks (per project)
HTTP(S) checks run from the control-plane leader (exactly once per interval cluster-wide, results straight into PostgreSQL). By default a check may only reach public addresses (`PAAS_UPTIME_ALLOW_PRIVATE=true` lifts that); redirects are followed (at most 5); an invalid or expired certificate fails the check. `uptime.down` after `failure_threshold` consecutive failures, `uptime.up` on the first success after that, `uptime.cert` once per certificate that expires within 14 days. Results are kept 7 days.

| Method | Path | Notes |
|---|---|---|
| GET | `/projects/{id}/uptime[?service={serviceId}]` | `{checks: [{id, service_id, name, url, method, expected_status, keyword, timeout_seconds, interval_seconds, failure_threshold, channel_ids, enabled, state: unknown|up|down, consecutive_failures, last_checked_at, last_error, state_changed_at, cert_expires_at, stats: {uptime_24h, uptime_7d, avg_latency_ms_24h, p95_latency_ms_24h, checks_24h}}]}` (percentages; `null` without results) |
| POST | `/projects/{id}/uptime` | `{url, name? (host), service_id?, method? GET|HEAD|POST, expected_status? (0 = any 2xx/3xx), keyword?, timeout_seconds? (10, max 30), interval_seconds? (60, min 30), failure_threshold? (3), channel_ids?, enabled?}`. At most 20 per project |
| GET | `/uptime/{id}?limit=100` | `{check, results: [{ts, ok, status_code, latency_ms, error}]}` newest first (limit ≤ 1000) |
| PATCH | `/uptime/{id}` | any field above but `service_id`; a new URL starts over (`unknown`) |
| DELETE | `/uptime/{id}` | deletes the history too |

### Prometheus
`GET /metrics` (same as `GET /api/v1/metrics/prometheus`) in the Prometheus text format. Authentication: `Authorization: Bearer $PAAS_METRICS_TOKEN` (set on the control plane), or an administrator's session or API token; anyone else gets 401 / 403. Families: `paas_node_up`, `paas_node_cpu_percent`, `paas_node_load1`, `paas_node_memory_used_bytes`, `paas_node_memory_total_bytes`, `paas_node_disk_used_bytes`, `paas_node_disk_total_bytes`; per service (`team`, `project`, `service`, `type` labels) `paas_service_running`, `paas_service_instances`, `paas_service_cpu_millicores`, `paas_service_memory_bytes`, `paas_service_restarts`, and for web / static services `paas_service_http_requests_5m`, `paas_service_http_errors_5xx_5m`; `paas_deployments{status}`, `paas_deployments_24h`; `paas_alert_firing`, `paas_alert_value`; `paas_uptime_up`, `paas_uptime_ratio_24h`, `paas_uptime_ratio_7d`, `paas_uptime_latency_p95_seconds`, `paas_uptime_cert_expiry_timestamp_seconds`. All are gauges (the platform keeps no cumulative counters).

```yaml
scrape_configs:
  - job_name: paas
    scheme: https
    authorization: {credentials: "<PAAS_METRICS_TOKEN>"}
    static_configs: [{targets: ["paas.example.com"]}]
```

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
