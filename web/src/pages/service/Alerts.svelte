<script lang="ts">
  import { onMount } from 'svelte';
  import {
    api,
    errorMessage,
    type AlertComparator,
    type AlertMetric,
    type AlertRule,
    type Channel,
    type ServiceDetail,
    type UptimeCheck,
    type UptimeResult,
  } from '../../lib/api';
  import { relTime, absTime } from '../../lib/format';
  import { poll } from '../../lib/poll';
  import { toast } from '../../lib/toast.svelte';
  import { confirmDialog } from '../../lib/confirm.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import Loading from '../../components/Loading.svelte';
  import Icon from '../../components/Icon.svelte';
  import Sparkline from '../../components/Sparkline.svelte';

  let { detail, canEdit }: { detail: ServiceDetail; canEdit: boolean } = $props();

  const sid = $derived(detail.service.id);
  const pid = $derived(detail.project.id);
  const teamId = $derived(detail.project.team_id);
  // Metric alerts need instances; static sites only get uptime checks.
  const hasMetrics = $derived(detail.service.type === 'web' || detail.service.type === 'worker');

  let rules = $state<AlertRule[] | null>(null);
  let metrics = $state<Record<string, string>>({});
  let checks = $state<UptimeCheck[] | null>(null);
  let channels = $state<Channel[]>([]);
  let error = $state('');

  async function load() {
    try {
      const [a, u] = await Promise.all([hasMetrics ? api.alerts(pid, sid) : Promise.resolve(null), api.uptimeChecks(pid, sid)]);
      if (a) {
        rules = a.rules ?? [];
        metrics = a.metrics ?? {};
      } else rules = [];
      checks = u.checks ?? [];
      error = '';
    } catch (e) {
      error = errorMessage(e);
    }
  }
  onMount(() => {
    load();
    api
      .channels(teamId)
      .then((r) => (channels = r.channels ?? []))
      .catch(() => {});
    return poll(load, 15000);
  });

  const metricLabels: Record<AlertMetric, string> = {
    cpu_percent: 'CPU %',
    memory_percent: 'Memory %',
    restarts: 'Restarts (5 min)',
    error_rate: 'HTTP 5xx %',
    instances: 'Running instances',
  };
  const unit = (m: string) => (m === 'cpu_percent' || m === 'memory_percent' || m === 'error_rate' ? '%' : '');
  const fmtDur = (s: number) => (s === 0 ? 'immediately' : s % 3600 === 0 ? `${s / 3600}h` : s % 60 === 0 ? `${s / 60}m` : `${s}s`);
  const fmtVal = (m: string, v: number | null | undefined) => (v == null ? '—' : `${Math.round(v * 10) / 10}${unit(m)}`);
  const channelName = (id: string) => channels.find((c) => c.id === id)?.name ?? 'deleted channel';
  const stateBadge: Record<string, string> = { firing: 'red', pending: 'yellow', ok: 'green', down: 'red', up: 'green', unknown: '' };

  function myState(r: AlertRule) {
    return r.states?.find((s) => s.service_id === sid);
  }

  // New alert rule.
  let aName = $state('');
  let aMetric = $state<AlertMetric>('cpu_percent');
  let aComparator = $state<AlertComparator>('>');
  let aThreshold = $state(80);
  let aFor = $state(5);
  let aCooldown = $state(30);
  let aProjectWide = $state(false);
  let aChannels = $state<string[]>([]);
  let aError = $state('');
  let busy = $state('');

  const defaults: Record<AlertMetric, [AlertComparator, number]> = {
    cpu_percent: ['>', 80],
    memory_percent: ['>', 90],
    restarts: ['>', 0],
    error_rate: ['>', 5],
    instances: ['<', 1],
  };
  function pickMetric(m: AlertMetric) {
    aMetric = m;
    [aComparator, aThreshold] = defaults[m];
  }

  async function addRule(e: SubmitEvent) {
    e.preventDefault();
    aError = '';
    busy = 'rule';
    try {
      await api.createAlert(pid, {
        name: aName.trim() || undefined,
        service_id: aProjectWide ? undefined : sid,
        metric: aMetric,
        comparator: aComparator,
        threshold: Number(aThreshold),
        for_seconds: Math.round(Number(aFor) * 60),
        cooldown_seconds: Math.round(Number(aCooldown) * 60),
        channel_ids: aChannels,
      });
      toast.success('Alert rule added');
      aName = '';
      await load();
    } catch (err) {
      aError = errorMessage(err);
    } finally {
      busy = '';
    }
  }

  async function toggleRule(r: AlertRule) {
    try {
      await api.updateAlert(r.id, { enabled: !r.enabled });
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }

  async function removeRule(r: AlertRule) {
    const scope = r.service_id ? '' : ' It applies to every service of the project.';
    if (!(await confirmDialog({ title: 'Delete alert rule', message: `Delete "${r.name}"?${scope}`, confirmLabel: 'Delete', danger: true }))) return;
    try {
      await api.deleteAlert(r.id);
      toast.success('Alert rule deleted');
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }

  // New uptime check.
  let uURL = $state('');
  let uMethod = $state('GET');
  let uExpect = $state<number | ''>('');
  let uKeyword = $state('');
  let uInterval = $state(60);
  let uFailures = $state(3);
  let uChannels = $state<string[]>([]);
  let uError = $state('');
  $effect(() => {
    if (!uURL && detail.urls[0]) uURL = detail.urls[0];
  });

  async function addCheck(e: SubmitEvent) {
    e.preventDefault();
    uError = '';
    busy = 'check';
    try {
      await api.createUptime(pid, {
        service_id: sid,
        url: uURL.trim(),
        method: uMethod,
        expected_status: uExpect === '' ? 0 : Number(uExpect),
        keyword: uKeyword,
        interval_seconds: Number(uInterval),
        failure_threshold: Number(uFailures),
        channel_ids: uChannels,
      });
      toast.success('Uptime check added; the first result comes within a minute');
      await load();
    } catch (err) {
      uError = errorMessage(err);
    } finally {
      busy = '';
    }
  }

  async function toggleCheck(c: UptimeCheck) {
    try {
      await api.updateUptime(c.id, { enabled: !c.enabled });
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }

  async function removeCheck(c: UptimeCheck) {
    if (!(await confirmDialog({ title: 'Delete uptime check', message: `Stop checking ${c.url} and delete its history?`, confirmLabel: 'Delete', danger: true }))) return;
    try {
      await api.deleteUptime(c.id);
      toast.success('Uptime check deleted');
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }

  // Recent results of the expanded check.
  let openCheck = $state('');
  let results = $state<UptimeResult[]>([]);
  async function expand(c: UptimeCheck) {
    if (openCheck === c.id) return void (openCheck = '');
    openCheck = c.id;
    results = [];
    try {
      results = (await api.uptimeCheck(c.id, 60)).results ?? [];
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }
  const oldestFirst = $derived([...results].reverse());

  const pct = (v: number | null | undefined) => (v == null ? '—' : `${v >= 99.995 ? '100' : v.toFixed(2)}%`);
  const ms = (v: number | null | undefined) => (v == null ? '—' : `${Math.round(v)} ms`);
  function certDays(c: UptimeCheck): number | null {
    if (!c.cert_expires_at) return null;
    return Math.floor((new Date(c.cert_expires_at).getTime() - Date.now()) / 86400000);
  }
</script>

<div class="stack-lg">
  {#if error}<ErrorMsg {error} onretry={load} />{/if}

  {#if hasMetrics}
    <div class="card">
      <div class="card-head">
        <div>
          <h3>Alert rules</h3>
          <p class="hint">Evaluated every minute against this service's metrics. Notifies when a rule starts firing and when it resolves.</p>
        </div>
      </div>
      {#if rules === null && !error}
        <div class="card-body"><Loading /></div>
      {:else if rules && rules.length === 0}
        <div class="card-body muted small">No alert rules yet.</div>
      {:else if rules}
        <div class="table-wrap">
          <table>
            <thead><tr><th>Rule</th><th>Condition</th><th>State</th><th class="hide-sm">Notify</th><th class="right"></th></tr></thead>
            <tbody>
              {#each rules as r (r.id)}
                {@const st = myState(r)}
                <tr class:dim={!r.enabled}>
                  <td>
                    <div>{r.name}</div>
                    {#if !r.service_id}<span class="badge outline">project-wide</span>{/if}
                  </td>
                  <td class="nowrap">
                    {metricLabels[r.metric] ?? r.metric} <code>{r.comparator} {r.threshold}{unit(r.metric)}</code>
                    <div class="faint small">for {fmtDur(r.for_seconds)} · cooldown {fmtDur(r.cooldown_seconds)}</div>
                  </td>
                  <td>
                    {#if !r.enabled}
                      <span class="badge">paused</span>
                    {:else if st}
                      <span class="badge {stateBadge[st.state]}" title={st.firing_since ? `firing since ${absTime(st.firing_since)}` : ''}>{st.state}</span>
                      <span class="small muted">{fmtVal(r.metric, st.value)}</span>
                    {:else}
                      <span class="faint small">not evaluated yet</span>
                    {/if}
                  </td>
                  <td class="hide-sm small">
                    {#if r.channel_ids.length}{r.channel_ids.map(channelName).join(', ')}{:else}<span class="faint">channels subscribed to alerts</span>{/if}
                  </td>
                  <td class="right nowrap">
                    {#if canEdit}
                      <button class="sm" onclick={() => toggleRule(r)} aria-label="{r.enabled ? 'Pause' : 'Resume'} {r.name}"><Icon name={r.enabled ? 'pause' : 'play'} size={12} /></button>
                      <button class="sm danger" onclick={() => removeRule(r)} aria-label="Delete {r.name}"><Icon name="trash" size={12} /></button>
                    {/if}
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
      {#if canEdit}
        <form class="card-body stack add" onsubmit={addRule}>
          <h3>Add an alert rule</h3>
          <div class="grid-3">
            <label class="field">
              <span>Metric</span>
              <select value={aMetric} onchange={(e) => pickMetric((e.currentTarget as HTMLSelectElement).value as AlertMetric)}>
                {#each Object.keys(metricLabels) as m (m)}<option value={m}>{metricLabels[m as AlertMetric]}</option>{/each}
              </select>
              {#if metrics[aMetric]}<span class="hint">{metrics[aMetric]}</span>{/if}
            </label>
            <label class="field">
              <span>Condition</span>
              <div class="row nowrap">
                <select bind:value={aComparator} style="width:auto">{#each ['>', '>=', '<', '<='] as c (c)}<option value={c}>{c}</option>{/each}</select>
                <input type="number" min="0" step="any" bind:value={aThreshold} required aria-label="Threshold" />
              </div>
            </label>
            <label class="field">
              <span>Name</span>
              <input bind:value={aName} maxlength="100" placeholder="{metricLabels[aMetric]} {aComparator} {aThreshold}" />
            </label>
            <label class="field">
              <span>For (minutes)</span>
              <input type="number" min="0" max="1440" bind:value={aFor} />
              <span class="hint">How long the condition must hold before firing.</span>
            </label>
            <label class="field">
              <span>Cooldown (minutes)</span>
              <input type="number" min="0" max="10080" bind:value={aCooldown} />
              <span class="hint">No new notification sooner after the previous one.</span>
            </label>
          </div>
          <label class="check small"><input type="checkbox" bind:checked={aProjectWide} />Apply to every web and worker service of {detail.project.name}</label>
          {#if channels.length}
            <fieldset>
              <legend class="label">Notify</legend>
              <div class="chans">
                {#each channels as c (c.id)}<label class="check small"><input type="checkbox" bind:group={aChannels} value={c.id} />{c.name}</label>{/each}
              </div>
              <span class="hint">None picked: the team's channels subscribed to alert events.</span>
            </fieldset>
          {/if}
          <ErrorMsg error={aError} />
          <div><button class="primary" type="submit" disabled={busy === 'rule'}>{#if busy === 'rule'}<span class="spinner"></span>{/if}Add rule</button></div>
        </form>
      {/if}
    </div>
  {/if}

  <div class="card">
    <div class="card-head">
      <div>
        <h3>Uptime checks</h3>
        <p class="hint">HTTP checks run from the control plane. Notifies after consecutive failures, on recovery, and 14 days before a TLS certificate expires.</p>
      </div>
    </div>
    {#if checks === null && !error}
      <div class="card-body"><Loading /></div>
    {:else if checks && checks.length === 0}
      <div class="card-body muted small">No uptime checks yet.</div>
    {:else if checks}
      <div class="table-wrap">
        <table>
          <thead><tr><th>Check</th><th>State</th><th>24h</th><th class="hide-sm">7 days</th><th class="hide-sm">p95</th><th class="hide-sm">Certificate</th><th class="right"></th></tr></thead>
          <tbody>
            {#each checks as c (c.id)}
              {@const days = certDays(c)}
              <tr class:dim={!c.enabled}>
                <td>
                  <button class="link" onclick={() => expand(c)} aria-expanded={openCheck === c.id}>
                    <Icon name={openCheck === c.id ? 'chevdown' : 'chevron'} size={12} />{c.name}
                  </button>
                  <div class="faint small mono truncate">{c.method} {c.url} · every {fmtDur(c.interval_seconds)}</div>
                </td>
                <td>
                  {#if !c.enabled}<span class="badge">paused</span>{:else}<span class="badge {stateBadge[c.state]}">{c.state}</span>{/if}
                  {#if c.last_checked_at}<div class="faint small" title={absTime(c.last_checked_at)}>{relTime(c.last_checked_at)}</div>{/if}
                </td>
                <td>{pct(c.stats?.uptime_24h)}</td>
                <td class="hide-sm">{pct(c.stats?.uptime_7d)}</td>
                <td class="hide-sm">{ms(c.stats?.p95_latency_ms_24h)}</td>
                <td class="hide-sm small">
                  {#if days == null}<span class="faint">—</span>{:else}<span class:warn={days < 14} title={absTime(c.cert_expires_at)}>{days} days</span>{/if}
                </td>
                <td class="right nowrap">
                  {#if canEdit}
                    <button class="sm" onclick={() => toggleCheck(c)} aria-label="{c.enabled ? 'Pause' : 'Resume'} {c.name}"><Icon name={c.enabled ? 'pause' : 'play'} size={12} /></button>
                    <button class="sm danger" onclick={() => removeCheck(c)} aria-label="Delete {c.name}"><Icon name="trash" size={12} /></button>
                  {/if}
                </td>
              </tr>
              {#if openCheck === c.id}
                <tr class="expanded">
                  <td colspan="7">
                    {#if c.last_error}<div class="small" style="color:var(--red)">Last error: {c.last_error}</div>{/if}
                    {#if results.length}
                      <div class="strip" role="img" aria-label="Last {results.length} results, oldest first">
                        {#each oldestFirst as r (r.ts)}<span class="tick" class:fail={!r.ok} title="{absTime(r.ts)} · {r.ok ? 'ok' : r.error} · {r.latency_ms} ms"></span>{/each}
                      </div>
                      <div class="row small muted" style="gap:10px">
                        Latency
                        <Sparkline values={oldestFirst.filter((r) => r.ok).map((r) => r.latency_ms)} label="Response time" format={(v) => `${Math.round(v)} ms`} width={220} />
                        avg 24h {ms(c.stats?.avg_latency_ms_24h)}
                      </div>
                    {:else}
                      <span class="faint small">No results yet.</span>
                    {/if}
                  </td>
                </tr>
              {/if}
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
    {#if canEdit}
      <form class="card-body stack add" onsubmit={addCheck}>
        <h3>Add an uptime check</h3>
        <label class="field">
          <span>URL</span>
          <input class="mono" type="url" bind:value={uURL} required placeholder="https://example.com/health" spellcheck="false" />
        </label>
        <div class="grid-3">
          <label class="field">
            <span>Method</span>
            <select bind:value={uMethod}><option>GET</option><option>HEAD</option><option>POST</option></select>
          </label>
          <label class="field">
            <span>Expected status</span>
            <input type="number" min="100" max="599" bind:value={uExpect} placeholder="any 2xx/3xx" />
          </label>
          <label class="field">
            <span>Keyword</span>
            <input bind:value={uKeyword} maxlength="200" placeholder="optional text in the body" />
          </label>
          <label class="field">
            <span>Interval (seconds)</span>
            <input type="number" min="30" max="86400" bind:value={uInterval} />
          </label>
          <label class="field">
            <span>Alert after failures</span>
            <input type="number" min="1" max="20" bind:value={uFailures} />
          </label>
        </div>
        {#if channels.length}
          <fieldset>
            <legend class="label">Notify</legend>
            <div class="chans">
              {#each channels as c (c.id)}<label class="check small"><input type="checkbox" bind:group={uChannels} value={c.id} />{c.name}</label>{/each}
            </div>
            <span class="hint">None picked: the team's channels subscribed to uptime events.</span>
          </fieldset>
        {/if}
        <ErrorMsg error={uError} />
        <div><button class="primary" type="submit" disabled={busy === 'check' || !uURL.trim()}>{#if busy === 'check'}<span class="spinner"></span>{/if}Add check</button></div>
      </form>
    {/if}
  </div>
</div>

<style>
  .add {
    border-top: 1px solid var(--border);
  }
  fieldset {
    border: 0;
    margin: 0;
    padding: 0;
  }
  legend {
    padding: 0;
    margin-bottom: 6px;
  }
  .chans {
    display: flex;
    flex-wrap: wrap;
    gap: 6px 16px;
  }
  tr.dim td {
    opacity: 0.6;
  }
  .link {
    background: none;
    border: 0;
    padding: 0;
    color: var(--text);
    font: inherit;
    display: inline-flex;
    align-items: center;
    gap: 4px;
    cursor: pointer;
  }
  .warn {
    color: var(--yellow);
    font-weight: 600;
  }
  .expanded td {
    background: var(--bg-soft, transparent);
  }
  .strip {
    display: flex;
    gap: 2px;
    margin: 6px 0 8px;
    flex-wrap: wrap;
  }
  .tick {
    width: 6px;
    height: 18px;
    border-radius: 2px;
    background: var(--green);
  }
  .tick.fail {
    background: var(--red);
  }
</style>
