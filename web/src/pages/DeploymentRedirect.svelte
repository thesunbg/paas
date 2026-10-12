<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage } from '../lib/api';
  import { navigate } from '../lib/router.svelte';
  import ErrorMsg from '../components/ErrorMsg.svelte';
  import Loading from '../components/Loading.svelte';

  let { id }: { id: string } = $props();
  let error = $state('');
  onMount(async () => {
    try {
      const d = await api.deployment(id);
      navigate(`/services/${d.service.id}/deployments/${id}`, true);
    } catch (e) {
      error = errorMessage(e);
    }
  });
</script>

<div class="page">
  {#if error}<ErrorMsg {error} />{:else}<Loading />{/if}
</div>
