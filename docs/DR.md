# Backups and disaster recovery

What the platform keeps, where, and what to do when a node or the control plane is lost.

## Before anything breaks

1. **Configure off-site storage** (Settings → Backups, or `PAAS_S3_*` on the control plane). Any
   S3-compatible bucket outside the cluster: AWS S3, Cloudflare R2, Backblaze B2, MinIO on another
   server. Saving writes, reads and deletes a test object first. Without it every backup lives on
   the node it describes and dies with it.
2. **Keep a copy of `/var/lib/paas/master.key`** somewhere outside the cluster (a password manager
   is fine). It encrypts every secret in the platform database, and the platform state backups. A
   state backup without this key cannot be opened.
3. **Keep the bucket credentials outside the cluster too.** After losing the control plane they are
   the only way to reach the state backups.

## What is backed up

`<prefix>` is the storage prefix (default `paas-backups`).

| What | How | Where | Kept |
|---|---|---|---|
| Platform state (projects, services, env vars, settings, users; secrets encrypted) | `pg_dump` of the control-plane database every hour, sealed with the master key (AES-256-GCM) | `/var/lib/paas/state-backups` on the leader + `<prefix>/state/` in the bucket | 24 local; off-site 48 hourly + 30 daily |
| Databases | engine-native dump on the schedule set per database (default daily) | the database's node + `<prefix>/<db>/` in the bucket | per database; pinned restore points are kept |
| Database restore checks | the newest dump is restored into a scratch database weekly, then dropped | result on the database page | — |
| PostgreSQL PITR (when on) | base backups + WAL | `<prefix>/pitr/<db>/` in the bucket | per database |
| Service volumes | tar of the volumes with the container paused for a moment, daily by default (Settings → Volume backups per service) | `<prefix>/volumes/<service>/` in the bucket | 7 |
| Static sites | the built site of each deployment | `<prefix>/static/<service>/` in the bucket | the live deployment only |
| Images | not backed up: they are rebuilt from git on the next deploy | — | — |

Stale backups raise a `backup.failed` notification once a day: no successful dump in twice the
schedule, or a failed restore check. The database page shows the same state.

## A worker node is lost

Stateless services (no volumes) move by themselves: once the node is offline the reconciler starts
their instances elsewhere. Static sites built on that node are restored from the bucket onto another
build node within a few minutes. PostgreSQL databases with high availability fail over to their
standby in about 45–60 seconds.

What stays down is anything that keeps data on that node: services with volumes, and database
servers. Nothing of it moves by itself: a restore from a backup loses what was written after the
backup, so it is always an administrator's decision. The `node.offline` notification says how many
there are. The node page lists them (Nodes → *Recover…*, or `GET /api/v1/nodes/{id}/stuck`), with
the newest off-site backup of each. If the node is coming back (reboot, network), wait. If it is not:

1. **Services with volumes**: all at once from the node page (Nodes → *Recover…* → *Restore on
   other nodes*, `POST /api/v1/nodes/{id}/recover` with `confirm_data_loss`,
   `paas restore-volume --node <node> --yes`), or one by one: Service → Settings → Volume backups →
   *Recover on another node* (`POST /api/v1/services/{id}/recover`,
   `paas restore-volume <service> --yes`). Each service is restored on another node from its newest
   off-site volume backup (or a chosen one), then pinned there and started. Changes since that
   backup are lost: the restore records the backup's time (`rpo_at`, shown on the service page and
   in the `volume.move` notification). Services without a backup are skipped unless `allow_empty`.
2. **Each dedicated database**: Database → Backups → *Recover on another node*
   (`POST /api/v1/databases/{id}/recover`). A new server is created elsewhere from the newest
   off-site dump; linked services restart with the new address.
3. **A shared database server**: the same button on any of its databases (platform administrators).
   The shared server is recreated and every database it held is restored from its own dump. The
   notification reports how many were restored, started empty or failed.
4. **Remove the node** once the list is empty. Removing it with `force=true` while something is
   still listed starts those things *empty* elsewhere.

