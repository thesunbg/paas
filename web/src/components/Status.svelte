<script lang="ts">
  // Status dot plus label, colour-coded by status name.
  let { status, label }: { status: string; label?: string } = $props();
</script>

<span class="st"><span class="dot {status}" aria-hidden="true"></span><span>{label ?? status}</span></span>

<style>
  .st {
    display: inline-flex;
    align-items: center;
    gap: 8px;
    font-size: 13px;
    text-transform: capitalize;
    white-space: nowrap;
  }
</style>
