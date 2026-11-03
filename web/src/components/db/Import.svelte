<script lang="ts">
  import { api, errorMessage, type DatabaseDetail } from '../../lib/api';
  import { toast } from '../../lib/toast.svelte';
  import { confirmDialog } from '../../lib/confirm.svelte';
  import { t } from '../../lib/i18n.svelte';

  let { detail, reload }: { detail: DatabaseDetail; reload: () => Promise<void> } = $props();
  const db = $derived(detail.database);
  const examples: Record<string, string> = {
    postgres: 'postgresql://user:password@old-server:5432/dbname',
    mysql: 'mysql://user:password@old-server:3306/dbname',
    mariadb: 'mysql://user:password@old-server:3306/dbname',
    mongo: 'mongodb://user:password@old-server:27017/dbname?authSource=admin',
  };
  let source = $state('');
  let busy = $state(false);

  async function submit(e: SubmitEvent) {
    e.preventDefault();
    if (!source.trim()) return;
    const ok = await confirmDialog({
      title: t('Import database'),
      message: t('Copy the source database into {name}? Tables that exist in both are replaced. A backup of {name} is taken first.', { name: db.name }),
      confirmLabel: t('Import'),
    });
    if (!ok) return;
    busy = true;
    try {
      await api.importDatabase(db.id, source.trim());
      source = '';
      toast.success(t('Import started'));
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
      <h3>{t('Import from another server')}</h3>
      <p class="hint">{t('Move an existing database here: the platform dumps it and loads it into {name}.', { name: db.name })}</p>
    </div>
  </div>
  <form class="card-body stack" onsubmit={submit}>
    {#if db.status === 'importing'}
      <p class="small"><span class="spinner"></span> {t('Importing… large databases can take a while.')}</p>
    {:else}
      <input type="password" class="mono" bind:value={source} placeholder={examples[db.engine]} autocomplete="off" spellcheck="false" />
      <p class="hint">{t("The source must accept connections from this platform's servers (open its port to them). The URL is used once and not stored.")} {#if db.engine === 'postgres'}{db.version ? t("The source's PostgreSQL version must not be newer than {version}.", { version: db.version }) : t("The source's PostgreSQL version must not be newer than this one.")}{/if}</p>
      <div><button class="sm primary" disabled={busy || !source.trim() || db.status !== 'ready'}>{#if busy}<span class="spinner"></span>{/if}{t('Import')}</button></div>
    {/if}
  </form>
</div>
