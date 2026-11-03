<script lang="ts">
  // Status dot plus label, colour-coded by status name.
  import { t } from '../lib/i18n.svelte';
  let { status, label }: { status: string; label?: string } = $props();
  // Known status names (services, deployments, databases, nodes, backups), also when passed
  // as the label; any other label or status shows as is.
  const names = $derived<Record<string, string>>({
    created: t('Created'),
    running: t('Running'),
    sleeping: t('Sleeping'),
    stopped: t('Stopped'),
    failed: t('Failed'),
    active: t('Active'),
    ready: t('Ready'),
    online: t('Online'),
    offline: t('Offline'),
    lost: t('Lost'),
    completed: t('Completed'),
    deploying: t('Deploying'),
    building: t('Building'),
    queued: t('Queued'),
    provisioning: t('Provisioning'),
    upgrading: t('Upgrading'),
    importing: t('Importing'),
    starting: t('Starting'),
    'running-backup': t('Running backup'),
    superseded: t('Superseded'),
    cancelled: t('Cancelled'),
    cordoned: t('Cordoned'),
    deleting: t('Deleting'),
  });
</script>

<span class="st"><span class="dot {status}" aria-hidden="true"></span><span>{names[label ?? status] ?? label ?? status}</span></span>

<style>
  .st {
    display: inline-flex;
    align-items: center;
    gap: 8px;
    font-size: 13px;
    text-transform: capitalize;
    white-space: nowrap;
  }
  /* Vietnamese labels are already in sentence case; capitalize would capitalise every word. */
  :global(:root:lang(vi)) .st {
    text-transform: none;
  }
</style>
