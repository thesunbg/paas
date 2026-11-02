<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type OIDCIdentity } from '../../lib/api';
  import { toast } from '../../lib/toast.svelte';
  import { confirmDialog } from '../../lib/confirm.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';

  // Single sign-on identities of the signed-in account: link one explicitly
  // (the safe way to connect an existing account) or disconnect it.
  let providers = $state<{ slug: string; name: string }[]>([]);
  let linked = $state<OIDCIdentity[]>([]);
  let error = $state('');

  async function load() {
    try {
      const [setup, ids] = await Promise.all([api.setup(), api.myOIDC()]);
      providers = setup.sso ?? [];
      linked = ids ?? [];
    } catch (e) {
      error = errorMessage(e);
    }
  }

  onMount(async () => {
    const params = new URLSearchParams(location.hash.split('?')[1] ?? '');
    if (params.get('sso_error')) error = params.get('sso_error')!;
    if (params.get('sso') === 'linked') toast.success('Single sign-on account linked');
    if (params.has('sso_error') || params.has('sso')) history.replaceState(null, '', '#/settings/security');
    await load();
  });

  const identity = (slug: string) => linked.find((l) => l.provider_slug === slug);

  async function unlink(p: { slug: string; name: string }) {
    if (!(await confirmDialog({ title: `Disconnect ${p.name}`, message: `Stop signing in with ${p.name}? Make sure you know your password first.`, confirmLabel: 'Disconnect', danger: true }))) return;
    try {
      await api.unlinkMyOIDC(p.slug);
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }
</script>

{#if providers.length || linked.length}
  <div class="card">
    <div class="card-head"><h3>Single sign-on</h3></div>
    <div class="card-body stack">
      <ErrorMsg {error} />
      {#each providers as p (p.slug)}
        {@const id = identity(p.slug)}
        <div class="row between">
          <div>
            <b>{p.name}</b>
            {#if id}<span class="muted small"> · linked{id.email ? ` as ${id.email}` : ''}</span>{:else}<span class="muted small"> · not linked</span>{/if}
          </div>
          {#if id}
            <button class="sm danger" onclick={() => unlink(p)}>Disconnect</button>
          {:else}
            <a class="btn sm" href={`/api/v1/me/oidc/${encodeURIComponent(p.slug)}/link`}>Link</a>
          {/if}
        </div>
      {/each}
    </div>
  </div>
{/if}
