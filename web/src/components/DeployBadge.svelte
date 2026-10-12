<script lang="ts">
  let { status }: { status: string } = $props();
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

<span class="badge {color[status] ?? ''}"><span class="dot {status}" style="width:6px;height:6px;box-shadow:none"></span>{status}</span>
