<script lang="ts">
  import { api, errorMessage, type Channel, type ChannelKind } from '../../lib/api';
  import { session, currentTeam, can } from '../../lib/session.svelte';
  import { relTime } from '../../lib/format';
  import { toast } from '../../lib/toast.svelte';
  import { confirmDialog } from '../../lib/confirm.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import Loading from '../../components/Loading.svelte';
  import Icon from '../../components/Icon.svelte';
  import { t } from '../../lib/i18n.svelte';

  let channels = $state<Channel[] | null>(null);
  let events = $state<string[]>([]);
  let error = $state('');
  let busy = $state('');

  let name = $state('');
  let kind = $state<ChannelKind>('slack');
  let target = $state('');
  let picked = $state<string[]>(['deploy.failed', 'deploy.vulnerable', 'instance.crashloop', 'node.offline', 'backup.failed', 'job.failed']);
  let formError = $state('');

  const team = $derived(currentTeam());
  const isAdmin = $derived(team ? can('admin', team.id) : false);

  async function load() {
    const tm = team;
    if (!tm) return;
    try {
      const r = await api.channels(tm.id);
      channels = r.channels ?? [];
      events = r.events ?? [];
      error = '';
    } catch (e) {
      error = errorMessage(e);
    }
  }
  $effect(() => {
    void session.teamId;
    channels = null;
    load();
  });

  // The label is also the default channel name sent to the API: kept in English.
  const kinds: { id: ChannelKind; label: string; placeholder: string; hint: string }[] = $derived([
    { id: 'slack', label: 'Slack', placeholder: 'https://hooks.slack.com/services/…', hint: t('An incoming-webhook URL.') },
    { id: 'discord', label: 'Discord', placeholder: 'https://discord.com/api/webhooks/…', hint: t('Channel settings → Integrations → Webhooks.') },
    { id: 'telegram', label: 'Telegram', placeholder: '123456:ABC-DEF…:-1001234567890', hint: t('Format: <bot token>:<chat id>.') },
    { id: 'webhook', label: 'Webhook', placeholder: 'https://example.com/hooks/paas', hint: t('Receives a JSON POST for each event.') },
  ]);
  const kindInfo = $derived(kinds.find((k) => k.id === kind)!);
  const eventLabels: Record<string, string> = $derived({
    'deploy.succeeded': t('Deploy succeeded'),
    'deploy.failed': t('Deploy failed'),
    'deploy.vulnerable': t('Vulnerabilities found in a deployed image'),
    'instance.crashloop': t('Instance crash-looping'),
    'instance.evicted': t('Instance evicted'),
    'node.offline': t('Node offline'),
    'backup.failed': t('Backup failed'),
    'job.failed': t('Cron job failed'),
    autoscale: t('Autoscaling changes'),
    'alert.firing': t('Alert firing'),
    'alert.resolved': t('Alert resolved'),
    'uptime.down': t('Uptime check down'),
    'uptime.up': t('Uptime check recovered'),
    'uptime.cert': t('TLS certificate expiring'),
    'update.available': t('New paas release available (admins)'),
    'update.failed': t('paas update stopped (admins)'),
  });

  async function add(e: SubmitEvent) {
    e.preventDefault();
    if (!team) return;
    formError = '';
    if (!picked.length) return void (formError = t('Pick at least one event'));
    busy = 'add';
    try {
      await api.createChannel(team.id, { name: name.trim() || kindInfo.label, kind, target: target.trim(), events: picked });
      toast.success(t('Channel added'));
      name = '';
      target = '';
      await load();
    } catch (err) {
      formError = errorMessage(err);
    } finally {
      busy = '';
    }
  }
  async function test(c: Channel) {
    if (!team) return;
    busy = c.id;
    try {
      await api.testChannel(team.id, c.id);
      toast.success(t('Test message sent to {name}', { name: c.name }));
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      busy = '';
    }
  }
  async function remove(c: Channel) {
    if (!team) return;
    if (!(await confirmDialog({ title: t('Delete channel'), message: t('Stop sending notifications to {name}?', { name: c.name }), confirmLabel: t('Delete'), danger: true }))) return;
    try {
      await api.deleteChannel(team.id, c.id);
      toast.success(t('Channel deleted'));
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }
</script>

<div class="card">
  <div class="card-head">
    <div>
      <h3>{t('Notifications')} · {team?.name}</h3>
      <p class="hint">{t('Send deploy, crash, node, backup and job events to chat or a webhook.')}</p>
    </div>
  </div>
  {#if error}<div class="card-body"><ErrorMsg {error} onretry={load} /></div>{/if}
  {#if channels === null && !error}
    <div class="card-body"><Loading /></div>
  {:else if channels}
    {#if channels.length === 0}
      <div class="card-body muted small">{t('No channels yet.')}</div>
    {:else}
      <div class="table-wrap">
        <table>
          <thead><tr><th>{t('Name')}</th><th>{t('Kind')}</th><th>{t('Events')}</th><th class="hide-sm">{t('Added')}</th><th class="right"></th></tr></thead>
          <tbody>
            {#each channels as c (c.id)}
              <tr>
                <td><Icon name="bell" size={13} /> {c.name}</td>
                <td><span class="badge">{c.kind}</span></td>
                <td class="evs">{#each c.events as ev (ev)}<span class="badge outline">{ev}</span>{/each}</td>
                <td class="hide-sm faint small">{relTime(c.created_at)}</td>
                <td class="right nowrap">
                  <button class="sm" onclick={() => test(c)} disabled={busy === c.id}>{#if busy === c.id}<span class="spinner"></span>{/if}{t('Test')}</button>
                  {#if isAdmin}<button class="sm danger" onclick={() => remove(c)} aria-label={t('Delete {name}', { name: c.name })}><Icon name="trash" size={12} /></button>{/if}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
    {#if isAdmin}
      <form class="card-body stack add" onsubmit={add}>
        <h3>{t('Add a channel')}</h3>
        <div class="grid-3">
          <label class="field">
            <span>{t('Kind')}</span>
            <select bind:value={kind}>{#each kinds as k (k.id)}<option value={k.id}>{k.label}</option>{/each}</select>
          </label>
          <label class="field">
            <span>{t('Name')}</span>
            <input bind:value={name} placeholder={kindInfo.label + ' #ops'} maxlength="64" />
          </label>
        </div>
        <label class="field">
          <span>{kind === 'telegram' ? t('Bot token and chat id') : t('Webhook URL')}</span>
          <input class="mono" type={kind === 'telegram' ? 'password' : 'url'} bind:value={target} required placeholder={kindInfo.placeholder} autocomplete="off" spellcheck="false" />
          <span class="hint">{kindInfo.hint} {t('Stored encrypted.')}</span>
        </label>
        <fieldset>
          <legend class="label">{t('Events')}</legend>
          <div class="events">
            {#each events as ev (ev)}
              <label class="check small"><input type="checkbox" bind:group={picked} value={ev} />{eventLabels[ev] ?? ev}</label>
            {/each}
          </div>
        </fieldset>
        <ErrorMsg error={formError} />
        <div><button class="primary" type="submit" disabled={busy === 'add' || !target.trim()}>{#if busy === 'add'}<span class="spinner"></span>{/if}{t('Add channel')}</button></div>
      </form>
    {:else}
      <div class="card-body hint">{t('Only team admins can add channels.')}</div>
    {/if}
  {/if}
</div>

<style>
  .evs {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
    max-width: 420px;
  }
  .add {
    border-top: 1px solid var(--border);
  }
  fieldset {
    border: 0;
    margin: 0;
    padding: 0;
  }
  legend {
    padding: 0;
    margin-bottom: 6px;
  }
  .events {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(200px, 1fr));
    gap: 6px 12px;
  }
</style>
