<script lang="ts">
  import { t } from '../lib/i18n.svelte';
  // Autoscaling editor: enabled, min/max replicas, CPU and request-rate targets.
  export interface AutoscaleForm {
    enabled: boolean;
    min: number;
    max: number;
    target_cpu_percent: number;
    target_rps: number;
  }
  let { value = $bindable(), disabled = false }: { value: AutoscaleForm; disabled?: boolean } = $props();
</script>

<div class="box">
  <label class="check"><input type="checkbox" bind:checked={value.enabled} {disabled} />{t('Autoscaling')}</label>
  <p class="hint">{t('Adds or removes replicas between min and max to hold the targets. Sleep-when-idle is off while autoscaling.')}</p>
  {#if value.enabled}
    <div class="grid-4">
      <label class="field"><span>{t('Min replicas')}</span><input type="number" min="1" max="50" bind:value={value.min} {disabled} /></label>
      <label class="field"><span>{t('Max replicas')}</span><input type="number" min="1" max="50" bind:value={value.max} {disabled} /></label>
      <label class="field"><span>{t('Target CPU %')}</span><input type="number" min="0" max="100" bind:value={value.target_cpu_percent} placeholder="70" {disabled} /></label>
      <label class="field"><span>{t('Target req/s per replica')}</span><input type="number" min="0" bind:value={value.target_rps} placeholder={t('off')} {disabled} /></label>
    </div>
    <p class="hint">{t('CPU is measured against the CPU request. Set either target to 0 to ignore it.')}</p>
  {/if}
</div>

<style>
  .box {
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 12px;
    background: var(--bg-elev-2);
    display: flex;
    flex-direction: column;
    gap: 6px;
  }
  .grid-4 {
    display: grid;
    grid-template-columns: repeat(4, minmax(0, 1fr));
    gap: 10px;
    margin-top: 6px;
  }
  @media (max-width: 680px) {
    .grid-4 {
      grid-template-columns: 1fr 1fr;
    }
  }
</style>
