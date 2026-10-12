<script lang="ts">
  import CopyButton from './CopyButton.svelte';
  let { value, label, multiline = false }: { value: string; label?: string; multiline?: boolean } = $props();
</script>

<div class="cf" class:multi={multiline}>
  <code class="v" aria-label={label}>{value}</code>
  <CopyButton text={value} />
</div>

<style>
  .cf {
    display: flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
  }
  .v {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    padding: 5px 8px;
  }
  .multi .v {
    white-space: pre-wrap;
    word-break: break-all;
  }
</style>
