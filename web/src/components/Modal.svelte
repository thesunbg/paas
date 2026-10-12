<script lang="ts">
  import type { Snippet } from 'svelte';
  import Icon from './Icon.svelte';

  let {
    open = $bindable(false),
    title,
    width = 520,
    onclose,
    children,
    footer,
  }: {
    open: boolean;
    title: string;
    width?: number;
    onclose?: () => void;
    children: Snippet;
    footer?: Snippet;
  } = $props();

  let panel: HTMLDivElement | undefined = $state();
  let previouslyFocused: Element | null = null;
  const titleId = 'm' + Math.random().toString(36).slice(2, 8);

  function close() {
    open = false;
    onclose?.();
  }

  function onkeydown(e: KeyboardEvent) {
    if (!open) return;
    if (e.key === 'Escape') {
      e.stopPropagation();
      close();
    } else if (e.key === 'Tab' && panel) {
      const items = panel.querySelectorAll<HTMLElement>('button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])');
      const f = Array.from(items).filter((el) => !el.hasAttribute('disabled'));
      if (!f.length) return;
      const first = f[0];
      const last = f[f.length - 1];
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first.focus();
      }
    }
  }

  $effect(() => {
    if (open) {
      previouslyFocused = document.activeElement;
      queueMicrotask(() => {
        const el = panel?.querySelector<HTMLElement>('[autofocus], input:not([type=hidden]), select, textarea, button.primary');
        (el ?? panel)?.focus();
      });
      return () => {
        if (previouslyFocused instanceof HTMLElement) previouslyFocused.focus();
      };
    }
  });
</script>

<svelte:window {onkeydown} />

{#if open}
  <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
  <div class="backdrop" onclick={(e) => e.target === e.currentTarget && close()}>
    <div class="panel" role="dialog" aria-modal="true" aria-labelledby={titleId} tabindex="-1" bind:this={panel} style:max-width="{width}px">
      <div class="head">
        <h2 id={titleId}>{title}</h2>
        <button class="ghost icon sm" aria-label="Close dialog" onclick={close}><Icon name="x" /></button>
      </div>
      <div class="body">
        {@render children()}
      </div>
      {#if footer}
        <div class="foot">{@render footer()}</div>
      {/if}
    </div>
  </div>
{/if}

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    background: rgba(0, 0, 0, 0.6);
    backdrop-filter: blur(2px);
    z-index: 100;
    display: flex;
    align-items: flex-start;
    justify-content: center;
    padding: 8vh 16px 16px;
    overflow-y: auto;
    animation: fade 0.12s ease-out;
  }
  .panel {
    width: 100%;
    background: var(--bg-elev);
    border: 1px solid var(--border-strong);
    border-radius: 10px;
    box-shadow: var(--shadow);
    outline: none;
    animation: rise 0.14s ease-out;
  }
  .head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: 14px 16px;
    border-bottom: 1px solid var(--border);
  }
  .body {
    padding: 16px;
  }
  .foot {
    display: flex;
    justify-content: flex-end;
    gap: 8px;
    padding: 12px 16px;
    border-top: 1px solid var(--border);
    background: var(--bg-elev-2);
    border-radius: 0 0 10px 10px;
  }
  @keyframes fade {
    from {
      opacity: 0;
    }
  }
  @keyframes rise {
    from {
      transform: translateY(6px);
      opacity: 0;
    }
  }
</style>
