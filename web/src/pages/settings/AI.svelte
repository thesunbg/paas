<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type AIInput, type AISettings } from '../../lib/api';
  import { toast } from '../../lib/toast.svelte';
  import { confirmDialog } from '../../lib/confirm.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import Loading from '../../components/Loading.svelte';
  import CopyField from '../../components/CopyField.svelte';

  let settings = $state<AISettings | null>(null);
  let form = $state<AIInput>({ provider: 'anthropic', model: '', base_url: '', api_key: '' });
  let error = $state('');
  let busy = $state(false);
  const mcpURL = `${location.origin}/mcp`;

  function fill(s: AISettings) {
    settings = s;
    form = { provider: s.provider || 'anthropic', model: s.model, base_url: s.base_url, api_key: '' };
  }

  onMount(async () => {
    try {
      fill(await api.aiSettings());
    } catch (e) {
      error = errorMessage(e);
    }
  });

  async function save(e: SubmitEvent) {
    e.preventDefault();
    busy = true;
    error = '';
    try {
      fill(await api.putAISettings(form));
      toast.success('AI provider saved');
    } catch (err) {
      error = errorMessage(err);
    } finally {
      busy = false;
    }
  }

  async function disable() {
    if (!(await confirmDialog({ title: 'Turn off AI diagnosis', message: 'Remove the provider and its key? Cached diagnoses stay visible.', confirmLabel: 'Turn off', danger: true }))) return;
    busy = true;
    try {
      fill(await api.putAISettings({ provider: '', model: '', base_url: '', api_key: '' }));
      toast.success('AI diagnosis turned off');
    } catch (err) {
      error = errorMessage(err);
    } finally {
      busy = false;
    }
  }

  const keyChanged = $derived(settings?.provider !== form.provider);
</script>

<div class="stack-lg">
  <form class="card" onsubmit={save}>
    <div class="card-head">
      <div>
        <h3>AI failure diagnosis</h3>
        <p class="hint">
          Adds an “Explain this failure” button to failed deployments (and <code>paas diagnose</code>). The tail of the build log, the app's output and the error are sent to
          the model with secrets redacted; it only suggests fixes. Off until a provider is set.
        </p>
      </div>
      {#if settings}<span class="badge {settings.enabled ? 'green' : 'yellow'}">{settings.enabled ? 'on' : 'off'}</span>{/if}
    </div>
    <div class="card-body stack">
      {#if !settings}
        {#if error}<ErrorMsg {error} />{:else}<Loading />{/if}
      {:else}
        <div class="grid-3">
          <label class="field">
            <span>Provider</span>
            <select bind:value={form.provider}>
              <option value="anthropic">Anthropic (Claude)</option>
              <option value="openai">OpenAI-compatible (self-hosted, Ollama, vLLM…)</option>
            </select>
          </label>
          <label class="field">
            <span>Model</span>
            <input bind:value={form.model} placeholder={form.provider === 'anthropic' ? settings.default_model : 'llama3.1'} />
          </label>
          <label class="field">
            <span>API key</span>
            <input type="password" bind:value={form.api_key} autocomplete="new-password" placeholder={settings.has_key && !keyChanged ? 'unchanged' : form.provider === 'openai' ? 'optional' : ''} />
          </label>
        </div>
        <label class="field">
          <span>Base URL</span>
          <input bind:value={form.base_url} placeholder={form.provider === 'anthropic' ? 'https://api.anthropic.com' : 'http://ollama.internal:11434/v1'} />
          <span class="hint">{form.provider === 'anthropic' ? 'Leave empty for api.anthropic.com.' : 'The endpoint that serves /chat/completions.'} The key is stored encrypted and never shown again.</span>
        </label>
        <ErrorMsg {error} />
        <div class="row">
          <button class="primary" type="submit" disabled={busy}>{#if busy}<span class="spinner"></span>{/if}Save</button>
          {#if settings.enabled}<button type="button" class="danger" onclick={disable} disabled={busy}>Turn off</button>{/if}
        </div>
      {/if}
    </div>
  </form>

  <div class="card">
    <div class="card-head">
      <div>
        <h3>MCP endpoint for AI agents</h3>
        <p class="hint">
          Claude Code, Claude Desktop and other MCP clients can inspect and operate apps through this endpoint with an API token (Settings → API tokens). Each tool
          call has the token owner's permissions; write tools need the developer role and are audited.
        </p>
      </div>
    </div>
    <div class="card-body stack">
      <CopyField value={mcpURL} label="Endpoint" />
      <CopyField value={`claude mcp add --transport http paas ${mcpURL} --header "Authorization: Bearer <API token>"`} label="Claude Code" />
    </div>
  </div>
</div>
