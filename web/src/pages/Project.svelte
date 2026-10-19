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

  // Search, filters, sort and layout. Layout and sort are remembered per browser.
  type Svc = ProjectDetail['services'][number];
  const PREFS = 'paas.project.view';
  function loadPrefs(): { view: 'grid' | 'list'; sort: string } {
    try {
      const v = JSON.parse(localStorage.getItem(PREFS) ?? '{}');
      return { view: v.view === 'list' ? 'list' : 'grid', sort: typeof v.sort === 'string' ? v.sort : 'status' };
    } catch {
      return { view: 'grid', sort: 'status' };
    }
  }
  const prefs = loadPrefs();
  let query = $state('');
  let statusFilter = $state<'all' | 'running' | 'failed' | 'deploying' | 'idle'>('all');
  let typeFilter = $state('all');
  let sort = $state(prefs.sort);
  let view = $state<'grid' | 'list'>(prefs.view);
  let searchEl = $state<HTMLInputElement | null>(null);
  $effect(() => {
    const v = JSON.stringify({ view, sort });
    try {
      localStorage.setItem(PREFS, v);
    } catch {
      /* private mode */
    }
  });

  const statusGroup = (s: string) =>
    s === 'running' ? 'running' : s === 'failed' ? 'failed' : s === 'deploying' || s === 'created' ? 'deploying' : 'idle';
  const statusRank: Record<string, number> = { failed: 0, deploying: 1, running: 2, idle: 3 };
  const counts = $derived.by(() => {
    const c: Record<string, number> = { all: topLevel.length, running: 0, failed: 0, deploying: 0, idle: 0 };
    for (const s of topLevel) c[statusGroup(s.status)]++;
    return c;
  });
  const types = $derived([...new Set(topLevel.map((s) => s.type))].sort());

  function haystack(s: Svc): string {
    return [s.name, s.slug, s.type, s.status, s.url ?? '', source(s), ...previewsOf(s.id).map((p) => `${p.name} #${p.preview_pr}`)].join(' ').toLowerCase();
  }
  const terms = $derived(query.toLowerCase().split(/\s+/).filter(Boolean));
  const matches = (text: string) => terms.every((t) => text.includes(t));
  const visible = $derived.by(() => {
    const list = topLevel.filter(
      (s) => (statusFilter === 'all' || statusGroup(s.status) === statusFilter) && (typeFilter === 'all' || s.type === typeFilter) && matches(haystack(s)),
    );
    const byName = (a: Svc, b: Svc) => a.name.localeCompare(b.name);
    if (sort === 'name') list.sort(byName);
    else if (sort === 'recent') list.sort((a, b) => b.updated_at.localeCompare(a.updated_at));
    else if (sort === 'memory') list.sort((a, b) => b.memory_mb - a.memory_mb || byName(a, b));
    else list.sort((a, b) => statusRank[statusGroup(a.status)] - statusRank[statusGroup(b.status)] || byName(a, b));
    return list;
  });
  const visibleDBs = $derived(
    (data?.databases ?? []).filter((d) => matches([d.name, d.engine, d.mode, d.status, d.version ?? ''].join(' ').toLowerCase())),
  );
  const filtered = $derived(terms.length > 0 || statusFilter !== 'all' || typeFilter !== 'all');
  function clearFilters() {
    query = '';
    statusFilter = 'all';
    typeFilter = 'all';
  }

  // "/" focuses the search box, Escape clears it.
  function onKey(e: KeyboardEvent) {
    const t = e.target as HTMLElement;
    const typing = t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.tagName === 'SELECT' || t.isContentEditable);
    if (e.key === '/' && !typing) {
      e.preventDefault();
      searchEl?.focus();
    }
  }

  // Rename (display name only; the slug in domains stays).
  let editing = $state(false);
  let newName = $state('');
  let renaming = $state(false);
  function startRename() {
    if (!data) return;
    newName = data.project.name;
    editing = true;
  }
  async function rename(e?: Event) {
    e?.preventDefault();
    if (!data) return;
    const name = newName.trim();
    if (!name || name === data.project.name) {
      editing = false;
      return;
    }
    renaming = true;
    try {
      data.project = await api.renameProject(data.project.id, name);
      editing = false;
      toast.success('Project renamed');
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      renaming = false;
    }
  }
  function focusSelect(el: HTMLInputElement) {
    el.focus();
    el.select();
  }

  // "Add from…" menu (templates, compose).
  let addOpen = $state(false);
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

