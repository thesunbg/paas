<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage } from '../../lib/api';
  import { session, loadMe } from '../../lib/session.svelte';
  import { toast } from '../../lib/toast.svelte';
  import { confirmDialog } from '../../lib/confirm.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import { t } from '../../lib/i18n.svelte';

  let available = $state(false);
  let error = $state('');
  const login = $derived(session.me?.user.github_login ?? '');

  onMount(async () => {
    const params = new URLSearchParams(location.hash.split('?')[1] ?? '');
    if (params.get('github_error')) error = params.get('github_error')!;
    if (params.get('github') === 'linked') {
      toast.success(t('GitHub account connected'));
      await loadMe();
    }
    if (params.size) history.replaceState(null, '', '#/settings/security');
    try {
      available = !!(await api.setup()).github_login;
    } catch {
      /* ignore */
    }
  });

  async function unlink() {
    if (!(await confirmDialog({ title: t('Disconnect GitHub'), message: t('Stop signing in as {login} on GitHub? Make sure you know your password first.', { login }), confirmLabel: t('Disconnect'), danger: true }))) return;
    try {
      await api.unlinkMyGitHub();
      await loadMe();
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }
</script>

{#if available || login}
  <div class="card">
    <div class="card-head"><h3>{t('Sign in with GitHub')}</h3></div>
    <div class="card-body stack">
      <ErrorMsg {error} />
      {#if login}
        <p>
          {t('Connected to')} <a href={`https://github.com/${login}`} target="_blank" rel="noreferrer"><b>{login}</b></a>.
          {t('You can use “Continue with GitHub” on the sign-in page.')}
        </p>
        <div><button class="sm danger" onclick={unlink}>{t('Disconnect')}</button></div>
      {:else}
        <p class="muted">{t('Connect your GitHub account to sign in without a password.')}</p>
        <div><a class="btn primary" href="/api/v1/me/github/link">{t('Connect GitHub')}</a></div>
      {/if}
    </div>
  </div>
{/if}
