<script lang="ts">
  import { api, errorMessage, type ServiceDetail } from '../../lib/api';
  import { relTime } from '../../lib/format';
  import { toast } from '../../lib/toast.svelte';
  import { confirmDialog } from '../../lib/confirm.svelte';
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
      toast.success(`Added ${d.hostname}`);
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
    if (!(await confirmDialog({ title: 'Remove domain', message: `Stop serving ${detail.service.name} on ${host}?`, confirmLabel: 'Remove', danger: true }))) return;
    try {
      await api.deleteDomain(detail.service.id, id);
      toast.success(`Removed ${host}`);
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
      <thead><tr><th>Hostname</th><th>Type</th><th class="hide-sm">Added</th><th class="right"></th></tr></thead>
      <tbody>
        {#each detail.domains as d (d.id)}
          <tr>
            <td>
              <a href="{scheme}://{d.hostname}" target="_blank" rel="noreferrer" class="row nowrap" style="gap:4px;display:inline-flex">{d.hostname}<Icon name="external" size={12} /></a>
            </td>
            <td><span class="badge {d.is_generated ? '' : 'blue'}">{d.is_generated ? 'generated' : 'custom'}</span></td>
            <td class="hide-sm faint small">{relTime(d.created_at)}</td>
            <td class="right">
              {#if canEdit && !d.is_generated}
                <button class="sm danger" onclick={() => remove(d.id, d.hostname)} aria-label="Remove {d.hostname}"><Icon name="trash" size={12} /></button>
              {/if}
            </td>
          </tr>
        {:else}
          <tr><td colspan="4" class="muted">No domains.</td></tr>
        {/each}
      </tbody>
    </table>
  </div>

  {#if canEdit}
    <form class="card" onsubmit={add}>
      <div class="card-head"><h3>Add a custom domain</h3></div>
      <div class="card-body stack">
        <div class="row nowrap">
          <label class="sr-only" for="newdomain">Hostname</label>
          <input id="newdomain" class="mono" bind:value={hostname} placeholder="www.example.com" spellcheck="false" autocomplete="off" />
          <button class="primary" type="submit" disabled={busy || !hostname.trim()}>{#if busy}<span class="spinner"></span>{/if}Add</button>
        </div>
        <ErrorMsg {error} />
        <div class="alert info">
          <Icon name="globe" />
          <div class="small">
            <b>DNS:</b> point an <code>A</code> record for {lastAdded || hostname.trim() || 'your domain'} to your edge node's public IP (or a <code>CNAME</code> to one of the generated domains).
            TLS certificates are issued automatically once DNS resolves.
            {#if detail.domains.find((d) => d.is_generated)}
              {@const g = detail.domains.find((d) => d.is_generated)!}
              <div class="row nowrap" style="margin-top:6px">CNAME target: <code>{g.hostname}</code><CopyButton text={g.hostname} /></div>
            {/if}
          </div>
        </div>
      </div>
    </form>
  {/if}
</div>
