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
  import { t, tn } from '../lib/i18n.svelte';

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
    act('resize', () => api.resizeDatabase(id, resizeSize), t('Resizing to {size} — the server restarts', { size: resizeSize }));
  }
  let upgradeSize = $state('small');
  let versionOpen = $state(false);
  let versionTarget = $state('');
  let versionConfirm = $state(false);

  const transitional = (s?: string) => s === 'provisioning' || s === 'upgrading' || s === 'deleting' || s === 'importing';
  // Labels for the values shown as text (database status, mode, backup status, backup health).
  function stateText(s: string): string {
    const labels: Record<string, string> = {
      provisioning: t('provisioning'),
      upgrading: t('upgrading'),
      deleting: t('deleting'),
      importing: t('importing'),
      shared: t('shared'),
      dedicated: t('dedicated'),
      pending: t('pending'),
      running: t('running'),
      completed: t('completed'),
      failed: t('failed'),
      off: t('off'),
      stale: t('stale'),
      failing: t('failing'),
    };
    return labels[s] ?? s;
  }

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
      toast.success(t('Linked {service} as {env}', { service: serviceName(linkService), env: r.env_name }));
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
    if (!(await confirmDialog({ title: t('Unlink service'), message: t("Remove the connection variable from {service}? It takes effect on the service's next restart.", { service: serviceName(sid) }), confirmLabel: t('Unlink'), danger: true }))) return;
    act('unlink', () => api.unlinkDatabase(id, sid), t('Unlinked {service}', { service: serviceName(sid) }));
  }

  async function recover(allowEmpty: boolean) {
    const shared = db?.mode === 'shared';
    if (!(await confirmDialog({
      title: t('Recover on another node'),
      message: shared
        ? t('The shared server is recreated on another node and every database it held is restored from its newest off-site backup. Changes after those backups are lost.')
        : t('A new server is created on another node and the newest off-site backup restored into it. Changes after that backup are lost. Linked services restart with the new address.'),
      confirmLabel: t('Recover'),
      danger: true,
    }))) return;
    busy = 'recover';
    try {
      await api.recoverDatabase(id, allowEmpty);
      toast.success(t('Recovery started'));
      await load();
    } catch (e) {
      const msg = errorMessage(e);
      if (!allowEmpty && /allow_empty|no off-site backup|no backup/i.test(msg)) {
        busy = '';
        if (await confirmDialog({ title: t('No backup to recover from'), message: msg + '\n\n' + t('Start it empty instead? Its apps can connect again, but the data is gone.'), confirmLabel: t('Start empty'), danger: true })) {
          busy = 'recover';
          try {
            await api.recoverDatabase(id, true);
            toast.success(t('Recovery started'));
            await load();
          } catch (e2) {
            toast.error(errorMessage(e2));
          }
        }
      } else toast.error(msg);
    } finally {
      busy = '';
    }
  }

  async function restore(b: Backup) {
    const ok = await confirmDialog({
      title: t('Restore backup'),
      message: t('Restore {name} to the backup from {time}? Current data will be overwritten.', { name: db?.name, time: absTime(b.created_at) }),
      confirmLabel: t('Restore'),
      danger: true,
      requireText: db?.name,
    });
    if (ok) act('restore-' + b.id, () => api.restoreBackup(id, b.id), t('Backup restored'));
  }

  async function del() {
    if (!db) return;
    const ok = await confirmDialog({
      title: t('Delete database'),
      message:
        db.mode === 'dedicated'
          ? t('This deletes {name} and all its data, its server and backups. Linked services lose their connection variable.', { name: db.name })
          : t('This deletes {name} and all its data and backups. Linked services lose their connection variable.', { name: db.name }),
      confirmLabel: t('Delete database'),
      danger: true,
      requireText: db.name,
    });
    if (!ok) return;
    try {
      await api.deleteDatabase(id);
      toast.success(t('Database {name} deleted', { name: db.name }));
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
      title: t('Update to the latest minor version'),
      message:
        t('Pulls the newest {engine} {version}.x build and restarts the server: {name} is unavailable while it restarts.', { engine: engineLabel[db.engine] ?? db.engine, version: detail?.version ?? '', name: db.name }) +
        ' ' +
        t('A backup is taken first and kept as the only way back; a minor update cannot be rolled back. The major version never changes.') +
        (shared ? ' ' + t('This is a shared server: every database on it restarts, for every team that uses it.') : ''),
      confirmLabel: shared ? t('Restart shared server') : t('Update and restart'),
      danger: shared,
    });
    if (!ok) return;
    await act('update-minor', () => api.updateMinor(id, shared), t('Minor update started'));
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
    await act('version', () => api.upgradeVersion(id, versionTarget), t('Upgrade to {version} started', { version: versionTarget }));
  }

  async function upgrade(e: SubmitEvent) {
    e.preventDefault();
    upgradeOpen = false;
    await act('upgrade', () => api.upgradeDatabase(id, upgradeSize), t('Upgrade to a dedicated server started'));
  }
