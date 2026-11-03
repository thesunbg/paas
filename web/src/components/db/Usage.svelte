<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type DatabaseDetail, type DBMetricsResponse } from '../../lib/api';
  import { session } from '../../lib/session.svelte';
  import { bytes } from '../../lib/format';
  import { poll } from '../../lib/poll';
  import { toast } from '../../lib/toast.svelte';
  import Chart from '../Chart.svelte';
  import { t } from '../../lib/i18n.svelte';

  let { detail }: { detail: DatabaseDetail } = $props();
  const db = $derived(detail.database);
  let m = $state<DBMetricsResponse | null>(null);
  let hours = $state(24);
  let quotaInput = $state(0);
  let editingQuota = $state(false);

  async function load() {
    try {
      m = await api.databaseMetrics(db.id, hours);
    } catch {
      /* metrics are optional */
    }
  }
  onMount(() => {
    load();
    return poll(load, 60000);
  });

  const last = $derived(m?.points.at(-1));
  const used = $derived(last?.size_bytes ?? 0);
  const quotaBytes = $derived((m?.quota_mb ?? 0) * 1024 * 1024);
  const pct = $derived(quotaBytes ? Math.min(100, (used / quotaBytes) * 100) : 0);
  const sizePts = $derived((m?.points ?? []).map((p) => ({ t: Date.parse(p.ts), v: p.size_bytes })));
  const connPts = $derived((m?.points ?? []).map((p) => ({ t: Date.parse(p.ts), v: p.connections })));

  async function saveQuota(e: SubmitEvent) {
    e.preventDefault();
    try {
      await api.databaseQuota(db.id, quotaInput);
      editingQuota = false;
      toast.success(t('Quota updated'));
      await load();
    } catch (err) {
      toast.error(errorMessage(err));
    }
  }
</script>

<div class="card">
  <div class="card-head">
    <div>
      <h3>{t('Usage')}</h3>
      <p class="hint">{t('Sampled every minute, kept for 7 days.')}</p>
    </div>
    <select bind:value={hours} onchange={load} aria-label={t('Range')} style="width:auto">
      <option value={6}>6 h</option>
      <option value={24}>24 h</option>
      <option value={168}>{t('7 days')}</option>
    </select>
  </div>
  <div class="card-body stack">
    {#if m?.read_only}
      <div class="alert error">{t('This database is over its storage quota and is')} <b>{t('read-only')}</b>. {t('Delete data (or ask an administrator for a larger quota); writes resume below 90% of the quota.')}</div>
    {/if}
    {#if !m || m.points.length === 0}
      <p class="muted small">{t('No samples yet — the first one arrives within a minute of the database being ready.')}</p>
    {:else}
      <div>
        <div class="row" style="justify-content:space-between">
          <span>{t('Storage')} <b>{bytes(used)}</b> {t('of {total}', { total: bytes(quotaBytes) })}</span>
          <span class="faint small">{pct.toFixed(0)}%</span>
        </div>
        <div class="bar"><div class="fill" class:warn={pct > 80} class:full={pct >= 100} style="width:{pct}%"></div></div>
      </div>
      <div class="small">{t('Connections now')} <b>{last?.connections ?? 0}</b>{m.max_connections ? ` · ${t('limit {n}', { n: m.max_connections })}` : ''}</div>
      <Chart title={t('Size')} points={sizePts} format={(v) => bytes(v)} height={110} />
      <Chart title={t('Connections')} points={connPts} minTop={2} height={110} />
    {/if}
    {#if session.me?.user.is_admin}
      {#if editingQuota}
        <form class="row" onsubmit={saveQuota}>
          <label class="field" style="max-width:200px"><span>{t('Quota (MB, 0 = default)')}</span><input type="number" min="0" bind:value={quotaInput} /></label>
          <button class="primary sm" type="submit">{t('Save')}</button>
          <button class="sm ghost" type="button" onclick={() => (editingQuota = false)}>{t('Cancel')}</button>
        </form>
      {:else}
        <div><button class="sm" onclick={() => ((quotaInput = db.options?.quota_mb ?? 0), (editingQuota = true))}>{t('Change quota')}</button></div>
      {/if}
    {/if}
  </div>
</div>

<style>
  .bar {
    height: 8px;
    border-radius: 4px;
    background: var(--bg-hover);
    overflow: hidden;
    margin-top: 6px;
  }
  .fill {
    height: 100%;
    background: var(--accent);
  }
  .fill.warn {
    background: var(--yellow, #d9a400);
  }
  .fill.full {
    background: var(--red);
  }
</style>
