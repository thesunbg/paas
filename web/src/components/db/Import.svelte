<script lang="ts">
  import { api, errorMessage, type DatabaseDetail } from '../../lib/api';
  import { toast } from '../../lib/toast.svelte';
  import { confirmDialog } from '../../lib/confirm.svelte';

  let { detail, reload }: { detail: DatabaseDetail; reload: () => Promise<void> } = $props();
  const db = $derived(detail.database);
  const examples: Record<string, string> = {
    postgres: 'postgresql://user:password@old-server:5432/dbname',
    mysql: 'mysql://user:password@old-server:3306/dbname',
    mongo: 'mongodb://user:password@old-server:27017/dbname?authSource=admin',
  };
  let source = $state('');
  let busy = $state(false);

  async function submit(e: SubmitEvent) {
    e.preventDefault();
    if (!source.trim()) return;
    const ok = await confirmDialog({
      title: 'Import database',
      message: `Copy the source database into ${db.name}? Tables that exist in both are replaced. A backup of ${db.name} is taken first.`,
      confirmLabel: 'Import',
    });
    if (!ok) return;
    busy = true;
    try {
      await api.importDatabase(db.id, source.trim());
      source = '';
      toast.success('Import started');
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
      <h3>Import from another server</h3>
      <p class="hint">Move an existing database here: the platform dumps it and loads it into {db.name}.</p>
    </div>
  </div>
  <form class="card-body stack" onsubmit={submit}>
    {#if db.status === 'importing'}
      <p class="small"><span class="spinner"></span> Importing… large databases can take a while.</p>
    {:else}
      <input type="password" class="mono" bind:value={source} placeholder={examples[db.engine]} autocomplete="off" spellcheck="false" />
      <p class="hint">The source must accept connections from this platform's servers (open its port to them). The URL is used once and not stored. {#if db.engine === 'postgres'}The source's PostgreSQL version must not be newer than {db.version || 'this one'}.{/if}</p>
      <div><button class="sm primary" disabled={busy || !source.trim() || db.status !== 'ready'}>{#if busy}<span class="spinner"></span>{/if}Import</button></div>
    {/if}
  </form>
</div>
