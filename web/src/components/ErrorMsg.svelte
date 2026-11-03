<script lang="ts">
  import Icon from './Icon.svelte';
  import { t } from '../lib/i18n.svelte';
  let { error, onretry }: { error: string | null | undefined; onretry?: () => void } = $props();
</script>

{#if error}
  <div class="alert error" role="alert">
    <Icon name="x" />
    <span style="flex:1">{error}</span>
    {#if onretry}<button class="sm" onclick={onretry}>{t('Retry')}</button>{/if}
  </div>
{/if}
