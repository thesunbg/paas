<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type VolumeMove, type VolumeMoveStatus, type VolumeMovePhase } from '../lib/api';
  import { relTime, absTime, bytes, mb } from '../lib/format';
  import { poll } from '../lib/poll';
  import { toast } from '../lib/toast.svelte';
  import { confirmDialog } from '../lib/confirm.svelte';
  import ErrorMsg from './ErrorMsg.svelte';
  import Loading from './Loading.svelte';

  // Node placement of a service with volumes: where it lives, a move to another node, its progress.
  let { serviceId }: { serviceId: string } = $props();

  let data = $state<VolumeMoveStatus | null>(null);
  let error = $state('');
  let busy = $state(false);
  let target = $state('');
  let retainDays = $state(7);

  async function load() {
    try {
      data = await api.volumeMove(serviceId);
      error = '';
      if (data.targets.length > 0 && !data.targets.some((t) => t.id === target)) target = data.targets[0].id;
    } catch (e) {
      error = errorMessage(e);
    }
  }
  onMount(() => {
    load();
    // Fast while a move runs (to show its phase), slow otherwise.
    let last = 0;
    return poll(() => {
      if (data?.active || Date.now() - last > 30000) {
        last = Date.now();
        return load();
      }
    }, 2000);
  });

  const steps: { phase: VolumeMovePhase; label: string }[] = [
    { phase: 'stopping', label: 'Stop' },
    { phase: 'copying', label: 'Copy' },
    { phase: 'verifying', label: 'Verify' },
    { phase: 'switching', label: 'Switch' },
    { phase: 'starting', label: 'Start' },
  ];
  const restoreSteps: { phase: VolumeMovePhase; label: string }[] = [
    { phase: 'copying', label: 'Download' },
    { phase: 'verifying', label: 'Check' },
    { phase: 'switching', label: 'Switch' },
    { phase: 'starting', label: 'Start' },
  ];
  function stepState(m: VolumeMove, i: number, list: typeof steps): 'done' | 'cur' | '' {
    const cur = list.findIndex((s) => s.phase === m.phase);
    if (m.phase === 'pending') return '';
    if (cur < 0) return '';
    return i < cur ? 'done' : i === cur ? 'cur' : '';
  }
  const badge: Record<string, string> = { done: 'green', failed: 'red', aborted: 'yellow' };

  function nodeName(id: string | null): string {
    if (!id) return '—';
    if (data?.node?.id === id) return data.node.name;
    return data?.targets.find((t) => t.id === id)?.name ?? id.slice(0, 8);
  }

  async function start() {
    const t = data?.targets.find((x) => x.id === target);
    if (!t) return;
    const ok = await confirmDialog({
      title: `Move to ${t.name}`,
      message:
        `The service stops, its volumes are copied to ${t.name} over the cluster mesh and checked, then it starts there and traffic follows. ` +
        `It is down for the whole copy (about the volume size divided by the network speed between the nodes). ` +
        `If anything fails before the switch, it starts again on ${data?.node?.name ?? 'its node'}; the old copy is kept ${retainDays} days.`,
      confirmLabel: 'Move',
      danger: true,
    });
    if (!ok) return;
    busy = true;
    try {
      await api.startVolumeMove(serviceId, t.id, retainDays);
      toast.success(`Moving to ${t.name}`);
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      busy = false;
    }
  }

  async function abort() {
    if (!(await confirmDialog({ title: 'Abort the move', message: 'The copy stops, the copy on the new node is removed and the service starts again on its node.', confirmLabel: 'Abort', danger: true }))) return;
    busy = true;
    try {
      await api.abortVolumeMove(serviceId);
      toast.success('Aborting');
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      busy = false;
    }
  }

  let history = $derived((data?.history ?? []).filter((m) => m.id !== data?.active?.id).slice(0, 5));
</script>

