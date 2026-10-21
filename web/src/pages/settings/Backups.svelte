<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type StorageSettings, type StateBackups } from '../../lib/api';
  import { relTime, absTime, bytes } from '../../lib/format';
  import { toast } from '../../lib/toast.svelte';
  import { confirmDialog } from '../../lib/confirm.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import Loading from '../../components/Loading.svelte';

  let storage = $state<StorageSettings | null>(null);
  let form = $state({ endpoint: '', region: '', bucket: '', access_key: '', secret_key: '', prefix: '', virtual_host: false });
  let storageError = $state('');
  let storageBusy = $state(false);

  let state_ = $state<StateBackups | null>(null);
  let stateError = $state('');
  let stateBusy = $state(false);

  function fill(s: StorageSettings) {
    storage = s;
    form = { endpoint: s.endpoint, region: s.region, bucket: s.bucket, access_key: s.access_key, secret_key: '', prefix: s.prefix, virtual_host: s.virtual_host };
  }

  async function loadState() {
    try {
      state_ = await api.stateBackups();
      stateError = '';
    } catch (e) {
      stateError = errorMessage(e);
    }
  }

  onMount(async () => {
    try {
      fill(await api.storage());
    } catch (e) {
      storageError = errorMessage(e);
    }
    loadState();
  });

  async function save(e: SubmitEvent) {
    e.preventDefault();
    storageBusy = true;
    storageError = '';
    try {
      fill(await api.putStorage(form));
      toast.success(storage?.enabled ? 'Storage checked and saved' : 'Off-site storage turned off');
      loadState();
    } catch (err) {
      storageError = errorMessage(err);
    } finally {
      storageBusy = false;
    }
  }

  async function disable() {
    if (!(await confirmDialog({ title: 'Turn off off-site storage', message: 'New backups stay on the nodes only, and are lost with a node. Existing objects in the bucket are kept.', confirmLabel: 'Turn off', danger: true }))) return;
    form = { endpoint: '', region: '', bucket: '', access_key: '', secret_key: '', prefix: '', virtual_host: false };
    storageBusy = true;
    try {
      fill(await api.putStorage(form));
      toast.success('Off-site storage turned off');
    } catch (err) {
      storageError = errorMessage(err);
    } finally {
      storageBusy = false;
    }
  }

  async function backupNow() {
    stateBusy = true;
    try {
      const r = await api.stateBackupNow();
      if (r.error) toast.error(r.error);
      else toast.success(r.offsite ? 'Platform state backed up and copied off-site' : 'Platform state backed up on this server');
      await loadState();
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      stateBusy = false;
    }
  }

  // state-YYYYMMDD-HHMMSS.pgstate -> ISO time
  function nameTime(n: string): string | null {
    const m = /state-(\d{4})(\d{2})(\d{2})-(\d{2})(\d{2})(\d{2})/.exec(n);
    return m ? `${m[1]}-${m[2]}-${m[3]}T${m[4]}:${m[5]}:${m[6]}Z` : null;
  }
  let sorted = $derived([...(state_?.backups ?? [])].sort((a, b) => b.name.localeCompare(a.name)));
</script>

