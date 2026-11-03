<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type VolumeBackups } from '../lib/api';
  import { relTime, absTime, bytes } from '../lib/format';
  import { toast } from '../lib/toast.svelte';
  import { confirmDialog } from '../lib/confirm.svelte';
  import { t } from '../lib/i18n.svelte';
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
      if (b.status === 'failed') toast.error(b.error || t('Backup failed'));
      else toast.success(t('Volumes backed up'));
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      busy = '';
    }
  }

  async function recover(backupId?: string) {
    const msg = backupId
      ? t('The service moves to another node and its volumes are filled from this backup.')
      : done[0]
        ? t('The service moves to another node and its volumes are filled from the newest off-site backup ({time}). Changes since that backup are lost.', { time: absTime(done[0].created_at) })
        : t('The service moves to another node and its volumes are filled from the newest off-site backup. Changes since that backup are lost.');
    if (!(await confirmDialog({ title: t('Recover on another node'), message: msg, confirmLabel: t('Recover'), danger: true }))) return;
    await run(false, backupId);
  }

  async function run(allowEmpty: boolean, backupId?: string) {
    busy = 'recover';
    try {
      const r = await api.recoverService(serviceId, allowEmpty, backupId);
      toast.success(
        r.rpo_at
          ? t('Restoring on {node} with the data of {time}; progress is shown under Node', { node: r.node, time: absTime(r.rpo_at) })
          : t('Restoring on {node} with empty volumes; progress is shown under Node', { node: r.node }),
      );
      await load();
    } catch (e) {
      const m = errorMessage(e);
      busy = '';
      if (!allowEmpty && /allow_empty/.test(m)) {
        if (await confirmDialog({ title: t('No backup to recover from'), message: t('There is no off-site volume backup of this service. Start it on another node with empty volumes instead?'), confirmLabel: t('Start empty'), danger: true })) {
          await run(true);
        }
      } else toast.error(m);
    } finally {
      busy = '';
    }
  }

  // Backup statuses (running | completed | failed).
  const statusLabel = $derived<Record<string, string>>({ running: t('running'), completed: t('completed'), failed: t('failed') });
  let done = $derived((data?.backups ?? []).filter((b) => b.status === 'completed'));
</script>

<div class="card">
  <div class="card-head">
    <h3>{t('Volume backups')}</h3>
    {#if canEdit && data?.offsite && data.node_online}
      <button class="sm" onclick={backupNow} disabled={!!busy}>{#if busy === 'backup'}<span class="spinner"></span>{/if}{t('Back up now')}</button>
    {/if}
  </div>
  <div class="card-body stack small">
    {#if !data}
      {#if error}<ErrorMsg {error} />{:else}<Loading />{/if}
    {:else}
      {#if !data.offsite}
        <p class="muted">
          {t("Off-site storage is not configured, so volumes live only on the service's node.")}
          {#if session.me?.user.is_admin}{t('Set it up in')} <a href={href('/settings/backups')}>{t('Settings → Backups')}</a>.{:else}{t('Ask an administrator to set it up.')}{/if}
        </p>
      {:else if !data.enabled}
        <p class="muted">{t('Volume backups are off for this service.')}</p>
      {:else}
        <p class="muted">{data.every_hours ? t('The volumes are archived every {n} hours with the container paused for a moment; the last 7 are kept.', { n: data.every_hours }) : t('The volumes are archived daily with the container paused for a moment; the last 7 are kept.')}</p>
      {/if}
      {#if !data.node_online}
        <p><span class="badge red">{t('node offline')}</span> {t('The node holding the volumes is offline. If it is not coming back, recover the service on another node.')}</p>
        {#if canEdit}
          <div><button class="sm danger" onclick={() => recover()} disabled={!!busy}>{#if busy === 'recover'}<span class="spinner"></span>{/if}{t('Recover on another node')}</button></div>
        {/if}
      {/if}
      {#if (data.backups ?? []).length > 0}
        <table>
          <tbody>
            {#each data.backups ?? [] as b (b.id)}
              <tr>
                <td title={absTime(b.created_at)} class="nowrap">{relTime(b.created_at)}</td>
                <td>{#if b.status === 'completed'}{bytes(b.size_bytes)}{:else}<span class="badge {b.status === 'failed' ? 'red' : 'yellow'}" title={b.error}>{statusLabel[b.status] ?? b.status}</span>{/if}</td>
                <td class="right">
                  {#if canEdit && !data.node_online && b.status === 'completed' && done[0]?.id !== b.id}
                    <button class="sm" onclick={() => recover(b.id)} disabled={!!busy}>{t('Recover from this')}</button>
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
