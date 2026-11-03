<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type ServiceDetail, type ServiceMetrics } from '../../lib/api';
  import { mb, cpu, bytes, rate } from '../../lib/format';
  import { poll } from '../../lib/poll';
  import Chart, { type Pt } from '../../components/Chart.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import Loading from '../../components/Loading.svelte';
  import { t } from '../../lib/i18n.svelte';

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
  // Per-minute byte counts, drawn as an average rate over the minute.
  const netIn = $derived(series((p) => (p.net_rx_bytes ?? 0) / 60));
  const netOut = $derived(series((p) => (p.net_tx_bytes ?? 0) / 60));
  const netTotals = $derived({
    rx: pts.reduce((a, p) => a + (p.net_rx_bytes ?? 0), 0),
    tx: pts.reduce((a, p) => a + (p.net_tx_bytes ?? 0), 0),
  });
  const tr = $derived(data?.traffic);
  const limit = $derived(detail.service.spec.resources.memory_limit_mb);
  const totals = $derived({
    req: pts.reduce((a, p) => a + p.requests, 0),
    err: pts.reduce((a, p) => a + p.errors_5xx, 0),
  });
  const n0 = (v: number) => (v >= 1000 ? `${(v / 1000).toFixed(1)}k` : String(Math.round(v)));
</script>

<div class="stack">
  <div class="between">
    <div class="seg" role="radiogroup" aria-label={t('Time range')}>
      {#each ranges as h (h)}
        <button type="button" role="radio" aria-checked={hours === h} class="sm" class:on={hours === h} onclick={() => pick(h)}>{h}h</button>
      {/each}
    </div>
    <div class="row small muted" style="gap:16px">
      {#if loading}<span class="spinner"></span>{/if}
      {#if data}
        <span>{t('Now:')} <b class="num">{data.rps_5m.toFixed(2)}</b> req/s</span>
        <span>{t('5xx (5 min):')} <b class="num" style:color={data.errors_5m ? 'var(--red)' : undefined}>{data.errors_5m}</b></span>
        <span>{hours}h: <b class="num">{n0(totals.req)}</b> {t('requests')}, <b class="num">{totals.err}</b> {t('errors')}{totals.req ? ` (${((totals.err / totals.req) * 100).toFixed(2)}%)` : ''}</span>
      {/if}
    </div>
  </div>
  <ErrorMsg {error} onretry={load} />
  {#if !data && !error}
    <Loading rows={2} />
  {:else if data}
    <div class="charts">
      <Chart title={t('Memory (sum of instances) · limit {limit}/instance', { limit: mb(limit) })} points={memory} format={(v) => mb(v)} />
      <Chart title={t('CPU (sum of instances)')} points={cpuS} format={(v) => cpu(Math.round(v))} />
      <Chart title={t('Requests per minute')} points={reqs} kind="bars" format={n0} minTop={2} />
      <Chart title={t('5xx errors per minute')} points={errs} kind="bars" color="var(--red)" format={n0} minTop={2} summary={totals.err ? t('{n} in {hours}h', { n: totals.err, hours }) : t('none')} />
      <Chart title={t('Network in (received)')} points={netIn} format={rate} summary={t('{n} in {hours}h', { n: bytes(netTotals.rx), hours })} />
      <Chart title={t('Network out (sent)')} points={netOut} format={rate} color="var(--purple, #8b5cf6)" summary={t('{n} in {hours}h', { n: bytes(netTotals.tx), hours })} />
      <Chart title={t('Running instances')} points={inst} format={(v) => String(Math.round(v))} minTop={2} />
    </div>
    {#if tr}
      <div class="card traffic">
        <div class="card-head">
          <h3>{t('Bandwidth')}</h3>
          <span class="faint small">{tr.since ? t('since {date}', { date: tr.since }) : t('last 30 days')} · {t('days in {tz}', { tz: tr.tz })}</span>
        </div>
        <div class="card-body stack">
          <div class="row small" style="gap:24px">
            <span>{t('Today:')} <b class="num">↓ {bytes(tr.rx_today)}</b> <b class="num">↑ {bytes(tr.tx_today)}</b></span>
            <span>{t('30 days:')} <b class="num">↓ {bytes(tr.rx_30d)}</b> <b class="num">↑ {bytes(tr.tx_30d)}</b> · {t('total')} <b class="num">{bytes(tr.rx_30d + tr.tx_30d)}</b></span>
          </div>
          {#if tr.daily.length}
            {@const top = Math.max(1, ...tr.daily.map((d) => d.rx_bytes + d.tx_bytes))}
            <div class="days" role="img" aria-label={t('Traffic per day')}>
              {#each tr.daily as d (d.day)}
                <div class="day" title="{d.day}: ↓ {bytes(d.rx_bytes)} ↑ {bytes(d.tx_bytes)}">
                  <span class="tx" style:height="{(d.tx_bytes / top) * 100}%"></span>
                  <span class="rx" style:height="{(d.rx_bytes / top) * 100}%"></span>
                </div>
              {/each}
            </div>
            <p class="hint">{t('Per day:')} <span class="key rx"></span> {t('received')} <span class="key tx"></span> {t('sent.')} {t('All traffic of the containers, including calls to databases and other services of the project.')}</p>
          {:else}
            <p class="hint">{t('No traffic recorded yet.')}</p>
          {/if}
        </div>
      </div>
    {/if}
    <p class="hint">{t('One point per minute, recorded by the control plane. Hover or use the arrow keys on a chart to read values.')}</p>
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
  .days {
    display: flex;
    align-items: flex-end;
    gap: 3px;
    height: 90px;
  }
  .day {
    flex: 1;
    max-width: 28px;
    display: flex;
    flex-direction: column-reverse;
    height: 100%;
    min-width: 4px;
  }
  .day .rx,
  .key.rx {
    background: var(--accent);
  }
  .day .tx,
  .key.tx {
    background: var(--purple, #8b5cf6);
  }
  .key {
    display: inline-block;
    width: 10px;
    height: 10px;
    border-radius: 2px;
    vertical-align: middle;
    margin-left: 6px;
  }
  @media (max-width: 820px) {
    .charts {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