<div class="stack-lg">
  <form class="card" onsubmit={save}>
    <div class="card-head">
      <div>
        <h3>Off-site storage</h3>
        <p class="hint">An S3-compatible bucket (AWS, Cloudflare R2, Backblaze B2, MinIO…) outside this cluster. Database dumps, volume backups, static sites and the platform's own state are copied there, so they survive the loss of a node.</p>
      </div>
      {#if storage}
        <span class="badge {storage.enabled ? 'green' : 'yellow'}">{storage.enabled ? 'on' : 'off'}</span>
      {/if}
    </div>
    <div class="card-body stack">
      {#if !storage}
        {#if storageError}<ErrorMsg error={storageError} />{:else}<Loading />{/if}
      {:else if storage.from_env}
        <p>Set by <code>PAAS_S3_*</code> environment variables on the control plane: <code>{storage.endpoint}</code>, bucket <code>{storage.bucket}</code>. Remove them to manage it here.</p>
      {:else}
        <div class="grid-3">
          <label class="field"><span>Endpoint</span><input bind:value={form.endpoint} placeholder="https://s3.eu-central-1.amazonaws.com" /></label>
          <label class="field"><span>Bucket</span><input bind:value={form.bucket} placeholder="paas-backups" /></label>
          <label class="field"><span>Region</span><input bind:value={form.region} placeholder="us-east-1" /></label>
          <label class="field"><span>Access key</span><input bind:value={form.access_key} autocomplete="off" /></label>
          <label class="field">
            <span>Secret key</span>
            <input type="password" bind:value={form.secret_key} autocomplete="new-password" placeholder={storage.has_secret ? 'unchanged' : ''} />
          </label>
          <label class="field"><span>Prefix</span><input bind:value={form.prefix} placeholder="paas-backups" /></label>
        </div>
        <label class="check"><input type="checkbox" bind:checked={form.virtual_host} />Virtual-hosted style URLs (bucket.endpoint)</label>
        <p class="hint">Saving writes, reads and deletes a test object first; nothing is saved if that fails. Use a bucket only this platform writes to, and keep its credentials somewhere outside the cluster too: you need them to restore after losing the control plane.</p>
        <ErrorMsg error={storageError} />
        <div class="row">
          <button class="primary" type="submit" disabled={storageBusy || !form.endpoint || !form.bucket}>{#if storageBusy}<span class="spinner"></span>{/if}Test and save</button>
          {#if storage.enabled}<button type="button" class="danger" onclick={disable} disabled={storageBusy}>Turn off</button>{/if}
        </div>
      {/if}
    </div>
  </form>

  <div class="card">
    <div class="card-head">
      <div>
        <h3>Platform state</h3>
        <p class="hint">
          The control-plane database (projects, services, settings, encrypted secrets), dumped every {(state_?.every ?? '1h').replace(/0m0s$/, '').replace(/0s$/, '')} and encrypted with the master key. Restore it on a new server with
          <code>paasd restore-state --latest</code>. Without <code>/var/lib/paas/master.key</code> these backups cannot be opened: keep a copy of that file somewhere safe.
        </p>
      </div>
      <button onclick={backupNow} disabled={stateBusy}>{#if stateBusy}<span class="spinner"></span>{/if}Back up now</button>
    </div>
    <div class="card-body stack">
      {#if !state_}
        {#if stateError}<ErrorMsg error={stateError} />{:else}<Loading />{/if}
      {:else}
        {#if state_.last}
          <p>
            Last run {relTime(state_.last.at)}:
            {#if state_.last.error}<span class="badge red">failed</span> {state_.last.error}
            {:else}<span class="badge green">ok</span> {bytes(state_.last.size)}{state_.last.duration ? ` in ${state_.last.duration}` : ''},
              {state_.last.offsite ? 'copied off-site' : 'on this server only'}{/if}
          </p>
        {/if}
        {#if !storage?.enabled}<p class="hint">Off-site storage is off: these backups live on the control-plane server and are lost with it.</p>{/if}
        {#if state_.offsite_error}<ErrorMsg error={'Off-site listing: ' + state_.offsite_error} />{/if}
        {#if sorted.length === 0}
          <p class="hint">No backups yet.</p>
        {:else}
          <table>
            <thead><tr><th>Backup</th><th>Taken</th><th>Where</th><th></th></tr></thead>
            <tbody>
              {#each sorted.slice(0, 30) as b (b.name)}
                <tr>
                  <td><code>{b.name}</code></td>
                  <td title={absTime(nameTime(b.name))}>{relTime(nameTime(b.name))}</td>
                  <td>
                    {#if b.local}<span class="badge">server</span>{/if}
                    {#if b.offsite}<span class="badge blue">off-site</span>{/if}
                  </td>
                  <td class="right">{#if b.local}<a href={api.stateBackupURL(b.name)} download>Download</a>{/if}</td>
                </tr>
              {/each}
            </tbody>
          </table>
          {#if sorted.length > 30}<p class="hint">{sorted.length - 30} older backups not shown.</p>{/if}
        {/if}
      {/if}
    </div>
  </div>
</div>
