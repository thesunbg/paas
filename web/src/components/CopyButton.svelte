<script lang="ts">
  import { copy } from '../lib/clipboard';
  import Icon from './Icon.svelte';
  import { t } from '../lib/i18n.svelte';
  let { text, label, small = true }: { text: string; label?: string; small?: boolean } = $props();
  // Without a custom label a small button shows only the icon.
  const custom = $derived(label !== undefined && label !== 'Copy' && label !== t('Copy'));
  const shown = $derived(label ?? t('Copy'));
</script>

<button type="button" class:sm={small} onclick={() => copy(text)} aria-label={shown} title={shown}><Icon name="copy" size={13} />{#if custom || !small}<span class="hide-sm">{shown}</span>{/if}</button>
