<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type VolumeBackups } from '../lib/api';
  import { relTime, absTime, bytes } from '../lib/format';
  import { toast } from '../lib/toast.svelte';
  import { confirmDialog } from '../lib/confirm.svelte';
  import { href } from '../lib/router.svelte';
  import { session } from '../lib/session.svelte';
  import ErrorMsg from './ErrorMsg.svelte';
  import Loading from './Loading.svelte';

  let { serviceId, canEdit }: { serviceId: string; canEdit: boolean } = $props();

  let data = $state<VolumeBackups | null>(null);
  let error = $state('');
  let busy = $state('');

  async function load() {
    try {
      data = await api.volumeBackups(serviceId);
      error = '';
    } catch (e) {
      error = errorMessage(e);
    }
  }
  onMount(load);

  async function backupNow() {
    busy = 'backup';
    try {
      const b = await api.volumeBackupNow(serviceId);
      if (b.status === 'failed') toast.error(b.error || 'Backup failed');
      else toast.success('Volumes backed up');
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      busy = '';
    }
  }

  async function recover(backupId?: string) {
    const msg = backupId
      ? 'The service moves to another node and its volumes are filled from this backup.'
      : `The service moves to another node and its volumes are filled from the newest off-site backup${done[0] ? ` (${absTime(done[0].created_at)})` : ''}. Changes since that backup are lost.`;
    if (!(await confirmDialog({ title: 'Recover on another node', message: msg, confirmLabel: 'Recover', danger: true }))) return;
    await run(false, backupId);
  }

  async function run(allowEmpty: boolean, backupId?: string) {
    busy = 'recover';
    try {
      const r = await api.recoverService(serviceId, allowEmpty, backupId);
      toast.success(`Restoring on ${r.node} ${r.rpo_at ? `with the data of ${absTime(r.rpo_at)}` : 'with empty volumes'}; progress is shown under Node`);
      await load();
    } catch (e) {
      const m = errorMessage(e);
      busy = '';
      if (!allowEmpty && /allow_empty/.test(m)) {
        if (await confirmDialog({ title: 'No backup to recover from', message: 'There is no off-site volume backup of this service. Start it on another node with empty volumes instead?', confirmLabel: 'Start empty', danger: true })) {
          await run(true);
        }
      } else toast.error(m);
    } finally {
      busy = '';
    }
  }

  let done = $derived((data?.backups ?? []).filter((b) => b.status === 'completed'));
</script>

<div class="card">
  <div class="card-head">
    <h3>Volume backups</h3>
    {#if canEdit && data?.offsite && data.node_online}
      <button class="sm" onclick={backupNow} disabled={!!busy}>{#if busy === 'backup'}<span class="spinner"></span>{/if}Back up now</button>
    {/if}
  </div>
  <div class="card-body stack small">
    {#if !data}
      {#if error}<ErrorMsg {error} />{:else}<Loading />{/if}
    {:else}
      {#if !data.offsite}
        <p class="muted">
          Off-site storage is not configured, so volumes live only on the service's node.
          {#if session.me?.user.is_admin}Set it up in <a href={href('/settings/backups')}>Settings → Backups</a>.{:else}Ask an administrator to set it up.{/if}
        </p>
      {:else if !data.enabled}
        <p class="muted">Volume backups are off for this service.</p>
      {:else}
        <p class="muted">The volumes are archived {data.every_hours ? `every ${data.every_hours} hours` : 'daily'} with the container paused for a moment; the last 7 are kept.</p>
      {/if}
      {#if !data.node_online}
        <p><span class="badge red">node offline</span> The node holding the volumes is offline. If it is not coming back, recover the service on another node.</p>
        {#if canEdit}
          <div><button class="sm danger" onclick={() => recover()} disabled={!!busy}>{#if busy === 'recover'}<span class="spinner"></span>{/if}Recover on another node</button></div>
        {/if}
      {/if}
      {#if (data.backups ?? []).length > 0}
        <table>
          <tbody>
            {#each data.backups ?? [] as b (b.id)}
              <tr>
                <td title={absTime(b.created_at)} class="nowrap">{relTime(b.created_at)}</td>
                <td>{#if b.status === 'completed'}{bytes(b.size_bytes)}{:else}<span class="badge {b.status === 'failed' ? 'red' : 'yellow'}" title={b.error}>{b.status}</span>{/if}</td>
                <td class="right">
                  {#if canEdit && !data.node_online && b.status === 'completed' && done[0]?.id !== b.id}
                    <button class="sm" onclick={() => recover(b.id)} disabled={!!busy}>Recover from this</button>
                  {/if}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      {/if}
    {/if}
  </div>
</div>
