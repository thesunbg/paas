<script lang="ts">
  // Monospace log viewer that sticks to the bottom unless the user scrolls up.
  import { tick } from 'svelte';

  let {
    lines,
    height = '460px',
    empty = 'No output yet.',
    filter = '',
  }: { lines: string[]; height?: string; empty?: string; filter?: string } = $props();

  let el: HTMLPreElement | undefined = $state();
  let stick = $state(true);

  const shown = $derived.by(() => {
    if (!filter) return lines;
    const f = filter.toLowerCase();
    return lines.filter((l) => l.toLowerCase().includes(f));
  });

  function classify(l: string): string {
    if (/\b(error|fail(ed|ure)?|fatal|panic)\b/i.test(l)) return 'err';
    if (l.startsWith('==>')) return 'step';
    if (/\b(healthy|deployment live|succeeded|successfully)\b/i.test(l)) return 'ok';
    return '';
  }

  function onscroll() {
    if (!el) return;
    stick = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
  }

  $effect(() => {
    void shown.length;
    if (stick && el) {
      tick().then(() => {
        if (el) el.scrollTop = el.scrollHeight;
      });
    }
  });

  export function scrollToBottom() {
    stick = true;
    if (el) el.scrollTop = el.scrollHeight;
  }
</script>

<div class="wrap">
  <!-- svelte-ignore a11y_no_noninteractive_tabindex (focusable so the log can be scrolled with the keyboard) -->
  <pre class="log" bind:this={el} {onscroll} style:height tabindex="0" aria-label="Log output">{#if shown.length === 0}<span class="faint">{filter && lines.length ? 'No lines match the filter.' : empty}</span>{:else}{#each shown as l, i (i)}<span class="ln {classify(l)}">{l}</span>{/each}{/if}</pre>
  {#if !stick}
    <button class="sm jump" onclick={scrollToBottom}>↓ Jump to latest</button>
  {/if}
</div>

<style>
  .wrap {
    position: relative;
  }
  .jump {
    position: absolute;
    right: 14px;
    bottom: 12px;
    box-shadow: var(--shadow);
  }
</style>
