<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type GitHubAppStatus, type TeamGitHub } from '../../lib/api';
  import { currentTeam, can } from '../../lib/session.svelte';
  import { toast } from '../../lib/toast.svelte';
  import { confirmDialog } from '../../lib/confirm.svelte';
  import { relTime } from '../../lib/format';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import Loading from '../../components/Loading.svelte';
  import Icon from '../../components/Icon.svelte';
  import { t } from '../../lib/i18n.svelte';

  let app = $state<GitHubAppStatus | null>(null);
  let teamGH = $state<TeamGitHub | null>(null);
  let error = $state('');
  let org = $state('');
  let busy = $state(false);
  const team = $derived(currentTeam());

  // Messages passed back by the server redirect (#/settings/github?github=…).
  const params = new URLSearchParams(location.hash.split('?')[1] ?? '');
  const flash = params.get('github');
  const flashError = params.get('github_error');

  async function load() {
    try {
      app = await api.githubApp();
      if (team) teamGH = await api.teamGitHub(team.id, '/#/settings/github');
      error = '';
    } catch (e) {
      error = errorMessage(e);
    }
  }
  onMount(() => {
    load();
    if (flash === 'created') toast.success(t('GitHub App created. Now connect your GitHub account.'));
    if (flash === 'connected') toast.success(t('GitHub account connected'));
    if (flash || flashError) history.replaceState(null, '', '#/settings/github');
  });

  // Creating the app is a form POST to GitHub carrying the manifest.
  async function createApp() {
    busy = true;
    try {
      const m = await api.githubManifest(org.trim() || undefined);
      const form = document.createElement('form');
      form.method = 'post';
      form.action = m.action_url;
      const input = document.createElement('input');
      input.type = 'hidden';
      input.name = 'manifest';
      input.value = m.manifest;
      form.appendChild(input);
      document.body.appendChild(form);
      form.submit();
    } catch (e) {
      error = errorMessage(e);
      busy = false;
    }
  }

  async function removeApp() {
    if (!(await confirmDialog({
          title: t('Disconnect GitHub App'),
          message: t('Services using it can no longer clone or receive pushes until a new app is created and the GitHub accounts are connected to it again (their links to this app are removed). Delete the app on GitHub too.'),
          confirmLabel: t('Disconnect'),
          danger: true,
        }))) return;
    try {
      await api.deleteGitHubApp();
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }

  async function unlink(id: number, login: string) {
    if (!(await confirmDialog({ title: t('Unlink account'), message: t('Unlink {login} from this team? Uninstall the app on GitHub to revoke access completely.', { login }), confirmLabel: t('Unlink'), danger: true }))) return;
    try {
      await api.unlinkGitHub(team!.id, id);
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }

  // GitHub account types, as reported by the API.
  function accountType(v: string): string {
    return ({ User: t('User'), Organization: t('Organization') } as Record<string, string>)[v] ?? v;
  }
</script>

<div class="card">
  <div class="card-head">
    <h3><Icon name="git" size={16} /> GitHub</h3>
  </div>
  <div class="card-body stack">
    {#if flashError}<ErrorMsg error={flashError} />{/if}
    <ErrorMsg {error} />
    {#if !app}
      <Loading />
    {:else if !app.configured}
      <p>
        {t('Connect GitHub like Vercel: create a')} <b>GitHub App</b>
        {t('for this server in one click, then install it on your account or organization and choose which repositories it may access. Builds, push deploys, PR previews and commit statuses then work without personal tokens or manual webhooks.')}
      </p>
      {#if app.can_configure}
        <p class="hint">
          {t('The app will call back to')} <code>{app.base_url}</code> {t('— GitHub must be able to reach this address for webhooks.')}
        </p>
        <div class="row">
          <label class="field" style="max-width:280px">
            <span>{t('Organization')} <span class="faint">{t('(optional)')}</span></span>
            <input class="mono" bind:value={org} placeholder={t('leave empty for your personal account')} spellcheck="false" />
          </label>
        </div>
        <div><button class="primary" onclick={createApp} disabled={busy}>{#if busy}<span class="spinner"></span>{:else}<Icon name="git" size={14} />{/if}{t('Create GitHub App')}</button></div>
      {:else}
        <p class="muted">{t('Ask an administrator to set up the GitHub App.')}</p>
      {/if}
    {:else}
      <p>
        GitHub App <a href={app.html_url} target="_blank" rel="noreferrer"><b>{app.name}</b></a>
        {#if app.owner}<span class="faint">{t('owned by {owner}', { owner: app.owner })}</span>{/if}
      </p>
      <p class="hint">
        {t('Other people can only sign in with GitHub or connect their accounts when the app is')} <b>{t('public')}</b>{t('; a private app shows them a 404 page.')}
        {t('Check it under')} <a href={`${app.html_url?.replace('/apps/', '/settings/apps/')}/advanced`} target="_blank" rel="noreferrer">{t('app settings')} → Advanced → Make public</a>.
      </p>
      {#if teamGH}
        <div class="section-title">{t('Connected accounts for {team}', { team: team?.name })}</div>
        {#if teamGH.installations.length === 0}
          <p class="muted">{t('No GitHub account connected yet.')}</p>
        {:else}
          <div class="table-wrap">
            <table>
              <thead><tr><th>{t('Account')}</th><th>{t('Type')}</th><th>{t('Connected')}</th><th class="right"></th></tr></thead>
              <tbody>
                {#each teamGH.installations as i (i.installation_id)}
                  <tr>
                    <td><a href={`https://github.com/${i.account_login}`} target="_blank" rel="noreferrer">{i.account_login}</a></td>
                    <td class="small">{accountType(i.account_type)}</td>
                    <td class="small">{relTime(i.created_at)}</td>
                    <td class="right">
                      <a class="btn sm" href={`https://github.com/settings/installations/${i.installation_id}`} target="_blank" rel="noreferrer">{t('Repositories')}</a>
                      {#if can('admin')}<button class="sm danger" onclick={() => unlink(i.installation_id, i.account_login)}>{t('Unlink')}</button>{/if}
                    </td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        {/if}
        {#if teamGH.install_url}
          <div><a class="btn primary" href={teamGH.install_url}><Icon name="git" size={14} />{teamGH.installations.length ? t('Connect another account') : t('Connect GitHub account')}</a></div>
          <p class="hint">{t('GitHub asks which repositories to grant. You can change the selection later from “Repositories”.')}</p>
        {/if}
      {/if}
      {#if app.can_configure}
        <hr style="margin:0" />
        <div><button class="sm danger" onclick={removeApp}>{t('Disconnect app')}</button></div>
      {/if}
    {/if}
  </div>
</div>
