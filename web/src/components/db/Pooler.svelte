<script lang="ts">
  import { api, errorMessage, type DatabaseDetail } from '../../lib/api';
  import { toast } from '../../lib/toast.svelte';
  import Secret from '../Secret.svelte';
  import { t } from '../../lib/i18n.svelte';

  let { detail, isAdmin, reload }: { detail: DatabaseDetail; isAdmin: boolean; reload: () => Promise<void> } = $props();
  const db = $derived(detail.database);
  const on = $derived(!!db.options?.pooler);
  let busy = $state(false);

  async function toggle() {
    busy = true;
    try {
      await api.databasePooler(db.id, !on);
      toast.success(on ? t('Pooler removed') : t('PgBouncer is running'));
      await reload();
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      busy = false;
    }
  }
</script>

<div class="card">
  <div class="card-head">
    <div>
      <h3>{t('Connection pooling')}</h3>
      <p class="hint">{t('PgBouncer in transaction mode: thousands of client connections over a few server connections.')}</p>
    </div>
    {#if isAdmin}
      <button class="sm {on ? 'danger' : 'primary'}" onclick={toggle} disabled={busy || db.status !== 'ready'}>{#if busy}<span class="spinner"></span>{/if}{on ? t('Turn off') : t('Enable PgBouncer')}</button>
    {/if}
  </div>
  <div class="card-body stack">
    {#if on && detail.connection?.pooled_url}
      <dl class="kv"><dt>{t('Pooled URL')}</dt><dd><Secret value={detail.connection.pooled_url} /></dd></dl>
      <p class="hint">{t('Linked services also receive it as')} <code>&lt;variable&gt;_POOLED</code> ({t('for example')} <code>DATABASE_URL_POOLED</code>). {t('Use the direct URL for migrations and session features (LISTEN/NOTIFY, advisory locks).')}</p>
    {:else if on}
      <p class="muted small">{t('Starting…')}</p>
    {:else}
      <p class="muted small">{t('Off. Recommended for serverless functions and apps with many workers.')}</p>
    {/if}
  </div>
</div>
