<script lang="ts">
  import { t } from '../lib/i18n.svelte';
  let { label, rows = 0 }: { label?: string; rows?: number } = $props();
  const text = $derived(label ?? t('Loading…'));
</script>

{#if rows > 0}
  <div class="stack" aria-busy="true" aria-label={text}>
    {#each Array(rows) as _, i (i)}
      <div class="skeleton" style="height:{i === 0 ? 72 : 44}px"></div>
    {/each}
  </div>
{:else}
  <div class="row muted" aria-busy="true"><span class="spinner"></span>{text}</div>
{/if}
