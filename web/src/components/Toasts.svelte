<script lang="ts">
  import { toasts, dismiss } from '../lib/toast.svelte';
  import Icon from './Icon.svelte';
</script>

<div class="toasts" role="status" aria-live="polite">
  {#each toasts as t (t.id)}
    <div class="toast {t.kind}">
      <span class="ic"><Icon name={t.kind === 'error' ? 'x' : t.kind === 'success' ? 'check' : 'activity'} size={14} /></span>
      <span class="txt">{t.text}</span>
      <button class="ghost icon sm" aria-label="Dismiss" onclick={() => dismiss(t.id)}><Icon name="x" size={12} /></button>
    </div>
  {/each}
</div>

<style>
  .toasts {
    position: fixed;
    right: 16px;
    bottom: 16px;
    z-index: 200;
    display: flex;
    flex-direction: column;
    gap: 8px;
    max-width: min(380px, calc(100vw - 32px));
  }
  .toast {
    display: flex;
    align-items: center;
    gap: 10px;
    background: var(--bg-elev-2);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    padding: 8px 8px 8px 12px;
    box-shadow: var(--shadow);
    font-size: 13px;
    animation: slide 0.16s ease-out;
  }
  .txt {
    flex: 1;
    overflow-wrap: anywhere;
  }
  .ic {
    display: flex;
    width: 20px;
    height: 20px;
    border-radius: 50%;
    align-items: center;
    justify-content: center;
    flex: none;
  }
  .success .ic {
    background: var(--green-soft);
    color: var(--green);
  }
  .error .ic {
    background: var(--red-soft);
    color: var(--red);
  }
  .info .ic {
    background: var(--accent-soft);
    color: var(--accent);
  }
  @keyframes slide {
    from {
      transform: translateY(8px);
      opacity: 0;
    }
  }
</style>
