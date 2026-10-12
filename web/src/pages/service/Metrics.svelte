<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type ServiceDetail, type ServiceMetrics } from '../../lib/api';
  import { mb, cpu } from '../../lib/format';
  import { poll } from '../../lib/poll';
  import Chart, { type Pt } from '../../components/Chart.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import Loading from '../../components/Loading.svelte';

  let { detail }: { detail: ServiceDetail } = $props();

  const ranges = [1, 6, 24, 48];
  const KEY = 'paas.metrics.hours';
  function initial(): number {
    try {
      const v = Number(localStorage.getItem(KEY));
      return ranges.includes(v) ? v : 6;
    } catch {
      return 6;
    }
  }
  let hours = $state(initial());
  let data = $state<ServiceMetrics | null>(null);
  let error = $state('');
  let loading = $state(false);

  async function load() {
    loading = true;
    try {
      data = await api.metrics(detail.service.id, hours);
      error = '';
    } catch (e) {
      error = errorMessage(e);
    } finally {
      loading = false;
    }
  }
  function pick(h: number) {
    hours = h;
    try {
      localStorage.setItem(KEY, String(h));
    } catch {
      /* ignore */
    }
    data = null;
    load();
  }
  onMount(() => {
    load();
    return poll(load, 60000);
  });

  const pts = $derived(data?.points ?? []);
  const series = (f: (p: (typeof pts)[number]) => number): Pt[] => pts.map((p) => ({ t: Date.parse(p.ts), v: f(p) }));
  const memory = $derived(series((p) => p.memory_mb));
  const cpuS = $derived(series((p) => p.cpu_millis));
  const reqs = $derived(series((p) => p.requests));
  const errs = $derived(series((p) => p.errors_5xx));
  const inst = $derived(series((p) => p.instances));
  const limit = $derived(detail.service.spec.resources.memory_limit_mb);
  const totals = $derived({
    req: pts.reduce((a, p) => a + p.requests, 0),
    err: pts.reduce((a, p) => a + p.errors_5xx, 0),
  });
  const n0 = (v: number) => (v >= 1000 ? `${(v / 1000).toFixed(1)}k` : String(Math.round(v)));
</script>

<div class="stack">
  <div class="between">
    <div class="seg" role="radiogroup" aria-label="Time range">
      {#each ranges as h (h)}
        <button type="button" role="radio" aria-checked={hours === h} class="sm" class:on={hours === h} onclick={() => pick(h)}>{h}h</button>
      {/each}
    </div>
    <div class="row small muted" style="gap:16px">
      {#if loading}<span class="spinner"></span>{/if}
      {#if data}
        <span>Now: <b class="num">{data.rps_5m.toFixed(2)}</b> req/s</span>
        <span>5xx (5 min): <b class="num" style:color={data.errors_5m ? 'var(--red)' : undefined}>{data.errors_5m}</b></span>
        <span>{hours}h: <b class="num">{n0(totals.req)}</b> requests, <b class="num">{totals.err}</b> errors{totals.req ? ` (${((totals.err / totals.req) * 100).toFixed(2)}%)` : ''}</span>
      {/if}
    </div>
  </div>
  <ErrorMsg {error} onretry={load} />
  {#if !data && !error}
    <Loading rows={2} />
  {:else if data}
    <div class="charts">
      <Chart title="Memory (sum of instances) · limit {mb(limit)}/instance" points={memory} format={(v) => mb(v)} />
      <Chart title="CPU (sum of instances)" points={cpuS} format={(v) => cpu(Math.round(v))} />
      <Chart title="Requests per minute" points={reqs} kind="bars" format={n0} minTop={2} />
      <Chart title="5xx errors per minute" points={errs} kind="bars" color="var(--red)" format={n0} minTop={2} summary={totals.err ? `${totals.err} in ${hours}h` : 'none'} />
      <Chart title="Running instances" points={inst} format={(v) => String(Math.round(v))} minTop={2} />
    </div>
    <p class="hint">One point per minute, recorded by the control plane. Hover or use the arrow keys on a chart to read values.</p>
  {/if}
</div>

<style>
  .seg {
    display: inline-flex;
    gap: 2px;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius-sm);
    padding: 2px;
  }
  .seg button {
    border: 0;
    background: transparent;
    min-width: 40px;
  }
  .seg button.on {
    background: var(--bg-hover);
  }
  .num {
    color: var(--text);
    font-variant-numeric: tabular-nums;
  }
  .charts {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 12px;
  }
  @media (max-width: 820px) {
    .charts {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
