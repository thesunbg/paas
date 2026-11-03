<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type DatabaseDetail, type HAInfo } from '../../lib/api';
  import { poll } from '../../lib/poll';
  import { toast } from '../../lib/toast.svelte';
  import { confirmDialog } from '../../lib/confirm.svelte';
  import Status from '../Status.svelte';
  import { t } from '../../lib/i18n.svelte';

  let { detail, isAdmin, reload }: { detail: DatabaseDetail; isAdmin: boolean; reload: () => Promise<void> } = $props();
  const db = $derived(detail.database);
  let info = $state<HAInfo | null>(null);
  let busy = $state(false);

  async function load() {
    try {
      info = await api.ha(db.id);
    } catch {
      /* ignore */
    }
  }
  onMount(() => {
    load();
    return poll(load, 10000);
  });

  // "streaming 0.0123" per standby
  const lag = $derived.by(() => {
    const first = (info?.replication ?? '').split(',').filter(Boolean)[0];
    if (!first) return null;
    const [state, secs] = first.split(' ');
    return { state, secs: Number(secs) };
  });

  async function toggle() {
    const on = !info?.enabled;
    if (!on && !(await confirmDialog({ title: t('Turn off high availability'), message: t('The standby is deleted. If the primary node fails, the database is down until the node is back.'), confirmLabel: t('Turn off'), danger: true }))) return;
    if (on && !(await confirmDialog({ title: t('Enable high availability'), message: t('A standby copy is created on another node (it uses the same memory as the primary). Linked services restart once to use the failover address.'), confirmLabel: t('Enable') }))) return;
    busy = true;
    try {
      await api.setHA(db.id, on);
      toast.success(on ? t('Creating the standby…') : t('High availability turned off'));
      await reload();
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      busy = false;
    }
  }

  async function switchover() {
    if (!(await confirmDialog({ title: t('Switch over'), message: t('The standby becomes the primary (a few seconds of downtime) and a new standby is created. Use it before maintenance on the primary node.'), confirmLabel: t('Switch over') }))) return;
    busy = true;
    try {
      await api.switchover(db.id);
      toast.success(t('Switched over'));
      await reload();
      await load();
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
      <h3>{t('High availability')}</h3>
      <p class="hint">{t("A hot standby on another node takes over automatically if the primary's node fails.")}</p>
    </div>
    {#if info?.capable && isAdmin}
      <div class="row">
        {#if info.enabled && info.standby && lag?.state === 'streaming'}<button class="sm" onclick={switchover} disabled={busy}>{t('Switch over')}</button>{/if}
        <button class="sm {info.enabled ? 'danger' : 'primary'}" onclick={toggle} disabled={busy || db.status !== 'ready'}>{info.enabled ? t('Turn off') : t('Enable')}</button>
      </div>
    {/if}
  </div>
  <div class="card-body stack">
    {#if !info}
      <p class="muted small">{t('Loading…')}</p>
    {:else if !info.capable}
      <p class="muted small">{t('Available for dedicated PostgreSQL databases on a cluster with at least two worker nodes.')}</p>
    {:else if !info.enabled}
      <p class="muted small">{t('Off. The database runs on a single node.')}</p>
    {:else}
      <dl class="kv">
        {#if info.primary}<dt>{t('Primary')}</dt><dd><Status status={info.primary.status} /> <span class="small">{t('on')} <b>{info.primary.node}</b></span></dd>{/if}
        <dt>{t('Standby')}</dt>
        <dd>
          {#if info.standby}
            <Status status={info.standby.status} /> <span class="small">{t('on')} <b>{info.standby.node ?? '…'}</b></span>
          {:else}
            <span class="muted small">{t('being created (needs a second online node)…')}</span>
          {/if}
        </dd>
        <dt>{t('Replication')}</dt>
        <dd>{#if lag}{lag.state}{lag.state === 'streaming' ? ` · ${t('lag {secs}', { secs: lag.secs < 1 ? '< 1 s' : lag.secs.toFixed(1) + ' s' })}` : ''}{:else}<span class="muted small">{t('not connected yet')}</span>{/if}</dd>
        {#if info.alias}<dt>{t('Address')}</dt><dd><code class="small">{info.alias}</code></dd>{/if}
      </dl>
      <p class="hint">{t("Failover happens about 30 seconds after the primary's node stops responding. Transactions committed in the last moment before a crash can be lost (asynchronous replication).")}</p>
    {/if}
  </div>
</div>
