<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type Deployment, type ServiceDetail } from '../../lib/api';
  import { href, navigate } from '../../lib/router.svelte';
  import { relTime, absTime, between, shortSha } from '../../lib/format';
  import { poll } from '../../lib/poll';
  import { toast } from '../../lib/toast.svelte';
  import { confirmDialog } from '../../lib/confirm.svelte';
  import { t } from '../../lib/i18n.svelte';
  import DeployBadge from '../../components/DeployBadge.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import Loading from '../../components/Loading.svelte';
  import Icon from '../../components/Icon.svelte';

  let { detail, reload, canEdit }: { detail: ServiceDetail; reload: () => Promise<void>; canEdit: boolean } = $props();

  let deps = $state<Deployment[] | null>(null);
  let error = $state('');
  let busy = $state('');
  const sid = $derived(detail.service.id);

  async function load() {
    try {
      deps = (await api.deployments(sid)) ?? [];
      error = '';
    } catch (e) {
      error = errorMessage(e);
    }
  }
  onMount(() => {
    load();
    return poll(load, 5000);
  });

  const running = (d: Deployment) => ['queued', 'building', 'deploying'].includes(d.status);
  const canRollback = (d: Deployment) =>
    canEdit && d.id !== detail.service.current_deployment_id && (d.status === 'active' || d.status === 'superseded') && (!!d.image || !!d.static_path);

  async function rollback(d: Deployment) {
    const ok = await confirmDialog({
      title: t('Roll back'),
      message: t('Redeploy the artifact from {ref} ({time})? No rebuild is needed.', { ref: shortSha(d.commit_sha) || d.id.slice(0, 8), time: relTime(d.created_at) }),
      confirmLabel: t('Roll back'),
    });
    if (!ok) return;
    busy = d.id;
    try {
      const nd = await api.rollback(d.id);
      toast.success(t('Rollback started'));
      await reload();
      navigate(`/services/${sid}/deployments/${nd.id}`);
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      busy = '';
    }
  }

  async function cancel(d: Deployment) {
    busy = d.id;
    try {
      await api.cancelDeployment(d.id);
      toast.success(t('Deployment cancelled'));
      await load();
      reload();
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      busy = '';
    }
  }

  async function deploy() {
    busy = 'new';
    try {
      const d = await api.deploy(sid);
      toast.success(t('Deployment queued'));
      reload();
      navigate(`/services/${sid}/deployments/${d.id}`);
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      busy = '';
    }
  }
</script>

<div class="stack">
  <div class="between">
    <p class="muted small">{t('The latest 50 deployments. Click one to view its build and deploy log.')}</p>
    {#if canEdit}<button class="primary" onclick={deploy} disabled={!!busy}>{#if busy === 'new'}<span class="spinner"></span>{:else}<Icon name="rocket" size={14} />{/if}{t('Deploy now')}</button>{/if}
  </div>
  <ErrorMsg {error} onretry={load} />
  {#if deps === null && !error}
    <Loading rows={3} />
  {:else if deps && deps.length === 0}
    <div class="card empty">{t('No deployments yet.')}</div>
  {:else if deps}
    <div class="card table-wrap">
      <table>
        <thead><tr><th>{t('Status')}</th><th>{t('Commit')}</th><th class="hide-sm">{t('Trigger')}</th><th>{t('Created')}</th><th class="hide-sm">{t('Duration')}</th><th class="right">{t('Actions')}</th></tr></thead>
        <tbody>
          {#each deps as d (d.id)}
            <tr class="clickable" onclick={() => navigate(`/services/${sid}/deployments/${d.id}`)}>
              <td class="nowrap">
                <DeployBadge status={d.status} />
                {#if d.id === detail.service.current_deployment_id}<span class="badge outline" style="margin-left:4px">{t('current')}</span>{/if}
              </td>
              <td>
                <div class="commit">
                <a href={href(`/services/${sid}/deployments/${d.id}`)} onclick={(e) => e.stopPropagation()}>
                  {#if d.commit_sha}<code>{shortSha(d.commit_sha)}</code>{:else}<code>{d.id.slice(0, 8)}</code>{/if}
                </a>
                <span class="msg truncate">{d.commit_message || (d.image && !d.commit_sha ? d.image : '')}</span>
                {#if d.error}<span class="err small truncate">{d.error}</span>{/if}
                </div>
              </td>
              <td class="hide-sm"><span class="badge">{d.trigger}</span></td>
              <td class="nowrap" title={absTime(d.created_at)}>{relTime(d.created_at)}</td>
              <td class="hide-sm nowrap mono small">{between(d.started_at, d.finished_at, running(d))}</td>
              <td class="right nowrap">
                {#if canEdit && d.status === 'queued'}
                  <button class="sm" disabled={busy === d.id} onclick={(e) => (e.stopPropagation(), cancel(d))}>{t('Cancel')}</button>
                {/if}
                {#if canRollback(d)}
                  <button class="sm" disabled={busy === d.id} onclick={(e) => (e.stopPropagation(), rollback(d))}><Icon name="undo" size={12} />{t('Rollback')}</button>
                {/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</div>

<style>
  .commit {
    max-width: 420px;
    min-width: 160px;
  }
  .commit {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .commit a {
    align-self: flex-start;
  }
  .msg {
    color: var(--text-dim);
    font-size: 12.5px;
    max-width: 380px;
  }
  .err {
    color: var(--red);
    max-width: 380px;
  }
</style>