<div class="card">
  <div class="card-head"><h3>Node</h3></div>
  <div class="card-body stack small">
    {#if !data}
      {#if error}<ErrorMsg {error} />{:else}<Loading />{/if}
    {:else}
      <p>
        {#if data.node}
          The volumes live on <b>{data.node.name}</b>{#if !data.node.online} <span class="badge red">offline</span>{/if}.
        {:else}
          Not placed on a node yet.
        {/if}
      </p>
      {#if data.active}
        {@const m = data.active}
        {@const list = m.kind === 'restore' ? restoreSteps : steps}
        <div class="stack" aria-live="polite">
          <div>
            <b>{m.kind === 'restore' ? 'Restoring' : 'Moving'}</b> to {nodeName(m.to_node)}
            {#if m.abort_requested}<span class="badge yellow">aborting</span>{/if}
          </div>
          <ol class="mv-steps" aria-label="Progress">
            {#each list as st, i (st.phase)}
              <li class={stepState(m, i, list)} aria-current={stepState(m, i, list) === 'cur' ? 'step' : undefined}>{st.label}</li>
            {/each}
          </ol>
          {#if m.kind === 'move'}<p class="muted">The service is down until the Start step finishes.</p>{/if}
          {#if m.rpo_at}<p class="muted">Data as of the backup of <span title={absTime(m.rpo_at)}>{relTime(m.rpo_at)}</span>; later writes are lost.</p>{/if}
          {#if data.targets.length > 0 && ['pending', 'stopping', 'copying', 'verifying'].includes(m.phase) && !m.abort_requested}
            <div><button class="sm danger" onclick={abort} disabled={busy}>Abort</button></div>
          {/if}
        </div>
      {:else if data.node?.online && data.movable && data.targets.length > 0}
        <div class="row" style="gap:8px;flex-wrap:wrap;align-items:end">
          <label class="field">
            <span>Move to</span>
            <select bind:value={target}>
              {#each data.targets as t (t.id)}
                <option value={t.id}>{t.name} · {mb(t.memory_available_mb)} free</option>
              {/each}
            </select>
          </label>
          <label class="field" title="Days the old copy stays on the current node after the move succeeded">
            <span>Keep old copy (days)</span>
            <input type="number" min="1" max="90" bind:value={retainDays} style="width:6em" />
          </label>
          <button class="sm" onclick={start} disabled={busy || !target}>{#if busy}<span class="spinner"></span>{/if}Move to node</button>
        </div>
        <p class="hint">Short downtime: the service stops while its volumes are copied and checked.</p>
      {:else if !data.movable && data.reason}
        <p class="muted">{data.reason}</p>
      {/if}
      {#if history.length > 0}
        <table>
          <tbody>
            {#each history as m (m.id)}
              <tr>
                <td class="nowrap" title={absTime(m.created_at)}>{relTime(m.created_at)}</td>
                <td>{m.kind === 'restore' ? 'restore' : 'move'} → {nodeName(m.to_node)}{#if m.bytes} · {bytes(m.bytes)}{/if}</td>
                <td class="right"><span class="badge {badge[m.phase] ?? ''}" title={m.error}>{m.phase}</span></td>
              </tr>
              {#if m.error}<tr><td colspan="3" class="faint">{m.error}</td></tr>{/if}
            {/each}
          </tbody>
        </table>
      {/if}
    {/if}
  </div>
</div>

<style>
  .mv-steps {
    display: flex;
    gap: 4px;
    list-style: none;
    margin: 0;
    padding: 0;
    flex-wrap: wrap;
  }
  .mv-steps li {
    padding: 2px 8px;
    border-radius: 999px;
    border: 1px solid var(--border);
    color: var(--text-faint);
    font-size: 12px;
  }
  .mv-steps li.done {
    color: var(--green);
    background: var(--green-soft);
    border-color: transparent;
  }
  .mv-steps li.cur {
    color: var(--text);
    background: var(--accent-soft);
    border-color: var(--accent);
    font-weight: 600;
  }
</style>
