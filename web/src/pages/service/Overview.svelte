<script lang="ts">
  import { api, errorMessage, type ServiceDetail } from '../../lib/api';
  import { href, navigate } from '../../lib/router.svelte';
  import { mb, cpu, relTime, shortSha, between } from '../../lib/format';
  import { toast } from '../../lib/toast.svelte';
  import { confirmDialog } from '../../lib/confirm.svelte';
  import { can } from '../../lib/session.svelte';
  import Icon from '../../components/Icon.svelte';
  import Status from '../../components/Status.svelte';
  import DeployBadge from '../../components/DeployBadge.svelte';
  import CopyButton from '../../components/CopyButton.svelte';

  let { detail, reload, canEdit }: { detail: ServiceDetail; reload: () => Promise<void>; canEdit: boolean } = $props();

  const s = $derived(detail.service);
  const current = $derived(detail.deployments.find((d) => d.id === s.current_deployment_id));
  const latest = $derived(detail.deployments[0]);
  const inProgress = $derived(detail.deployments.find((d) => ['queued', 'building', 'deploying'].includes(d.status)));
  const canDelete = $derived(can('admin', detail.project.team_id));
  const isStatic = $derived(s.type === 'static');
  const stopped = $derived(s.status === 'stopped' || s.status === 'failed' || s.status === 'created');

  let busy = $state<string | null>(null);
  let replicas = $state(1);
  let replicasDirty = $state(false);
  $effect(() => {
    if (!replicasDirty) replicas = s.spec.replicas || 1;
  });

  async function act(name: string, fn: () => Promise<unknown>, ok: string, after?: (r: unknown) => void) {
    busy = name;
    try {
      const r = await fn();
      toast.success(ok);
      await reload();
      after?.(r);
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      busy = null;
    }
  }

  const deploy = () =>
    act('deploy', () => api.deploy(s.id), 'Deployment queued', (d) => navigate(`/services/${s.id}/deployments/${(d as { id: string }).id}`));
  const restart = () => act('restart', () => api.restart(s.id), 'Restart started');
  const stop = async () => {
    if (await confirmDialog({ title: 'Stop service', message: `Stop all instances of ${s.name}? It will be unreachable until started again.`, confirmLabel: 'Stop', danger: true }))
      act('stop', () => api.stop(s.id), 'Service stopped');
  };
  const start = () => act('start', () => api.start(s.id), 'Service starting');
  const scale = () =>
    act('scale', () => api.scale(s.id, replicas), `Scaled to ${replicas} replica${replicas === 1 ? '' : 's'}`, () => (replicasDirty = false));

  async function del() {
    const ok = await confirmDialog({
      title: 'Delete service',
      message: `This deletes ${s.name}, its instances, domains, deployments and environment variables. This cannot be undone.`,
      confirmLabel: 'Delete service',
      danger: true,
      requireText: s.name,
    });
    if (!ok) return;
    try {
      await api.deleteService(s.id);
      toast.success(`Service ${s.name} deleted`);
      navigate(`/projects/${detail.project.id}`);
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }

  async function applyRecommendation() {
    const r = detail.recommendation;
    if (!r) return;
    const res = { ...s.spec.resources, memory_limit_mb: r.suggested_limit_mb, memory_request_mb: Math.min(r.suggested_request_mb, r.suggested_limit_mb) };
    await act('apply', () => api.updateService(s.id, { resources: res }), `Memory limit set to ${mb(r.suggested_limit_mb)}`);
  }

  const tuning = $derived(Object.entries(detail.tuning_env ?? {}));
</script>

<div class="stack-lg">
  {#if canEdit}
    <div class="row actions">
      <button class="primary" onclick={deploy} disabled={!!busy}>{#if busy === 'deploy'}<span class="spinner"></span>{:else}<Icon name="rocket" size={14} />{/if}Deploy</button>
      {#if !isStatic}
        <button onclick={restart} disabled={!!busy || !s.current_deployment_id}>{#if busy === 'restart'}<span class="spinner"></span>{:else}<Icon name="refresh" size={14} />{/if}Restart</button>
        {#if stopped}
          <button onclick={start} disabled={!!busy || !s.current_deployment_id}>{#if busy === 'start'}<span class="spinner"></span>{:else}<Icon name="play" size={14} />{/if}Start</button>
        {:else}
          <button onclick={stop} disabled={!!busy}>{#if busy === 'stop'}<span class="spinner"></span>{:else}<Icon name="stop" size={14} />{/if}Stop</button>
        {/if}
        <form class="scale" onsubmit={(e) => (e.preventDefault(), scale())}>
          <label for="replicas" class="small muted">Replicas</label>
          <input id="replicas" type="number" min="1" max="50" bind:value={replicas} oninput={() => (replicasDirty = true)} />
          <button type="submit" disabled={!!busy || replicas === s.spec.replicas || replicas < 1}>{#if busy === 'scale'}<span class="spinner"></span>{/if}Scale</button>
        </form>
      {/if}
      <div class="spacer"></div>
      {#if canDelete}<button class="danger" onclick={del}><Icon name="trash" size={14} />Delete</button>{/if}
    </div>
  {/if}

  {#if inProgress}
    <a class="alert info progress" href={href(`/services/${s.id}/deployments/${inProgress.id}`)}>
      <span class="spinner"></span>
      <span style="flex:1">Deployment <b>{inProgress.status}</b> · {inProgress.trigger} · started {relTime(inProgress.started_at ?? inProgress.created_at)}</span>
      <span>View log →</span>
    </a>
  {:else if latest && latest.status === 'failed' && latest.id !== s.current_deployment_id}
    <a class="alert error progress" href={href(`/services/${s.id}/deployments/${latest.id}`)}>
      <Icon name="x" />
      <span style="flex:1">Latest deployment failed{latest.error ? `: ${latest.error}` : ''}</span>
      <span>View log →</span>
    </a>
  {/if}

  {#if detail.recommendation}
    {@const r = detail.recommendation}
    <div class="alert warn rec">
      <Icon name="activity" />
      <div style="flex:1;min-width:0">
        <b>Right-sizing suggestion</b>
        <p class="small muted">{r.message}</p>
        <p class="small faint">Peak {mb(r.peak_mb)} · limit {mb(r.current_limit_mb)} → {mb(r.suggested_limit_mb)} · request → {mb(r.suggested_request_mb)}</p>
      </div>
      {#if canEdit}<button class="sm" onclick={applyRecommendation} disabled={!!busy}>{#if busy === 'apply'}<span class="spinner"></span>{/if}Apply</button>{/if}
    </div>
  {/if}

  <div class="grid">
    <div class="card">
      <div class="card-head"><h3>Service</h3></div>
      <dl class="kv card-body">
        <dt>Status</dt><dd><Status status={s.status} /></dd>
        <dt>URLs</dt>
        <dd>
          {#if detail.urls.length}
            {#each detail.urls as u (u)}<div><a href={u} target="_blank" rel="noreferrer">{u}</a></div>{/each}
          {:else}<span class="faint">none</span>{/if}
        </dd>
        <dt>Internal host</dt><dd class="row nowrap"><code class="truncate">{detail.internal_host}{s.spec.port ? `:${s.spec.port}` : ''}</code><CopyButton text={detail.internal_host} /></dd>
        <dt>Source</dt>
        <dd class="mono small break">{s.source.kind === 'image' ? s.source.image : `${s.source.repo}@${s.source.branch ?? 'main'}${s.source.root_dir ? ' /' + s.source.root_dir : ''}`}</dd>
        <dt>Resources</dt>
        <dd>{mb(s.spec.resources.memory_limit_mb)} · {cpu(s.spec.resources.cpu_limit_millis)} <span class="badge">{s.spec.qos}</span></dd>
        {#if !isStatic}<dt>Replicas</dt><dd>{s.spec.replicas}</dd>{/if}
        {#if s.spec.sleep.enabled}<dt>Sleep</dt><dd>after {s.spec.sleep.idle_minutes ?? 15} min idle · last request {relTime(s.last_request_at)}</dd>{/if}
        {#if detail.databases.length}
          <dt>Databases</dt>
          <dd>{#each detail.databases as d (d.database_id)}<div><a href={href(`/databases/${d.database_id}`)}>{d.name}</a> <span class="faint small">({d.engine}) → <code>{d.env_name}</code></span></div>{/each}</dd>
        {/if}
      </dl>
    </div>

    <div class="card">
      <div class="card-head">
        <h3>Current deployment</h3>
        {#if current}<a class="small" href={href(`/services/${s.id}/deployments/${current.id}`)}>View log</a>{/if}
      </div>
      {#if current}
        <dl class="kv card-body">
          <dt>Status</dt><dd><DeployBadge status={current.status} /></dd>
          {#if current.commit_sha}
            <dt>Commit</dt><dd><code>{shortSha(current.commit_sha)}</code> <span class="muted">{current.commit_message}</span></dd>
          {/if}
          {#if current.branch}<dt>Branch</dt><dd class="mono small">{current.branch}</dd>{/if}
          <dt>Language</dt><dd>{current.detected || s.spec.language || '—'}</dd>
          <dt>Trigger</dt><dd>{current.trigger}</dd>
          <dt>Finished</dt><dd>{relTime(current.finished_at)} <span class="faint small">({between(current.started_at, current.finished_at)})</span></dd>
        </dl>
      {:else}
        <div class="card-body muted">Not deployed yet.</div>
      {/if}
      {#if tuning.length}
        <div class="card-body tuning">
          <div class="section-title" style="margin-bottom:6px">Runtime tuning (automatic)</div>
          {#each tuning as [k, v] (k)}<div class="mono small break"><span class="faint">{k}=</span>{v}</div>{/each}
          <p class="hint" style="margin-top:6px">Derived from the memory limit; set the variable yourself to override.</p>
        </div>
      {/if}
    </div>
  </div>

  {#if !isStatic}
    <div class="card">
      <div class="card-head"><h3>Instances</h3><span class="faint small">{detail.instances.length} total</span></div>
      {#if detail.instances.length === 0}
        <div class="card-body muted small">{s.status === 'sleeping' ? 'Sleeping — instances start on the next request.' : 'No instances.'}</div>
      {:else}
        <div class="table-wrap">
          <table>
            <thead>
              <tr><th>Container</th><th>Node</th><th>IP</th><th>Status</th><th class="right">Memory</th><th class="right">Peak</th><th class="right">CPU</th><th class="right">Restarts</th><th>Updated</th></tr>
            </thead>
            <tbody>
              {#each detail.instances as i (i.id)}
                {@const pctUsed = s.spec.resources.memory_limit_mb ? i.memory_usage_mb / s.spec.resources.memory_limit_mb : 0}
                <tr>
                  <td class="mono small nowrap">{i.container_id}</td>
                  <td>{i.node_name || i.node_id.slice(0, 8)}</td>
                  <td class="mono small">{i.ip || '—'}{i.port ? `:${i.port}` : ''}</td>
                  <td><Status status={i.status} /></td>
                  <td class="right nowrap">
                    <div class="mem">
                      <span>{mb(i.memory_usage_mb)}</span>
                      <span class="bar" class:warn={pctUsed > 0.75} class:crit={pctUsed > 0.9} style="min-width:50px;width:50px"><span style="width:{Math.min(100, pctUsed * 100)}%"></span></span>
                    </div>
                  </td>
                  <td class="right nowrap">{mb(i.memory_peak_mb)}</td>
                  <td class="right nowrap">{cpu(i.cpu_usage_millis)}</td>
                  <td class="right" class:restarts={i.restarts > 0}>{i.restarts}</td>
                  <td class="faint small nowrap">{relTime(i.updated_at)}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
    </div>
  {/if}
</div>

<style>
  .actions {
    gap: 8px;
  }
  .scale {
    display: flex;
    align-items: center;
    gap: 6px;
    margin-left: 4px;
  }
  .scale input {
    width: 64px;
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 12px;
  }
  .progress {
    color: var(--text);
    align-items: center;
  }
  .progress:hover {
    text-decoration: none;
  }
  .rec {
    align-items: center;
  }
  .tuning {
    border-top: 1px solid var(--border);
  }
  .mem {
    display: inline-flex;
    align-items: center;
    gap: 8px;
  }
  .restarts {
    color: var(--orange);
    font-weight: 600;
  }
  @media (max-width: 900px) {
    .grid {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
