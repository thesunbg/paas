<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type UserSummary } from '../../lib/api';
  import { session } from '../../lib/session.svelte';
  import { toast } from '../../lib/toast.svelte';
  import { confirmDialog } from '../../lib/confirm.svelte';
  import { relTime } from '../../lib/format';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import Loading from '../../components/Loading.svelte';
  import CopyField from '../../components/CopyField.svelte';
  import { t } from '../../lib/i18n.svelte';

  let users = $state<UserSummary[] | null>(null);
  let error = $state('');
  let email = $state('');
  let name = $state('');
  let password = $state('');
  let admin = $state(false);
  let busy = $state(false);
  let shown = $state<{ email: string; password: string } | null>(null);
  let query = $state('');

  async function load() {
    try {
      users = await api.users();
      error = '';
    } catch (e) {
      error = errorMessage(e);
    }
  }
  onMount(load);

  const filtered = $derived(
    (users ?? []).filter((u) => !query || (u.email + ' ' + u.name + ' ' + (u.github_login ?? '')).toLowerCase().includes(query.toLowerCase())),
  );

  async function create(e: SubmitEvent) {
    e.preventDefault();
    busy = true;
    error = '';
    try {
      const r = await api.createUser({ email: email.trim(), name: name.trim() || undefined, password: password || undefined, is_admin: admin });
      toast.success(t('Account {email} created', { email: r.user.email }));
      shown = r.password ? { email: r.user.email, password: r.password } : null;
      email = name = password = '';
      admin = false;
      await load();
    } catch (err) {
      error = errorMessage(err);
    } finally {
      busy = false;
    }
  }

  async function toggleAdmin(u: UserSummary) {
    try {
      await api.updateUser(u.id, { is_admin: !u.is_admin });
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }
  async function resetPassword(u: UserSummary) {
    if (!(await confirmDialog({ title: t('Reset password'), message: t('Generate a new password for {email}? The old one stops working.', { email: u.email }), confirmLabel: t('Reset') }))) return;
    try {
      const r = await api.updateUser(u.id, { reset_password: true });
      if (r.password) shown = { email: u.email, password: r.password };
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }
  async function disable2FA(u: UserSummary) {
    if (!(await confirmDialog({ title: t('Turn off two-factor auth'), message: t('Turn off two-factor authentication for {email}?', { email: u.email }), confirmLabel: t('Turn off'), danger: true }))) return;
    try {
      await api.updateUser(u.id, { disable_2fa: true });
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }
  async function remove(u: UserSummary) {
    if (!(await confirmDialog({ title: t('Delete account'), message: t('Delete {email}? Their sessions and API tokens stop working.', { email: u.email }), confirmLabel: t('Delete'), danger: true }))) return;
    try {
      await api.deleteUser(u.id);
      toast.success(t('Account deleted'));
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }
</script>

<div class="stack-lg">
  <form class="card" onsubmit={create}>
    <div class="card-head">
      <div>
        <h3>{t('Create account')}</h3>
        <p class="hint">{t('The new user gets their own team. Leave the password empty to generate one.')}</p>
      </div>
    </div>
    <div class="card-body stack">
      <div class="grid-3">
        <label class="field"><span>{t('Email')}</span><input type="email" bind:value={email} required placeholder="user@example.com" autocomplete="off" /></label>
        <label class="field"><span>{t('Name')} <span class="faint">{t('(optional)')}</span></span><input bind:value={name} autocomplete="off" /></label>
        <label class="field"><span>{t('Password')} <span class="faint">{t('(optional)')}</span></span><input type="password" bind:value={password} minlength="8" autocomplete="new-password" placeholder={t('generate')} /></label>
      </div>
      <label class="check"><input type="checkbox" bind:checked={admin} />{t('Administrator (full access, no limits)')}</label>
      <ErrorMsg {error} />
      <div><button class="primary" type="submit" disabled={busy || !email.trim()}>{#if busy}<span class="spinner"></span>{/if}{t('Create account')}</button></div>
      {#if shown}
        <div class="alert success stack" style="gap:8px">
          <span>{t('Password for')} <b>{shown.email}</b> {t('— share it securely, it is shown only once:')}</span>
          <CopyField value={shown.password} label={t('Password')} />
          <div><button type="button" class="sm" onclick={() => (shown = null)}>{t('Done')}</button></div>
        </div>
      {/if}
    </div>
  </form>

  <div class="card">
    <div class="card-head">
      <h3>{t('Accounts')} {#if users}<span class="faint">({users.length})</span>{/if}</h3>
      <input class="search" bind:value={query} placeholder={t('Search…')} />
    </div>
    {#if users === null}
      <div class="card-body"><Loading /></div>
    {:else}
      <div class="table-wrap" style="border-top:1px solid var(--border)">
        <table>
          <thead><tr><th>{t('User')}</th><th>{t('Sign-in')}</th><th>{t('Teams')}</th><th>{t('Created at')}</th><th class="right"></th></tr></thead>
          <tbody>
            {#each filtered as u (u.id)}
              <tr>
                <td>
                  <b>{u.name}</b>{#if u.id === session.me?.user.id} <span class="faint small">{t('(you)')}</span>{/if}
                  {#if u.is_admin}<span class="badge">{t('admin')}</span>{/if}
                  <div class="small muted">{u.email}</div>
                </td>
                <td class="small">
                  {t('password')}{#if u.github_login} · GitHub <a href={`https://github.com/${u.github_login}`} target="_blank" rel="noreferrer">{u.github_login}</a>{/if}{#if u.totp_enabled} · 2FA{/if}
                </td>
                <td class="small">{u.teams}</td>
                <td class="small">{relTime(u.created_at)}</td>
                <td class="right">
                  {#if u.id !== session.me?.user.id}
                    <div class="acts">
                    <button class="sm" onclick={() => toggleAdmin(u)}>{u.is_admin ? t('Remove admin') : t('Make admin')}</button>
                    <button class="sm" onclick={() => resetPassword(u)}>{t('Reset password')}</button>
                    {#if u.totp_enabled}<button class="sm" onclick={() => disable2FA(u)}>{t('Turn off 2FA')}</button>{/if}
                    <button class="sm danger" onclick={() => remove(u)}>{t('Delete')}</button>
                    </div>
                  {/if}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </div>
</div>

<style>
  .search {
    max-width: 220px;
  }
  .badge {
    margin-left: 6px;
  }
  .acts {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
    justify-content: flex-end;
    min-width: 200px;
  }
</style>
