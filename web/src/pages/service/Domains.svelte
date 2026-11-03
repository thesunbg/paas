<script lang="ts">
  import { api, errorMessage, type ServiceDetail } from '../../lib/api';
  import { relTime } from '../../lib/format';
  import { toast } from '../../lib/toast.svelte';
  import { confirmDialog } from '../../lib/confirm.svelte';
  import { t } from '../../lib/i18n.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import Icon from '../../components/Icon.svelte';
  import CopyButton from '../../components/CopyButton.svelte';

  let { detail, reload, canEdit }: { detail: ServiceDetail; reload: () => Promise<void>; canEdit: boolean } = $props();

  let hostname = $state('');
  let busy = $state(false);
  let error = $state('');
  let lastAdded = $state('');

  async function add(e: SubmitEvent) {
    e.preventDefault();
    const h = hostname.trim().toLowerCase();
    if (!h) return;
    busy = true;
    error = '';
    try {
      const d = await api.addDomain(detail.service.id, h);
      toast.success(t('Added {host}', { host: d.hostname }));
      lastAdded = d.hostname;
      hostname = '';
      await reload();
    } catch (err) {
      error = errorMessage(err);
    } finally {
      busy = false;
    }
  }

  async function remove(id: string, host: string) {
    if (!(await confirmDialog({ title: t('Remove domain'), message: t('Stop serving {name} on {host}?', { name: detail.service.name, host }), confirmLabel: t('Remove'), danger: true }))) return;
    try {
      await api.deleteDomain(detail.service.id, id);
      toast.success(t('Removed {host}', { host }));
      if (lastAdded === host) lastAdded = '';
      await reload();
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }

  const scheme = $derived((detail.urls[0] ?? 'http://').split('://')[0]);
</script>

<div class="stack-lg">
  <div class="card table-wrap">
    <table>
      <thead><tr><th>{t('Hostname')}</th><th>{t('Type')}</th><th class="hide-sm">{t('Added')}</th><th class="right"></th></tr></thead>
      <tbody>
        {#each detail.domains as d (d.id)}
          <tr>
            <td>
              <a href="{scheme}://{d.hostname}" target="_blank" rel="noreferrer" class="row nowrap" style="gap:4px;display:inline-flex">{d.hostname}<Icon name="external" size={12} /></a>
            </td>
            <td><span class="badge {d.is_generated ? '' : 'blue'}">{d.is_generated ? t('generated') : t('custom')}</span></td>
            <td class="hide-sm faint small">{relTime(d.created_at)}</td>
            <td class="right">
              {#if canEdit && !d.is_generated}
                <button class="sm danger" onclick={() => remove(d.id, d.hostname)} aria-label={t('Remove {host}', { host: d.hostname })}><Icon name="trash" size={12} /></button>
              {/if}
            </td>
          </tr>
        {:else}
          <tr><td colspan="4" class="muted">{t('No domains.')}</td></tr>
        {/each}
      </tbody>
    </table>
  </div>

  {#if canEdit}
    <form class="card" onsubmit={add}>
      <div class="card-head"><h3>{t('Add a custom domain')}</h3></div>
      <div class="card-body stack">
        <div class="row nowrap">
          <label class="sr-only" for="newdomain">{t('Hostname')}</label>
          <input id="newdomain" class="mono" bind:value={hostname} placeholder="www.example.com" spellcheck="false" autocomplete="off" />
          <button class="primary" type="submit" disabled={busy || !hostname.trim()}>{#if busy}<span class="spinner"></span>{/if}{t('Add')}</button>
        </div>
        <ErrorMsg {error} />
        <div class="alert info">
          <Icon name="globe" />
          <div class="small">
            <b>DNS:</b> {t('point an')} <code>A</code> {t("record for {host} to your edge node's public IP (or a", { host: lastAdded || hostname.trim() || t('your domain') })} <code>CNAME</code> {t('to one of the generated domains).')}
            {t('TLS certificates are issued automatically once DNS resolves.')}
            {#if detail.domains.find((d) => d.is_generated)}
              {@const g = detail.domains.find((d) => d.is_generated)!}
              <div class="row nowrap" style="margin-top:6px">{t('CNAME target:')} <code>{g.hostname}</code><CopyButton text={g.hostname} /></div>
            {/if}
          </div>
        </div>
      </div>
    </form>
  {/if}
</div>
