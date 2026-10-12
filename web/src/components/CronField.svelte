<script lang="ts">
  // Cron schedule input with presets and a live "next runs" hint.
  import { parseCron, nextRuns, describeCron, cronPresets } from '../lib/cron';
  import { relTime } from '../lib/format';

  let { value = $bindable('*/15 * * * *'), id = 'cron-' + Math.random().toString(36).slice(2, 7) }: { value: string; id?: string } = $props();

  const parsed = $derived.by(() => {
    try {
      const s = parseCron(value);
      return { ok: true as const, next: nextRuns(s, 3), desc: describeCron(value) };
    } catch (e) {
      return { ok: false as const, error: (e as Error).message };
    }
  });
</script>

<div class="field">
  <label for={id} class="label">Schedule <span class="faint">(cron, UTC)</span></label>
  <input {id} class="mono" bind:value placeholder="*/15 * * * *" spellcheck="false" autocomplete="off" aria-describedby="{id}-hint" aria-invalid={!parsed.ok} />
  <div class="presets" role="group" aria-label="Schedule presets">
    {#each cronPresets as p (p.expr)}
      <button type="button" class="sm" class:on={value.trim() === p.expr} onclick={() => (value = p.expr)}>{p.label}</button>
    {/each}
  </div>
  <div id="{id}-hint" class="hint" aria-live="polite">
    {#if parsed.ok}
      {#if parsed.desc}<b class="desc">{parsed.desc}.</b>{/if}
      {#if parsed.next.length}
        Next run {relTime(parsed.next[0].toISOString())} ({parsed.next[0].toLocaleString([], { weekday: 'short', hour: '2-digit', minute: '2-digit' })} your time){#if parsed.next[1]}, then {parsed.next[1].toLocaleString([], { weekday: 'short', hour: '2-digit', minute: '2-digit' })}{/if}.
      {:else}
        This schedule never fires.
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
