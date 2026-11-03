<script lang="ts">
  import Modal from './Modal.svelte';
  import { confirmState, settleConfirm } from '../lib/confirm.svelte';
  import { t } from '../lib/i18n.svelte';

  let typed = $state('');
  let open = $state(false);
  const c = $derived(confirmState.current);

  $effect(() => {
    open = !!c;
    typed = '';
  });

  const ok = $derived(!c?.requireText || typed === c.requireText);
</script>

{#if c}
  <Modal bind:open title={c.title} width={460} onclose={() => settleConfirm(false)}>
    <form
      id="confirm-form"
      class="stack"
      onsubmit={(e) => {
        e.preventDefault();
        if (ok) settleConfirm(true);
      }}
    >
      <p class="muted">{c.message}</p>
      {#if c.requireText}
        <label class="field">
          <span>{t('To confirm, type')} <code>{c.requireText}</code></span>
          <input bind:value={typed} autocomplete="off" spellcheck="false" />
        </label>
      {/if}
    </form>
    {#snippet footer()}
      <button type="button" onclick={() => settleConfirm(false)}>{t('Cancel')}</button>
      <button type="submit" form="confirm-form" class={c.danger ? 'danger solid' : 'primary'} disabled={!ok}>{c.confirmLabel ?? t('Confirm')}</button>
    {/snippet}
  </Modal>
{/if}
