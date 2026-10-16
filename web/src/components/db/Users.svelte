<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type DatabaseDetail, type DBUserInfo } from '../../lib/api';
  import { toast } from '../../lib/toast.svelte';
  import { confirmDialog } from '../../lib/confirm.svelte';
  import ErrorMsg from '../ErrorMsg.svelte';
  import Secret from '../Secret.svelte';

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
      toast.success(`User ${u.username} created`);
      name = '';
      await load();
    } catch (err) {
      error = errorMessage(err);
    } finally {
      busy = false;
    }
  }
  async function rotate(u: DBUserInfo) {
    if (!(await confirmDialog({ title: 'New password', message: `Generate a new password for ${u.username}? The old one stops working immediately.`, confirmLabel: 'Rotate' }))) return;
    try {
      await api.rotateDBUser(db.id, u.username);
      toast.success('Password rotated');
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }
  async function remove(u: DBUserInfo) {
    if (!(await confirmDialog({ title: 'Delete user', message: `Delete ${u.username}? Its connections are closed.`, confirmLabel: 'Delete', danger: true }))) return;
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
      <h3>Users</h3>
      <p class="hint">Extra logins for analysts, BI tools or services that should not change the schema.</p>
    </div>
  </div>
  <div class="card-body stack">
    <ErrorMsg {error} />
    {#if users && users.length}
      <div class="table-wrap">
        <table>
          <thead><tr><th>User</th><th>Role</th><th>URL</th><th class="right"></th></tr></thead>
          <tbody>
            {#each users as u (u.username)}
              <tr>
                <td class="mono small">{u.username}</td>
                <td><span class="badge">{u.role === 'readonly' ? 'read only' : 'read / write'}</span></td>
                <td style="min-width:180px"><Secret value={u.url} /></td>
                <td class="right nowrap">
                  {#if isAdmin}
                    <button class="sm" onclick={() => rotate(u)}>New password</button>
                    <button class="sm danger" onclick={() => remove(u)}>Delete</button>
                  {/if}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {:else if users}
      <p class="muted small">Only the owner account <code>{db.username}</code>.</p>
    {/if}
    {#if isAdmin}
      <form class="row" onsubmit={create} style="align-items:flex-end;flex-wrap:wrap">
        <label class="field" style="max-width:200px"><span>Name</span><input class="mono" bind:value={name} placeholder="analytics" required /></label>
        <label class="field" style="max-width:180px">
          <span>Role</span>
          <select bind:value={role}><option value="readonly">read only</option><option value="readwrite">read / write</option></select>
        </label>
        <button class="primary sm" type="submit" disabled={busy || !name.trim() || db.status !== 'ready'}>{#if busy}<span class="spinner"></span>{/if}Add user</button>
      </form>
      <p class="hint">The login is <code>{db.username}_&lt;name&gt;</code>. Read/write users can change rows but not the schema.</p>
    {/if}
  </div>
</div>
