<script lang="ts">
  import { href } from '../lib/router.svelte';
</script>

<div class="page">
  <div class="card empty">
    <h2>Page not found</h2>
    <p>The page you are looking for does not exist.</p>
    <a class="btn" href={href('/')}>Back to projects</a>
  </div>
</div>
