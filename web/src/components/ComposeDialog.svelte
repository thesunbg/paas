<script lang="ts">
  import { api, errorMessage, type Service } from '../lib/api';
  import { toast } from '../lib/toast.svelte';
  import Modal from './Modal.svelte';
  import ErrorMsg from './ErrorMsg.svelte';
  import Icon from './Icon.svelte';

  let { open = $bindable(false), projectId, ondone }: { open: boolean; projectId: string; ondone: () => void } = $props();

  let text = $state('');
  let busy = $state(false);
  let error = $state('');
  let result = $state<{ services: Service[]; warnings: string[] } | null>(null);
  let fileInput: HTMLInputElement | undefined = $state();

  $effect(() => {
    if (!open) {
      error = '';
      result = null;
    }
  });

  async function readFile(e: Event) {
    const f = (e.currentTarget as HTMLInputElement).files?.[0];
    if (f) text = await f.text();
  }

  async function submit(e: SubmitEvent) {
    e.preventDefault();
    if (!text.trim()) return;
    busy = true;
    error = '';
    try {
      const r = await api.importCompose(projectId, text);
      result = { services: r.services ?? [], warnings: r.warnings ?? [] };
      toast.success(`Imported ${result.services.length} service${result.services.length === 1 ? '' : 's'}`);
      ondone();
      if (!result.warnings.length) open = false;
    } catch (err) {
      error = errorMessage(err);
    } finally {
      busy = false;
    }
  }
</script>

<Modal bind:open title="Import docker-compose" width={680}>
  {#if result}
    <div class="stack">
      <div class="alert success">Created {result.services.map((s) => s.name).join(', ') || 'no services'}.</div>
      {#if result.warnings.length}
        <div class="alert warn">
          <Icon name="activity" />
          <div>
            <b>Warnings</b>
            <ul>{#each result.warnings as w, i (i)}<li class="small">{w}</li>{/each}</ul>
          </div>
        </div>
      {/if}
    </div>
  {:else}
    <form id="compose-form" class="stack" onsubmit={submit}>
      <p class="muted small">Image-based services are imported (no <code>build:</code>). Named volumes are kept and services reach each other by their compose name.</p>
      <label class="field">
        <span>docker-compose.yml</span>
        <textarea class="mono" rows="14" bind:value={text} spellcheck="false" placeholder={'services:\n  web:\n    image: nginx:alpine\n    ports: ["80:80"]\n  cache:\n    image: redis:7-alpine'}></textarea>
      </label>
      <div class="row">
        <input type="file" accept=".yml,.yaml,text/yaml" bind:this={fileInput} onchange={readFile} hidden />
        <button type="button" class="sm" onclick={() => fileInput?.click()}><Icon name="upload" size={12} />Load file…</button>
      </div>
      <ErrorMsg {error} />
    </form>
  {/if}
  {#snippet footer()}
    {#if result}
      <button type="button" class="primary" onclick={() => (open = false)}>Done</button>
    {:else}
      <button type="button" onclick={() => (open = false)}>Cancel</button>
      <button type="submit" form="compose-form" class="primary" disabled={busy || !text.trim()}>{#if busy}<span class="spinner"></span>{/if}Import</button>
    {/if}
  {/snippet}
</Modal>

<style>
  ul {
    margin: 4px 0 0;
    padding-left: 18px;
  }
</style>
