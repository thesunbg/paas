<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type SystemSettings } from '../../lib/api';
  import { toast } from '../../lib/toast.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import Loading from '../../components/Loading.svelte';
  import CopyField from '../../components/CopyField.svelte';

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
      settings = await api.putSystemSettings({ allow_signup: settings.allow_signup, limits: settings.limits });
      toast.success('Settings saved');
    } catch (err) {
      error = errorMessage(err);
    } finally {
      busy = false;
    }
  }

  const fields: { key: keyof SystemSettings['limits']; label: string; hint: string }[] = [
    { key: 'max_projects', label: 'Projects per team', hint: '' },
    { key: 'max_services', label: 'Services per team', hint: 'PR previews are not counted' },
    { key: 'max_databases', label: 'Databases per team', hint: '' },
    { key: 'max_memory_mb', label: 'Memory per instance (MB)', hint: '' },
    { key: 'max_replicas', label: 'Replicas per service', hint: 'Including autoscaling' },
    { key: 'max_teams', label: 'Teams per user', hint: 'Owned teams, including the personal one' },
    { key: 'max_signups_per_ip_per_day', label: 'Sign-ups per IP per day', hint: 'Against spam' },
  ];
</script>

{#if !settings}
  {#if error}<ErrorMsg {error} />{:else}<Loading />{/if}
{:else}
  <form class="card" onsubmit={save}>
    <div class="card-head">
      <div>
        <h3>Sign-up and limits</h3>
        <p class="hint">Applies to everyone except administrators.</p>
      </div>
    </div>
    <div class="card-body stack-lg">
      <div class="stack">
        <label class="check"><input type="checkbox" bind:checked={settings.allow_signup} />Allow anyone to create an account</label>
        <p class="hint">New users get their own team and can deploy within the limits below. Turn it off to accept invited members only.</p>
        {#if settings.allow_signup}
          <CopyField value={signupURL} label="Sign-up link" />
        {/if}
      </div>
      <div class="grid-3">
        {#each fields as f (f.key)}
          <label class="field">
            <span>{f.label}</span>
            <input type="number" min="0" bind:value={settings.limits[f.key]} />
            <span class="hint">{f.hint ? f.hint + ' · ' : ''}0 = unlimited</span>
          </label>
        {/each}
      </div>
      <ErrorMsg {error} />
      <div><button class="primary" type="submit" disabled={busy}>{#if busy}<span class="spinner"></span>{/if}Save</button></div>
    </div>
  </form>
{/if}
