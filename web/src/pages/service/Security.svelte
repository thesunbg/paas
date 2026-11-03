<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type ImageScan, type ScanPolicy, type ScanMode, type ServiceDetail } from '../../lib/api';
  import { can } from '../../lib/session.svelte';
  import { poll } from '../../lib/poll';
  import { toast } from '../../lib/toast.svelte';
  import { t } from '../../lib/i18n.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import Loading from '../../components/Loading.svelte';
  import ScanPanel from '../../components/ScanPanel.svelte';

  let { detail }: { detail: ServiceDetail } = $props();

  let scan = $state<ImageScan | null>(null);
  let policy = $state<ScanPolicy | null>(null);
  let loaded = $state(false);
  let error = $state('');
  let mode = $state<ScanMode>('warn');
  let threshold = $state<ScanPolicy['threshold']>('CRITICAL');
  let saving = $state(false);

  const isAdmin = $derived(can('admin', detail.project.team_id));
  const dirty = $derived(!!policy && (policy.mode !== mode || policy.threshold !== threshold));

  async function load() {
    try {
      const r = await api.serviceScan(detail.service.id);
      // Keep an edit in progress when the poll comes back.
      if (!policy || !dirty) {
        mode = r.policy.mode;
        threshold = r.policy.threshold;
      }
      scan = r.scan;
      policy = r.policy;
      error = '';
    } catch (e) {
      error = errorMessage(e);
    } finally {
      loaded = true;
    }
  }
  onMount(() => {
    load();
    return poll(load, 15000);
  });

  async function save(e: SubmitEvent) {
    e.preventDefault();
    saving = true;
    try {
      policy = await api.setScanPolicy(detail.project.id, { mode, threshold });
      toast.success(t('Scan policy saved'));
    } catch (err) {
      toast.error(errorMessage(err));
    } finally {
      saving = false;
    }
  }

  const modeHelp: Record<ScanMode, string> = $derived({
    off: t('Images are not scanned.'),
    warn: t('Every deployment is scanned and deploys; findings at or above the threshold are recorded and notified (deploy.vulnerable).'),
    block: t('A deployment with findings at or above the threshold fails before it goes live. Without a scanner on the build node, deployments fail too.'),
  });
</script>

<div class="stack">
  <form class="card" onsubmit={save}>
    <div class="card-head">
      <h3>{t('Vulnerability scan policy')}</h3>
      <span class="small faint">{t('Applies to every service of project {name}', { name: detail.project.name })}</span>
    </div>
    <div class="card-body stack">
      <div class="grid-2">
        <label class="field">
          <span>{t('Mode')}</span>
          <select bind:value={mode} disabled={!isAdmin}>
            <option value="off">{t('Off')}</option>
            <option value="warn">{t('Warn')}</option>
            <option value="block">{t('Block')}</option>
          </select>
        </label>
        <label class="field">
          <span>{t('Threshold')}</span>
          <select bind:value={threshold} disabled={!isAdmin || mode === 'off'}>
            <option value="CRITICAL">{t('Critical')}</option>
            <option value="HIGH">{t('High or worse')}</option>
            <option value="MEDIUM">{t('Medium or worse')}</option>
            <option value="LOW">{t('Low or worse')}</option>
          </select>
        </label>
      </div>
      <p class="small muted">{modeHelp[mode]}</p>
      {#if isAdmin}
        <div class="row"><button class="primary sm" type="submit" disabled={!dirty || saving}>{saving ? t('Saving…') : t('Save policy')}</button></div>
      {:else}
        <p class="small faint">{t('Team admins can change the policy.')}</p>
      {/if}
    </div>
  </form>

  <div class="card">
    <div class="card-head"><h3>{t('Latest scan')}</h3></div>
    <div class="card-body">
      {#if !loaded}
        <Loading rows={3} />
      {:else if error}
        <ErrorMsg {error} onretry={load} />
      {:else if scan}
        <ScanPanel {scan} />
      {:else if detail.service.type === 'static'}
        <p class="muted small">{t('Static sites have no image to scan.')}</p>
      {:else}
        <p class="muted small">{policy?.mode === 'off' ? t("No scan yet. The next deployment's image is scanned on the build node once the policy is not off.") : t("No scan yet. The next deployment's image is scanned on the build node.")}</p>
      {/if}
    </div>
  </div>
</div>
