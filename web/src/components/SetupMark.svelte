<script lang="ts">
  // Status of a setup checklist step: ok (✓), warning (!), failed (✕) or skipped (–).
  import type { SetupStatusLevel } from '../lib/api';
  import { t } from '../lib/i18n.svelte';
  let { status, skipped = false, size = 20 }: { status: SetupStatusLevel | undefined; skipped?: boolean; size?: number } = $props();
  const kind = $derived(skipped && status !== 'ok' ? 'skipped' : (status ?? 'unknown'));
  const glyph: Record<string, string> = { ok: '✓', warn: '!', fail: '✕', skipped: '–', unknown: '…' };
  const label = $derived<Record<string, string>>({ ok: t('OK'), warn: t('Needs attention'), fail: t('Failed'), skipped: t('Skipped'), unknown: t('Not checked') });
</script>

<span class="mark {kind}" style="width:{size}px;height:{size}px;font-size:{Math.round(size * 0.6)}px" role="img" aria-label={label[kind]} title={label[kind]}>{glyph[kind]}</span>

<style>
  .mark {
    flex: none;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    border-radius: 50%;
    font-weight: 700;
    line-height: 1;
    background: var(--gray-soft);
    color: var(--text-dim);
  }
  .ok {
    background: var(--green-soft);
    color: var(--green);
  }
  .warn {
    background: var(--yellow-soft);
    color: var(--yellow);
  }
  .fail {
    background: var(--red-soft);
    color: var(--red);
  }
</style>
