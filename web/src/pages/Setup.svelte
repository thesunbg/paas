<script lang="ts">
  // First-run setup wizard: one page per checklist step, each with its server-side
  // check, a short how-to and the form to fix it inline where that is small.
  import { onMount } from 'svelte';
  import {
    api,
    errorMessage,
    type SetupChecklist,
    type SetupStep,
    type SetupStepId,
    type StorageSettings,
    type Template,
  } from '../lib/api';
  import { session, currentTeam } from '../lib/session.svelte';
  import { navigate, href } from '../lib/router.svelte';
  import { relTime } from '../lib/format';
  import { toast } from '../lib/toast.svelte';
  import ErrorMsg from '../components/ErrorMsg.svelte';
  import Loading from '../components/Loading.svelte';
  import CopyButton from '../components/CopyButton.svelte';
  import Icon from '../components/Icon.svelte';
  import SetupMark from '../components/SetupMark.svelte';
  import TwoFactor from './settings/TwoFactor.svelte';
  import Notifications from './settings/Notifications.svelte';
  import LangSwitch from '../components/LangSwitch.svelte';
  import { t, intlLocale } from '../lib/i18n.svelte';

  let { step }: { step?: string } = $props();

  let list = $state<SetupChecklist | null>(null);
  let error = $state('');
  let checking = $state('');
  let saving = $state(false);

  const steps = $derived(list?.steps ?? []);
  const index = $derived.by(() => {
    const i = steps.findIndex((s) => s.id === step);
    if (i >= 0) return i;
    const open = steps.findIndex((s) => !s.completed && !s.skipped);
    return open >= 0 ? open : 0;
  });
  const cur = $derived<SetupStep | undefined>(steps[index]);
  const done = $derived(steps.filter((s) => s.completed || s.skipped).length);

  async function load() {
    try {
      list = await api.setupChecklist();
      error = '';
    } catch (e) {
      error = errorMessage(e);
    }
  }
  onMount(load);

  function go(i: number) {
    const s = steps[i];
    if (s) navigate(`/setup/${s.id}`, true);
    window.scrollTo({ top: 0 });
  }

  async function recheck(id: SetupStepId) {
    if (!list) return;
    checking = id;
    try {
      const r = await api.checkSetupStep(id);
      list.steps = list.steps.map((s) => (s.id === id ? r : s));
    } catch (e) {
      toast.error(errorMessage(e));
    } finally {
      checking = '';
    }
  }

  async function save(patch: { completed?: SetupStepId[]; skipped?: SetupStepId[]; dismissed?: boolean; finished?: boolean }) {
    if (!list) return false;
    saving = true;
    try {
      const p = await api.putSetupChecklist(patch);
      list.completed = p.completed;
      list.skipped = p.skipped;
      list.dismissed = p.dismissed;
      list.finished = p.finished;
      list.steps = list.steps.map((s) => ({ ...s, completed: p.completed.includes(s.id), skipped: p.skipped.includes(s.id) }));
      if (session.me) session.me.setup_pending = p.pending;
      return true;
    } catch (e) {
      toast.error(errorMessage(e));
      return false;
    } finally {
      saving = false;
    }
  }

  // Next: a step whose check passes counts as completed.
  async function next() {
    if (!list || !cur) return;
    const id = cur.id;
    if (cur.status === 'ok' && !cur.completed) {
      if (!(await save({ completed: [...list.completed, id], skipped: list.skipped.filter((x) => x !== id) }))) return;
    }
    if (index === steps.length - 1) return finish();
    go(index + 1);
  }

  async function skipStep() {
    if (!list || !cur) return;
    const id = cur.id;
    if (!(await save({ skipped: [...list.skipped.filter((x) => x !== id), id], completed: list.completed.filter((x) => x !== id) }))) return;
    if (index === steps.length - 1) return finish();
    go(index + 1);
  }

  async function finish() {
    if (await save({ finished: true })) {
      toast.success(t('Setup finished. Re-open the checklist any time from Settings → System.'));
      navigate('/');
    }
  }

  async function skipAll() {
    if (await save({ dismissed: true })) navigate('/');
  }

  // Two-factor turned on in the embedded form: the admin step passes now.
  let adminRechecked = false; // not reactive: re-check once, never loop
  $effect(() => {
    if (session.me?.user.totp_enabled && cur?.id === 'admin' && cur.status !== 'ok' && !adminRechecked) {
      adminRechecked = true;
      recheck('admin');
    }
  });

  // --- Off-site backups (inline storage form) ---
  let storage = $state<StorageSettings | null>(null);
  let storageForm = $state({ endpoint: '', region: '', bucket: '', access_key: '', secret_key: '', prefix: '', virtual_host: false });
  let storageError = $state('');
  let storageBusy = $state(false);
  $effect(() => {
    if (cur?.id === 'backups' && !storage && !storageError) {
      api
        .storage()
        .then((s) => {
          storage = s;
          storageForm = { endpoint: s.endpoint, region: s.region, bucket: s.bucket, access_key: s.access_key, secret_key: '', prefix: s.prefix, virtual_host: s.virtual_host };
        })
        .catch((e) => (storageError = errorMessage(e)));
    }
  });
  async function saveStorage(e: SubmitEvent) {
    e.preventDefault();
    storageBusy = true;
    storageError = '';
    try {
      storage = await api.putStorage(storageForm);
      storageForm.secret_key = '';
      toast.success(t('Connection tested: upload, download and delete work. Saved.'));
      await recheck('backups');
    } catch (err) {
      storageError = errorMessage(err);
    } finally {
      storageBusy = false;
    }
  }

  // --- First app (one-click template) ---
  const suggested = ['it-tools', 'excalidraw', 'uptime-kuma', 'memos'];
  let templates = $state<Template[] | null>(null);
  let tplError = $state('');
  let deploying = $state('');
  let deployed = $state<{ project: string; title: string } | null>(null);
  const picks = $derived((templates ?? []).filter((tpl) => suggested.includes(tpl.id)).slice(0, 3));
  $effect(() => {
    if (cur?.id === 'first_app' && templates === null && !tplError) {
      api
        .templates()
        .then((list) => (templates = list ?? []))
        .catch((e) => (tplError = errorMessage(e)));
    }
  });
  async function deployTemplate(tpl: Template) {
    const team = currentTeam();
    if (!team) return;
    deploying = tpl.id;
    tplError = '';
    try {
      const projects = (await api.projects(team.id)) ?? [];
      const p = projects.find((x) => x.slug === 'getting-started') ?? (await api.createProject(team.id, 'getting-started'));
      await api.installTemplate(p.id, tpl.id);
      deployed = { project: p.id, title: tpl.title };
      toast.success(t('{name} is deploying', { name: tpl.title }));
    } catch (e) {
      tplError = errorMessage(e);
    } finally {
      deploying = '';
    }
  }

  const infoList = (v: unknown): string[] => (Array.isArray(v) ? v.map(String) : []);
  const stepIcon: Record<string, string> = {
    admin: 'user',
    dns: 'globe',
    https: 'shield',
    ports: 'server',
    github: 'git',
    backups: 'database',
    notifications: 'bell',
    first_app: 'rocket',
  };
