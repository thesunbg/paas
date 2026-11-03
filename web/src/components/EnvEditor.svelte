<script lang="ts">
  // Key/value rows with masking, add/remove and "paste .env" bulk import.
  import { parseDotEnv, mergeRows, type EnvRow } from '../lib/env';
  import Icon from './Icon.svelte';
  import { t, tn } from '../lib/i18n.svelte';

  let { rows = $bindable([]), masked = false, disabled = false }: { rows: EnvRow[]; masked?: boolean; disabled?: boolean } = $props();

  let pasteOpen = $state(false);
  let pasteText = $state('');
  let revealed = $state<Record<number, boolean>>({});
  let revealAll = $state(false);

  function add() {
    rows.push({ key: '', value: '' });
  }
  function remove(i: number) {
    rows.splice(i, 1);
  }
  function importPaste() {
    const parsed = parseDotEnv(pasteText);
    rows = mergeRows(rows, parsed);
    pasteText = '';
    pasteOpen = false;
  }
  function onKeyPaste(e: ClipboardEvent, i: number) {
    // Pasting a multi-line .env into a key field imports all of it.
    const text = e.clipboardData?.getData('text') ?? '';
    if (text.includes('\n') && text.includes('=')) {
      e.preventDefault();
      const next = rows.slice();
      if (!next[i].key && !next[i].value) next.splice(i, 1);
      rows = mergeRows(next, parseDotEnv(text));
    }
  }
  const parsedCount = $derived(parseDotEnv(pasteText).length);
</script>

<div class="env">
  {#if rows.length}
    <div class="rows" role="table" aria-label={t('Environment variables')}>
      {#each rows as row, i (i)}
        <div class="r" role="row">
          <input
            class="mono k"
            placeholder="KEY"
            aria-label={t('Variable name {n}', { n: i + 1 })}
            bind:value={row.key}
            {disabled}
            spellcheck="false"
            autocomplete="off"
            onpaste={(e) => onKeyPaste(e, i)}
          />
          <input
            class="mono v"
            placeholder={t('value')}
            aria-label={row.key ? t('Value of {name}', { name: row.key }) : t('Value of variable {n}', { n: i + 1 })}
            type={masked && !revealAll && !revealed[i] ? 'password' : 'text'}
            bind:value={row.value}
            {disabled}
            spellcheck="false"
            autocomplete="off"
          />
          {#if masked}
            <button
              type="button"
              class="icon"
              aria-label={revealed[i] || revealAll ? t('Hide value') : t('Reveal value')}
              onclick={() => (revealed[i] = !revealed[i])}><Icon name={revealed[i] || revealAll ? 'eyeoff' : 'eye'} size={14} /></button
            >
          {/if}
          <button type="button" class="icon ghost" aria-label={row.key ? t('Remove {name}', { name: row.key }) : t('Remove row')} onclick={() => remove(i)} {disabled}><Icon name="trash" size={14} /></button>
        </div>
      {/each}
    </div>
  {:else}
    <p class="faint small">{t('No variables.')}</p>
  {/if}
  <div class="row">
    <button type="button" class="sm" onclick={add} {disabled}><Icon name="plus" size={13} />{t('Add variable')}</button>
    <button type="button" class="sm" onclick={() => (pasteOpen = !pasteOpen)} aria-expanded={pasteOpen} {disabled}>{t('Paste .env')}</button>
    {#if masked && rows.length}
      <button type="button" class="sm ghost" onclick={() => (revealAll = !revealAll)}>{revealAll ? t('Hide all') : t('Reveal all')}</button>
    {/if}
  </div>
  {#if pasteOpen}
    <div class="paste">
      <label class="field">
        <span>{t('Paste the contents of a .env file')}</span>
        <textarea class="mono" rows="6" bind:value={pasteText} placeholder={'DATABASE_URL=postgres://…\nNODE_ENV=production\n# comments are ignored'}></textarea>
      </label>
      <div class="row">
        <button type="button" class="sm primary" disabled={!parsedCount} onclick={importPaste}>{parsedCount ? tn(parsedCount, 'Import {n} variable', 'Import {n} variables') : t('Import variables')}</button>
        <button type="button" class="sm ghost" onclick={() => (pasteOpen = false)}>{t('Cancel')}</button>
      </div>
    </div>
  {/if}
</div>

<style>
  .env {
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .rows {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .r {
    display: flex;
    gap: 6px;
    align-items: center;
  }
  .k {
    flex: 0 0 34%;
    min-width: 0;
  }
  .v {
    flex: 1;
    min-width: 0;
  }
  .paste {
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding: 12px;
    border: 1px dashed var(--border-strong);
    border-radius: var(--radius-sm);
  }
</style>
