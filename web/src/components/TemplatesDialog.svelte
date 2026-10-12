<script lang="ts">
  import { api, errorMessage, type Template } from '../lib/api';
  import { toast } from '../lib/toast.svelte';
  import Modal from './Modal.svelte';
  import ErrorMsg from './ErrorMsg.svelte';
  import Loading from './Loading.svelte';
  import Icon from './Icon.svelte';

  let { open = $bindable(false), projectId, ondone }: { open: boolean; projectId: string; ondone: () => void } = $props();

  let templates = $state<Template[] | null>(null);
  let loadError = $state('');
  let picked = $state<Template | null>(null);
  let domain = $state('');
  let category = $state('All');
  let busy = $state(false);
  let error = $state('');

  $effect(() => {
    if (open && templates === null && !loadError) {
      api
        .templates()
        .then((t) => (templates = t ?? []))
        .catch((e) => (loadError = errorMessage(e)));
    }
    if (!open) {
      picked = null;
      error = '';
      domain = '';
    }
  });

  const categories = $derived(['All', ...new Set((templates ?? []).map((t) => t.category))]);
  const shown = $derived((templates ?? []).filter((t) => category === 'All' || t.category === category));

  async function install(e: SubmitEvent) {
    e.preventDefault();
    if (!picked) return;
    busy = true;
    error = '';
    try {
      const r = await api.installTemplate(projectId, picked.id, domain.trim() || undefined);
      const ns = r.services?.length ?? 0;
      const nd = r.databases?.length ?? 0;
      toast.success(`${picked.title}: ${ns} service${ns === 1 ? '' : 's'}${nd ? ` and ${nd} database${nd === 1 ? '' : 's'}` : ''} created — deploying`);
      open = false;
      ondone();
    } catch (err) {
      error = errorMessage(err);
    } finally {
      busy = false;
    }
  }
</script>

<Modal bind:open title={picked ? `Install ${picked.title}` : 'New from template'} width={760}>
  {#if loadError}
    <ErrorMsg error={loadError} />
  {:else if templates === null}
    <Loading />
  {:else if !picked}
    <div class="stack">
      <div class="row" role="group" aria-label="Category">
        {#each categories as c (c)}
          <button type="button" class="sm" class:on={category === c} aria-pressed={category === c} onclick={() => (category = c)}>{c}</button>
        {/each}
      </div>
      <div class="gallery">
        {#each shown as t (t.id)}
          <button type="button" class="tpl" onclick={() => (picked = t)}>
            <span class="between nowrap" style="width:100%">
              <b>{t.title}</b>
              <span class="badge">{t.category}</span>
            </span>
            <span class="muted small desc">{t.description}</span>
            <span class="faint small">
              <Icon name="box" size={12} /> {t.services.length} service{t.services.length === 1 ? '' : 's'}
              {#if t.databases?.length}· <Icon name="database" size={12} /> {t.databases.map((d) => d.engine).join(', ')}{/if}
            </span>
          </button>
        {:else}
          <p class="muted">No templates.</p>
        {/each}
      </div>
    </div>
  {:else}
    <form id="tpl-form" class="stack" onsubmit={install}>
      <p class="muted">{picked.description}</p>
      <div class="card">
        <table>
          <thead><tr><th>Creates</th><th>Image / engine</th><th>Details</th></tr></thead>
          <tbody>
            {#each picked.databases ?? [] as d (d.name)}
              <tr><td><Icon name="database" size={13} /> {d.name}</td><td>{d.engine}</td><td class="faint small">{d.mode ?? 'shared'} database, provisioned first</td></tr>
            {/each}
            {#each picked.services as sv (sv.name)}
              <tr>
                <td><Icon name="box" size={13} /> {sv.name}</td>
                <td class="mono small break">{sv.image}</td>
                <td class="faint small">{sv.type}{sv.port ? ` · port ${sv.port}` : ''}{sv.size ? ` · ${sv.size}` : ''}{sv.volumes?.length ? ` · ${sv.volumes.length} volume` : ''}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
      <label class="field">
        <span>Custom domain <span class="faint">(optional)</span></span>
        <input class="mono" bind:value={domain} placeholder="blog.example.com" spellcheck="false" />
        <span class="hint">Used for the first web service; a generated domain is always added.</span>
      </label>
      <ErrorMsg {error} />
    </form>
  {/if}
  {#snippet footer()}
    {#if picked}
      <button type="button" onclick={() => (picked = null)}><Icon name="arrowleft" size={14} />Back</button>
      <button type="submit" form="tpl-form" class="primary" disabled={busy}>{#if busy}<span class="spinner"></span>{/if}Install</button>
    {:else}
      <button type="button" onclick={() => (open = false)}>Close</button>
    {/if}
  {/snippet}
</Modal>

<style>
  .gallery {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(210px, 1fr));
    gap: 8px;
  }
  .tpl {
    height: auto;
    flex-direction: column;
    align-items: flex-start;
    gap: 6px;
    padding: 12px;
    text-align: left;
    white-space: normal;
    font-weight: 400;
    background: var(--bg-elev);
  }
  .tpl:hover:not(:disabled) {
    border-color: var(--accent);
  }
  .desc {
    display: -webkit-box;
    -webkit-line-clamp: 3;
    line-clamp: 3;
    -webkit-box-orient: vertical;
    overflow: hidden;
  }
  .on {
    border-color: var(--accent);
    background: var(--accent-soft);
  }
</style>
