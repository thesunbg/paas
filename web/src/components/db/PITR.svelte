<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type DatabaseDetail, type PITRInfo } from '../../lib/api';
  import { navigate } from '../../lib/router.svelte';
  import { absTime, bytes, relTime } from '../../lib/format';
  import { toast } from '../../lib/toast.svelte';
  import { confirmDialog } from '../../lib/confirm.svelte';
  import ErrorMsg from '../ErrorMsg.svelte';

  let { detail, isAdmin, reload }: { detail: DatabaseDetail; isAdmin: boolean; reload: () => Promise<void> } = $props();
  const db = $derived(detail.database);
  let info = $state<PITRInfo | null>(null);
  let retention = $state(7);
  let target = $state('');
  let busy = $state(false);
  let error = $state('');

  async function load() {
    try {
      info = await api.pitr(db.id);
      retention = info.retention_days;
    } catch (e) {
      error = errorMessage(e);
    }
  }
  onMount(load);

  // datetime-local is in the viewer's time zone.
  const localNow = () => {
    const d = new Date(Date.now() - new Date().getTimezoneOffset() * 60000);
    return d.toISOString().slice(0, 16);
  };

  async function toggle(on: boolean) {
    if (!on && !(await confirmDialog({ title: 'Turn off point-in-time recovery', message: 'Base backups are deleted and WAL archiving stops. The server restarts.', confirmLabel: 'Turn off', danger: true }))) return;
    busy = true;
    try {
      await api.setPITR(db.id, on, retention);
      toast.success(on ? 'Archiving enabled — the first base backup is running' : 'Point-in-time recovery turned off');
      await reload();
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      busy = false;
    }
  }

  async function saveRetention() {
    try {
      await api.setPITR(db.id, true, retention);
      toast.success('Retention updated');
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }

  async function restore(e: SubmitEvent) {
    e.preventDefault();
    const when = target ? new Date(target).toISOString() : undefined;
    const label = when ? new Date(when).toLocaleString() : 'the latest moment';
    if (!(await confirmDialog({ title: 'Restore to a point in time', message: `Create a new database with the data as it was at ${label}? ${db.name} itself is not changed.`, confirmLabel: 'Restore' }))) return;
    busy = true;
    error = '';
    try {
      const nd = await api.pitrRestore(db.id, when);
      toast.success(`Restoring into ${nd.name}`);
      navigate(`/databases/${nd.id}`);
    } catch (err) {
      error = errorMessage(err);
    } finally {
      busy = false;
    }
  }
</script>

<div class="card">
  <div class="card-head">
    <div>
      <h3>Point-in-time recovery</h3>
      <p class="hint">Restore the data as it was at any moment of the retention window (within ~5 minutes).</p>
    </div>
    {#if info?.capable && isAdmin}
      <button class="sm {info.enabled ? 'danger' : 'primary'}" onclick={() => toggle(!info?.enabled)} disabled={busy || db.status !== 'ready'}>{info.enabled ? 'Turn off' : 'Enable'}</button>
    {/if}
  </div>
  <div class="card-body stack">
    <ErrorMsg {error} />
    {#if !info}
      <p class="muted small">Loading…</p>
    {:else if !info.capable}
      <p class="muted small">Available for dedicated PostgreSQL databases. Upgrade this database to a dedicated server first.</p>
    {:else if !info.enabled}
      <p class="muted small">Off. When enabled, every change is archived continuously and a base backup is taken daily on the server's node (the server restarts once).</p>
    {:else}
      <dl class="kv">
        <dt>Earliest point</dt><dd>{info.earliest ? `${absTime(info.earliest)} (${relTime(info.earliest)})` : 'after the first base backup completes'}</dd>
        <dt>Base backups</dt><dd>{(info.base_backups ?? []).length} · {bytes((info.base_backups ?? []).reduce((a, b) => a + b.size_bytes, 0))}</dd>
        <dt>Retention</dt>
        <dd class="row">
          <select bind:value={retention} onchange={saveRetention} disabled={!isAdmin}>
            {#each [1, 3, 7, 14, 30] as d (d)}<option value={d}>{d} days</option>{/each}
          </select>
        </dd>
      </dl>
      {#if isAdmin && info.earliest}
        <form class="row" onsubmit={restore} style="align-items:flex-end;flex-wrap:wrap">
          <label class="field" style="max-width:240px">
            <span>Restore to</span>
            <input type="datetime-local" bind:value={target} max={localNow()} />
            <span class="hint">Empty = now. Your local time.</span>
          </label>
          <button class="primary sm" type="submit" disabled={busy}>{#if busy}<span class="spinner"></span>{/if}Restore into a new database</button>
        </form>
      {/if}
    {/if}
  </div>
</div>
