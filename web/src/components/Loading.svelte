<script lang="ts">
  let { label = 'Loading…', rows = 0 }: { label?: string; rows?: number } = $props();
</script>

{#if rows > 0}
  <div class="stack" aria-busy="true" aria-label={label}>
    {#each Array(rows) as _, i (i)}
      <div class="skeleton" style="height:{i === 0 ? 72 : 44}px"></div>
    {/each}
  </div>
{:else}
  <div class="row muted" aria-busy="true"><span class="spinner"></span>{label}</div>
{/if}