</script>

<div class="page">
  {#if !detail && !error}
    <Loading rows={3} />
  {:else if !detail || !db}
    <ErrorMsg {error} onretry={load} />
  {:else}
    <div class="crumbs">
      <a href={href('/')}>{t('Projects')}</a><Icon name="chevron" size={12} />
      {#if project}<a href={href(`/projects/${project.project.id}`)}>{project.project.name}</a><Icon name="chevron" size={12} />{/if}
      <span>{db.name}</span>
    </div>
    <div class="page-head">
      <div>
        <div class="row">
          <h1>{db.name}</h1>
          <span class="badge">{engineLabel[db.engine] ?? db.engine} {detail.version ?? ''}</span>
          <span class="badge {db.mode === 'dedicated' ? 'blue' : ''}">{stateText(db.mode)}</span>
          <Status status={db.status} />
        </div>
        <p class="sub">{t('Created {time}', { time: relTime(db.created_at) })} · {db.backup_schedule_hours ? t('backups every {n} h', { n: db.backup_schedule_hours }) : t('backups manual only')}</p>
      </div>
      <div class="row">
        {#if isDev && db.status === 'ready'}
          <a class="btn primary" href={href(`/projects/${db.project_id}/studio?db=${db.id}`)} target="_blank" rel="noopener" title={t('Browse and query this database in dbweb')}><Icon name="terminal" size={14} />{t('Open in dbweb')}</a>
        {/if}
        {#if isAdmin && db.mode === 'shared' && db.status === 'ready'}
          <button onclick={() => (upgradeOpen = true)} disabled={!!busy}><Icon name="zap" size={14} />{t('Upgrade to dedicated')}</button>
        {/if}
        {#if canUpdateMinor}
          <button onclick={updateMinor} disabled={!!busy} title={t('Pull the newest minor build of this version and restart the server')}><Icon name="zap" size={14} />{t('Update minor version')}</button>
        {/if}
        {#if canUpgradeVersion}
          <button onclick={() => ((versionTarget = upgradeTargets[0].name), (versionConfirm = false), (versionOpen = true))} disabled={!!busy} title={t('Move to a newer major version (one way)')}><Icon name="zap" size={14} />{t('Upgrade version')}</button>
        {/if}
        {#if isAdmin && db.mode === 'dedicated' && db.status === 'ready'}
          <button onclick={() => ((resizeSize = db.size_preset), (resizeOpen = true))} disabled={!!busy}><Icon name="zap" size={14} />{t('Change size')}</button>
        {/if}
        {#if isAdmin}<button class="danger" onclick={del}><Icon name="trash" size={14} />{t('Delete')}</button>{/if}
      </div>
    </div>
    {#if error}<div style="margin-bottom:12px"><ErrorMsg {error} /></div>{/if}

    {#if detail?.version_status === 'eol' || detail?.version_status === 'ending'}
      <div class="alert {detail.version_status === 'eol' ? 'error' : 'warn'}" style="margin-bottom:16px">
        <Icon name="x" /><span style="flex:1">{engineLabel[db.engine] ?? db.engine} {detail.version}
          {detail.version_status === 'eol' ? t('reached end of life on {date} and no longer gets security fixes.', { date: detail.version_eol }) : t('reaches end of life on {date}.', { date: detail.version_eol })}
          {t('Create a new database on a newer version and move your data to it.')}</span>
      </div>
    {/if}
    {#if transitional(db.status)}
      <div class="alert info" style="margin-bottom:16px"><span class="spinner"></span>{t('Database is {status}… this page refreshes every 3 seconds.', { status: stateText(db.status) })}</div>
    {/if}
    {#if db.error}
      <div class="alert {db.status === 'failed' ? 'error' : 'warn'}" style="margin-bottom:16px">
        <Icon name="x" /><span style="flex:1">{db.error}</span>
        {#if db.status === 'failed' && isDev}<button class="sm" onclick={() => act('retry', () => api.retryDatabase(id), t('Retrying provisioning'))} disabled={!!busy}>{t('Retry')}</button>{/if}
      </div>
    {/if}

    <div class="grid">
      <div class="card">
        <div class="card-head"><h3>{t('Connection')}</h3>{#if detail.connection}<CopyButton text={detail.connection.url} label={t('Copy URL')} small />{/if}</div>
        {#if detail.connection}
          {@const c = detail.connection}
          <dl class="kv card-body">
            <dt>{t('Host')}</dt><dd class="row nowrap"><code class="truncate">{c.host}</code><CopyButton text={c.host} /></dd>
            <dt>{t('Port')}</dt><dd><code>{c.port}</code></dd>
            {#if c.database}<dt>{t('Database')}</dt><dd><code>{c.database}</code></dd>{/if}
            {#if c.username}<dt>{t('Username')}</dt><dd class="row nowrap"><code>{c.username}</code><CopyButton text={c.username} /></dd>{/if}
            <dt>{t('Password')}</dt><dd><Secret value={c.password} /></dd>
            <dt>URL</dt><dd><Secret value={c.url} /></dd>
          </dl>
          <p class="hint card-body" style="padding-top:0">{t('Reachable from services in this project over the private network. Linked services receive the URL as an env variable.')}</p>
        {:else if db.status !== 'ready'}
          <div class="card-body muted small">{t('Connection details appear once the database is ready.')}</div>
        {:else}
          <div class="card-body muted small">{t('You need the developer role to view credentials.')}</div>
        {/if}
      </div>

      <div class="card">
        <div class="card-head"><h3>{t('Details')}</h3></div>
        <dl class="kv card-body">
          <dt>{t('Engine')}</dt><dd>{engineLabel[db.engine] ?? db.engine} {detail.version ?? ''}</dd>
          {#if detail.server_version}<dt>{t('Server version')}</dt><dd class="mono small">{detail.server_version}</dd>{/if}
          {#if detail.image_pinned !== undefined}
            <dt>{t('Image')}</dt>
            <dd class="mono small">
              {#if detail.image_pinned && detail.image_digest}{detail.image_digest}
              {:else}<span class="muted">{t('not pinned yet')}</span> <span class="faint">({t('follows the tag; this node did not report a digest')})</span>{/if}
            </dd>
          {/if}
          <dt>{t('Mode')}</dt><dd>{db.mode === 'shared' ? t('Shared server (logical database)') : t('Dedicated server')}</dd>
          <dt>{t('Size')}</dt><dd>{db.mode === 'shared' ? t('shared') : `${db.size_preset}${sizePreset ? ` — ${mb(sizePreset.memory_mb)}, ${cpu(sizePreset.cpu_millis)}, ${tn(sizePreset.max_connections, '{n} connection', '{n} connections')}` : ''}`}</dd>
          {#if detail.server}
            <dt>{t('Server')}</dt>
            <dd>
              <Status status={detail.server.status} />
              <span class="faint small">· {detail.server.memory_usage_mb != null ? `${mb(detail.server.memory_usage_mb)} / ` : ''}{mb(detail.server.memory_limit_mb)}</span>
            </dd>
          {/if}
          <dt>{t('Last backup')}</dt><dd title={absTime(db.last_backup_at)}>{relTime(db.last_backup_at)}</dd>
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
      <div class="card-head"><h3>{t('Linked services')}</h3><span class="faint small">{detail.links.length}</span></div>
      {#if detail.links.length}
        <div class="table-wrap">
          <table>
            <thead><tr><th>{t('Service')}</th><th>{t('Variable')}</th><th class="right"></th></tr></thead>
            <tbody>
              {#each detail.links as l (l.service_id)}
                <tr>
                  <td><a href={href(`/services/${l.service_id}`)}>{serviceName(l.service_id)}</a></td>
                  <td><code>{l.env_name}</code></td>
                  <td class="right">{#if isDev}<button class="sm" onclick={() => unlink(l.service_id)} disabled={!!busy}>{t('Unlink')}</button>{/if}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {:else}
        <div class="card-body muted small">{t('No services use this database yet.')}</div>
      {/if}
      {#if isDev && linkable.length}
        <form class="card-body link-form" onsubmit={link}>
          <label class="field">
            <span>{t('Service')}</span>
            <select bind:value={linkService} required>
              <option value="" disabled>{t('Select a service…')}</option>
              {#each linkable as s (s.id)}<option value={s.id}>{s.name}</option>{/each}
            </select>
          </label>
          <label class="field">
            <span>{t('Env variable')}</span>
            <input class="mono" bind:value={linkEnv} placeholder={defaultEnvFor(db.engine, cat)} />
          </label>
          <label class="check small"><input type="checkbox" bind:checked={linkRestart} />{t('Restart service')}</label>
          <button class="primary" type="submit" disabled={!linkService || busy === 'link'}>{#if busy === 'link'}<span class="spinner"></span>{/if}{t('Link')}</button>
        </form>
        {#if linkError}<div class="card-body" style="padding-top:0"><ErrorMsg error={linkError} /></div>{/if}
      {/if}
    </div>

    <div class="card" style="margin-top:16px">
      <div class="card-head">
        <h3>{t('Backups')}</h3>
        {#if isDev}
          <div class="row">
            <button class="sm" onclick={() => act('verify', () => api.verifyBackup(id), t('Restore check passed'))} disabled={!!busy || db.status !== 'ready' || !detail.backup_health?.last_success} title={t('Restore the newest backup into a scratch database, then drop it')}>
              {#if busy === 'verify'}<span class="spinner"></span>{/if}{t('Verify')}
            </button>
            <button class="sm" onclick={() => act('backup', () => api.backupNow(id), t('Backup completed'))} disabled={!!busy || db.status !== 'ready'}>
              {#if busy === 'backup'}<span class="spinner"></span>{/if}{t('Backup now')}
            </button>
          </div>
        {/if}
      </div>
      {#if detail.server?.node_online === false}
        <div class="card-body stack" style="border-bottom:1px solid var(--border)">
          <p><span class="badge red">{t('node offline')}</span> {t("The node holding this database's data is offline. If it is not coming back, recover the database on another node from its newest off-site backup.")}</p>
          {#if isAdmin}
            <div class="row">
              <button class="sm danger" onclick={() => recover(false)} disabled={!!busy}>{#if busy === 'recover'}<span class="spinner"></span>{/if}{t('Recover on another node')}</button>
            </div>
          {/if}
        </div>
      {/if}
      {#if detail.backup_health}
        {@const h = detail.backup_health}
        <div class="card-body small" style="padding-bottom:0">
          <span class="badge {h.state === 'ok' ? 'green' : h.state === 'off' ? '' : h.state === 'none' ? 'yellow' : 'red'}">{h.state === 'ok' ? t('healthy') : h.state === 'none' ? t('no backup') : stateText(h.state)}</span>
          {#if h.offsite}<span class="badge blue">{t('off-site')}</span>{/if}
          {#if h.message}<span class="muted">{h.message}</span>{/if}
          {#if h.last_verified}
            <span class="muted" title={absTime(h.last_verified)}>· {t('restore check {time}: {result}', { time: relTime(h.last_verified), result: h.verify_ok ? t('passed') : t('failed') })}{h.verify_note ? ` (${h.verify_note})` : ''}</span>
          {/if}
        </div>
      {/if}
      {#if backups === null}
        <div class="card-body"><Loading /></div>
      {:else if backups.filter((x) => x.kind !== 'base').length === 0}
        <div class="card-body muted small">{t('No backups yet.')}</div>
      {:else}
        <div class="table-wrap">
          <table>
            <thead><tr><th>{t('Created at')}</th><th>{t('Status')}</th><th class="right">{t('Size')}</th><th class="hide-sm">{t('Duration')}</th><th class="hide-sm">{t('File')}</th><th class="right"></th></tr></thead>
            <tbody>
              {#each backups.filter((x) => x.kind !== 'base') as b (b.id)}
                <tr>
                  <td title={absTime(b.created_at)} class="nowrap">{relTime(b.created_at)}</td>
                  <td>
                    <Status status={b.status === 'completed' ? 'completed' : b.status === 'failed' ? 'failed' : 'deploying'} label={stateText(b.status)} />
                    {#if b.pinned}<span class="badge" title={t('Restore point before a version update: kept until the database is deleted')}>{t('pinned')}</span>{/if}
                    {#if b.error}<div class="small" style="color:var(--red)">{b.error}</div>{/if}
                  </td>
                  <td class="right nowrap">{bytes(b.size_bytes)}</td>
                  <td class="hide-sm mono small">{between(b.created_at, b.finished_at)}</td>
                  <td class="hide-sm mono small faint truncate" style="max-width:260px" title={b.path}>{b.path.split('/').pop()}</td>
                  <td class="right">
                    {#if isAdmin && b.status === 'completed'}
                      <button class="sm" onclick={() => restore(b)} disabled={!!busy}>{#if busy === 'restore-' + b.id}<span class="spinner"></span>{/if}{t('Restore')}</button>
                    {/if}
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
    </div>

    <Modal bind:open={upgradeOpen} title={t('Upgrade to a dedicated server')} width={460}>
      <form id="upgrade-form" class="stack" onsubmit={upgrade}>
        <p class="muted small">{t('Moves {name} from the shared server to its own {engine} container. Data is copied with a dump and restore. Linked services are stopped while it runs and started again with the new URL.', { name: db.name, engine: engineLabel[db.engine] })}</p>
        <label class="field">
          <span>{t('Size')}</span>
          <select bind:value={upgradeSize}>
            {#each dbSizes as s (s.name)}<option value={s.name}>{s.name} — {mb(s.memory_mb)}, {cpu(s.cpu_millis)}</option>{/each}
          </select>
        </label>
      </form>
      {#snippet footer()}
        <button type="button" onclick={() => (upgradeOpen = false)}>{t('Cancel')}</button>
        <button type="submit" form="upgrade-form" class="primary">{t('Upgrade')}</button>
      {/snippet}
    </Modal>
    <Modal bind:open={versionOpen} title={t('Upgrade major version')} width={520}>
      <form id="version-form" class="stack" onsubmit={upgradeVersion}>
        <p class="muted small">
          {t('Copies {name} to a new {engine} server on the version you pick, checks that every table and row count matches, then switches over and deletes the old server.', { name: db.name, engine: engineLabel[db.engine] ?? db.engine })}
          <b>{t('This cannot be undone')}</b>: {t('there is no way back to {version}. A backup taken just before is kept until the database is deleted.', { version: detail?.version })}
        </p>
        <label class="field">
          <span>{t('New version')}</span>
          <select bind:value={versionTarget}>
            {#each upgradeTargets as v (v.name)}<option value={v.name}>{v.name}{v.status === 'ending' ? ` (${t('ends {date}', { date: v.eol })})` : ''}</option>{/each}
          </select>
        </label>
        <label class="row nowrap">
          <input type="checkbox" bind:checked={versionConfirm} />
          <span class="small">{t('I understand that services using this database stop until the upgrade is finished (from a few minutes, longer for big databases).')}</span>
        </label>
      </form>
      {#snippet footer()}
        <button type="button" onclick={() => (versionOpen = false)}>{t('Cancel')}</button>
        <button type="submit" form="version-form" class="primary" disabled={!versionConfirm || !versionTarget}>{t('Upgrade')}</button>
      {/snippet}
    </Modal>
    <Modal bind:open={resizeOpen} title={t('Change size')} width={460}>
      <form id="resize-form" class="stack" onsubmit={resize}>
        <p class="muted small">{t('The server restarts with the new memory and CPU limits (a few seconds of downtime). Data stays on its volume.')}</p>
        <label class="field">
          <span>{t('Size')}</span>
          <select bind:value={resizeSize}>
            {#each dbSizes as s (s.name)}<option value={s.name}>{s.name} — {mb(s.memory_mb)}, {cpu(s.cpu_millis)}, {tn(s.max_connections, '{n} connection', '{n} connections')}</option>{/each}
          </select>
        </label>
      </form>
      {#snippet footer()}
        <button type="button" onclick={() => (resizeOpen = false)}>{t('Cancel')}</button>
        <button type="submit" form="resize-form" class="primary">{t('Resize')}</button>
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