With no backup the recovery stops and offers to start the service or database empty. The data left
on the dead node is not touched; if it ever comes back, its old volumes are deleted after 7 days.

## Taking a node out of service (planned)

When the node is still up (maintenance, replacement), nothing needs a backup: drain it with its
volumes (Nodes → *Drain…* → *Drain and move volumes*, `POST /api/v1/nodes/{id}/drain` with
`move_volumes`, `paas drain <node> --move-volumes`), or move one service
(`paas move <service> --to <node> --yes`, Service → Settings → Node). Each service with volumes
stops, its volumes are streamed straight to the other node over the WireGuard mesh, checked
(bytes, entries and a SHA-256 of the whole tree on both sides), and it starts on the new node. The
downtime is the copy (volume size / mesh bandwidth) plus the check and the start: there is no live
pre-copy. A failure before the switch, an abort, or a service that does not start on the new node
puts it back on the old node, whose copy was not written to since the stop. The old copy stays for
7 days (`retain_days`) after a successful move. A control plane restart in the middle undoes a move
that had not switched yet and finishes one that had. Database servers stay (use their own tools).

## The control plane is lost

The apps keep running: nodes, containers and the edge proxies do not need the control plane to
serve traffic. Deploys, scaling, recovery and the dashboard stop until it is back.

On a new server:

```bash
# 1. Install a fresh control plane with the same domain (DNS: point the dashboard and *.domain at it)
curl -fsSL https://raw.githubusercontent.com/thesunbg/paas/main/internal/installer/install.sh \
  | sudo sh -s -- server --domain apps.example.com --email you@example.com

# 2. Put the old master key back and stop paasd
sudo systemctl stop paasd
sudo install -m 0600 master.key /var/lib/paas/master.key

# 3. Restore the newest platform state from the bucket
sudo PAAS_S3_ENDPOINT=https://… PAAS_S3_BUCKET=… PAAS_S3_ACCESS_KEY=… PAAS_S3_SECRET_KEY=… \
  paasd restore-state --latest          # or --list, --name state-…, --file ./state-….pgstate

# 4. Start it again
sudo systemctl start paasd
```

`restore-state` refuses to run while paasd answers on port 8080, and restores in one transaction:
either the whole backup is in, or nothing changed. Everything after the backup (at most an hour) is
lost: services created since then, the last deployments, new env vars.

The nodes reconnect by themselves when the new control plane answers on the same address (the
dashboard hostname, TCP 4222). If its address changed, re-join each node from the dashboard
(Nodes → Add node). If the old control plane was also a worker, its services with volumes and its
database servers are on the list of the previous section: recover them the same way.

## Running a second control plane

Losing the only control plane means an outage of deploys and the dashboard until it is restored.
To avoid it, run two or three control planes against one PostgreSQL that survives any one of them
(a managed PostgreSQL, or Patroni/repmgr on separate servers; a database on the first control plane
itself does not help).

```bash
curl -fsSL https://<dashboard>/install.sh | sudo sh -s -- server \
  --domain apps.example.com --email you@example.com \
  --database-url 'postgres://paas:…@db.internal:5432/paas' \
  --master-key-file ./master.key \
  --server-name cp2 --routes nats://cp1.internal:6222
```

and on the first control plane add to `/etc/paas/paasd.env` (then `systemctl restart paasd`):

```
PAAS_SERVER_NAME=cp1
PAAS_NATS_CLUSTER_ADDR=0.0.0.0:6222
PAAS_NATS_ROUTES=nats://cp2.internal:6222
```

Open TCP 6222 between the control planes and TCP 4222 from every node to every control plane. One
control plane is the leader (deploys, reconciler, backups) and another takes over within seconds
when it stops. Each control plane advertises its node address to the cluster
(`PAAS_NATS_ADVERTISE`, default: the public URL's host and port 4222); nodes remember every address
they learn, so a node that restarts while one control plane is down still reaches another. Point
the dashboard hostname at all of them (DNS round robin or a load balancer).
