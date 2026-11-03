<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type UpdateStatus, type RolloutStep } from '../../lib/api';
  import { relTime, absTime } from '../../lib/format';
  import { poll } from '../../lib/poll';
  import { toast } from '../../lib/toast.svelte';
  import { confirmDialog } from '../../lib/confirm.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import Loading from '../../components/Loading.svelte';

  let st = $state<UpdateStatus | null>(null);
  let error = $state('');
  let busy = $state('');
  let showNotes = $state(false);

  async function load() {
    try {
      st = await api.updateStatus();
      error = '';
    } catch (e) {
      error = errorMessage(e);
    }
  }

  let active = $derived(st?.rollout?.status === 'running' || st?.rollout?.status === 'paused');

  onMount(() => {
    load();
    const stop = poll(load, 4000);
    return () => stop();
  });

  async function check() {
    busy = 'check';
    try {
      st = await api.checkUpdate();
      if (st.check_error) toast.error(st.check_error);
      else toast.success(st.update_available ? `paas ${st.latest?.tag} is available` : 'Up to date');
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      busy = '';
    }
  }

  async function start() {
    const v = st?.latest?.tag;
    if (!v) return;
    const ok = await confirmDialog({
      title: `Update to ${v}`,
      message:
        'A backup of the platform state is taken first. Then every control plane (the leader last) and every node is updated and restarted, one at a time: apps keep running, the dashboard may be briefly unavailable while control planes restart. A machine that does not come back healthy is rolled back and the update stops. Offline and cordoned nodes are skipped.',
      confirmLabel: 'Update',
      requireText: v,
    });
    if (!ok) return;
    busy = 'start';
    try {
      await api.startUpdate(v);
      toast.success(`Updating to ${v}`);
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      busy = '';
    }
  }

  async function action(a: 'pause' | 'resume' | 'abort') {
    if (a === 'abort' && !(await confirmDialog({ title: 'Abort the update', message: 'No further machine is updated; the one being updated now finishes (or is rolled back by its own guard). Machines already updated keep the new version.', confirmLabel: 'Abort', danger: true }))) return;
    busy = a;
    try {
      await api.updateAction(a);
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      busy = '';
    }
  }

  async function rollback(id: string, name: string) {
    if (!(await confirmDialog({ title: `Roll back ${name}`, message: 'Puts the previous paasd binary (paasd.prev) back on this machine and restarts it. A node then keeps that binary until the next update. Its database schema is not rolled back.', confirmLabel: 'Roll back', danger: true }))) return;
    busy = 'rb-' + id;
    try {
      await api.rollbackNodeBinary(id);
      toast.success(`${name} is rolling back`);
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      busy = '';
    }
  }

  function stepLabel(s: RolloutStep): string {
    if (s.kind === 'control_plane') return `Control plane ${s.name}${s.leader ? ' (leader)' : ''}`;
    if (s.kind === 'node') return `Node ${s.name}${s.roles?.length ? ' · ' + s.roles.join(', ') : ''}`;
    return 'Backup of the platform state';
  }
  const stepColor: Record<string, string> = { done: 'green', skipped: 'yellow', failed: 'red', rolled_back: 'red', updating: 'blue', starting: 'blue', rolling_back: 'yellow', pending: '' };
  const rolloutColor: Record<string, string> = { running: 'blue', paused: 'yellow', failed: 'red', aborted: 'yellow', done: 'green' };

  let otherNodes = $derived((st?.nodes ?? []).filter((n) => !n.control_plane));
  let behind = (v: string) => !!st?.latest && v !== st.latest.tag;
</script>

