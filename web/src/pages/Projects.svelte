<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type Overview, type ProjectListItem } from '../lib/api';
  import { session, currentTeam, can } from '../lib/session.svelte';
  import { href, navigate } from '../lib/router.svelte';
  import { relTime, plural } from '../lib/format';
  import { poll } from '../lib/poll';
  import { toast } from '../lib/toast.svelte';
  import Modal from '../components/Modal.svelte';
  import ErrorMsg from '../components/ErrorMsg.svelte';
  import Icon from '../components/Icon.svelte';
  import { t } from '../lib/i18n.svelte';

  let overview = $state<Overview | null>(null);
  let projects = $state<ProjectListItem[] | null>(null);
  let error = $state('');
  let newOpen = $state(false);
  let newName = $state('');
  let creating = $state(false);
  let createError = $state('');

  const team = $derived(currentTeam());

  async function load() {
    const tm = team;
    if (!tm) {
      projects = [];
      return;
    }
    try {
      const [o, ps] = await Promise.all([api.overview(), api.projects(tm.id)]);
      overview = o;
      projects = ps ?? [];
      error = '';
    } catch (e) {
      error = errorMessage(e);
    }
  }

  $effect(() => {
    void session.teamId;
    projects = null;
    load();
  });

  onMount(() => poll(load, 5000));

  async function create(e: SubmitEvent) {
    e.preventDefault();
    if (!team || !newName.trim()) return;
    creating = true;
    createError = '';
    try {
      const p = await api.createProject(team.id, newName.trim());
      toast.success(t('Project {name} created', { name: p.name }));
      newOpen = false;
      newName = '';
      navigate(`/projects/${p.id}`);
    } catch (err) {
      createError = errorMessage(err);
    } finally {
      creating = false;
    }
  }

  const stats = $derived(
    overview
      ? [
          { label: t('Services'), value: overview.services, icon: 'box' },
          { label: t('Running'), value: overview.running, icon: 'activity', cls: 'green' },
          { label: t('Sleeping'), value: overview.sleeping, icon: 'moon', cls: 'purple' },
          { label: t('Databases'), value: overview.databases, icon: 'database' },
          ...(overview.nodes != null ? [{ label: t('Nodes'), value: overview.nodes, icon: 'server', link: '/nodes' }] : []),
        ]
      : [],
  );
</script>

<div class="page">
  <div class="page-head">
    <div>
      <h1>{t('Projects')}</h1>
      <p class="sub">{team ? team.name : t('No team selected')}</p>
    </div>
    {#if can('developer')}
      <button class="primary" onclick={() => ((newOpen = true), (createError = ''))}><Icon name="plus" />{t('New project')}</button>
    {/if}
  </div>

  {#if overview}
    <div class="stats">
      {#each stats as s (s.label)}
        {#if s.link}
          <a class="stat card" href={href(s.link)}>
            <span class="lbl"><Icon name={s.icon} size={14} />{s.label}</span><span class="val">{s.value}</span>
          </a>
        {:else}
          <div class="stat card">
            <span class="lbl"><Icon name={s.icon} size={14} />{s.label}</span><span class="val {s.cls ?? ''}">{s.value}</span>
          </div>
        {/if}
      {/each}
    </div>
  {/if}

  <ErrorMsg {error} onretry={load} />

  {#if projects === null && !error}
    <div class="grid"><div class="skeleton" style="height:120px"></div><div class="skeleton" style="height:120px"></div><div class="skeleton" style="height:120px"></div></div>
  {:else if projects && projects.length === 0}
    <div class="card empty">
      <Icon name="folder" size={28} />
      <h3>{t('No projects yet')}</h3>
      <p class="small">{t('Projects group services and databases that share a private network.')}</p>
      {#if can('developer')}<button class="primary" onclick={() => (newOpen = true)}><Icon name="plus" />{t('Create your first project')}</button>{/if}
    </div>
  {:else if projects}
    <div class="grid">
      {#each projects as p (p.id)}
        <a class="card proj" href={href(`/projects/${p.id}`)}>
          <div class="between nowrap">
            <div class="row nowrap" style="min-width:0">
              <span class="pav" aria-hidden="true">{p.name.slice(0, 1).toUpperCase()}</span>
              <div style="min-width:0">
                <h3 class="truncate">{p.name}</h3>
                <div class="faint small mono truncate">{p.slug}</div>
              </div>
            </div>
            <Icon name="chevron" />
          </div>
          <div class="meta">
            <span><Icon name="box" size={13} />{plural(p.services, 'service')}</span>
            <span><Icon name="database" size={13} />{plural(p.databases, 'database')}</span>
          </div>
          <div class="between small">
            <span class="row nowrap" style="gap:6px">
              <span class="dot {p.running > 0 ? 'running' : p.services ? 'stopped' : 'created'}"></span>
              <span class="muted">{t('{running}/{total} running', { running: p.running, total: p.services })}</span>
            </span>
            <span class="faint">{t('Created {time}', { time: relTime(p.created_at) })}</span>
          </div>
        </a>
      {/each}
    </div>
  {/if}
</div>

<Modal bind:open={newOpen} title={t('New project')} width={440}>
  <form id="new-project" class="stack" onsubmit={create}>
    <label class="field">
      <span>{t('Project name')}</span>
      <input bind:value={newName} required placeholder="my-app" maxlength="64" />
      <span class="hint">{t('Services in a project reach each other at')} <code>&lt;service&gt;.&lt;project&gt;.internal</code>.</span>
    </label>
    <ErrorMsg error={createError} />
  </form>
  {#snippet footer()}
    <button type="button" onclick={() => (newOpen = false)}>{t('Cancel')}</button>
    <button type="submit" form="new-project" class="primary" disabled={creating || !newName.trim()}>
      {#if creating}<span class="spinner"></span>{/if}{t('Create project')}
    </button>
  {/snippet}
</Modal>


<style>
  .stats {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(140px, 1fr));
    gap: 10px;
    margin-bottom: 20px;
  }
  .stat {
    padding: 12px 14px;
    display: flex;
    flex-direction: column;
    gap: 4px;
    color: var(--text);
  }
  a.stat:hover {
    text-decoration: none;
    border-color: var(--border-strong);
  }
  .lbl {
    display: flex;
    align-items: center;
    gap: 6px;
    color: var(--text-faint);
    font-size: 12px;
  }
  .val {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.02em;
    font-variant-numeric: tabular-nums;
  }
  .val.green {
    color: var(--green);
  }
  .val.purple {
    color: var(--purple);
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(280px, 1fr));
    gap: 12px;
  }
  .proj {
    padding: 16px;
    display: flex;
    flex-direction: column;
    gap: 14px;
    color: var(--text);
    transition: border-color 0.12s, background 0.12s;
  }
  .proj:hover {
    text-decoration: none;
    border-color: var(--border-strong);
    background: var(--bg-elev-2);
  }
  .pav {
    width: 34px;
    height: 34px;
    border-radius: 8px;
    background: var(--bg-hover);
    border: 1px solid var(--border-strong);
    display: inline-flex;
    align-items: center;
    justify-content: center;
    font-weight: 600;
    flex: none;
  }
  .meta {
    display: flex;
    gap: 14px;
    color: var(--text-dim);
    font-size: 12.5px;
  }
  .meta span {
    display: inline-flex;
    align-items: center;
    gap: 5px;
  }
</style>
