<script lang="ts">
  import { api, errorMessage, type TeamQuota, type TeamQuotaStatus, type TeamUsage } from '../../lib/api';
  import { session, currentTeam } from '../../lib/session.svelte';
  import { toast } from '../../lib/toast.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import Loading from '../../components/Loading.svelte';
  import { t } from '../../lib/i18n.svelte';

  // The current team's usage against its quota; administrators can override the quota here.
  let status = $state<TeamQuotaStatus | null>(null);
  let error = $state('');
  let edit = $state<Record<string, number | null>>({}); // override being edited: null = use the default
  let busy = $state(false);
  const team = $derived(currentTeam());
  const isAdmin = $derived(!!session.me?.user.is_admin);

  const mib = (v: number) => (v >= 1024 ? `${+(v / 1024).toFixed(1)} GiB` : `${v} MiB`);
  const cpu = (v: number) => `${+(v / 1000).toFixed(2)} CPU`;
  const num = (v: number) => String(v);
  const rows: { label: string; used: keyof TeamUsage; max: keyof TeamQuota; fmt: (v: number) => string; hint?: string }[] = $derived([
    { label: t('Projects'), used: 'projects', max: 'max_projects', fmt: num },
    { label: t('Services'), used: 'services', max: 'max_services', fmt: num },
    { label: t('Databases'), used: 'databases', max: 'max_databases', fmt: num },
    { label: t('Memory reserved'), used: 'memory_mb', max: 'max_memory_mb', fmt: mib, hint: t('What instances reserve on the nodes (requests)') },
    { label: t('Memory limits'), used: 'memory_limit_mb', max: 'max_memory_limit_mb', fmt: mib, hint: t('If every instance used its limit') },
    { label: t('CPU reserved'), used: 'cpu_millis', max: 'max_cpu_millis', fmt: cpu },
    { label: t('Database storage'), used: 'disk_mb', max: 'max_disk_mb', fmt: mib },
    { label: t('Preview deployments'), used: 'previews', max: 'max_previews', fmt: num },
    { label: t('Custom domains'), used: 'domains', max: 'max_domains', fmt: num },
  ]);

  async function load() {
    const tm = team;
    if (!tm) return;
    try {
      status = await api.teamQuota(tm.id);
      edit = Object.fromEntries(rows.map((r) => [r.max, status!.override[r.max] ?? null]));
      error = '';
    } catch (e) {
      error = errorMessage(e);
    }
  }
  $effect(() => {
    void session.teamId;
    status = null;
    load();
  });

  const pct = (used: number, max: number) => (max > 0 ? Math.min(100, (used / max) * 100) : 0);
  const level = (p: number) => (p >= 100 ? 'crit' : p >= 80 ? 'warn' : '');

  async function save(e: SubmitEvent) {
    e.preventDefault();
    if (!team) return;
    busy = true;
    try {
      const q: Partial<TeamQuota> = {};
      for (const r of rows) {
        const v = edit[r.max];
        if (v !== null && v !== undefined && String(v) !== "") q[r.max] = Number(v);
      }
      status = await api.putTeamQuota(team.id, q);
      toast.success(t('Quota saved'));
    } catch (err) {
      error = errorMessage(err);
    } finally {
      busy = false;
    }
  }
</script>

<div class="stack-lg">
  <div class="card">
    <div class="card-head">
      <div>
        <h3>{t('{team} usage', { team: team?.name ?? t('Team') })}</h3>
        <p class="hint">{t('Stopped and sleeping services keep their share: they can start again at any time. Over a quota, nothing is stopped; the team just cannot grow.')}</p>
      </div>
    </div>
    <div class="card-body">
      {#if error}<ErrorMsg {error} onretry={load} />{:else if !status}<Loading />{:else}
        <table>
          <tbody>
            {#each rows as r (r.max)}
              {@const used = status.usage[r.used]}
              {@const max = status.quota[r.max]}
              <tr>
                <td title={r.hint}>{r.label}</td>
                <td style="width:40%">
                  {#if max > 0}<span class="bar {level(pct(used, max))}"><span style="width:{pct(used, max)}%"></span></span>{/if}
                </td>
                <td class="right small">{r.fmt(used)} / {max > 0 ? r.fmt(max) : t('unlimited')}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      {/if}
    </div>
  </div>

  {#if isAdmin && status}
    <form class="card" onsubmit={save}>
      <div class="card-head">
        <div>
          <h3>{t('Quota for this team')}</h3>
          <p class="hint">{t('Empty fields use the instance default (Settings → System). 0 = unlimited. Memory in MB, CPU in millicores.')}</p>
        </div>
      </div>
      <div class="card-body stack">
        <div class="grid-3">
          {#each rows as r (r.max)}
            <label class="field">
              <span>{r.label}</span>
              <input type="number" min="0" bind:value={edit[r.max]} placeholder={t('default: {value}', { value: status.default[r.max] || t('unlimited') })} />
            </label>
          {/each}
        </div>
        <div><button class="primary" type="submit" disabled={busy}>{#if busy}<span class="spinner"></span>{/if}{t('Save')}</button></div>
      </div>
    </form>
  {/if}
</div>
