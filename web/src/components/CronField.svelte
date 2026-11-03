<script lang="ts">
  // Cron schedule input with presets and a live "next runs" hint.
  import { parseCron, nextRuns, describeCron, cronPresets } from '../lib/cron';
  import { relTime } from '../lib/format';
  import { t, intlLocale } from '../lib/i18n.svelte';

  let { value = $bindable('*/15 * * * *'), id = 'cron-' + Math.random().toString(36).slice(2, 7) }: { value: string; id?: string } = $props();

  const parsed = $derived.by(() => {
    try {
      const s = parseCron(value);
      return { ok: true as const, next: nextRuns(s, 3), desc: describeCron(value) };
    } catch (e) {
      return { ok: false as const, error: (e as Error).message };
    }
  });
  const when = (d: Date) => d.toLocaleString(intlLocale(), { weekday: 'short', hour: '2-digit', minute: '2-digit' });
</script>

<div class="field">
  <label for={id} class="label">{t('Schedule')} <span class="faint">(cron, UTC)</span></label>
  <input {id} class="mono" bind:value placeholder="*/15 * * * *" spellcheck="false" autocomplete="off" aria-describedby="{id}-hint" aria-invalid={!parsed.ok} />
  <div class="presets" role="group" aria-label={t('Schedule presets')}>
    {#each cronPresets() as p (p.expr)}
      <button type="button" class="sm" class:on={value.trim() === p.expr} onclick={() => (value = p.expr)}>{p.label}</button>
    {/each}
  </div>
  <div id="{id}-hint" class="hint" aria-live="polite">
    {#if parsed.ok}
      {#if parsed.desc}<b class="desc">{parsed.desc}.</b>{/if}
      {#if parsed.next.length}
        {#if parsed.next[1]}
          {t('Next run {when} ({time} your time), then {next}.', { when: relTime(parsed.next[0].toISOString()), time: when(parsed.next[0]), next: when(parsed.next[1]) })}
        {:else}
          {t('Next run {when} ({time} your time).', { when: relTime(parsed.next[0].toISOString()), time: when(parsed.next[0]) })}
        {/if}
      {:else}
        {t('This schedule never fires.')}
      {/if}
    {:else}
      <span class="err">{parsed.error}</span>
    {/if}
  </div>
</div>

<style>
  .presets {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
  }
  .presets .on {
    border-color: var(--accent);
    background: var(--accent-soft);
  }
  .desc {
    color: var(--text-dim);
    font-weight: 500;
  }
  .err {
    color: var(--red);
  }
</style>