</script>

<div class="setup">
  <header class="top">
    <span class="logo" aria-hidden="true"><svg viewBox="0 0 16 16" width="14" height="14"><path d="M2 13 8 2.5 14 13z" fill="currentColor" /></svg></span>
    <div class="title">
      <b>{t('Set up your platform')}</b>
      {#if list}<span class="faint small">{t('{done} of {total} done', { done, total: steps.length })}</span>{/if}
    </div>
    <span class="spacer"></span>
    <LangSwitch />
    <button class="ghost sm" onclick={skipAll} disabled={saving}>{t('Skip setup')}</button>
  </header>

  {#if !list}
    <div class="page">{#if error}<ErrorMsg {error} onretry={load} />{:else}<Loading label={t('Running checks…')} />{/if}</div>
  {:else}
    <div class="body">
      <nav class="rail" aria-label={t('Setup steps')}>
        <ol>
          {#each steps as s, i (s.id)}
            <li>
              <button type="button" class:on={i === index} aria-current={i === index ? 'step' : undefined} onclick={() => go(i)}>
                <SetupMark status={s.status} skipped={s.skipped} size={18} />
                <span class="label">{i + 1}. {s.title}</span>
              </button>
            </li>
          {/each}
        </ol>
      </nav>

      {#if cur}
        <main class="panel stack-lg">
          <div class="head">
            <span class="icon"><Icon name={stepIcon[cur.id] ?? 'check'} size={18} /></span>
            <div class="grow">
              <p class="faint small">{t('Step {n} of {total}', { n: index + 1, total: steps.length })}</p>
              <h2>{cur.title}</h2>
            </div>
          </div>

          <div class="card result {cur.status}">
            <div class="card-body stack">
              <div class="row nowrap top-align">
                <SetupMark status={cur.status} />
                <div class="grow stack-sm">
                  <b>{cur.summary}</b>
                  {#if cur.details?.length}
                    <ul class="details">
                      {#each cur.details as d, i (i)}<li>{d}</li>{/each}
                    </ul>
                  {/if}
                </div>
              </div>
              <div class="between">
                <span class="faint small">{cur.skipped ? t('Checked {time} · skipped', { time: relTime(cur.checked_at) }) : cur.completed ? t('Checked {time} · done', { time: relTime(cur.checked_at) }) : t('Checked {time}', { time: relTime(cur.checked_at) })}</span>
                <button class="sm" onclick={() => recheck(cur.id)} disabled={checking === cur.id}>
                  {#if checking === cur.id}<span class="spinner"></span>{:else}<Icon name="refresh" size={13} />{/if}{t('Check again')}
                </button>
              </div>
            </div>
          </div>

          <p class="muted">{cur.howto}</p>

          {#if cur.id === 'admin'}
            <TwoFactor />
          {:else if cur.id === 'dns'}
            <dl class="kv">
              <dt>{t('Base domain')}</dt><dd class="mono">{String(cur.info?.base_domain ?? '')}</dd>
              <dt>{t('Dashboard')}</dt><dd class="mono">{String(cur.info?.dashboard_domain ?? '')}</dd>
              <dt>{t('Server IPs')}</dt><dd class="mono">{infoList(cur.info?.server_ips).join(', ') || t('unknown (set PAAS_PUBLIC_IPS)')}</dd>
            </dl>
            {#if cur.records?.length}
              <div class="card">
                <div class="card-head"><h3>{t('Records to create')}</h3></div>
                <div class="table-wrap">
                  <table>
                    <thead><tr><th>{t('Name')}</th><th>{t('Type')}</th><th>{t('Value')}</th><th></th></tr></thead>
                    <tbody>
                      {#each cur.records as r (r.name + r.type + r.value)}
                        <tr>
                          <td class="mono break">{r.name}</td>
                          <td>{r.type}</td>
                          <td class="mono break">{r.value}</td>
                          <td class="right"><CopyButton text={r.value} /></td>
                        </tr>
                      {/each}
                    </tbody>
                  </table>
                </div>
              </div>
            {/if}
          {:else if cur.id === 'https'}
            {#if cur.info?.issuer}
              <dl class="kv">
                <dt>{t('Issuer')}</dt><dd>{String(cur.info.issuer)}</dd>
                {#if cur.info.not_after}<dt>{t('Valid until')}</dt><dd>{new Date(String(cur.info.not_after)).toLocaleDateString(intlLocale())}</dd>{/if}
              </dl>
            {/if}
            <p class="hint">{t("Let's Encrypt connects to")} <code>http://{String(cur.info?.host ?? t('your domain'))}/.well-known/acme-challenge/…</code>{t(': keep port 80 open even when everything is served over HTTPS.')}</p>
          {:else if cur.id === 'ports'}
            <div class="card">
              <div class="table-wrap">
                <table>
                  <thead><tr><th>{t('Port')}</th><th>{t('Protocol')}</th><th>{t('Open to')}</th><th>{t('Used for')}</th></tr></thead>
                  <tbody>
                    <tr><td>80</td><td>TCP</td><td>{t('Internet')}</td><td>{t('HTTP, certificate challenges')}</td></tr>
                    <tr><td>443</td><td>TCP</td><td>{t('Internet')}</td><td>{t('HTTPS (apps and dashboard)')}</td></tr>
                    <tr><td>51820</td><td>UDP</td><td>{t('Other nodes')}</td><td>{t('WireGuard mesh (multi-node)')}</td></tr>
                    <tr><td>4222</td><td>TCP</td><td>{t('Other nodes')}</td><td>{t('Control bus (multi-node)')}</td></tr>
                  </tbody>
                </table>
              </div>
            </div>
          {:else if cur.id === 'github'}
            <div class="row wrap">
              <a class="btn primary" href={href('/settings/github')}><Icon name="git" size={14} />{cur.info?.configured ? t('Manage the GitHub App') : t('Create the GitHub App')}</a>
              <a class="btn" href={href('/settings/git')}>GitLab, Gitea, Bitbucket…</a>
            </div>
            <p class="hint">{t('Creating the app takes you to GitHub and back to Settings → GitHub. Come back here from Settings → System → Setup checklist.')}</p>
          {:else if cur.id === 'backups'}
            {#if !storage}
              {#if storageError}<ErrorMsg error={storageError} />{:else}<Loading />{/if}
            {:else if storage.from_env}
              <p>{t('Configured by')} <code>PAAS_S3_*</code> {t('on the server:')} <code>{storage.endpoint}</code>{t(', bucket')} <code>{storage.bucket}</code>.</p>
            {:else}
              <form class="card" onsubmit={saveStorage}>
                <div class="card-body stack">
                  <div class="grid-2">
                    <label class="field"><span>{t('Endpoint')}</span><input bind:value={storageForm.endpoint} placeholder="https://s3.eu-central-1.amazonaws.com" /></label>
                    <label class="field"><span>{t('Bucket')}</span><input bind:value={storageForm.bucket} placeholder="paas-backups" /></label>
                    <label class="field"><span>{t('Access key')}</span><input bind:value={storageForm.access_key} autocomplete="off" /></label>
                    <label class="field">
                      <span>{t('Secret key')}</span>
                      <input type="password" bind:value={storageForm.secret_key} autocomplete="new-password" placeholder={storage.has_secret ? t('unchanged') : ''} />
                    </label>
                    <label class="field"><span>{t('Region')}</span><input bind:value={storageForm.region} placeholder="auto" /></label>
                    <label class="field"><span>{t('Prefix')}</span><input bind:value={storageForm.prefix} placeholder="paas-backups" /></label>
                  </div>
                  <label class="check"><input type="checkbox" bind:checked={storageForm.virtual_host} />{t('Virtual-hosted style URLs (bucket.endpoint)')}</label>
                  <p class="hint">{t('Testing writes, reads back and deletes a small object; nothing is saved if that fails. More options under Settings → Backups.')}</p>
                  <ErrorMsg error={storageError} />
                  <div>
                    <button class="primary" type="submit" disabled={storageBusy || !storageForm.endpoint || !storageForm.bucket}>
                      {#if storageBusy}<span class="spinner"></span>{/if}{t('Test connection and save')}
                    </button>
                  </div>
                </div>
              </form>
            {/if}
          {:else if cur.id === 'notifications'}
            <Notifications />
            <p class="hint">{t("Add a channel, press its test button, then check again. Cluster events (offline nodes, failed backups) go to administrators' teams.")}</p>
          {:else if cur.id === 'first_app'}
            {#if deployed}
              <div class="alert success">
                <Icon name="rocket" />
                <span>{t('{name} is deploying.', { name: deployed.title })} <a href={href(`/projects/${deployed.project}`)}>{t('Watch it in the project')}</a> {t('and open its URL when it is running: that proves builds, routing, DNS and certificates work.')}</span>
              </div>
            {/if}
            {#if tplError}<ErrorMsg error={tplError} />{/if}
            {#if templates === null && !tplError}
              <Loading />
            {:else}
              <div class="tpls">
                {#each picks as tpl (tpl.id)}
                  <div class="card card-pad stack-sm">
                    <b>{tpl.title}</b>
                    <span class="muted small">{tpl.description}</span>
                    <div>
                      <button class="sm primary" onclick={() => deployTemplate(tpl)} disabled={!!deploying}>
                        {#if deploying === tpl.id}<span class="spinner"></span>{/if}{t('Deploy')}
                      </button>
                    </div>
                  </div>
                {/each}
              </div>
              <p class="hint">{t('Templates go into a project named')} <code>getting-started</code>{t('; delete it afterwards. Or')} <a href={href('/projects')}>{t('deploy from a repository')}</a> {t('(Projects → New service).')}</p>
            {/if}
          {/if}

          <div class="nav">
            <button onclick={() => go(index - 1)} disabled={index === 0}><Icon name="arrowleft" size={14} />{t('Back')}</button>
            <span class="spacer"></span>
            <button class="ghost" onclick={skipStep} disabled={saving}>{t('Skip this step')}</button>
            <button class="primary" onclick={next} disabled={saving}>
              {#if index === steps.length - 1}{t('Finish')}{:else if cur.status === 'ok'}{t('Continue')}{:else}{t('Continue anyway')}{/if}
            </button>
          </div>
        </main>
      {/if}
    </div>
  {/if}
</div>

<style>
  .setup {
    min-height: 100vh;
    background: radial-gradient(1200px 500px at 50% -15%, var(--accent-soft), transparent 60%), var(--bg);
  }
  .top {
    position: sticky;
    top: 0;
    z-index: 5;
    display: flex;
    align-items: center;
    gap: 10px;
    height: var(--topbar-h);
    padding: 0 16px;
    border-bottom: 1px solid var(--border);
    background: color-mix(in srgb, var(--bg) 88%, transparent);
    backdrop-filter: blur(8px);
  }
  .logo {
    width: 26px;
    height: 26px;
    border-radius: 7px;
    background: var(--text);
    color: var(--bg);
    display: inline-flex;
    align-items: center;
    justify-content: center;
  }
  .title {
    display: flex;
    flex-direction: column;
    line-height: 1.2;
  }
  .body {
    display: grid;
    grid-template-columns: 240px minmax(0, 1fr);
    gap: 28px;
    max-width: 1040px;
    margin: 0 auto;
    padding: 28px 24px 48px;
  }
  .rail ol {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 2px;
    position: sticky;
    top: calc(var(--topbar-h) + 24px);
  }
  .rail button {
    width: 100%;
    justify-content: flex-start;
    gap: 10px;
    height: 36px;
    border: 0;
    background: transparent;
    color: var(--text-dim);
    font-weight: 500;
  }
  .rail button.on {
    background: var(--bg-hover);
    color: var(--text);
  }
  .label {
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .panel {
    min-width: 0;
  }
  .head {
    display: flex;
    align-items: center;
    gap: 12px;
  }
  .icon {
    width: 36px;
    height: 36px;
    border-radius: 9px;
    background: var(--accent-soft);
    color: var(--accent);
    display: inline-flex;
    align-items: center;
    justify-content: center;
    flex: none;
  }
  .grow {
    flex: 1;
    min-width: 0;
  }
  .stack-sm {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .top-align {
    align-items: flex-start;
  }
  .result.ok {
    border-color: color-mix(in srgb, var(--green) 35%, var(--border));
  }
  .result.warn {
    border-color: color-mix(in srgb, var(--yellow) 35%, var(--border));
  }
  .result.fail {
    border-color: color-mix(in srgb, var(--red) 35%, var(--border));
  }
  .details {
    margin: 0;
    padding-left: 18px;
    color: var(--text-dim);
    font-size: 13px;
    overflow-wrap: anywhere;
  }
  .wrap {
    flex-wrap: wrap;
  }
  .tpls {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(200px, 1fr));
    gap: 12px;
  }
  .nav {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
    padding-top: 16px;
    border-top: 1px solid var(--border);
  }
  .spacer {
    flex: 1;
  }
  @media (max-width: 760px) {
    .body {
      grid-template-columns: minmax(0, 1fr);
      gap: 16px;
      padding: 16px 16px 40px;
    }
    .rail ol {
      position: static;
      flex-direction: row;
      overflow-x: auto;
      gap: 4px;
      padding-bottom: 4px;
    }
    .rail li {
      flex: none;
    }
    .rail button {
      width: auto;
    }
    .rail button:not(.on) .label {
      display: none;
    }
    .nav .primary {
      flex: 1;
    }
  }
</style>