<div class="card">
  <div class="card-head">
    <div>
      <h3>Updates</h3>
      <p class="hint">
        Versions of paas on every control plane and node. Updates are verified against the release's SHA-256 checksums and rolled out one machine at a time; the
        previous binary is kept as <code>paasd.prev</code> for a rollback.
      </p>
    </div>
    <button onclick={check} disabled={!!busy}>{#if busy === 'check'}<span class="spinner"></span>{/if}Check now</button>
  </div>
  <div class="card-body stack">
    {#if !st}
      {#if error}<ErrorMsg {error} />{:else}<Loading />{/if}
    {:else}
      <div class="stack">
        <p>
          This control plane runs <strong>{st.version}</strong>
          {#if st.development}<span class="badge yellow">development build</span>{/if}
        </p>
        {#if st.latest}
          <p>
            Latest release: <strong>{st.latest.tag}</strong>, published <span title={absTime(st.latest.published_at)}>{relTime(st.latest.published_at)}</span>
            {#if st.update_available}<span class="badge green">update available</span>{:else}<span class="badge">up to date</span>{/if}
            {#if st.latest.url}· <a href={st.latest.url} target="_blank" rel="noopener noreferrer">release page</a>{/if}
            {#if st.latest.notes}· <button class="link" onclick={() => (showNotes = !showNotes)}>{showNotes ? 'Hide' : 'Show'} release notes</button>{/if}
          </p>
          {#if showNotes && st.latest.notes}<pre class="notes">{st.latest.notes}</pre>{/if}
        {:else if !st.check_enabled}
          <p class="hint">Automatic checks are off (<code>PAAS_UPDATE_CHECK=false</code>). Check now looks once.</p>
        {:else}
          <p class="hint">Not checked yet: the leader looks every 6 hours.</p>
        {/if}
        {#if st.checked_at}<p class="hint">Last checked {relTime(st.checked_at)}{st.check_error ? ': ' + st.check_error : ''}</p>{/if}
        {#if st.development}<p class="hint">A development build is never told a release is newer: what it contains is unknown.</p>{/if}
        {#if st.allow_unverified}<p class="hint"><span class="badge red">unsafe</span> <code>PAAS_UPDATE_ALLOW_UNVERIFIED=true</code>: binaries without a published checksum are installed.</p>{/if}
      </div>

      <div class="table-wrap">
        <table>
          <thead><tr><th>Machine</th><th>Version</th><th>Arch</th><th>Status</th><th></th></tr></thead>
          <tbody>
            {#each st.control_planes as cp (cp.name)}
              <tr>
                <td>Control plane <strong>{cp.name}</strong>{#if cp.leader} <span class="badge blue">leader</span>{/if}</td>
                <td><code>{cp.version || '-'}</code>{#if behind(cp.version)} <span class="badge yellow">behind</span>{/if}</td>
                <td>{cp.arch || '-'}</td>
                <td>{cp.online ? 'online' : 'offline · seen ' + relTime(cp.last_seen_at)}</td>
                <td>{#if cp.node_id}<button class="sm" disabled={!!busy || active || !cp.online} onclick={() => rollback(cp.node_id!, cp.name)}>Roll back</button>{/if}</td>
              </tr>
            {/each}
            {#each otherNodes as n (n.id)}
              <tr>
                <td>Node <strong>{n.name}</strong> <span class="hint">{n.roles.join(', ')}</span></td>
                <td><code>{n.version || '-'}</code>{#if behind(n.version)} <span class="badge yellow">behind</span>{/if}{#if n.pinned} <span class="badge" title="Rolled back: keeps its binary until the next update">pinned</span>{/if}</td>
                <td>{n.arch || '-'}</td>
                <td>{n.status}</td>
                <td><button class="sm" disabled={!!busy || active || !n.online} onclick={() => rollback(n.id, n.name)}>Roll back</button></td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>

      {#if st.rollout}
        {@const r = st.rollout}
        <div class="stack">
          <div class="row">
            <h4>Update to {r.version}</h4>
            <span class="badge {rolloutColor[r.status]}">{r.status}</span>
            <span class="hint">started {relTime(r.created_at)} by {r.requested_by}</span>
          </div>
          {#if r.message}<p class="hint" style={r.status === 'failed' ? 'color:var(--red)' : ''}>{r.message}</p>{/if}
          <div class="table-wrap">
            <table>
              <thead><tr><th>Step</th><th>Status</th><th>Details</th></tr></thead>
              <tbody>
                {#each r.steps as s, i (i)}
                  <tr>
                    <td>{stepLabel(s)}</td>
                    <td><span class="badge {stepColor[s.status] ?? ''}">{#if s.status === 'updating' || s.status === 'starting' || s.status === 'rolling_back'}<span class="spinner"></span>{/if}{s.status.replace('_', ' ')}</span></td>
                    <td class="hint">{s.message ?? ''}</td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
          {#if active}
            <div class="row">
              {#if r.status === 'running'}<button onclick={() => action('pause')} disabled={!!busy}>Pause</button>{/if}
              {#if r.status === 'paused'}<button class="primary" onclick={() => action('resume')} disabled={!!busy}>Resume</button>{/if}
              <button class="danger" onclick={() => action('abort')} disabled={!!busy}>Abort</button>
            </div>
          {/if}
        </div>
      {/if}

      {#if !active && st.latest && (st.update_available || st.rollout?.status === 'failed' || st.rollout?.status === 'aborted')}
        <div>
          <button class="primary" onclick={start} disabled={!!busy}>{#if busy === 'start'}<span class="spinner"></span>{/if}Update to {st.latest.tag}</button>
        </div>
        <p class="hint">
          Rolling back a machine restores only its binary: the database stays on the newer schema, which works because migrations are additive. If a release's notes say otherwise, restore the state backup
          taken by the update instead.
        </p>
      {/if}
    {/if}
  </div>
</div>

<style>
  .notes {
    white-space: pre-wrap;
    max-height: 320px;
    overflow: auto;
    font-size: 13px;
    padding: 12px;
    border: 1px solid var(--border);
    border-radius: 6px;
  }
  .link {
    background: none;
    border: none;
    padding: 0;
    color: var(--accent, inherit);
    text-decoration: underline;
    cursor: pointer;
  }
</style>
