<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type ServiceDetail } from '../lib/api';
  import { can } from '../lib/session.svelte';
  import { href } from '../lib/router.svelte';
  import { poll } from '../lib/poll';
  import ErrorMsg from '../components/ErrorMsg.svelte';
  import Loading from '../components/Loading.svelte';
  import Icon from '../components/Icon.svelte';
  import Status from '../components/Status.svelte';
  import Overview from './service/Overview.svelte';
  import Deployments from './service/Deployments.svelte';
  import DeploymentLog from './service/DeploymentLog.svelte';
  import Logs from './service/Logs.svelte';
  import Env from './service/Env.svelte';
  import Domains from './service/Domains.svelte';
  import SettingsTab from './service/Settings.svelte';
  import Console from './service/Console.svelte';
  import Runs from './service/Runs.svelte';
  import Metrics from './service/Metrics.svelte';
  import Alerts from './service/Alerts.svelte';

  let { id, tab = 'overview', sub }: { id: string; tab?: string; sub?: string } = $props();

  let detail = $state<ServiceDetail | null>(null);
  let error = $state('');

  let parentName = $state('');
  async function load() {
    try {
      detail = await api.service(id);
      error = '';
      const pid = detail.service.parent_id;
      if (pid && !parentName) api.service(pid).then((p) => (parentName = p.service.name)).catch(() => {});
    } catch (e) {
      error = errorMessage(e);
    }
  }

  onMount(() => {
    load();
    return poll(load, 5000);
  });

  const isDev = $derived(detail ? can('developer', detail.project.team_id) : false);

  const kind = $derived(detail?.service.type);
  const tabs = $derived(
    [
      { id: 'overview', label: 'Overview' },
      { id: 'runs', label: 'Runs', hide: kind !== 'cron' },
      { id: 'deployments', label: 'Deployments' },
      { id: 'metrics', label: 'Metrics', hide: kind === 'static' || kind === 'cron' },
      { id: 'alerts', label: 'Alerts', hide: kind === 'cron' || kind === 'database' },
      { id: 'logs', label: 'Logs', hide: kind === 'static' || kind === 'cron' },
      { id: 'env', label: 'Environment' },
      { id: 'domains', label: 'Domains', hide: kind === 'worker' || kind === 'cron' },
      { id: 'settings', label: 'Settings' },
      { id: 'console', label: 'Console', hide: !isDev || kind === 'static' || kind === 'cron' },
    ].filter((t) => !t.hide),
  );

  const typeBadge: Record<string, string> = { web: 'blue', worker: 'purple', static: 'green', cron: 'yellow' };
</script>

<div class="page">
  {#if !detail && !error}
    <Loading rows={4} />
  {:else if !detail}
    <ErrorMsg {error} onretry={load} />
  {:else}
    {@const s = detail.service}
    <div class="crumbs">
      <a href={href('/')}>Projects</a><Icon name="chevron" size={12} />
      <a href={href(`/projects/${detail.project.id}`)}>{detail.project.name}</a><Icon name="chevron" size={12} />
      <span>{s.name}</span>
    </div>
    <div class="page-head head">
      <div style="min-width:0">
        <div class="row">
          <h1 class="truncate">{s.name}</h1>
          <span class="badge {typeBadge[s.type] ?? ''}">{s.type}</span>
          <Status status={s.status} />
        </div>
        <p class="sub row" style="gap:12px">
          {#if detail.urls[0]}
            <a href={detail.urls[0]} target="_blank" rel="noreferrer" class="row nowrap" style="gap:4px">{detail.urls[0].replace(/^https?:\/\//, '')}<Icon name="external" size={12} /></a>
          {/if}
          {#if s.type !== 'cron'}<span class="mono faint small">{detail.internal_host}</span>{:else}<span class="faint small">Scheduled job · <code>{s.spec.schedule}</code> UTC</span>{/if}
        </p>
      </div>
    </div>
    {#if error}<div style="margin-bottom:12px"><ErrorMsg {error} /></div>{/if}
    {#if s.parent_id}
      <a class="alert info preview" href={href(`/services/${s.parent_id}`)}>
        <Icon name="branch" />
        <span style="flex:1">Preview of <b>{parentName || 'parent service'}</b> · PR #{s.preview_pr}. Sleeps when idle and is deleted when the pull request closes.</span>
        <span class="nowrap">Open parent →</span>
      </a>
    {/if}

    <div class="tabs" role="tablist" aria-label="Service sections">
      {#each tabs as t (t.id)}
        <a role="tab" class="tab" class:active={tab === t.id} aria-selected={tab === t.id} href={href(`/services/${id}/${t.id}`)}>{t.label}</a>
      {/each}
    </div>

    <div role="tabpanel">
      {#if tab === 'overview'}
        <Overview {detail} reload={load} canEdit={isDev} />
      {:else if tab === 'deployments' && sub}
        {#key sub}
          <DeploymentLog serviceId={id} deploymentId={sub} {detail} reload={load} canEdit={isDev} />
        {/key}
      {:else if tab === 'deployments'}
        <Deployments {detail} reload={load} canEdit={isDev} />
      {:else if tab === 'runs'}
        <Runs {detail} reload={load} canEdit={isDev} />
      {:else if tab === 'metrics'}
        <Metrics {detail} />
      {:else if tab === 'alerts'}
        <Alerts {detail} canEdit={isDev} />
      {:else if tab === 'logs'}
        <Logs serviceId={id} />
      {:else if tab === 'env'}
        <Env serviceId={id} {detail} reload={load} canEdit={isDev} />
      {:else if tab === 'domains'}
        <Domains {detail} reload={load} canEdit={isDev} />
      {:else if tab === 'settings'}
        <SettingsTab {detail} reload={load} canEdit={isDev} />
      {:else if tab === 'console'}
        <Console {detail} />
      {:else}
        <p class="muted">Unknown tab.</p>
      {/if}
    </div>
  {/if}
</div>

<style>
  .head {
    margin-bottom: 14px;
  }
  .preview {
    margin-bottom: 14px;
    color: var(--text);
    align-items: center;
  }
  .preview:hover {
    text-decoration: none;
  }
  .tabs {
    display: flex;
    gap: 2px;
    border-bottom: 1px solid var(--border);
    margin-bottom: 20px;
    overflow-x: auto;
    scrollbar-width: none;
  }
  .tab {
    padding: 8px 12px;
    color: var(--text-dim);
    font-weight: 500;
    font-size: 13.5px;
    border-bottom: 2px solid transparent;
    margin-bottom: -1px;
    white-space: nowrap;
  }
  .tab:hover {
    color: var(--text);
    text-decoration: none;
  }
  .tab.active {
    color: var(--text);
    border-bottom-color: var(--text);
  }
</style>
