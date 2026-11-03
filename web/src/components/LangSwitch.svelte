<script lang="ts">
  import { i18n, locales, setLocale, t } from '../lib/i18n.svelte';
</script>

<!-- EN | VI toggle; the choice is kept in this browser. -->
<div class="lang" role="group" aria-label={t('Language')}>
  {#each locales as l (l.id)}
    <button type="button" class="ghost" class:on={i18n.locale === l.id} aria-pressed={i18n.locale === l.id} title={l.label} onclick={() => setLocale(l.id)}>{l.short}</button>
  {/each}
</div>

<style>
  .lang {
    display: inline-flex;
    border: 1px solid var(--border);
    border-radius: 6px;
    overflow: hidden;
  }
  .lang button {
    padding: 2px 8px;
    font-size: 12px;
    border-radius: 0;
    color: var(--fg-muted, inherit);
  }
  .lang button.on {
    background: var(--bg-subtle, rgba(127, 127, 127, 0.15));
    color: inherit;
    font-weight: 600;
  }
</style>
