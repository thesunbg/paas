<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type JobRun, type ServiceDetail } from '../../lib/api';
  import { relTime, absTime, between } from '../../lib/format';
  import { describeCron } from '../../lib/cron';
  import { poll } from '../../lib/poll';
  import { toast } from '../../lib/toast.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import Loading from '../../components/Loading.svelte';
  import Icon from '../../components/Icon.svelte';
  import Status from '../../components/Status.svelte';

  let { detail, reload, canEdit }: { detail: ServiceDetail; reload: () => Promise<void>; canEdit: boolean } = $props();

  let runs = $state<JobRun[] | null>(null);
  let error = $state('');
  let busy = $state(false);
  let open = $state<Record<string, boolean>>({});
  let now = $state(Date.now());
  const sid = $derived(detail.service.id);

  async function load() {
    try {
      runs = (await api.runs(sid)) ?? [];
      error = '';
      now = Date.now();
    } catch (e) {
      error = errorMessage(e);
    }
  }
  onMount(() => {
    load();
    return poll(load, 5000);
  });

  async function runNow() {
    busy = true;
    try {
      const r = await api.runJob(sid);
      toast.success('Job started');
      open[r.id] = true;
      await load();
      reload();
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      busy = false;
    }
  }

  const st = (r: JobRun) => (r.status === 'succeeded' ? 'completed' : r.status === 'failed' ? 'failed' : 'deploying');
  const schedule = $derived(detail.service.spec.schedule ?? '');
  const desc = $derived(describeCron(schedule));
  const stats = $derived.by(() => {
    const done = (runs ?? []).filter((r) => r.status !== 'running');
    const ok = done.filter((r) => r.status === 'succeeded').length;
    return { total: done.length, ok, failed: done.length - ok };
  });
</script>

<div class="stack-lg">
  <div class="card head">
    <div class="row" style="gap:20px">
      <div>
        <div class="label">Schedule</div>
        <div><code>{schedule || '—'}</code> <span class="muted small">{desc}{desc ? ' · ' : ''}UTC</span></div>
      </div>
      <div>
        <div class="label">Next run</div>
        <div title={absTime(detail.next_run)}>{detail.next_run ? `${relTime(detail.next_run, now)} · ${new Date(detail.next_run).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}` : '—'}</div>
      </div>
      <div>
        <div class="label">Timeout</div>
        <div>{detail.service.spec.job_timeout_sec ? `${detail.service.spec.job_timeout_sec}s` : 'default'}</div>
      </div>
      <div>
        <div class="label">Recent runs ({stats.total})</div>
        <div><span style="color:var(--green)">{stats.ok} succeeded</span> · <span style:color={stats.failed ? 'var(--red)' : undefined}>{stats.failed} failed</span></div>
      </div>
      <div class="spacer"></div>
      {#if canEdit}
        <button class="primary" onclick={runNow} disabled={busy || !detail.service.current_deployment_id} title={detail.service.current_deployment_id ? '' : 'Deploy the job first'}>
          {#if busy}<span class="spinner"></span>{:else}<Icon name="play" size={14} />{/if}Run now
        </button>
      {/if}
    </div>
  </div>

  <ErrorMsg {error} onretry={load} />
  {#if runs === null && !error}
    <Loading rows={3} />
  {:else if runs && runs.length === 0}
    <div class="card empty"><Icon name="clock" size={28} /><p>No runs yet. The job runs at the next scheduled time{canEdit ? ', or press Run now' : ''}.</p></div>
  {:else if runs}
    <div class="card">
      {#each runs as r (r.id)}
        <div class="run" class:failed={r.status === 'failed'}>
          <button class="ghost line" aria-expanded={!!open[r.id]} onclick={() => (open[r.id] = !open[r.id])}>
            <Icon name={open[r.id] ? 'chevdown' : 'chevron'} size={14} />
            <Status status={st(r)} label={r.status} />
            <span class="badge {r.exit_code === 0 ? 'green' : r.exit_code == null ? '' : 'red'}">{r.exit_code == null ? '—' : `exit ${r.exit_code}`}</span>
            <span class="badge outline hide-sm">{r.trigger}</span>
            <span class="muted small truncate out1 hide-sm">{r.output.split('\n').find((l) => l.trim()) ?? ''}</span>
            <span class="spacer"></span>
            <span class="small nowrap" title={absTime(r.started_at)}>{relTime(r.started_at, now)}</span>
            <span class="faint small mono nowrap dur hide-sm">{between(r.started_at, r.finished_at, r.status === 'running')}</span>
          </button>
          {#if open[r.id]}
            <pre class="log out">{r.output || '(no output)'}</pre>
            <div class="faint small meta mono">{r.container_id} · started {absTime(r.started_at)}{r.finished_at ? ` · finished ${absTime(r.finished_at)}` : ''}</div>
          {/if}
        </div>
      {/each}
    </div>
  {/if}
</div>

<style>
  .head {
    padding: 14px 16px;
  }
  .run {
    border-bottom: 1px solid var(--border);
  }
  .run:last-child {
    border-bottom: 0;
  }
  .line {
    width: 100%;
    justify-content: flex-start;
    gap: 10px;
    height: auto;
    min-height: 42px;
    padding: 6px 14px;
    border-radius: 0;
    font-weight: 400;
    flex-wrap: wrap;
    white-space: normal;
  }
  .out1 {
    max-width: 420px;
  }
  .dur {
    min-width: 48px;
    text-align: right;
  }
  .out {
    margin: 0 14px 6px;
    max-height: 320px;
  }
  .meta {
    padding: 0 14px 10px;
  }
</style>
