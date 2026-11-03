<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type SystemSettings } from '../../lib/api';
  import { toast } from '../../lib/toast.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import Loading from '../../components/Loading.svelte';
  import CopyField from '../../components/CopyField.svelte';
  import Updates from './Updates.svelte';
  import { t } from '../../lib/i18n.svelte';

  let settings = $state<SystemSettings | null>(null);
  let error = $state('');
  let busy = $state(false);
  const signupURL = `${location.origin}/#/signup`;

  onMount(async () => {
    try {
      settings = await api.systemSettings();
    } catch (e) {
      error = errorMessage(e);
    }
  });

  async function save(e: SubmitEvent) {
    e.preventDefault();
    if (!settings) return;
    busy = true;
    error = '';
    try {
      settings = await api.putSystemSettings({ allow_signup: settings.allow_signup, limits: settings.limits, quota: settings.quota });
      toast.success(t('Settings saved'));
    } catch (err) {
      error = errorMessage(err);
    } finally {
      busy = false;
    }
  }

  const fields: { key: keyof SystemSettings['limits']; label: string; hint: string }[] = $derived([
    { key: 'max_projects', label: t('Projects per team'), hint: '' },
    { key: 'max_services', label: t('Services per team'), hint: t('PR previews are not counted') },
    { key: 'max_databases', label: t('Databases per team'), hint: '' },
    { key: 'max_memory_mb', label: t('Memory per instance (MB)'), hint: '' },
    { key: 'max_replicas', label: t('Replicas per service'), hint: t('Including autoscaling') },
    { key: 'max_teams', label: t('Teams per user'), hint: t('Owned teams, including the personal one') },
    { key: 'max_signups_per_ip_per_day', label: t('Sign-ups per IP per day'), hint: t('Against spam') },
  ]);
  const quotaFields: { key: keyof SystemSettings['quota']; label: string; hint: string }[] = $derived([
    { key: 'max_projects', label: t('Projects'), hint: '' },
    { key: 'max_services', label: t('Services'), hint: '' },
    { key: 'max_databases', label: t('Databases'), hint: '' },
    { key: 'max_memory_mb', label: t('Memory reserved (MB)'), hint: t('Sum of requests, as the scheduler counts') },
    { key: 'max_memory_limit_mb', label: t('Memory limits (MB)'), hint: t('Sum of limits: the worst case') },
    { key: 'max_cpu_millis', label: t('CPU reserved (millicores)'), hint: '1000 = 1 CPU' },
    { key: 'max_disk_mb', label: t('Database storage (MB)'), hint: '' },
    { key: 'max_previews', label: t('Preview deployments'), hint: '' },
    { key: 'max_domains', label: t('Custom domains'), hint: '' },
  ]);
</script>

<Updates />

{#if !settings}
  {#if error}<ErrorMsg {error} />{:else}<Loading />{/if}
{:else}
  <form class="card" onsubmit={save}>
    <div class="card-head">
      <div>
        <h3>{t('Sign-up and limits')}</h3>
        <p class="hint">{t('Applies to everyone except administrators.')}</p>
      </div>
    </div>
    <div class="card-body stack-lg">
      <div class="stack">
        <label class="check"><input type="checkbox" bind:checked={settings.allow_signup} />{t('Allow anyone to create an account')}</label>
        <p class="hint">{t('New users get their own team and can deploy within the limits below. Turn it off to accept invited members only.')}</p>
        {#if settings.allow_signup}
          <CopyField value={signupURL} label={t('Sign-up link')} />
        {/if}
      </div>
      <div class="grid-3">
        {#each fields as f (f.key)}
          <label class="field">
            <span>{f.label}</span>
            <input type="number" min="0" bind:value={settings.limits[f.key]} />
            <span class="hint">{f.hint ? f.hint + ' · ' : ''}{t('0 = unlimited')}</span>
          </label>
        {/each}
      </div>
      <div class="stack">
        <h4>{t('Default team quota')}</h4>
        <p class="hint">{t("Applies to every team, administrators' changes included (bulk installs too). Override it per team under Settings → Team usage.")}</p>
      </div>
      <div class="grid-3">
        {#each quotaFields as f (f.key)}
          <label class="field">
            <span>{f.label}</span>
            <input type="number" min="0" bind:value={settings.quota[f.key]} />
            <span class="hint">{f.hint ? f.hint + ' · ' : ''}{t('0 = unlimited')}</span>
          </label>
        {/each}
      </div>
      <ErrorMsg {error} />
      <div><button class="primary" type="submit" disabled={busy}>{#if busy}<span class="spinner"></span>{/if}{t('Save')}</button></div>
    </div>
  </form>
{/if}
