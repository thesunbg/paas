<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type Diagnosis } from '../lib/api';
  import { relTime, absTime } from '../lib/format';
  import ErrorMsg from './ErrorMsg.svelte';
  import Icon from './Icon.svelte';
  import { t } from '../lib/i18n.svelte';

  // AI explanation of a failed deployment. Hidden unless an administrator set
  // up a provider (or an earlier answer is cached).
  let { deploymentId, canEdit }: { deploymentId: string; canEdit: boolean } = $props();

  let enabled = $state(false);
  let result = $state<Diagnosis | null>(null);
  let busy = $state(false);
  let error = $state('');

  onMount(async () => {
    try {
      const r = await api.diagnosis(deploymentId);
      enabled = r.enabled;
      result = r.diagnosis;
    } catch {
      // Optional feature: stay hidden.
    }
  });

  async function explain(refresh: boolean) {
    busy = true;
    error = '';
    try {
      result = await api.diagnose(deploymentId, refresh);
    } catch (e) {
      error = errorMessage(e);
    } finally {
      busy = false;
    }
  }
</script>

{#if result || (enabled && canEdit)}
  <div class="card">
    <div class="card-head">
      <div>
        <h3>{t('AI diagnosis')}</h3>
        <p class="hint">{t('Suggestions from a language model, based on the redacted logs. Nothing is changed automatically.')}</p>
      </div>
      {#if enabled && canEdit}
        <button class="sm {result ? '' : 'primary'}" onclick={() => explain(!!result)} disabled={busy}>
          {#if busy}<span class="spinner"></span>{:else}<Icon name={result ? 'refresh' : 'zap'} size={12} />{/if}
          {result ? t('Ask again') : t('Explain this failure')}
        </button>
      {/if}
    </div>
    {#if error || result || busy}
      <div class="card-body stack">
        <ErrorMsg {error} />
        {#if busy && !result}<p class="muted small">{t('Asking the model, this can take up to a minute…')}</p>{/if}
        {#if result}
          <p>{result.summary}</p>
          {#if result.likely_cause}
            <div>
              <div class="label">{t('Likely cause')}</div>
              <p>{result.likely_cause}</p>
            </div>
          {/if}
          {#if result.suggested_fixes.length}
            <div>
              <div class="label">{t('Suggested fixes')}</div>
              <ol>
                {#each result.suggested_fixes as fix, i (i)}<li>{fix}</li>{/each}
              </ol>
            </div>
          {/if}
          <p class="faint small" title={absTime(result.created_at)}>{result.model} · {relTime(result.created_at)}</p>
        {/if}
      </div>
    {/if}
  </div>
{/if}

<style>
  .label {
    font-size: 12px;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--text-dim);
    margin-bottom: 4px;
  }
  ol {
    margin: 0;
    padding-left: 20px;
  }
  li + li {
    margin-top: 4px;
  }
  p {
    margin: 0;
  }
</style>
