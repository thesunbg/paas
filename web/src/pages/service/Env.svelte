<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type ServiceDetail } from '../../lib/api';
  import { href, navigate } from '../../lib/router.svelte';
  import { rowsToMap, validateRows, type EnvRow } from '../../lib/env';
  import { toast } from '../../lib/toast.svelte';
  import EnvEditor from '../../components/EnvEditor.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import Loading from '../../components/Loading.svelte';

  let { serviceId, detail, reload }: { serviceId: string; detail: ServiceDetail; reload: () => Promise<void> } = $props();

  let rows = $state<EnvRow[]>([]);
  let original = $state('');
  let linked = $state<Record<string, string>>({});
  let loading = $state(true);
  let error = $state('');
  let saveError = $state('');
  let saving = $state(false);
  let restart = $state(true);

  async function load() {
    loading = true;
    try {
      const r = await api.env(serviceId);
      rows = Object.entries(r.vars)
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([key, value]) => ({ key, value }));
      original = JSON.stringify(rowsToMap(rows));
      linked = r.linked ?? {};
      error = '';
    } catch (e) {
      error = errorMessage(e);
    } finally {
      loading = false;
    }
  }
  onMount(load);

  const dirty = $derived(JSON.stringify(rowsToMap(rows)) !== original);

  async function save() {
    saveError = validateRows(rows);
    if (saveError) return;
    saving = true;
    try {
      const res = await api.putEnv(serviceId, rowsToMap(rows), { replace: true, restart });
      original = JSON.stringify(rowsToMap(rows));
      if (res.deployment) {
        toast.success(detail.service.type === 'static' ? 'Saved — rebuilding the site' : 'Saved — restarting with the new variables');
        reload();
        navigate(`/services/${serviceId}/deployments/${res.deployment.id}`);
      } else {
        toast.success(restart ? 'Saved — variables apply on the next deploy' : 'Saved');
      }
    } catch (e) {
      saveError = errorMessage(e);
    } finally {
      saving = false;
    }
  }

  const linkedEntries = $derived(Object.entries(linked));
  const tuning = $derived(Object.entries(detail.tuning_env ?? {}));
</script>

<div class="stack-lg">
  <ErrorMsg {error} onretry={load} />
  {#if loading}
    <Loading rows={2} />
  {:else if !error}
    <div class="card">
      <div class="card-head">
        <div>
          <h3>Environment variables</h3>
          <p class="hint">Encrypted at rest and injected at runtime{detail.service.type === 'static' ? ' and build time' : ''}.</p>
        </div>
        {#if dirty}<span class="badge yellow">unsaved changes</span>{/if}
      </div>
      <div class="card-body stack">
        <EnvEditor bind:rows masked />
        <ErrorMsg error={saveError} />
      </div>
      <div class="foot">
        <label class="check small"><input type="checkbox" bind:checked={restart} />Restart the service to apply</label>
        <div class="spacer"></div>
        <button onclick={load} disabled={saving || !dirty}>Discard</button>
        <button class="primary" onclick={save} disabled={saving || !dirty}>{#if saving}<span class="spinner"></span>{/if}Save</button>
      </div>
    </div>

    {#if linkedEntries.length || tuning.length}
      <div class="card">
        <div class="card-head"><h3>Provided by the platform</h3><span class="hint">read-only</span></div>
        <div class="table-wrap">
          <table>
            <thead><tr><th>Name</th><th>Value</th><th>Source</th></tr></thead>
            <tbody>
              {#each linkedEntries as [k, v] (k)}
                {@const db = detail.databases.find((d) => d.env_name === k)}
                <tr>
                  <td class="mono small">{k}</td>
                  <td class="faint small">{v}</td>
                  <td>{#if db}<a href={href(`/databases/${db.database_id}`)}>{db.name}</a> <span class="faint small">({db.engine})</span>{:else}database{/if}</td>
                </tr>
              {/each}
              {#each tuning as [k, v] (k)}
                <tr>
                  <td class="mono small">{k}</td>
                  <td class="mono small break">{v}</td>
                  <td class="faint small">runtime tuning (overridable)</td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      </div>
    {/if}
  {/if}
</div>

<style>
  .foot {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 12px 16px;
    border-top: 1px solid var(--border);
    background: var(--bg-elev-2);
    border-radius: 0 0 var(--radius) var(--radius);
    flex-wrap: wrap;
  }
</style>
