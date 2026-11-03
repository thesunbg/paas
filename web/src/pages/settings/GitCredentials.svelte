<script lang="ts">
  import { api, errorMessage, type GitCredential, type GitProvider } from '../../lib/api';
  import { session, currentTeam, can } from '../../lib/session.svelte';
  import { relTime } from '../../lib/format';
  import { toast } from '../../lib/toast.svelte';
  import { confirmDialog } from '../../lib/confirm.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import Loading from '../../components/Loading.svelte';
  import Icon from '../../components/Icon.svelte';
  import { t } from '../../lib/i18n.svelte';

  let creds = $state<GitCredential[] | null>(null);
  let error = $state('');
  let busy = $state('');

  let provider = $state<GitProvider>('gitlab');
  let host = $state('');
  let username = $state('');
  let token = $state('');
  let name = $state('');
  let formError = $state('');

  const team = $derived(currentTeam());
  const isAdmin = $derived(team ? can('admin', team.id) : false);

  async function load() {
    const tm = team;
    if (!tm) return;
    try {
      creds = (await api.gitCredentials(tm.id)).credentials ?? [];
      error = '';
    } catch (e) {
      error = errorMessage(e);
    }
  }
  $effect(() => {
    void session.teamId;
    creds = null;
    load();
  });

  const providers: { id: GitProvider; label: string; host: string; hostRequired: boolean; tokenHint: string; userHint: string }[] = $derived([
    {
      id: 'gitlab',
      label: 'GitLab',
      host: 'gitlab.com',
      hostRequired: false,
      tokenHint: t('A personal, group or project access token with read_repository (add api to report commit statuses).'),
      userHint: t('Leave empty.'),
    },
    {
      id: 'gitea',
      label: 'Gitea / Forgejo',
      host: t('git.example.com or codeberg.org'),
      hostRequired: true,
      tokenHint: t('An access token with read access to repositories (write to report commit statuses).'),
      userHint: t('Leave empty.'),
    },
    {
      id: 'bitbucket',
      label: 'Bitbucket Cloud',
      host: 'bitbucket.org',
      hostRequired: false,
      tokenHint: t('A repository / workspace access token, or an app password with Repositories: Read (and Write for build statuses).'),
      userHint: t('Your Bitbucket username for an app password; empty for an access token.'),
    },
    {
      id: 'github',
      label: t('GitHub (token)'),
      host: 'github.com',
      hostRequired: false,
      tokenHint: t('A fine-grained personal access token with Contents: read. The GitHub App (GitHub tab) is preferred.'),
      userHint: t('Leave empty.'),
    },
  ]);
  const info = $derived(providers.find((p) => p.id === provider)!);
  const label = (p: GitProvider) => providers.find((x) => x.id === p)?.label ?? p;

  async function save(e: SubmitEvent) {
    e.preventDefault();
    if (!team) return;
    formError = '';
    busy = 'add';
    try {
      const c = await api.setGitCredential(team.id, { provider, host: host.trim() || undefined, username: username.trim() || undefined, token: token.trim(), name: name.trim() || undefined });
      toast.success(t('Credential saved for {host}', { host: c.host }));
      host = username = token = name = '';
      await load();
    } catch (err) {
      formError = errorMessage(err);
    } finally {
      busy = '';
    }
  }
  async function remove(c: GitCredential) {
    if (!team) return;
    if (!(await confirmDialog({
          title: t('Remove credential'),
          message: t('Remove the {provider} credential for {host}? Private repositories on this host stop deploying.', { provider: label(c.provider), host: c.host }),
          confirmLabel: t('Remove'),
          danger: true,
        }))) return;
    try {
      await api.deleteGitCredential(team.id, c.id);
      toast.success(t('Credential removed'));
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }
</script>

<div class="card">
  <div class="card-head">
    <div>
      <h3>{t('Git hosts')} · {team?.name}</h3>
      <p class="hint">{t('Deploy private repositories from GitLab, Gitea / Forgejo and Bitbucket. One token per host; it is stored encrypted and only ever sent to that host.')}</p>
    </div>
  </div>
  {#if error}<div class="card-body"><ErrorMsg {error} onretry={load} /></div>{/if}
  {#if creds === null && !error}
    <div class="card-body"><Loading /></div>
  {:else if creds}
    {#if creds.length === 0}
      <div class="card-body muted small">{t('No credentials yet: only public repositories (and GitHub through the app) can be deployed.')}</div>
    {:else}
      <div class="table-wrap">
        <table>
          <thead><tr><th>{t('Host')}</th><th>{t('Provider')}</th><th class="hide-sm">{t('Name')}</th><th class="hide-sm">{t('Updated')}</th><th class="right"></th></tr></thead>
          <tbody>
            {#each creds as c (c.id)}
              <tr>
                <td class="mono"><Icon name="git" size={13} /> {c.host}</td>
                <td><span class="badge">{label(c.provider)}</span>{#if c.username}<span class="faint small"> {t('as {username}', { username: c.username })}</span>{/if}</td>
                <td class="hide-sm">{c.name}</td>
                <td class="hide-sm faint small">{relTime(c.updated_at)}</td>
                <td class="right nowrap">
                  {#if isAdmin}<button class="sm danger" onclick={() => remove(c)} aria-label={t('Remove {host}', { host: c.host })}><Icon name="trash" size={12} /></button>{/if}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
    {#if isAdmin}
      <form class="card-body stack add" onsubmit={save}>
        <h3>{t('Add or replace a credential')}</h3>
        <div class="grid-3">
          <label class="field">
            <span>{t('Provider')}</span>
            <select bind:value={provider}>{#each providers as p (p.id)}<option value={p.id}>{p.label}</option>{/each}</select>
          </label>
          <label class="field">
            <span>{info.hostRequired ? t('Host') : t('Host (optional)')}</span>
            <input class="mono" bind:value={host} placeholder={info.host} required={info.hostRequired} autocomplete="off" spellcheck="false" />
          </label>
          <label class="field">
            <span>{t('Name (optional)')}</span>
            <input bind:value={name} maxlength="100" placeholder={t('e.g. work gitlab')} />
          </label>
        </div>
        <div class="grid-3">
          <label class="field">
            <span>{t('Username (optional)')}</span>
            <input class="mono" bind:value={username} autocomplete="off" spellcheck="false" />
            <span class="hint">{info.userHint}</span>
          </label>
          <label class="field span-2">
            <span>{t('Token')}</span>
            <input class="mono" type="password" bind:value={token} required autocomplete="off" spellcheck="false" />
            <span class="hint">{info.tokenHint} {t('Saving for a host that already has one replaces it.')}</span>
          </label>
        </div>
        <ErrorMsg error={formError} />
        <div><button class="primary" type="submit" disabled={busy === 'add' || !token.trim()}>{#if busy === 'add'}<span class="spinner"></span>{/if}{t('Save credential')}</button></div>
      </form>
    {:else}
      <div class="card-body hint">{t('Only team admins can add credentials.')}</div>
    {/if}
  {/if}
</div>

<style>
  .add {
    border-top: 1px solid var(--border);
  }
  .span-2 {
    grid-column: span 2;
  }
</style>
