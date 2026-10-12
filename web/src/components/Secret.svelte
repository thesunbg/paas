<script lang="ts">
  // Masked value with reveal toggle and copy button.
  import Icon from './Icon.svelte';
  import CopyButton from './CopyButton.svelte';
  let { value, copyable = true }: { value: string; copyable?: boolean } = $props();
  let shown = $state(false);
</script>

<span class="secret">
  <code class="val" class:masked={!shown}>{shown ? value : '•'.repeat(Math.min(Math.max(value.length, 8), 24))}</code>
  <button type="button" class="sm icon" aria-label={shown ? 'Hide value' : 'Reveal value'} aria-pressed={shown} onclick={() => (shown = !shown)}>
    <Icon name={shown ? 'eyeoff' : 'eye'} size={13} />
  </button>
  {#if copyable}<CopyButton text={value} />{/if}
</span>

<style>
  .secret {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
    max-width: 100%;
  }
  .val {
    overflow-wrap: anywhere;
    min-width: 0;
  }
  .val.masked {
    white-space: nowrap;
    overflow: hidden;
    text-overflow: clip;
  }
</style>
