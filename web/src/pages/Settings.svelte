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

  let { tab = 'tokens' }: { tab?: string } = $props();

  const tabs = [
    { id: 'tokens', label: 'API tokens' },
    { id: 'password', label: 'Password' },
    { id: 'security', label: 'Two-factor auth' },
    { id: 'members', label: 'Team members' },
    { id: 'notifications', label: 'Notifications' },
  ];

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
      toast.success('Token created');
      await loadTokens();
    } catch (err) {
      tokensError = errorMessage(err);
    } finally {
      tokenBusy = false;
    }
  }
  async function deleteToken(t: APIToken) {
    if (!(await confirmDialog({ title: 'Revoke token', message: `Revoke "${t.name}"? Anything using it stops working immediately.`, confirmLabel: 'Revoke', danger: true }))) return;
    try {
      await api.deleteToken(t.id);
      toast.success('Token revoked');
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
    if (newPw.length < 8) return void (pwError = 'New password must be at least 8 characters');
    if (newPw !== newPw2) return void (pwError = 'Passwords do not match');
    pwBusy = true;
    try {
      await api.changePassword(curPw, newPw);
      toast.success('Password changed');
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
    const t = team;
    if (!t) return;
    try {
      members = (await api.members(t.id)) ?? [];
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
      invited = { email: r.user.email, password: r.temporary_password };
      toast.success(`${r.user.email} added as ${inviteRole}`);
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
    if (!(await confirmDialog({ title: 'Remove member', message: `Remove ${m.email} from ${team.name}?`, confirmLabel: 'Remove', danger: true }))) return;
    try {
      await api.removeMember(team.id, m.user_id);
      toast.success(`${m.email} removed`);
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
</script>

<div class="page narrow">
  <div class="page-head">
    <div>
      <h1>Settings</h1>
      <p class="sub">{session.me?.user.email}</p>
    </div>
  </div>

  <div class="layout">
    <nav class="side" aria-label="Settings sections">
      {#each tabs as t (t.id)}
        <a href={href(`/settings/${t.id}`)} class:active={tab === t.id} aria-current={tab === t.id ? 'page' : undefined}>{t.label}</a>
      {/each}
    </nav>

    <div class="content">
      {#if tab === 'tokens'}
        <div class="card">
          <div class="card-head">
            <div>
              <h3>API tokens</h3>
              <p class="hint">Use with <code>Authorization: Bearer paas_…</code> for the CLI and CI.</p>
            </div>
          </div>
          <form class="card-body row nowrap" onsubmit={createToken}>
            <label class="sr-only" for="tokname">Token name</label>
            <input id="tokname" bind:value={tokenName} placeholder="Token name, e.g. github-actions" maxlength="64" />
            <button class="primary" type="submit" disabled={tokenBusy || !tokenName.trim()}>{#if tokenBusy}<span class="spinner"></span>{/if}Create</button>
          </form>
          {#if newToken}
            <div class="card-body" style="padding-top:0">
              <div class="alert success stack" style="gap:8px">
                <b>Copy your new token now — it will not be shown again.</b>
                <CopyField value={newToken} label="New API token" />
                <div><button class="sm" onclick={() => (newToken = '')}>Done</button></div>
              </div>
            </div>
          {/if}
          {#if tokensError}<div class="card-body" style="padding-top:0"><ErrorMsg error={tokensError} onretry={loadTokens} /></div>{/if}
          {#if tokens === null && !tokensError}
            <div class="card-body"><Loading /></div>
          {:else if tokens && tokens.length === 0}
            <div class="card-body muted small" style="border-top:1px solid var(--border)">No tokens yet.</div>
          {:else if tokens}
            <div class="table-wrap" style="border-top:1px solid var(--border)">
              <table>
                <thead><tr><th>Name</th><th>Created</th><th>Last used</th><th class="right"></th></tr></thead>
                <tbody>
                  {#each tokens as t (t.id)}
                    <tr>
                      <td><Icon name="key" size={13} /> {t.name}</td>
                      <td class="small" title={absTime(t.created_at)}>{relTime(t.created_at)}</td>
                      <td class="small faint">{t.last_used_at ? relTime(t.last_used_at) : 'never'}</td>
                      <td class="right"><button class="sm danger" onclick={() => deleteToken(t)}>Revoke</button></td>
                    </tr>
                  {/each}
                </tbody>
              </table>
            </div>
          {/if}
        </div>
      {:else if tab === 'password'}
        <form class="card" onsubmit={changePw}>
          <div class="card-head"><h3>Change password</h3></div>
          <div class="card-body stack" style="max-width:420px">
            <input type="email" autocomplete="username" value={session.me?.user.email} hidden readonly />
            <label class="field"><span>Current password</span><input type="password" bind:value={curPw} required autocomplete="current-password" /></label>
            <label class="field"><span>New password</span><input type="password" bind:value={newPw} required minlength="8" autocomplete="new-password" /><span class="hint">At least 8 characters.</span></label>
            <label class="field"><span>Confirm new password</span><input type="password" bind:value={newPw2} required autocomplete="new-password" /></label>
            <ErrorMsg error={pwError} />
            <div><button class="primary" type="submit" disabled={pwBusy || !curPw || !newPw}>{#if pwBusy}<span class="spinner"></span>{/if}Change password</button></div>
          </div>
        </form>
      {:else if tab === 'members'}
        <div class="card">
          <div class="card-head">
            <div>
              <h3>{team?.name ?? 'Team'} members</h3>
              <p class="hint">viewer: read · developer: deploy and configure · admin: delete, restore, manage members</p>
            </div>
          </div>
          {#if isTeamAdmin}
            <form class="card-body invite" onsubmit={invite}>
              <label class="field" style="flex:1;min-width:200px"><span>Email</span><input type="email" bind:value={inviteEmail} required placeholder="teammate@example.com" /></label>
              <label class="field">
                <span>Role</span>
                <select bind:value={inviteRole}>
                  <option value="viewer">viewer</option>
                  <option value="developer">developer</option>
                  <option value="admin">admin</option>
                </select>
              </label>
              <button class="primary" type="submit" disabled={inviteBusy || !inviteEmail.trim()}>{#if inviteBusy}<span class="spinner"></span>{/if}Invite</button>
            </form>
          {/if}
          {#if invited}
            <div class="card-body" style="padding-top:0">
              {#if invited.password}
                <div class="alert success stack" style="gap:8px">
                  <span>Created an account for <b>{invited.email}</b>. Share this temporary password securely — it is shown only once:</span>
                  <CopyField value={invited.password} label="Temporary password" />
                  <div><button class="sm" onclick={() => (invited = null)}>Done</button></div>
                </div>
              {:else}
                <div class="alert success">{invited.email} already had an account and was added to the team.</div>
              {/if}
            </div>
          {/if}
          {#if membersError}<div class="card-body" style="padding-top:0"><ErrorMsg error={membersError} /></div>{/if}
          {#if members === null && !membersError}
            <div class="card-body"><Loading /></div>
          {:else if members}
            <div class="table-wrap" style="border-top:1px solid var(--border)">
              <table>
                <thead><tr><th>Name</th><th>Email</th><th>Role</th><th class="right"></th></tr></thead>
                <tbody>
                  {#each members as m (m.user_id)}
                    <tr>
                      <td>{m.name}{#if m.user_id === session.me?.user.id} <span class="faint small">(you)</span>{/if}</td>
                      <td class="small">{m.email}</td>
                      <td><span class="badge {roleBadge[m.role] ?? ''}">{m.role}</span></td>
                      <td class="right">
                        {#if isTeamAdmin && m.user_id !== session.me?.user.id && m.role !== 'owner'}
                          <button class="sm danger" onclick={() => removeMember(m)}>Remove</button>
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
        <TwoFactor />
      {:else if tab === 'notifications'}
        <Notifications />
      {:else}
        <p class="muted">Unknown section.</p>
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
