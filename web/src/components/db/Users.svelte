<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type DatabaseDetail, type DBUserInfo } from '../../lib/api';
  import { toast } from '../../lib/toast.svelte';
  import { confirmDialog } from '../../lib/confirm.svelte';
  import ErrorMsg from '../ErrorMsg.svelte';
  import Secret from '../Secret.svelte';
  import { t } from '../../lib/i18n.svelte';

  let { detail, isAdmin }: { detail: DatabaseDetail; isAdmin: boolean } = $props();
  const db = $derived(detail.database);
  let users = $state<DBUserInfo[] | null>(null);
  let name = $state('');
  let role = $state<'readonly' | 'readwrite'>('readonly');
  let busy = $state(false);
  let error = $state('');

  async function load() {
    try {
      users = await api.dbUsers(db.id);
    } catch (e) {
      error = errorMessage(e);
    }
  }
  onMount(load);

  async function create(e: SubmitEvent) {
    e.preventDefault();
    busy = true;
    error = '';
    try {
      const u = await api.createDBUser(db.id, name.trim(), role);
      toast.success(t('User {name} created', { name: u.username }));
      name = '';
      await load();
    } catch (err) {
      error = errorMessage(err);
    } finally {
      busy = false;
    }
  }
  async function rotate(u: DBUserInfo) {
    if (!(await confirmDialog({ title: t('New password'), message: t('Generate a new password for {name}? The old one stops working immediately.', { name: u.username }), confirmLabel: t('Rotate') }))) return;
    try {
      await api.rotateDBUser(db.id, u.username);
      toast.success(t('Password rotated'));
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }
  async function remove(u: DBUserInfo) {
    if (!(await confirmDialog({ title: t('Delete user'), message: t('Delete {name}? Its connections are closed.', { name: u.username }), confirmLabel: t('Delete'), danger: true }))) return;
    try {
      await api.deleteDBUser(db.id, u.username);
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }
</script>

<div class="card">
  <div class="card-head">
    <div>
      <h3>{t('Users')}</h3>
      <p class="hint">{t('Extra logins for analysts, BI tools or services that should not change the schema.')}</p>
    </div>
  </div>
  <div class="card-body stack">
    <ErrorMsg {error} />
    {#if users && users.length}
      <div class="table-wrap">
        <table>
          <thead><tr><th>{t('User')}</th><th>{t('Role')}</th><th>URL</th><th class="right"></th></tr></thead>
          <tbody>
            {#each users as u (u.username)}
              <tr>
                <td class="mono small">{u.username}</td>
                <td><span class="badge">{u.role === 'readonly' ? t('read only') : t('read / write')}</span></td>
                <td style="min-width:180px"><Secret value={u.url} /></td>
                <td class="right nowrap">
                  {#if isAdmin}
                    <button class="sm" onclick={() => rotate(u)}>{t('New password')}</button>
                    <button class="sm danger" onclick={() => remove(u)}>{t('Delete')}</button>
                  {/if}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {:else if users}
      <p class="muted small">{t('Only the owner account')} <code>{db.username}</code>.</p>
    {/if}
    {#if isAdmin}
      <form class="row" onsubmit={create} style="align-items:flex-end;flex-wrap:wrap">
        <label class="field" style="max-width:200px"><span>{t('Name')}</span><input class="mono" bind:value={name} placeholder="analytics" required /></label>
        <label class="field" style="max-width:180px">
          <span>{t('Role')}</span>
          <select bind:value={role}><option value="readonly">{t('read only')}</option><option value="readwrite">{t('read / write')}</option></select>
        </label>
        <button class="primary sm" type="submit" disabled={busy || !name.trim() || db.status !== 'ready'}>{#if busy}<span class="spinner"></span>{/if}{t('Add user')}</button>
      </form>
      <p class="hint">{t('The login is')} <code>{db.username}_&lt;{t('name')}&gt;</code>. {t('Read/write users can change rows but not the schema.')}</p>
    {/if}
  </div>
</div>