<svelte:window onkeydown={onKey} />

<div class="page">
  {#if !data && !error}
    <Loading rows={4} />
  {:else if !data}
    <ErrorMsg {error} onretry={load} />
  {:else}
    {@const p = data.project}
    <div class="crumbs"><a href={href('/')}>Projects</a><Icon name="chevron" size={12} /><span>{p.name}</span></div>
    <div class="page-head">
      <div style="min-width:0">
        {#if editing}
          <form class="rename" onsubmit={rename}>
            <input
              class="title-input"
              bind:value={newName}
              maxlength="80"
              aria-label="Project name"
              use:focusSelect
              onkeydown={(e) => e.key === 'Escape' && (editing = false)}
              disabled={renaming}
            />
            <button class="primary sm" disabled={renaming || !newName.trim()}>{#if renaming}<span class="spinner"></span>{/if}Save</button>
            <button type="button" class="sm" onclick={() => (editing = false)} disabled={renaming}>Cancel</button>
          </form>
        {:else}
          <div class="row nowrap title">
            <h1 class="truncate">{p.name}</h1>
            {#if can('admin', p.team_id)}
              <button class="ghost icon sm" aria-label="Rename project" title="Rename project" onclick={startRename}><Icon name="pencil" size={14} /></button>
            {/if}
          </div>
        {/if}
        <p class="sub">
          <span class="mono" title="Used in domains and internal DNS names; it does not change when the project is renamed">{p.slug}</span> · {plural(topLevel.length, 'service')}{data.services.length > topLevel.length ? ` (+${plural(data.services.length - topLevel.length, 'preview')})` : ''} · {plural(data.databases.length, 'database')} · created {relTime(p.created_at)}
        </p>
      </div>
      <div class="row">
        {#if can('developer', p.team_id)}
          <div class="menu-wrap">
            <button class="add-from" onclick={() => (addOpen = !addOpen)} aria-haspopup="menu" aria-expanded={addOpen}><Icon name="grid" />Add from<Icon name="chevdown" size={12} /></button>
            {#if addOpen}
              <div class="menu-backdrop" role="presentation" onclick={() => (addOpen = false)}></div>
              <div class="menu" role="menu">
                <button role="menuitem" onclick={() => ((addOpen = false), (tplOpen = true))}><Icon name="grid" /><span><b>Template</b><br /><span class="faint small">WordPress, n8n and more</span></span></button>
                <button role="menuitem" onclick={() => ((addOpen = false), (composeOpen = true))}><Icon name="upload" /><span><b>docker-compose file</b><br /><span class="faint small">Paste or upload a compose file</span></span></button>
              </div>
            {/if}
          </div>
          {#if data.databases.length}
            <a class="btn" href={href(`/projects/${p.id}/studio`)} target="_blank" rel="noopener" title="Browse and query this project's databases in dbweb"><Icon name="terminal" />DB studio</a>
          {/if}
          <button onclick={() => (dbOpen = true)}><Icon name="database" />New database</button>
          <a class="btn primary" href={href(`/projects/${p.id}/new-service`)}><Icon name="plus" />New service</a>
        {/if}
        {#if can('admin', p.team_id)}
          <button class="danger icon" aria-label="Delete project" title="Delete project" onclick={deleteProject}><Icon name="trash" /></button>
        {/if}
      </div>
    </div>
    <ErrorMsg {error} />

    {#if topLevel.length > 0 || data.databases.length > 0}
      <div class="toolbar">
        <label class="search">
          <Icon name="search" size={14} />
          <input
            type="search"
            bind:this={searchEl}
            bind:value={query}
            placeholder="Search services and databases"
            aria-label="Search services and databases"
            onkeydown={(e) => e.key === 'Escape' && (query = '')}
          />
          {#if !query}<kbd class="faint">/</kbd>{/if}
        </label>
        <div class="seg" role="group" aria-label="Filter by status">
          {#each [['all', 'All'], ['running', 'Running'], ['failed', 'Failed'], ['deploying', 'Deploying'], ['idle', 'Stopped']] as [k, label] (k)}
            {#if k === 'all' || counts[k] > 0}
              <button class:sel={statusFilter === k} class:bad={k === 'failed'} aria-pressed={statusFilter === k} onclick={() => (statusFilter = k as typeof statusFilter)}>
                {#if k !== 'all'}<span class="dot {k === 'idle' ? 'stopped' : k}"></span>{/if}{label}<span class="count">{counts[k]}</span>
              </button>
            {/if}
          {/each}
        </div>
        <span class="spacer"></span>
        {#if types.length > 1}
          <select bind:value={typeFilter} aria-label="Filter by type">
            <option value="all">All types</option>
            {#each types as t (t)}<option value={t}>{t}</option>{/each}
          </select>
        {/if}
        <select bind:value={sort} aria-label="Sort">
          <option value="status">Sort: status</option>
          <option value="name">Sort: name</option>
          <option value="recent">Sort: recently updated</option>
          <option value="memory">Sort: memory</option>
        </select>
        <div class="seg" role="group" aria-label="Layout">
          <button class:sel={view === 'grid'} aria-pressed={view === 'grid'} aria-label="Grid" title="Grid" onclick={() => (view = 'grid')}><Icon name="grid" size={14} /></button>
          <button class:sel={view === 'list'} aria-pressed={view === 'list'} aria-label="List" title="List" onclick={() => (view = 'list')}><Icon name="list" size={14} /></button>
        </div>
      </div>
    {/if}

    <section class="stack">
      <h2 class="section-title">Services{#if filtered && topLevel.length}<span class="faint small"> · {visible.length} of {topLevel.length}</span>{/if}</h2>
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
      {:else if visible.length === 0}
        <div class="card empty">
          <Icon name="search" size={24} />
          <p>No services match{query ? ` “${query}”` : ' these filters'}.</p>
          <button onclick={clearFilters}>Clear filters</button>
        </div>
      {:else if view === 'list'}
        <div class="card table-wrap">
          <table>
            <thead><tr><th>Name</th><th>Status</th><th class="hide-sm">Source</th><th>URL</th><th class="hide-sm num">Instances</th><th class="hide-sm num">Memory</th></tr></thead>
            <tbody>
              {#each visible as s (s.id)}
                {@const pvs = previewsOf(s.id)}
                <tr class="clickable" class:failed={s.status === 'failed'} onclick={() => navigate(`/services/${s.id}`)}>
                  <td>
                    <a href={href(`/services/${s.id}`)} class="row nowrap name"><Icon name={typeIcon[s.type] ?? 'box'} size={14} /><b>{s.name}</b></a>
                    <span class="badge {typeBadge[s.type] ?? ''}">{s.type}</span>
                    {#if pvs.length}<span class="badge purple">{plural(pvs.length, 'preview')}</span>{/if}
                  </td>
                  <td><Status status={s.status} /></td>
                  <td class="hide-sm faint small mono truncate cell-src" title={source(s)}>{source(s)}</td>
                  <td class="small">
                    {#if s.url}<a href={s.url} target="_blank" rel="noreferrer" onclick={(e) => e.stopPropagation()} class="truncate url-cell">{s.url.replace(/^https?:\/\//, '')}</a>{:else}<span class="faint">{s.type === 'cron' ? s.spec.schedule + ' UTC' : '—'}</span>{/if}
                  </td>
                  <td class="hide-sm num muted small">{s.type === 'static' || s.type === 'cron' ? '—' : `${s.instances}/${s.spec.replicas}`}</td>
                  <td class="hide-sm num muted small">{s.type === 'static' || s.type === 'cron' ? '—' : `${mb(s.memory_mb)} / ${mb(s.spec.resources.memory_limit_mb)}`}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {:else}
        <div class="svc-grid">
          {#each visible as s (s.id)}
            {@const pvs = previewsOf(s.id)}
            <div class="card svc" class:failed={s.status === 'failed'}>
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
      <h2 class="section-title">Databases{#if terms.length && data.databases.length}<span class="faint small"> · {visibleDBs.length} of {data.databases.length}</span>{/if}</h2>
      {#if data.databases.length === 0}
        <div class="card empty">
          <Icon name="database" size={28} />
          <p>No databases. Add PostgreSQL, MySQL, Redis or MongoDB.</p>
          {#if can('developer', p.team_id)}<button onclick={() => (dbOpen = true)}><Icon name="plus" />New database</button>{/if}
        </div>
      {:else if visibleDBs.length === 0}
        <p class="faint small">No databases match “{query}”.</p>
      {:else}
        <div class="card table-wrap">
          <table>
            <thead><tr><th>Name</th><th>Engine</th><th>Mode</th><th class="hide-sm">Size</th><th>Status</th><th class="hide-sm">Last backup</th></tr></thead>
            <tbody>
              {#each visibleDBs as d (d.id)}
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
  .title {
    gap: 6px;
  }
  .title h1 {
    min-width: 0;
  }
  .rename {
    display: flex;
    gap: 8px;
    align-items: center;
    flex-wrap: wrap;
  }
  .title-input {
    font-size: 20px;
    font-weight: 600;
    padding: 2px 8px;
    height: 36px;
    width: min(420px, 100%);
    flex: 0 1 auto;
  }
  .menu-wrap {
    position: relative;
  }
  .menu-backdrop {
    position: fixed;
    inset: 0;
    z-index: 20;
  }
  .menu {
    position: absolute;
    right: 0;
    top: calc(100% + 6px);
    z-index: 21;
    min-width: 260px;
    padding: 6px;
    background: var(--bg-elev);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    box-shadow: 0 12px 32px rgba(0, 0, 0, 0.35);
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .menu button {
    display: flex;
    align-items: flex-start;
    gap: 10px;
    text-align: left;
    border: 0;
    background: transparent;
    padding: 8px 10px;
    height: auto;
    width: 100%;
    justify-content: flex-start;
  }
  .menu button:hover {
    background: var(--bg-hover);
  }
  .toolbar {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 8px;
    margin-bottom: 16px;
  }
  .search {
    position: relative;
    display: flex;
    align-items: center;
    flex: 1 1 260px;
    max-width: 380px;
    color: var(--text-dim);
  }
  .search :global(svg) {
    position: absolute;
    left: 10px;
    pointer-events: none;
  }
  .search input {
    width: 100%;
    padding-left: 32px;
    padding-right: 28px;
  }
  .search kbd {
    position: absolute;
    right: 8px;
    font-size: 11px;
    border: 1px solid var(--border-strong);
    border-radius: 4px;
    padding: 0 5px;
    pointer-events: none;
  }
  .seg {
    display: inline-flex;
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 2px;
    gap: 2px;
    background: var(--bg-input);
  }
  .seg button {
    border: 0;
    background: transparent;
    height: 28px;
    padding: 0 10px;
    display: inline-flex;
    align-items: center;
    gap: 6px;
    font-size: 13px;
    color: var(--text-dim);
    border-radius: 6px;
  }
  .seg button:hover {
    color: var(--text);
  }
  .seg button.sel {
    background: var(--bg-hover);
    color: var(--text);
  }
  .seg button.bad .count {
    color: var(--red);
  }
  .count {
    font-variant-numeric: tabular-nums;
    color: var(--text-faint, var(--text-dim));
    font-size: 12px;
  }
  .spacer {
    flex: 1;
  }
  .toolbar select {
    width: auto;
    height: 34px;
  }
  .section-title .faint {
    text-transform: none;
    letter-spacing: 0;
    font-weight: 400;
  }
  .svc.failed {
    border-color: color-mix(in srgb, var(--red) 45%, var(--border));
  }
  tr.failed td:first-child {
    box-shadow: inset 3px 0 0 var(--red);
  }
  .name {
    display: inline-flex;
    gap: 6px;
    margin-right: 6px;
    color: var(--text);
  }
  .cell-src {
    max-width: 260px;
  }
  .url-cell {
    display: inline-block;
    max-width: 280px;
    vertical-align: bottom;
  }
  .num {
    text-align: right;
    white-space: nowrap;
  }
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
