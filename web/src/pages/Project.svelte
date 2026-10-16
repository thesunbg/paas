<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type ProjectDetail } from '../lib/api';
  import { can } from '../lib/session.svelte';
  import { href, navigate } from '../lib/router.svelte';
  import { mb, relTime, plural } from '../lib/format';
  import { poll } from '../lib/poll';
  import { toast } from '../lib/toast.svelte';
  import { confirmDialog } from '../lib/confirm.svelte';
  import ErrorMsg from '../components/ErrorMsg.svelte';
  import Loading from '../components/Loading.svelte';
  import Icon from '../components/Icon.svelte';
  import Status from '../components/Status.svelte';
  import NewDatabaseDialog from '../components/NewDatabaseDialog.svelte';
  import TemplatesDialog from '../components/TemplatesDialog.svelte';
  import ComposeDialog from '../components/ComposeDialog.svelte';

  let { id }: { id: string } = $props();

  let data = $state<ProjectDetail | null>(null);
  let error = $state('');
  let dbOpen = $state(false);
  let tplOpen = $state(false);
  let composeOpen = $state(false);

  // Preview services are grouped under their parent instead of being top-level cards.
  const topLevel = $derived((data?.services ?? []).filter((s) => !s.parent_id || !data?.services.some((p) => p.id === s.parent_id)));
  const previewsOf = (id: string) => (data?.services ?? []).filter((s) => s.parent_id === id).sort((a, b) => (b.preview_pr ?? 0) - (a.preview_pr ?? 0));

  async function load() {
    try {
      data = await api.project(id);
      error = '';
    } catch (e) {
      error = errorMessage(e);
    }
  }

  onMount(() => {
    load();
    return poll(load, 5000);
  });

  async function deleteProject() {
    if (!data) return;
    const p = data.project;
    const ok = await confirmDialog({
      title: 'Delete project',
      message: `This permanently deletes ${p.name}, its ${data.services.length} service(s) and ${data.databases.length} database(s), including data and backups.`,
      confirmLabel: 'Delete project',
      danger: true,
      requireText: p.name,
    });
    if (!ok) return;
    try {
      await api.deleteProject(p.id);
      toast.success(`Project ${p.name} deleted`);
      navigate('/');
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }

  const typeBadge: Record<string, string> = { web: 'blue', worker: 'purple', static: 'green', cron: 'yellow' };
  const typeIcon: Record<string, string> = { web: 'box', worker: 'zap', static: 'globe', cron: 'clock' };
  const engineLabel: Record<string, string> = { postgres: 'PostgreSQL', mysql: 'MySQL', redis: 'Redis', mongo: 'MongoDB' };

  function source(s: ProjectDetail['services'][number]): string {
    if (s.source.kind === 'image') return s.source.image ?? '';
    const r = (s.source.repo ?? '').replace(/^https?:\/\/(www\.)?/, '').replace(/^file:\/\//, '').replace(/\.git$/, '');
    return r + (s.source.branch ? `@${s.source.branch}` : '');
  }
</script>

<div class="page">
  {#if !data && !error}
    <Loading rows={4} />
  {:else if !data}
    <ErrorMsg {error} onretry={load} />
  {:else}
    {@const p = data.project}
    <div class="crumbs"><a href={href('/')}>Projects</a><Icon name="chevron" size={12} /><span>{p.name}</span></div>
    <div class="page-head">
      <div>
        <h1>{p.name}</h1>
        <p class="sub">
          <span class="mono">{p.slug}</span> · {plural(topLevel.length, 'service')}{data.services.length > topLevel.length ? ` (+${plural(data.services.length - topLevel.length, 'preview')})` : ''} · {plural(data.databases.length, 'database')} · created {relTime(p.created_at)}
        </p>
      </div>
      <div class="row">
        {#if can('developer', p.team_id)}
          <button onclick={() => (tplOpen = true)}><Icon name="grid" />Templates</button>
          <button onclick={() => (composeOpen = true)}><Icon name="upload" />Import compose</button>
          <button onclick={() => (dbOpen = true)}><Icon name="database" />New database</button>
          <a class="btn primary" href={href(`/projects/${p.id}/new-service`)}><Icon name="plus" />New service</a>
        {/if}
        {#if can('admin', p.team_id)}
          <button class="danger icon" aria-label="Delete project" title="Delete project" onclick={deleteProject}><Icon name="trash" /></button>
        {/if}
      </div>
    </div>
    <ErrorMsg {error} />

    <section class="stack">
      <h2 class="section-title">Services</h2>
      {#if data.services.length === 0}
        <div class="card empty">
          <Icon name="box" size={28} />
          <p>No services yet. Deploy from a Git repository or a Docker image, start from a template, or import a docker-compose file.</p>
          {#if can('developer', p.team_id)}
            <div class="row" style="justify-content:center">
              <a class="btn primary" href={href(`/projects/${p.id}/new-service`)}><Icon name="plus" />New service</a>
              <button onclick={() => (tplOpen = true)}><Icon name="grid" />From template</button>
              <button onclick={() => (composeOpen = true)}><Icon name="upload" />Import compose</button>
            </div>
          {/if}
        </div>
      {:else}
        <div class="svc-grid">
          {#each topLevel as s (s.id)}
            {@const pvs = previewsOf(s.id)}
            <div class="card svc">
              <a class="cover" href={href(`/services/${s.id}`)} aria-label="Open {s.name}"></a>
              <div class="between nowrap">
                <div class="row nowrap" style="min-width:0">
                  <span class="sic"><Icon name={typeIcon[s.type] ?? 'box'} /></span>
                  <h3 class="truncate">{s.name}</h3>
                </div>
                <span class="badge {typeBadge[s.type] ?? ''}">{s.type}</span>
              </div>
              <div class="faint small mono truncate" title={source(s)}><Icon name={s.source.kind === 'image' ? 'layers' : 'git'} size={12} /> {source(s)}</div>
              {#if s.url}
                <a class="url small truncate" href={s.url} target="_blank" rel="noreferrer">{s.url.replace(/^https?:\/\//, '')}<Icon name="external" size={12} /></a>
              {:else if s.type === 'cron'}
                <span class="faint small mono"><Icon name="clock" size={12} /> {s.spec.schedule} UTC</span>
              {:else}
                <span class="faint small">{s.type === 'worker' ? 'No public URL (worker)' : 'No domain'}</span>
              {/if}
              <div class="between small foot">
                <Status status={s.status} />
                <span class="muted">
                  {#if s.type === 'cron'}
                    cron job
                  {:else if s.type !== 'static'}
                    {s.instances}/{s.spec.replicas}{s.spec.autoscale?.enabled ? ` (auto ${s.spec.autoscale.min ?? 1}–${s.spec.autoscale.max ?? 3})` : ''} inst · {mb(s.memory_mb)} / {mb(s.spec.resources.memory_limit_mb)}
                  {:else}
                    static
                  {/if}
                </span>
              </div>
              {#if pvs.length}
                <ul class="pvs" aria-label="Pull request previews of {s.name}">
                  {#each pvs as pv (pv.id)}
                    <li>
                      <a href={href(`/services/${pv.id}`)} class="pv">
                        <span class="dot {pv.status}"></span>
                        <span class="badge purple">PR #{pv.preview_pr}</span>
                        <span class="truncate small">{pv.name}</span>
                        <span class="spacer"></span>
                        <span class="faint small">{pv.status}</span>
                      </a>
                    </li>
                  {/each}
                </ul>
              {/if}
            </div>
          {/each}
        </div>
      {/if}
    </section>

    <section class="stack" style="margin-top:28px">
      <h2 class="section-title">Databases</h2>
      {#if data.databases.length === 0}
        <div class="card empty">
          <Icon name="database" size={28} />
          <p>No databases. Add PostgreSQL, MySQL, Redis or MongoDB.</p>
          {#if can('developer', p.team_id)}<button onclick={() => (dbOpen = true)}><Icon name="plus" />New database</button>{/if}
        </div>
      {:else}
        <div class="card table-wrap">
          <table>
            <thead><tr><th>Name</th><th>Engine</th><th>Mode</th><th class="hide-sm">Size</th><th>Status</th><th class="hide-sm">Last backup</th></tr></thead>
            <tbody>
              {#each data.databases as d (d.id)}
                <tr class="clickable" onclick={() => navigate(`/databases/${d.id}`)}>
                  <td><a href={href(`/databases/${d.id}`)}><Icon name="database" size={14} /> {d.name}</a></td>
                  <td>{engineLabel[d.engine] ?? d.engine} <span class="faint small">{d.version ?? ""}</span></td>
                  <td><span class="badge {d.mode === 'dedicated' ? 'blue' : ''}">{d.mode}</span></td>
                  <td class="hide-sm mono small">{d.mode === 'dedicated' ? d.size_preset : '—'}</td>
                  <td><Status status={d.status} /></td>
                  <td class="hide-sm muted">{relTime(d.last_backup_at)}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
    </section>

    <NewDatabaseDialog bind:open={dbOpen} projectId={p.id} services={data.services} />
    <TemplatesDialog bind:open={tplOpen} projectId={p.id} ondone={load} />
    <ComposeDialog bind:open={composeOpen} projectId={p.id} ondone={load} />
  {/if}
</div>

<style>
  .svc-grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(270px, 1fr));
    gap: 12px;
  }
  .svc {
    position: relative;
    padding: 14px;
    display: flex;
    flex-direction: column;
    gap: 8px;
    transition: border-color 0.12s, background 0.12s;
  }
  .svc:hover {
    border-color: var(--border-strong);
    background: var(--bg-elev-2);
  }
  .cover {
    position: absolute;
    inset: 0;
    border-radius: var(--radius);
    z-index: 0;
  }
  .svc > :not(.cover) {
    position: relative;
    z-index: 1;
    pointer-events: none;
  }
  .svc .url {
    pointer-events: auto;
    display: inline-flex;
    align-items: center;
    gap: 4px;
    align-self: flex-start;
    max-width: 100%;
  }
  .sic {
    width: 28px;
    height: 28px;
    border-radius: 7px;
    border: 1px solid var(--border-strong);
    display: inline-flex;
    align-items: center;
    justify-content: center;
    color: var(--text-dim);
    flex: none;
  }
  .pvs {
    list-style: none;
    margin: 0;
    padding: 6px 0 0;
    border-top: 1px dashed var(--border);
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .svc .pvs {
    pointer-events: auto;
  }
  .pv {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 3px 4px;
    border-radius: 4px;
    color: var(--text);
  }
  .pv:hover {
    background: var(--bg-hover);
    text-decoration: none;
  }
  .foot {
    border-top: 1px solid var(--border);
    padding-top: 10px;
    margin-top: 2px;
  }
</style>
