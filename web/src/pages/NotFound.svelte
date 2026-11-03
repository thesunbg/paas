<script lang="ts">
  import { href } from '../lib/router.svelte';
  import { t } from '../lib/i18n.svelte';
</script>

<div class="page">
  <div class="card empty">
    <h2>{t('Page not found')}</h2>
    <p>{t('The page you are looking for does not exist.')}</p>
    <a class="btn" href={href('/')}>{t('Back to projects')}</a>
  </div>
</div>
