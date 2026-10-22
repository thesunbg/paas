<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, setUnauthorizedHandler, type SetupStatus } from './lib/api';
  import { session, loadMe } from './lib/session.svelte';
  import { route } from './lib/router.svelte';
  import Layout from './components/Layout.svelte';
  import Toasts from './components/Toasts.svelte';
  import ConfirmHost from './components/ConfirmHost.svelte';
  import Login from './pages/Login.svelte';
  import Projects from './pages/Projects.svelte';
  import ProjectPage from './pages/Project.svelte';
  import NewService from './pages/NewService.svelte';
  import Studio from './pages/Studio.svelte';
  import ServicePage from './pages/Service.svelte';
  import DatabasePage from './pages/Database.svelte';
  import Nodes from './pages/Nodes.svelte';
  import Batches from './pages/Batches.svelte';
  import Settings from './pages/Settings.svelte';
  import NotFound from './pages/NotFound.svelte';
  import DeploymentRedirect from './pages/DeploymentRedirect.svelte';

  let setup = $state<SetupStatus | null>(null);
  let phase = $state<'loading' | 'auth' | 'app' | 'error'>('loading');
  let bootError = $state('');

  async function boot() {
    phase = 'loading';
    bootError = '';
    try {
      setup = await api.setup();
      if (setup.needs_setup) {
        phase = 'auth';
        return;
      }
      try {
        await loadMe();
        phase = 'app';
      } catch (e) {
        if ((e as { status?: number }).status === 401) phase = 'auth';
        else throw e;
      }
    } catch (e) {
      bootError = errorMessage(e);
      phase = 'error';
    }
  }

  onMount(() => {
    setUnauthorizedHandler(() => {
      session.me = null;
      phase = 'auth';
      api.setup().then((s) => (setup = s)).catch(() => {});
    });
    boot();
  });

  async function onAuthed() {
    await loadMe();
    phase = 'app';
    if (!location.hash || location.hash === '#/' || location.hash === '#') location.hash = '#/';
  }

  const p = $derived(route.parts);
  const section = $derived(p[0] ?? 'projects');

  $effect(() => {
    // Keep the document title in sync with the section.
    const names: Record<string, string> = { projects: 'Projects', nodes: 'Nodes', batches: 'Batches', settings: 'Settings', services: 'Service', databases: 'Database' };
    document.title = phase === 'app' ? `${names[section] ?? 'Dashboard'} · PaaS` : 'Sign in · PaaS';
  });
</script>

{#if phase === 'loading'}
  <div class="boot"><span class="spinner"></span></div>
{:else if phase === 'error'}
  <div class="boot">
    <div class="card card-pad stack" style="max-width:420px">
      <h2>Cannot reach the control plane</h2>
      <p class="muted">{bootError}</p>
      <button class="primary" onclick={boot}>Retry</button>
    </div>
  </div>
{:else if phase === 'auth' && setup}
  <Login {setup} onauthed={onAuthed} />
{:else if phase === 'app' && session.me}
  <Layout {section} onlogout={() => (phase = 'auth')}>
    {#key p.slice(0, 2).join('/')}
      {#if p.length === 0 || p[0] === 'projects' && p.length === 1}
        <Projects />
      {:else if p[0] === 'projects' && p[2] === 'studio'}
        <Studio projectId={p[1]} />
      {:else if p[0] === 'projects' && p[2] === 'new-service'}
        <NewService projectId={p[1]} />
      {:else if p[0] === 'projects' && p[1]}
        <ProjectPage id={p[1]} />
      {:else if p[0] === 'services' && p[1]}
        <ServicePage id={p[1]} tab={p[2] ?? 'overview'} sub={p[3]} />
      {:else if p[0] === 'deployments' && p[1]}
        <DeploymentRedirect id={p[1]} />
      {:else if p[0] === 'databases' && p[1]}
        <DatabasePage id={p[1]} />
      {:else if p[0] === 'nodes'}
        <Nodes />
      {:else if p[0] === 'batches'}
        <Batches id={p[1]} />
      {:else if p[0] === 'settings'}
        <Settings tab={p[1] ?? 'tokens'} />
      {:else}
        <NotFound />
      {/if}
    {/key}
  </Layout>
{/if}

<Toasts />
<ConfirmHost />

<style>
  .boot {
    min-height: 100vh;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 16px;
  }
</style>
