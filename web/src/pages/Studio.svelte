<script lang="ts">
  // Opens the project's DB studio (dbweb) in this tab: asks the control plane
  // for it, waits while it builds or wakes up, then follows the one-time URL.
  import { onMount } from 'svelte';
  import { api, errorMessage, type StudioState } from '../lib/api';
  import { href } from '../lib/router.svelte';
  import Icon from '../components/Icon.svelte';

  let { projectId }: { projectId: string } = $props();
  const db = new URLSearchParams(location.hash.split('?')[1] ?? '').get('db') ?? undefined;

  let st = $state<StudioState | null>(null);
  let error = $state('');
  let stopped = false;

  async function tick() {
    if (stopped) return;
    try {
      st = await api.openStudio(projectId, db);
      error = '';
      if (st.status === 'ready' && st.url) {
        stopped = true;
        location.replace(st.url);
        return;
      }
      if (st.status === 'failed') {
        stopped = true;
        return;
      }
    } catch (e) {
      error = errorMessage(e);
    }
    setTimeout(tick, 3000);
  }

  onMount(() => {
    tick();
    return () => (stopped = true);
  });

  const steps: Record<string, string> = {
    building: 'Building the DB studio (the first time takes a few minutes)…',
    starting: 'Starting the DB studio…',
    ready: 'Opening…',
  };
</script>

<div class="page narrow">
  <div class="card studio">
    <div class="logo"><Icon name="database" size={22} /></div>
    <h1>DB studio</h1>
    {#if error}
      <p class="error">{error}</p>
      <p class="hint">Retrying…</p>
    {:else if st?.status === 'failed'}
      <p class="error">The DB studio could not be built: {st.deployment?.error || 'see the deployment log'}.</p>
      {#if st.deployment}
        <a class="btn" href={href(`/services/${st.service_id}/deployments/${st.deployment.id}`)}>Open the deployment log</a>
      {/if}
    {:else}
      <p><span class="spinner"></span> {steps[st?.status ?? 'starting'] ?? steps.starting}</p>
      {#if st?.status === 'building' && st.deployment}
        <a class="small" href={href(`/services/${st.service_id}/deployments/${st.deployment.id}`)}>Follow the build log</a>
      {/if}
      <p class="hint">dbweb runs next to this project's databases and is only reachable through this dashboard. It goes to sleep after 20 minutes without use.</p>
    {/if}
  </div>
</div>

<style>
  .narrow {
    max-width: 560px;
    margin: 0 auto;
  }
  .studio {
    margin-top: 12vh;
    padding: 28px;
    display: flex;
    flex-direction: column;
    align-items: center;
    text-align: center;
    gap: 10px;
  }
  .logo {
    width: 44px;
    height: 44px;
    border-radius: 10px;
    border: 1px solid var(--border-strong);
    display: flex;
    align-items: center;
    justify-content: center;
    color: var(--accent);
  }
  h1 {
    margin: 0;
    font-size: 20px;
  }
  .error {
    color: var(--red);
  }
</style>
