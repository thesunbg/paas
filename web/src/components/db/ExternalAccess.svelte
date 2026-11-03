<script lang="ts">
  import { api, ApiError, errorMessage, type DatabaseDetail } from '../../lib/api';
  import { toast } from '../../lib/toast.svelte';
  import { confirmDialog } from '../../lib/confirm.svelte';
  import ErrorMsg from '../ErrorMsg.svelte';
  import Secret from '../Secret.svelte';
  import CopyButton from '../CopyButton.svelte';
  import { t } from '../../lib/i18n.svelte';

  let { detail, isAdmin, reload }: { detail: DatabaseDetail; isAdmin: boolean; reload: () => Promise<void> } = $props();

  const db = $derived(detail.database);
  const ext = $derived(detail.external);
  let editing = $state(false);
  let cidrs = $state('');
  let busy = $state(false);
  let error = $state('');
  // Version of the options the form was opened on; sent as If-Match.
  let formVersion = 0;

  function startEdit() {
    formVersion = db.options_version;
    cidrs = (ext?.allowed_cidrs ?? []).join('\n');
    editing = true;
    error = '';
  }

  async function myIP() {
    try {
      const { ip } = await api.myIP();
      cidrs = (cidrs ? cidrs + '\n' : '') + ip;
    } catch {
      error = t('Could not detect your IP address');
    }
  }

  async function save(e: SubmitEvent) {
    e.preventDefault();
    const list = cidrs.split(/[\s,]+/).filter(Boolean);
    if (list.some((c) => c === '0.0.0.0/0' || c === '::/0')) {
      const ok = await confirmDialog({ title: t('Allow every address'), message: t('Anyone on the internet will be able to reach this database (a password is still required). Continue?'), confirmLabel: t('Allow everyone'), danger: true });
      if (!ok) return;
    }
    busy = true;
    error = '';
    try {
      await api.databasePublic(db.id, true, list, formVersion);
      toast.success(t('External access updated'));
      editing = false;
      await reload();
    } catch (err) {
      error = errorMessage(err);
      if (err instanceof ApiError && err.status === 412) await reload();
    } finally {
      busy = false;
    }
  }

  async function disable() {
    if (!(await confirmDialog({ title: t('Turn off external access'), message: t('Clients outside the platform will be disconnected and the public port released.'), confirmLabel: t('Turn off'), danger: true }))) return;
    busy = true;
    try {
      await api.databasePublic(db.id, false);
      toast.success(t('External access turned off'));
      await reload();
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      busy = false;
    }
  }
</script>

<div class="card">
  <div class="card-head">
    <div>
      <h3>{t('External access')}</h3>
      <p class="hint">{t('Connect from your computer or servers outside the platform.')}</p>
    </div>
    {#if ext && isAdmin && !editing}
      <div class="row"><button class="sm" onclick={startEdit}>{t('Edit allowlist')}</button><button class="sm danger" onclick={disable} disabled={busy}>{t('Turn off')}</button></div>
    {/if}
  </div>
  <div class="card-body stack">
    {#if ext && !editing}
      <dl class="kv">
        <dt>{t('Host')}</dt><dd class="row nowrap"><code>{ext.host}</code><CopyButton text={ext.host} /></dd>
        <dt>{t('Port')}</dt><dd><code>{ext.port}</code></dd>
        <dt>TLS</dt><dd>{ext.tls ? t('required for PostgreSQL, Redis (rediss://) and MongoDB (tls=true)') : t('off — enable HTTPS on the platform to encrypt connections')}</dd>
        <dt>URL</dt><dd><Secret value={ext.url} /></dd>
        <dt>{t('Allowed from')}</dt><dd class="mono small">{ext.allowed_cidrs.join(', ')}</dd>
      </dl>
      <p class="hint">{t('Open TCP port {port} in the server firewall. Connections from other addresses are closed immediately.', { port: ext.port })}</p>
    {:else if editing || (!ext && isAdmin)}
      {#if !editing}
        <p class="muted small">{t('Off. Only services on the platform can connect.')}</p>
        <div><button class="primary sm" onclick={startEdit} disabled={db.status !== 'ready'}>{t('Enable external access')}</button></div>
      {:else}
        <form class="stack" onsubmit={save}>
          <label class="field">
            <span>{t('Allowed IP addresses or CIDR ranges')}</span>
            <textarea class="mono" rows="4" bind:value={cidrs} placeholder={'203.0.113.10\n198.51.100.0/24'}></textarea>
            <span class="hint">{t('One per line.')} <code>0.0.0.0/0</code> {t('allows everyone.')}</span>
          </label>
          <ErrorMsg {error} />
          <div class="row">
            <button class="primary sm" type="submit" disabled={busy}>{#if busy}<span class="spinner"></span>{/if}{t('Save')}</button>
            <button class="sm" type="button" onclick={myIP}>{t('Add my IP')}</button>
            <button class="sm ghost" type="button" onclick={() => (editing = false)}>{t('Cancel')}</button>
          </div>
        </form>
      {/if}
    {:else}
      <p class="muted small">{t('Off. Only services on the platform can connect.')}</p>
    {/if}
  </div>
</div>
