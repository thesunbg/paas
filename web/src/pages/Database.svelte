<script lang="ts">
  import { onMount } from 'svelte';
  import { api, catalog, defaultEnvFor, errorMessage, type Backup, type DatabaseDetail, type ProjectDetail, type SizesResponse } from '../lib/api';
  import { can, session } from '../lib/session.svelte';
  import { href, navigate } from '../lib/router.svelte';
  import { mb, cpu, relTime, absTime, bytes, between } from '../lib/format';
  import { poll } from '../lib/poll';
  import { toast } from '../lib/toast.svelte';
  import { confirmDialog } from '../lib/confirm.svelte';
  import ErrorMsg from '../components/ErrorMsg.svelte';
  import Loading from '../components/Loading.svelte';
  import Icon from '../components/Icon.svelte';
  import Status from '../components/Status.svelte';
  import Secret from '../components/Secret.svelte';
  import CopyButton from '../components/CopyButton.svelte';
  import Modal from '../components/Modal.svelte';
  import ExternalAccess from '../components/db/ExternalAccess.svelte';
  import Usage from '../components/db/Usage.svelte';
  import Pooler from '../components/db/Pooler.svelte';
  import Users from '../components/db/Users.svelte';
  import PITR from '../components/db/PITR.svelte';
  import HA from '../components/db/HA.svelte';
  import Import from '../components/db/Import.svelte';

  let { id }: { id: string } = $props();

  let detail = $state<DatabaseDetail | null>(null);
  let project = $state<ProjectDetail | null>(null);
  let backups = $state<Backup[] | null>(null);
  let error = $state('');
  let busy = $state('');
  let lastFull = 0;

  // Link form
  let linkService = $state('');
  let linkEnv = $state('');
  let linkRestart = $state(true);
  let linkError = $state('');

  // Upgrade dialog
  let upgradeOpen = $state(false);
  let resizeOpen = $state(false);
  let resizeSize = $state('small');
  async function resize(e: SubmitEvent) {
    e.preventDefault();
    resizeOpen = false;
    act('resize', () => api.resizeDatabase(id, resizeSize), `Resizing to ${resizeSize} — the server restarts`);
  }
  let upgradeSize = $state('small');
  let versionOpen = $state(false);
  let versionTarget = $state('');
  let versionConfirm = $state(false);

  const transitional = (s?: string) => s === 'provisioning' || s === 'upgrading' || s === 'deleting' || s === 'importing';

  async function load() {
    try {
      const d = await api.database(id);
      detail = d;
      error = '';
      const [p, b] = await Promise.all([
        !project || project.project.id !== d.database.project_id ? api.project(d.database.project_id) : Promise.resolve(project),
        api.backups(id),
      ]);
      project = p;
      backups = b ?? [];
      lastFull = Date.now();
    } catch (e) {
      error = errorMessage(e);
    }
  }

  // Every 3 s while provisioning/upgrading (or a backup runs); otherwise every 15 s.
  async function tick() {
    const busyState = transitional(detail?.database.status) || backups?.some((b) => b.status === 'running' || b.status === 'pending');
    if (busyState || Date.now() - lastFull > 15000) await load();
  }

  onMount(() => {
    load();
    catalog().then((c) => (cat = c)).catch(() => {});
    return poll(tick, 3000);
  });

  const db = $derived(detail?.database);
  const teamId = $derived(project?.project.team_id);
  const isDev = $derived(teamId ? can('developer', teamId) : false);
  const isAdmin = $derived(teamId ? can('admin', teamId) : false);
  const serviceName = (sid: string) => project?.services.find((s) => s.id === sid)?.name ?? sid.slice(0, 8);
  const linkable = $derived((project?.services ?? []).filter((s) => !detail?.links.some((l) => l.service_id === s.id)));
  const engineLabel: Record<string, string> = { postgres: 'PostgreSQL', mysql: 'MySQL', mariadb: 'MariaDB', redis: 'Redis', mongo: 'MongoDB' };
  let cat = $state<SizesResponse | null>(null);
  const dbSizes = $derived(cat?.database_sizes ?? []);
  const sizePreset = $derived(dbSizes.find((s) => s.name === db?.size_preset));

  async function act(name: string, fn: () => Promise<unknown>, ok: string) {
    busy = name;
    try {
      await fn();
      toast.success(ok);
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      busy = '';
    }
  }

  async function link(e: SubmitEvent) {
    e.preventDefault();
    if (!linkService || !db) return;
    linkError = '';
    busy = 'link';
    try {
      const r = await api.linkDatabase(id, linkService, linkEnv.trim() || undefined, linkRestart);
      toast.success(`Linked ${serviceName(linkService)} as ${r.env_name}`);
      linkService = '';
      linkEnv = '';
      await load();
    } catch (err) {
      linkError = errorMessage(err);
    } finally {
      busy = '';
    }
  }

  async function unlink(sid: string) {
    if (!(await confirmDialog({ title: 'Unlink service', message: `Remove the connection variable from ${serviceName(sid)}? It takes effect on the service's next restart.`, confirmLabel: 'Unlink', danger: true }))) return;
    act('unlink', () => api.unlinkDatabase(id, sid), `Unlinked ${serviceName(sid)}`);
  }

  async function restore(b: Backup) {
    const ok = await confirmDialog({
      title: 'Restore backup',
      message: `Restore ${db?.name} to the backup from ${absTime(b.created_at)}? Current data will be overwritten.`,
      confirmLabel: 'Restore',
      danger: true,
      requireText: db?.name,
    });
    if (ok) act('restore-' + b.id, () => api.restoreBackup(id, b.id), 'Backup restored');
  }

  async function del() {
    if (!db) return;
    const ok = await confirmDialog({
      title: 'Delete database',
      message: `This deletes ${db.name} and all its data${db.mode === 'dedicated' ? ', its server' : ''} and backups. Linked services lose their connection variable.`,
      confirmLabel: 'Delete database',
      danger: true,
      requireText: db.name,
    });
    if (!ok) return;
    try {
      await api.deleteDatabase(id);
      toast.success(`Database ${db.name} deleted`);
      navigate(`/projects/${db.project_id}`);
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }

  // Only a platform administrator may restart a shared server (it hosts every tenant's database).
  const canUpdateMinor = $derived(
    !!db && db.status === 'ready' && !db.options?.ha && (db.mode === 'shared' ? !!session.me?.user.is_admin : isAdmin)
  );

  async function updateMinor() {
    if (!db) return;
    const shared = db.mode === 'shared';
    const ok = await confirmDialog({
      title: 'Update to the latest minor version',
      message:
        `Pulls the newest ${engineLabel[db.engine] ?? db.engine} ${detail?.version ?? ''}.x build and restarts the server: ${db.name} is unavailable while it restarts. ` +
        `A backup is taken first and kept as the only way back; a minor update cannot be rolled back. The major version never changes.` +
        (shared ? ' This is a shared server: every database on it restarts, for every team that uses it.' : ''),
      confirmLabel: shared ? 'Restart shared server' : 'Update and restart',
      danger: shared,
    });
    if (!ok) return;
    await act('update-minor', () => api.updateMinor(id, shared), 'Minor update started');
  }

  // Newer, not end-of-life versions of this engine (the catalog lists newest first). Redis cannot be
  // upgraded; HA, PITR and the pooler must be off first (the API answers 409 otherwise).
  const upgradeTargets = $derived.by(() => {
    const vs = cat?.engines?.[db?.engine ?? '']?.versions ?? [];
    const i = vs.findIndex((v) => v.name === detail?.version);
    return i < 0 ? [] : vs.slice(0, i).filter((v) => v.status !== 'eol');
  });
  const canUpgradeVersion = $derived(
    !!db && isAdmin && db.status === 'ready' && db.engine !== 'redis' && !db.options?.ha && !db.options?.pitr && !db.options?.pooler && upgradeTargets.length > 0
  );

  async function upgradeVersion(e: SubmitEvent) {
    e.preventDefault();
    if (!versionConfirm || !versionTarget) return;
    versionOpen = false;
    await act('version', () => api.upgradeVersion(id, versionTarget), `Upgrade to ${versionTarget} started`);
  }

  async function upgrade(e: SubmitEvent) {
    e.preventDefault();
    upgradeOpen = false;
    await act('upgrade', () => api.upgradeDatabase(id, upgradeSize), 'Upgrade to a dedicated server started');
  }
</script>

<div class="page">
  {#if !detail && !error}
    <Loading rows={3} />
  {:else if !detail || !db}
    <ErrorMsg {error} onretry={load} />
  {:else}
    <div class="crumbs">
      <a href={href('/')}>Projects</a><Icon name="chevron" size={12} />
      {#if project}<a href={href(`/projects/${project.project.id}`)}>{project.project.name}</a><Icon name="chevron" size={12} />{/if}
      <span>{db.name}</span>
    </div>
    <div class="page-head">
      <div>
        <div class="row">
          <h1>{db.name}</h1>
          <span class="badge">{engineLabel[db.engine] ?? db.engine} {detail.version ?? ''}</span>
          <span class="badge {db.mode === 'dedicated' ? 'blue' : ''}">{db.mode}</span>
          <Status status={db.status} />
        </div>
        <p class="sub">Created {relTime(db.created_at)} · backups {db.backup_schedule_hours ? `every ${db.backup_schedule_hours} h` : 'manual only'}</p>
      </div>
      <div class="row">
        {#if isDev && db.status === 'ready'}
          <a class="btn primary" href={href(`/projects/${db.project_id}/studio?db=${db.id}`)} target="_blank" rel="noopener" title="Browse and query this database in dbweb"><Icon name="terminal" size={14} />Open in dbweb</a>
        {/if}
        {#if isAdmin && db.mode === 'shared' && db.status === 'ready'}
          <button onclick={() => (upgradeOpen = true)} disabled={!!busy}><Icon name="zap" size={14} />Upgrade to dedicated</button>
        {/if}
        {#if canUpdateMinor}
          <button onclick={updateMinor} disabled={!!busy} title="Pull the newest minor build of this version and restart the server"><Icon name="zap" size={14} />Update minor version</button>
        {/if}
        {#if canUpgradeVersion}
          <button onclick={() => ((versionTarget = upgradeTargets[0].name), (versionConfirm = false), (versionOpen = true))} disabled={!!busy} title="Move to a newer major version (one way)"><Icon name="zap" size={14} />Upgrade version</button>
        {/if}
        {#if isAdmin && db.mode === 'dedicated' && db.status === 'ready'}
          <button onclick={() => ((resizeSize = db.size_preset), (resizeOpen = true))} disabled={!!busy}><Icon name="zap" size={14} />Change size</button>
        {/if}
        {#if isAdmin}<button class="danger" onclick={del}><Icon name="trash" size={14} />Delete</button>{/if}
      </div>
    </div>
    {#if error}<div style="margin-bottom:12px"><ErrorMsg {error} /></div>{/if}

    {#if detail?.version_status === 'eol' || detail?.version_status === 'ending'}
      <div class="alert {detail.version_status === 'eol' ? 'error' : 'warn'}" style="margin-bottom:16px">
        <Icon name="x" /><span style="flex:1">{engineLabel[db.engine] ?? db.engine} {detail.version}
          {detail.version_status === 'eol' ? `reached end of life on ${detail.version_eol} and no longer gets security fixes.` : `reaches end of life on ${detail.version_eol}.`}
          Create a new database on a newer version and move your data to it.</span>
      </div>
    {/if}
    {#if transitional(db.status)}
      <div class="alert info" style="margin-bottom:16px"><span class="spinner"></span>Database is {db.status}… this page refreshes every 3 seconds.</div>
    {/if}
    {#if db.error}
      <div class="alert {db.status === 'failed' ? 'error' : 'warn'}" style="margin-bottom:16px">
        <Icon name="x" /><span style="flex:1">{db.error}</span>
        {#if db.status === 'failed' && isDev}<button class="sm" onclick={() => act('retry', () => api.retryDatabase(id), 'Retrying provisioning')} disabled={!!busy}>Retry</button>{/if}
      </div>
    {/if}

    <div class="grid">
      <div class="card">
        <div class="card-head"><h3>Connection</h3>{#if detail.connection}<CopyButton text={detail.connection.url} label="Copy URL" small />{/if}</div>
        {#if detail.connection}
          {@const c = detail.connection}
          <dl class="kv card-body">
            <dt>Host</dt><dd class="row nowrap"><code class="truncate">{c.host}</code><CopyButton text={c.host} /></dd>
            <dt>Port</dt><dd><code>{c.port}</code></dd>
            {#if c.database}<dt>Database</dt><dd><code>{c.database}</code></dd>{/if}
            {#if c.username}<dt>Username</dt><dd class="row nowrap"><code>{c.username}</code><CopyButton text={c.username} /></dd>{/if}
            <dt>Password</dt><dd><Secret value={c.password} /></dd>
            <dt>URL</dt><dd><Secret value={c.url} /></dd>
          </dl>
          <p class="hint card-body" style="padding-top:0">Reachable from services in this project over the private network. Linked services receive the URL as an env variable.</p>
        {:else if db.status !== 'ready'}
          <div class="card-body muted small">Connection details appear once the database is ready.</div>
        {:else}
          <div class="card-body muted small">You need the developer role to view credentials.</div>
        {/if}
      </div>

      <div class="card">
        <div class="card-head"><h3>Details</h3></div>
        <dl class="kv card-body">
          <dt>Engine</dt><dd>{engineLabel[db.engine] ?? db.engine} {detail.version ?? ''}</dd>
          {#if detail.server_version}<dt>Server version</dt><dd class="mono small">{detail.server_version}</dd>{/if}
          {#if detail.image_pinned !== undefined}
            <dt>Image</dt>
            <dd class="mono small">
              {#if detail.image_pinned && detail.image_digest}{detail.image_digest}
              {:else}<span class="muted">not pinned yet</span> <span class="faint">(follows the tag; this node did not report a digest)</span>{/if}
            </dd>
          {/if}
          <dt>Mode</dt><dd>{db.mode === 'shared' ? 'Shared server (logical database)' : 'Dedicated server'}</dd>
          <dt>Size</dt><dd>{db.mode === 'shared' ? 'shared' : `${db.size_preset}${sizePreset ? ` — ${mb(sizePreset.memory_mb)}, ${cpu(sizePreset.cpu_millis)}, ${sizePreset.max_connections} connections` : ''}`}</dd>
          {#if detail.server}
            <dt>Server</dt>
            <dd>
              <Status status={detail.server.status} />
              <span class="faint small">· {detail.server.memory_usage_mb != null ? `${mb(detail.server.memory_usage_mb)} / ` : ''}{mb(detail.server.memory_limit_mb)}</span>
            </dd>
          {/if}
          <dt>Last backup</dt><dd title={absTime(db.last_backup_at)}>{relTime(db.last_backup_at)}</dd>
          <dt>ID</dt><dd class="mono small">{db.id}</dd>
        </dl>
      </div>
    </div>

    <div class="features">
      {#if db.status === 'ready'}<Usage {detail} />{/if}
      <ExternalAccess {detail} {isAdmin} reload={load} />
      {#if db.engine === 'postgres'}<Pooler {detail} {isAdmin} reload={load} />{/if}
      {#if (db.engine === 'postgres' || db.engine === 'mysql' || db.engine === 'mariadb') && isDev}<Users {detail} {isAdmin} />{/if}
      {#if db.engine === 'postgres'}<PITR {detail} {isAdmin} reload={load} />{/if}
      {#if db.engine === 'postgres'}<HA {detail} {isAdmin} reload={load} />{/if}
      {#if isAdmin && db.engine !== 'redis'}<Import {detail} reload={load} />{/if}
    </div>

    <div class="card" style="margin-top:16px">
      <div class="card-head"><h3>Linked services</h3><span class="faint small">{detail.links.length}</span></div>
      {#if detail.links.length}
        <div class="table-wrap">
          <table>
            <thead><tr><th>Service</th><th>Variable</th><th class="right"></th></tr></thead>
            <tbody>
              {#each detail.links as l (l.service_id)}
                <tr>
                  <td><a href={href(`/services/${l.service_id}`)}>{serviceName(l.service_id)}</a></td>
                  <td><code>{l.env_name}</code></td>
                  <td class="right">{#if isDev}<button class="sm" onclick={() => unlink(l.service_id)} disabled={!!busy}>Unlink</button>{/if}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {:else}
        <div class="card-body muted small">No services use this database yet.</div>
      {/if}
      {#if isDev && linkable.length}
        <form class="card-body link-form" onsubmit={link}>
          <label class="field">
            <span>Service</span>
            <select bind:value={linkService} required>
              <option value="" disabled>Select a service…</option>
              {#each linkable as s (s.id)}<option value={s.id}>{s.name}</option>{/each}
            </select>
          </label>
          <label class="field">
            <span>Env variable</span>
            <input class="mono" bind:value={linkEnv} placeholder={defaultEnvFor(db.engine, cat)} />
          </label>
          <label class="check small"><input type="checkbox" bind:checked={linkRestart} />Restart service</label>
          <button class="primary" type="submit" disabled={!linkService || busy === 'link'}>{#if busy === 'link'}<span class="spinner"></span>{/if}Link</button>
        </form>
        {#if linkError}<div class="card-body" style="padding-top:0"><ErrorMsg error={linkError} /></div>{/if}
      {/if}
    </div>

    <div class="card" style="margin-top:16px">
      <div class="card-head">
        <h3>Backups</h3>
        {#if isDev}
          <button class="sm" onclick={() => act('backup', () => api.backupNow(id), 'Backup completed')} disabled={!!busy || db.status !== 'ready'}>
            {#if busy === 'backup'}<span class="spinner"></span>{/if}Backup now
          </button>
        {/if}
      </div>
      {#if backups === null}
        <div class="card-body"><Loading /></div>
      {:else if backups.filter((x) => x.kind !== 'base').length === 0}
        <div class="card-body muted small">No backups yet.</div>
      {:else}
        <div class="table-wrap">
          <table>
            <thead><tr><th>Created</th><th>Status</th><th class="right">Size</th><th class="hide-sm">Duration</th><th class="hide-sm">File</th><th class="right"></th></tr></thead>
            <tbody>
              {#each backups.filter((x) => x.kind !== 'base') as b (b.id)}
                <tr>
                  <td title={absTime(b.created_at)} class="nowrap">{relTime(b.created_at)}</td>
                  <td>
                    <Status status={b.status === 'completed' ? 'completed' : b.status === 'failed' ? 'failed' : 'deploying'} label={b.status} />
                    {#if b.pinned}<span class="badge" title="Restore point before a version update: kept until the database is deleted">pinned</span>{/if}
                    {#if b.error}<div class="small" style="color:var(--red)">{b.error}</div>{/if}
                  </td>
                  <td class="right nowrap">{bytes(b.size_bytes)}</td>
                  <td class="hide-sm mono small">{between(b.created_at, b.finished_at)}</td>
                  <td class="hide-sm mono small faint truncate" style="max-width:260px" title={b.path}>{b.path.split('/').pop()}</td>
                  <td class="right">
                    {#if isAdmin && b.status === 'completed'}
                      <button class="sm" onclick={() => restore(b)} disabled={!!busy}>{#if busy === 'restore-' + b.id}<span class="spinner"></span>{/if}Restore</button>
                    {/if}
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
    </div>

    <Modal bind:open={upgradeOpen} title="Upgrade to a dedicated server" width={460}>
      <form id="upgrade-form" class="stack" onsubmit={upgrade}>
        <p class="muted small">Moves {db.name} from the shared server to its own {engineLabel[db.engine]} container. Data is copied with a dump and restore. Linked services are stopped while it runs and started again with the new URL.</p>
        <label class="field">
          <span>Size</span>
          <select bind:value={upgradeSize}>
            {#each dbSizes as s (s.name)}<option value={s.name}>{s.name} — {mb(s.memory_mb)}, {cpu(s.cpu_millis)}</option>{/each}
          </select>
        </label>
      </form>
      {#snippet footer()}
        <button type="button" onclick={() => (upgradeOpen = false)}>Cancel</button>
        <button type="submit" form="upgrade-form" class="primary">Upgrade</button>
      {/snippet}
    </Modal>
    <Modal bind:open={versionOpen} title="Upgrade major version" width={520}>
      <form id="version-form" class="stack" onsubmit={upgradeVersion}>
        <p class="muted small">
          Copies {db.name} to a new {engineLabel[db.engine] ?? db.engine} server on the version you pick, checks that every table and row count matches, then switches over and
          deletes the old server. <b>This cannot be undone</b>: there is no way back to {detail?.version}. A backup taken just before is kept until the database is deleted.
        </p>
        <label class="field">
          <span>New version</span>
          <select bind:value={versionTarget}>
            {#each upgradeTargets as v (v.name)}<option value={v.name}>{v.name}{v.status === 'ending' ? ` (ends ${v.eol})` : ''}</option>{/each}
          </select>
        </label>
        <label class="row nowrap">
          <input type="checkbox" bind:checked={versionConfirm} />
          <span class="small">I understand that services using this database stop until the upgrade is finished (from a few minutes, longer for big databases).</span>
        </label>
      </form>
      {#snippet footer()}
        <button type="button" onclick={() => (versionOpen = false)}>Cancel</button>
        <button type="submit" form="version-form" class="primary" disabled={!versionConfirm || !versionTarget}>Upgrade</button>
      {/snippet}
    </Modal>
    <Modal bind:open={resizeOpen} title="Change size" width={460}>
      <form id="resize-form" class="stack" onsubmit={resize}>
        <p class="muted small">The server restarts with the new memory and CPU limits (a few seconds of downtime). Data stays on its volume.</p>
        <label class="field">
          <span>Size</span>
          <select bind:value={resizeSize}>
            {#each dbSizes as s (s.name)}<option value={s.name}>{s.name} — {mb(s.memory_mb)}, {cpu(s.cpu_millis)}, {s.max_connections} connections</option>{/each}
          </select>
        </label>
      </form>
      {#snippet footer()}
        <button type="button" onclick={() => (resizeOpen = false)}>Cancel</button>
        <button type="submit" form="resize-form" class="primary">Resize</button>
      {/snippet}
    </Modal>
  {/if}
</div>

<style>
  .features {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(380px, 1fr));
    gap: 16px;
    margin-top: 16px;
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 16px;
  }
  .link-form {
    display: flex;
    align-items: flex-end;
    gap: 10px;
    border-top: 1px solid var(--border);
    flex-wrap: wrap;
  }
  .link-form .field {
    min-width: 180px;
    flex: 1;
  }
  .link-form .check {
    height: 32px;
  }
  @media (max-width: 900px) {
    .grid {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
