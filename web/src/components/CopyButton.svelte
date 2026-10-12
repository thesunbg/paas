<script lang="ts">
  import { copy } from '../lib/clipboard';
  import Icon from './Icon.svelte';
  let { text, label = 'Copy', small = true }: { text: string; label?: string; small?: boolean } = $props();
</script>

<button type="button" class:sm={small} onclick={() => copy(text)} aria-label={label} title={label}><Icon name="copy" size={13} />{#if label !== 'Copy' || !small}<span class="hide-sm">{label}</span>{/if}</button>
