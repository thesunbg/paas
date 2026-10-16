<script lang="ts">
  // Variables an app expects (from its .env.example) for the user to fill in:
  // required ones first, optional ones folded away.
  import type { EnvInput } from '../lib/api';

  let { inputs, values = $bindable({}), disabled = false }: { inputs: EnvInput[]; values: Record<string, string>; disabled?: boolean } = $props();
  const required = $derived(inputs.filter((i) => i.required));
  const optional = $derived(inputs.filter((i) => !i.required));
  const filled = $derived(required.filter((i) => (values[i.key] ?? '').trim() !== '').length);
  let showOptional = $state(false);
  let shown = $state<Record<string, boolean>>({});
  const secretish = (k: string) => /PASS|SECRET|TOKEN|KEY|CREDENTIAL|PRIVATE/i.test(k);
</script>

{#snippet field(i: EnvInput)}
  <label class="f">
    <span class="k mono">{i.key}{#if i.required}<span class="req" title="Required">*</span>{/if}</span>
    <span class="in">
      <input
        class="mono"
        type={secretish(i.key) && !shown[i.key] ? 'password' : 'text'}
        bind:value={values[i.key]}
        placeholder={i.placeholder || (i.required ? 'required' : 'optional')}
        autocomplete="off"
        spellcheck="false"
        {disabled}
      />
      {#if secretish(i.key)}
        <button type="button" class="ghost sm" onclick={() => (shown[i.key] = !shown[i.key])}>{shown[i.key] ? 'Hide' : 'Show'}</button>
      {/if}
    </span>
    {#if i.hint}<span class="hint">{i.hint}</span>{/if}
  </label>
{/snippet}

<div class="inputs">
  {#if required.length}
    <div class="head small"><b>Needs your values</b><span class="faint"> · {filled}/{required.length} filled · you can also add them later</span></div>
    {#each required as i (i.key)}{@render field(i)}{/each}
  {/if}
  {#if optional.length}
    <button type="button" class="ghost sm toggle" onclick={() => (showOptional = !showOptional)} aria-expanded={showOptional}>
      {showOptional ? 'Hide' : 'Show'} {optional.length} optional variable{optional.length === 1 ? '' : 's'}
    </button>
    {#if showOptional}
      {#each optional as i (i.key)}{@render field(i)}{/each}
    {/if}
  {/if}
</div>

<style>
  .inputs {
    display: flex;
    flex-direction: column;
    gap: 10px;
    width: 100%;
  }
  .f {
    display: grid;
    grid-template-columns: minmax(140px, 220px) 1fr;
    gap: 4px 12px;
    align-items: center;
  }
  .k {
    font-size: 12.5px;
    overflow-wrap: anywhere;
  }
  .req {
    color: var(--red);
    margin-left: 2px;
  }
  .in {
    display: flex;
    gap: 6px;
    min-width: 0;
  }
  .in input {
    flex: 1;
    min-width: 0;
  }
  .hint {
    grid-column: 2;
    margin: 0;
  }
  .toggle {
    align-self: flex-start;
  }
  @media (max-width: 640px) {
    .f {
      grid-template-columns: 1fr;
    }
    .hint {
      grid-column: 1;
    }
  }
</style>
