<script lang="ts">
  import { api, errorMessage, type APIToken, type Member, type Role } from '../lib/api';
  import { session, currentTeam, can } from '../lib/session.svelte';
  import { href } from '../lib/router.svelte';
  import { relTime, absTime } from '../lib/format';
  import { toast } from '../lib/toast.svelte';
  import { confirmDialog } from '../lib/confirm.svelte';
  import ErrorMsg from '../components/ErrorMsg.svelte';
  import Loading from '../components/Loading.svelte';
  import Icon from '../components/Icon.svelte';
  import CopyField from '../components/CopyField.svelte';
  import Notifications from './settings/Notifications.svelte';
  import TwoFactor from './settings/TwoFactor.svelte';
  import GitHub from './settings/GitHub.svelte';
  import GitCredentials from './settings/GitCredentials.svelte';
  import System from './settings/System.svelte';
  import SetupChecklist from './settings/SetupChecklist.svelte';
  import Users from './settings/Users.svelte';
  import GitHubSignIn from './settings/GitHubSignIn.svelte';
  import SSOSignIn from './settings/SSOSignIn.svelte';
  import SSOProviders from './settings/SSOProviders.svelte';
  import Quota from './settings/Quota.svelte';
  import Backups from './settings/Backups.svelte';
  import AI from './settings/AI.svelte';
  import { i18n, locales, setLocale, t, type Locale } from '../lib/i18n.svelte';

  let { tab = 'tokens' }: { tab?: string } = $props();

  const tabs = $derived([
    { id: 'tokens', label: t('API tokens') },
    { id: 'password', label: t('Password') },
    { id: 'security', label: t('Security') },
    { id: 'members', label: t('Team members') },
    { id: 'quota', label: t('Team usage') },
    { id: 'notifications', label: t('Notifications') },
    { id: 'github', label: 'GitHub' },
    { id: 'git', label: t('Git hosts') },
    ...(session.me?.user.is_admin
      ? [
          { id: 'users', label: t('Users') },
          { id: 'sso', label: t('Single sign-on') },
          { id: 'backups', label: t('Backups') },
          { id: 'ai', label: 'AI' },
          { id: 'system', label: t('System') },
        ]
      : []),
  ]);

  // Tokens
  let tokens = $state<APIToken[] | null>(null);
  let tokensError = $state('');
  let tokenName = $state('');
  let newToken = $state('');
  let tokenBusy = $state(false);

  async function loadTokens() {
    try {
      tokens = (await api.tokens()) ?? [];
      tokensError = '';
    } catch (e) {
      tokensError = errorMessage(e);
    }
  }
  async function createToken(e: SubmitEvent) {
    e.preventDefault();
    if (!tokenName.trim()) return;
    tokenBusy = true;
    try {
      const r = await api.createToken(tokenName.trim());
      newToken = r.token;
      tokenName = '';
      toast.success(t('Token created'));
      await loadTokens();
    } catch (err) {
      tokensError = errorMessage(err);
    } finally {
      tokenBusy = false;
    }
  }
  async function deleteToken(tok: APIToken) {
    if (!(await confirmDialog({ title: t('Revoke token'), message: t('Revoke “{name}”? Anything using it stops working immediately.', { name: tok.name }), confirmLabel: t('Revoke'), danger: true }))) return;
    try {
      await api.deleteToken(tok.id);
      toast.success(t('Token revoked'));
      await loadTokens();
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }

  // Password
  let curPw = $state('');
  let newPw = $state('');
  let newPw2 = $state('');
  let pwError = $state('');
  let pwBusy = $state(false);
  async function changePw(e: SubmitEvent) {
    e.preventDefault();
    pwError = '';
    if (newPw.length < 8) return void (pwError = t('New password must be at least 8 characters'));
    if (newPw !== newPw2) return void (pwError = t('Passwords do not match'));
    pwBusy = true;
    try {
      await api.changePassword(curPw, newPw);
      toast.success(t('Password changed'));
      curPw = newPw = newPw2 = '';
    } catch (err) {
      pwError = errorMessage(err);
    } finally {
      pwBusy = false;
    }
  }

  // Members
  let members = $state<Member[] | null>(null);
  let membersError = $state('');
  let inviteEmail = $state('');
  let inviteRole = $state<Role>('developer');
  let inviteBusy = $state(false);
  let invited = $state<{ email: string; password?: string } | null>(null);
  const team = $derived(currentTeam());
  const isTeamAdmin = $derived(team ? can('admin', team.id) : false);

  async function loadMembers() {
    const tm = team;
    if (!tm) return;
    try {
      members = (await api.members(tm.id)) ?? [];
      membersError = '';
    } catch (e) {
      membersError = errorMessage(e);
    }
  }
  async function invite(e: SubmitEvent) {
    e.preventDefault();
    if (!team || !inviteEmail.trim()) return;
    inviteBusy = true;
    membersError = '';
    try {
      const r = await api.addMember(team.id, inviteEmail.trim(), inviteRole);
      invited = { email: r.email, password: r.temporary_password };
      toast.success(t('{email} added as {role}', { email: r.email, role: roleLabels[inviteRole] ?? inviteRole }));
      inviteEmail = '';
      await loadMembers();
    } catch (err) {
      membersError = errorMessage(err);
    } finally {
      inviteBusy = false;
    }
  }
  async function removeMember(m: Member) {
    if (!team) return;
    if (!(await confirmDialog({ title: t('Remove member'), message: t('Remove {email} from {team}?', { email: m.email, team: team.name }), confirmLabel: t('Remove'), danger: true }))) return;
    try {
      await api.removeMember(team.id, m.user_id);
      toast.success(t('{email} removed', { email: m.email }));
      await loadMembers();
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }

  $effect(() => {
    if (tab === 'tokens' && tokens === null) loadTokens();
  });
  $effect(() => {
    void session.teamId;
    if (tab === 'members') {
      members = null;
      invited = null;
      loadMembers();
    }
  });

  const roleBadge: Record<string, string> = { owner: 'purple', admin: 'blue', developer: 'green', viewer: '' };
  const roleLabels: Record<string, string> = $derived({ owner: t('owner'), admin: t('admin'), developer: t('developer'), viewer: t('viewer') });
</script>

<div class="page narrow">
  <div class="page-head">
    <div>
      <h1>{t('Settings')}</h1>
      <p class="sub">{session.me?.user.email}</p>
    </div>
  </div>

  <div class="layout">
    <nav class="side" aria-label={t('Settings sections')}>
      {#each tabs as tb (tb.id)}
        <a href={href(`/settings/${tb.id}`)} class:active={tab === tb.id} aria-current={tab === tb.id ? 'page' : undefined}>{tb.label}</a>
      {/each}
    </nav>

    <div class="content">
      {#if tab === 'tokens'}
        <div class="card">
          <div class="card-head">
            <div>
              <h3>{t('API tokens')}</h3>
              <p class="hint">{t('Use with')} <code>Authorization: Bearer paas_…</code> {t('for the CLI and CI.')}</p>
            </div>
          </div>
          <form class="card-body row nowrap" onsubmit={createToken}>
            <label class="sr-only" for="tokname">{t('Token name')}</label>
            <input id="tokname" bind:value={tokenName} placeholder={t('Token name, e.g. github-actions')} maxlength="64" />
            <button class="primary" type="submit" disabled={tokenBusy || !tokenName.trim()}>{#if tokenBusy}<span class="spinner"></span>{/if}{t('Create')}</button>
          </form>
          {#if newToken}
            <div class="card-body" style="padding-top:0">
              <div class="alert success stack" style="gap:8px">
                <b>{t('Copy your new token now — it will not be shown again.')}</b>
                <CopyField value={newToken} label={t('New API token')} />
                <div><button class="sm" onclick={() => (newToken = '')}>{t('Done')}</button></div>
              </div>
            </div>
          {/if}
          {#if tokensError}<div class="card-body" style="padding-top:0"><ErrorMsg error={tokensError} onretry={loadTokens} /></div>{/if}
          {#if tokens === null && !tokensError}
            <div class="card-body"><Loading /></div>
          {:else if tokens && tokens.length === 0}
            <div class="card-body muted small" style="border-top:1px solid var(--border)">{t('No tokens yet.')}</div>
          {:else if tokens}
            <div class="table-wrap" style="border-top:1px solid var(--border)">
              <table>
                <thead><tr><th>{t('Name')}</th><th>{t('Created')}</th><th>{t('Last used')}</th><th class="right"></th></tr></thead>
                <tbody>
                  {#each tokens as tok (tok.id)}
                    <tr>
                      <td><Icon name="key" size={13} /> {tok.name}</td>
                      <td class="small" title={absTime(tok.created_at)}>{relTime(tok.created_at)}</td>
                      <td class="small faint">{tok.last_used_at ? relTime(tok.last_used_at) : t('never')}</td>
                      <td class="right"><button class="sm danger" onclick={() => deleteToken(tok)}>{t('Revoke')}</button></td>
                    </tr>
                  {/each}
                </tbody>
              </table>
            </div>
          {/if}
        </div>
      {:else if tab === 'password'}
        <div class="stack-lg">
          <div class="card">
            <div class="card-head">
              <div>
                <h3>{t('Language')}</h3>
                <p class="hint">{t('The language of this dashboard. The choice is saved in this browser.')}</p>
              </div>
            </div>
            <div class="card-body">
              <label class="field" style="max-width:240px">
                <span class="sr-only">{t('Language')}</span>
                <select value={i18n.locale} onchange={(e) => setLocale(e.currentTarget.value as Locale)}>
                  {#each locales as l (l.id)}<option value={l.id}>{l.label}</option>{/each}
                </select>
              </label>
            </div>
          </div>
          <form class="card" onsubmit={changePw}>
            <div class="card-head"><h3>{t('Change password')}</h3></div>
            <div class="card-body stack" style="max-width:420px">
              <input type="email" autocomplete="username" value={session.me?.user.email} hidden readonly />
              <label class="field"><span>{t('Current password')}</span><input type="password" bind:value={curPw} required autocomplete="current-password" /></label>
              <label class="field"><span>{t('New password')}</span><input type="password" bind:value={newPw} required minlength="8" autocomplete="new-password" /><span class="hint">{t('At least 8 characters.')}</span></label>
              <label class="field"><span>{t('Confirm new password')}</span><input type="password" bind:value={newPw2} required autocomplete="new-password" /></label>
              <ErrorMsg error={pwError} />
              <div><button class="primary" type="submit" disabled={pwBusy || !curPw || !newPw}>{#if pwBusy}<span class="spinner"></span>{/if}{t('Change password')}</button></div>
            </div>
          </form>
        </div>
      {:else if tab === 'members'}
        <div class="card">
          <div class="card-head">
            <div>
              <h3>{t('{team} members', { team: team?.name ?? t('Team') })}</h3>
              <p class="hint">{t('viewer: read · developer: deploy and configure · admin: delete, restore, manage members')}</p>
            </div>
          </div>
          {#if isTeamAdmin}
            <form class="card-body invite" onsubmit={invite}>
              <label class="field" style="flex:1;min-width:200px"><span>{t('Email')}</span><input type="email" bind:value={inviteEmail} required placeholder="teammate@example.com" /></label>
              <label class="field">
                <span>{t('Role')}</span>
                <select bind:value={inviteRole}>
                  <option value="viewer">{roleLabels.viewer}</option>
                  <option value="developer">{roleLabels.developer}</option>
                  <option value="admin">{roleLabels.admin}</option>
                </select>
              </label>
              <button class="primary" type="submit" disabled={inviteBusy || !inviteEmail.trim()}>{#if inviteBusy}<span class="spinner"></span>{/if}{t('Invite')}</button>
            </form>
          {/if}
          {#if invited}
            <div class="card-body" style="padding-top:0">
              {#if invited.password}
                <div class="alert success stack" style="gap:8px">
                  <span>{t('Created an account for')} <b>{invited.email}</b>. {t('Share this temporary password securely — it is shown only once:')}</span>
                  <CopyField value={invited.password} label={t('Temporary password')} />
                  <div><button class="sm" onclick={() => (invited = null)}>{t('Done')}</button></div>
                </div>
              {:else}
                <div class="alert success">{t('{email} already had an account and was added to the team.', { email: invited.email })}</div>
              {/if}
            </div>
          {/if}
          {#if membersError}<div class="card-body" style="padding-top:0"><ErrorMsg error={membersError} /></div>{/if}
          {#if members === null && !membersError}
            <div class="card-body"><Loading /></div>
          {:else if members}
            <div class="table-wrap" style="border-top:1px solid var(--border)">
              <table>
                <thead><tr><th>{t('Name')}</th><th>{t('Email')}</th><th>{t('Role')}</th><th class="right"></th></tr></thead>
                <tbody>
                  {#each members as m (m.user_id)}
                    <tr>
                      <td>{m.name}{#if m.user_id === session.me?.user.id} <span class="faint small">{t('(you)')}</span>{/if}</td>
                      <td class="small">{m.email}</td>
                      <td><span class="badge {roleBadge[m.role] ?? ''}">{roleLabels[m.role] ?? m.role}</span></td>
                      <td class="right">
                        {#if isTeamAdmin && m.user_id !== session.me?.user.id && m.role !== 'owner'}
                          <button class="sm danger" onclick={() => removeMember(m)}>{t('Remove')}</button>
                        {/if}
                      </td>
                    </tr>
                  {/each}
                </tbody>
              </table>
            </div>
          {/if}
        </div>
      {:else if tab === 'security'}
        <div class="stack-lg">
          <TwoFactor />
          <SSOSignIn />
          <GitHubSignIn />
        </div>
      {:else if tab === 'quota'}
        <Quota />
      {:else if tab === 'notifications'}
        <Notifications />
      {:else if tab === 'github'}
        <GitHub />
      {:else if tab === 'git'}
        <GitCredentials />
      {:else if tab === 'users' && session.me?.user.is_admin}
        <Users />
      {:else if tab === 'sso' && session.me?.user.is_admin}
        <SSOProviders />
      {:else if tab === 'backups' && session.me?.user.is_admin}
        <Backups />
      {:else if tab === 'ai' && session.me?.user.is_admin}
        <AI />
      {:else if tab === 'system' && session.me?.user.is_admin}
        <div class="stack-lg">
          <SetupChecklist />
          <System />
        </div>
      {:else}
        <p class="muted">{t('Unknown section.')}</p>
      {/if}
    </div>
  </div>
</div>

<style>
  .narrow {
    max-width: 1040px;
  }
  .layout {
    display: grid;
    grid-template-columns: 180px minmax(0, 1fr);
    gap: 24px;
    align-items: start;
  }
  .side {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .side a {
    padding: 7px 10px;
    border-radius: var(--radius-sm);
    color: var(--text-dim);
    font-weight: 500;
  }
  .side a:hover {
    background: var(--bg-hover);
    color: var(--text);
    text-decoration: none;
  }
  .side a.active {
    background: var(--bg-elev-2);
    color: var(--text);
    box-shadow: inset 0 0 0 1px var(--border);
  }
  .invite {
    display: flex;
    gap: 10px;
    align-items: flex-end;
    flex-wrap: wrap;
  }
  @media (max-width: 760px) {
    .layout {
      grid-template-columns: minmax(0, 1fr);
      gap: 12px;
    }
    .side {
      flex-direction: row;
      overflow-x: auto;
    }
    .side a {
      white-space: nowrap;
    }
  }
</style>
