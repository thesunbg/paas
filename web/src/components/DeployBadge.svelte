<script lang="ts">
  import { t } from '../lib/i18n.svelte';
  let { status }: { status: string } = $props();
  const names = $derived<Record<string, string>>({
    active: t('active'),
    queued: t('queued'),
    building: t('building'),
    deploying: t('deploying'),
    failed: t('failed'),
    superseded: t('superseded'),
    cancelled: t('cancelled'),
  });
  const color: Record<string, string> = {
    active: 'green',
    queued: 'yellow',
    building: 'yellow',
    deploying: 'yellow',
    failed: 'red',
    superseded: '',
    cancelled: '',
  };
</script>

<span class="badge {color[status] ?? ''}"><span class="dot {status}" style="width:6px;height:6px;box-shadow:none"></span>{names[status] ?? status}</span>
