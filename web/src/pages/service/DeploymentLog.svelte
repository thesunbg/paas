<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type Deployment, type ServiceDetail } from '../../lib/api';
  import { href, navigate } from '../../lib/router.svelte';
  import { relTime, between, shortSha, absTime } from '../../lib/format';
  import { toast } from '../../lib/toast.svelte';
  import DeployBadge from '../../components/DeployBadge.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import LogView from '../../components/LogView.svelte';
  import Icon from '../../components/Icon.svelte';
  import Diagnosis from '../../components/Diagnosis.svelte';

  let {
    serviceId,
    deploymentId,
    detail,
    reload,
    canEdit,
  }: { serviceId: string; deploymentId: string; detail: ServiceDetail; reload: () => Promise<void>; canEdit: boolean } = $props();

  let dep = $state<Deployment | null>(null);
  let lines = $state<string[]>([]);
  let streaming = $state(false);
  let final = $state<string | null>(null);
  let error = $state('');
  let filter = $state('');
  let tickNow = $state(Date.now());

  const terminal = ['active', 'superseded', 'failed', 'cancelled'];

  async function refresh() {
    try {
      const d = await api.deployment(deploymentId);
      dep = d.deployment;
      if (d.service.id !== serviceId) navigate(`/services/${d.service.id}/deployments/${deploymentId}`, true);
    } catch (e) {
      error = errorMessage(e);
    }
  }

  onMount(() => {
    refresh();
    const es = new EventSource(api.deploymentLogStreamURL(deploymentId));
    streaming = true;
    // Lines carry SSE ids; on reconnect the browser sends Last-Event-ID and the
    // server resumes after it, so the buffer is kept.
    es.addEventListener('log', (ev) => {
      lines.push((ev as MessageEvent).data);
    });
    es.addEventListener('done', (ev) => {
      final = (ev as MessageEvent).data;
      streaming = false;
      es.close();
      refresh();
      reload();
      if (final === 'active') toast.success('Deployment is live');
      else if (final === 'failed') toast.error('Deployment failed');
    });
    es.onerror = () => {
      if (es.readyState === EventSource.CLOSED) {
        streaming = false;
        if (!final) error = 'Log stream disconnected';
      }
    };
    const t = setInterval(() => {
      tickNow = Date.now();
      if (streaming && dep && !terminal.includes(dep.status)) refresh();
    }, 3000);
    return () => {
      es.close();
      clearInterval(t);
    };
  });

  const status = $derived(final ?? dep?.status ?? 'queued');
  const isRunning = $derived(!terminal.includes(status));

  async function cancel() {
    try {
      await api.cancelDeployment(deploymentId);
      toast.success('Deployment cancelled');
      refresh();
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }
  async function redeploy() {
    try {
      const d = await api.deploy(serviceId);
      toast.success('Deployment queued');
      navigate(`/services/${serviceId}/deployments/${d.id}`);
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }

  function download() {
    const blob = new Blob([lines.join('\n') + '\n'], { type: 'text/plain' });
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = `deployment-${deploymentId.slice(0, 8)}.log`;
    a.click();
    URL.revokeObjectURL(a.href);
  }
</script>

<div class="stack">
  <div class="between">
    <a href={href(`/services/${serviceId}/deployments`)} class="small row nowrap" style="gap:4px"><Icon name="arrowleft" size={12} />All deployments</a>
    <div class="row">
      {#if canEdit && status === 'queued'}<button class="sm" onclick={cancel}>Cancel</button>{/if}
      {#if canEdit && status === 'failed'}<button class="sm primary" onclick={redeploy}><Icon name="rocket" size={12} />Redeploy</button>{/if}
      <button class="sm" onclick={download} disabled={!lines.length}>Download</button>
    </div>
  </div>

  <div class="card head">
    <div class="row" style="gap:14px">
      <DeployBadge {status} />
      {#if isRunning}<span class="row nowrap small muted" style="gap:6px"><span class="spinner"></span>{streaming ? 'Streaming live' : 'Waiting'}</span>{/if}
      <span class="mono small">#{deploymentId.slice(0, 8)}</span>
      {#if dep}
        {#if dep.commit_sha}<span class="small"><code>{shortSha(dep.commit_sha)}</code> <span class="muted">{dep.commit_message}</span></span>{/if}
        <span class="small faint">{dep.trigger}</span>
        {#if dep.detected}<span class="badge">{dep.detected}</span>{/if}
        <span class="spacer"></span>
        <span class="small faint" title={absTime(dep.created_at)}>
          {relTime(dep.created_at, tickNow)} · {between(dep.started_at, dep.finished_at, isRunning)}
        </span>
      {/if}
    </div>
    {#if dep?.error}<div class="alert error" style="margin-top:10px">{dep.error}</div>{/if}
  </div>

  {#if status === 'failed'}<Diagnosis {deploymentId} {canEdit} />{/if}

  <ErrorMsg {error} />

  <div class="row">
    <label class="sr-only" for="dlfilter">Filter log</label>
    <input id="dlfilter" class="filter" type="search" placeholder="Filter lines…" bind:value={filter} />
    <span class="faint small">{lines.length} lines</span>
  </div>

  <LogView {lines} {filter} height="calc(100vh - 360px)" empty={isRunning ? 'Waiting for output…' : 'No log output.'} />

  {#if final}
    <div class="alert {final === 'active' ? 'success' : final === 'failed' ? 'error' : ''}">
      <Icon name={final === 'active' ? 'check' : final === 'failed' ? 'x' : 'activity'} />
      Deployment finished with status <b>{final}</b>.
      {#if final === 'active' && detail.urls[0]}<a href={detail.urls[0]} target="_blank" rel="noreferrer">Open {detail.urls[0].replace(/^https?:\/\//, '')}</a>{/if}
    </div>
  {/if}
</div>

<style>
  .head {
    padding: 12px 14px;
  }
  .filter {
    max-width: 280px;
  }
</style>
