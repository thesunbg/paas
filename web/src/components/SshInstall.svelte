<script lang="ts">
  // "Install over SSH": the control plane logs in to a machine, checks it, runs the installer
  // and waits until the node is online. Credentials stay in this form and are sent once.
  import { onMount } from 'svelte';
  import { api, errorMessage, type SSHHostKey, type SSHInstall } from '../lib/api';
  import { relTime, absTime } from '../lib/format';
  import { toast } from '../lib/toast.svelte';
  import ErrorMsg from './ErrorMsg.svelte';
  import Icon from './Icon.svelte';
  import LogView from './LogView.svelte';
  import Status from './Status.svelte';
  import { t } from '../lib/i18n.svelte';

  let { ondone }: { ondone?: () => void } = $props();

  let host = $state('');
  let port = $state(22);
  let user = $state('root');
  let auth = $state<'key' | 'password'>('key');
  let privateKey = $state('');
  let passphrase = $state('');
  let password = $state('');
  let sudoPassword = $state('');
  let roles = $state<string[]>(['worker']);
  let name = $state('');
  let reinstall = $state(false);
  let allowCP = $state(false);

  let hostKey = $state<SSHHostKey | null>(null);
  let keyFor = $state(''); // host:port the key was read for
  let confirmed = $state(false);
  let expected = $state('');
  let probing = $state(false);
  let formError = $state('');

  let current = $state<SSHInstall | null>(null);
  let lines = $state<string[]>([]);
  let es: EventSource | null = null;
  let pollTimer: ReturnType<typeof setInterval> | undefined;
  let starting = $state(false);
  let history = $state<SSHInstall[]>([]);

  const target = $derived(`${host.trim()}:${port}`);
  const norm = (fp: string) => {
    let f = fp.trim();
    const i = f.indexOf(':');
    if (i >= 0 && f.slice(0, i).toLowerCase() === 'sha256') f = f.slice(i + 1);
    return 'SHA256:' + f.replace(/=+$/, '');
  };
  const expectedMismatch = $derived(!!hostKey && expected.trim() !== '' && norm(expected) !== hostKey.fingerprint);
  const keyOK = $derived(!!hostKey && keyFor === target && (confirmed || (expected.trim() !== '' && !expectedMismatch)));
  const credsOK = $derived(auth === 'key' ? privateKey.trim() !== '' : password !== '');
  const running = $derived(current?.status === 'running');

  async function loadHistory() {
    try {
      history = (await api.sshInstalls()) ?? [];
    } catch {
      /* the list is a convenience */
    }
  }
  onMount(() => {
    loadHistory();
    return () => detach();
  });

  async function probe() {
    formError = '';
    hostKey = null;
    confirmed = false;
    if (!host.trim()) return void (formError = t('Enter the host name or IP address'));
    probing = true;
    try {
      hostKey = await api.sshHostKey(host.trim(), Number(port));
      keyFor = target;
    } catch (e) {
      formError = errorMessage(e);
    } finally {
      probing = false;
    }
  }

  function toggleRole(r: string) {
    roles = roles.includes(r) ? roles.filter((x) => x !== r) : [...roles, r];
  }

  async function start(preflightOnly: boolean) {
    formError = '';
    if (!hostKey || !keyOK) return void (formError = t('Confirm the host key first'));
    if (!credsOK) return void (formError = auth === 'key' ? t('Paste the private key') : t('Enter the password'));
    if (roles.length === 0) return void (formError = t('Pick at least one role'));
    starting = true;
    try {
      const inst = await api.createSSHInstall({
        host: host.trim(),
        port: Number(port),
        user: user.trim() || 'root',
        auth,
        private_key: auth === 'key' ? privateKey : undefined,
        passphrase: auth === 'key' && passphrase ? passphrase : undefined,
        password: auth === 'password' ? password : sudoPassword || undefined,
        roles,
        name: name.trim() || undefined,
        fingerprint: hostKey.fingerprint,
        reinstall,
        preflight_only: preflightOnly,
        allow_control_plane: allowCP,
      });
      attach(inst);
      loadHistory();
    } catch (e) {
      formError = errorMessage(e);
    } finally {
      starting = false;
    }
  }

  function detach() {
    es?.close();
    es = null;
    if (pollTimer) clearInterval(pollTimer);
    pollTimer = undefined;
  }

  // attach follows an attempt: its log live, its step and checklist by polling.
  function attach(inst: SSHInstall) {
    detach();
    current = inst;
    lines = [];
    es = new EventSource(api.sshInstallLogStreamURL(inst.id));
    es.addEventListener('log', (ev) => lines.push((ev as MessageEvent).data));
    es.addEventListener('done', (ev) => {
      es?.close();
      refresh().then(() => {
        if (current?.status === 'succeeded') {
          if (current.preflight_only) toast.success(t('All checks passed'));
          else toast.success(t('{name} joined the cluster', { name: current.node_name || current.host }));
          ondone?.();
        } else if (current?.status === 'failed') toast.error(t('Install failed'));
        loadHistory();
      });
    });
    pollTimer = setInterval(refresh, 2000);
  }

  async function refresh() {
    if (!current) return;
    try {
      current = await api.sshInstall(current.id);
      if (current.status !== 'running' && pollTimer) {
        clearInterval(pollTimer);
        pollTimer = undefined;
      }
    } catch {
      /* keep the last state */
    }
  }

  async function cancel() {
    if (!current) return;
    try {
      await api.cancelSSHInstall(current.id);
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }

  function back() {
    detach();
    current = null;
  }

  const steps: { id: SSHInstall['step']; label: string }[] = $derived([
    { id: 'connect', label: t('Connect') },
    { id: 'preflight', label: t('Checks') },
    { id: 'install', label: t('Install') },
    { id: 'enroll', label: t('Join') },
    { id: 'done', label: t('Online') },
  ]);
  // Shown as text: attempt status (running, succeeded, failed), step (connect, preflight, install,
  // enroll, done) and preflight check result (ok, warn, fail).
  function stateText(s: string): string {
    const labels: Record<string, string> = {
      running: t('running'),
      succeeded: t('succeeded'),
      failed: t('failed'),
      connect: t('connect'),
      preflight: t('preflight'),
      install: t('install'),
      enroll: t('enroll'),
      done: t('done'),
      ok: t('ok'),
      warn: t('warn'),
      fail: t('fail'),
    };
    return labels[s] ?? s;
  }
  const stepIndex = (s: SSHInstall['step']) => steps.findIndex((x) => x.id === s);
  const checkBadge = { ok: 'green', warn: 'yellow', fail: 'red' } as const;
</script>

{#if current}
  {@const at = stepIndex(current.step)}
  <div class="stack">
    <div class="between">
      <div>
        <b>{current.user}@{current.host}:{current.port}</b>
        <span class="faint small">· {current.roles.join(', ')}{current.node_name ? ` · ${current.node_name}` : ''}</span>
      </div>
      <Status status={current.status === 'succeeded' ? 'online' : current.status === 'running' ? 'deploying' : 'failed'} label={stateText(current.status)} />
    </div>
    <ol class="steps" aria-label={t('Progress')}>
      {#each steps as s, i (s.id)}
        {#if !current.preflight_only || i <= 1}
          <li class:done={i < at || current.status === 'succeeded'} class:now={i === at && current.status === 'running'} class:bad={i === at && current.status === 'failed'}>{s.label}</li>
        {/if}
      {/each}
    </ol>
    {#if current.preflight?.checks?.length}
      <ul class="checks">
        {#each current.preflight.checks as c (c.id)}
          <li><span class="badge {checkBadge[c.status]}">{stateText(c.status)}</span><b>{c.title}</b><span class="muted">{c.detail}</span></li>
        {/each}
      </ul>
    {/if}
    {#if current.error}
      <div class="alert error">{current.error}</div>
    {/if}
    <LogView {lines} height="320px" empty={t('Waiting for output…')} />
    <p class="hint">{t('Host key {fingerprint}. The credentials were used for this install only and are not stored.', { fingerprint: current.host_key_fingerprint })}</p>
    <div class="row">
      {#if running}
        <button class="danger" onclick={cancel}>{t('Cancel')}</button>
      {:else}
        <button onclick={back}><Icon name="arrowleft" size={13} />{current.status === 'failed' ? t('Back to the form') : t('Install another')}</button>
        {#if current.status === 'succeeded' && current.preflight_only}
          <button class="primary" onclick={() => start(false)} disabled={starting}>{t('Install now')}</button>
        {:else if current.status === 'failed' && hostKey && credsOK && current.host === host.trim()}
          <button class="primary" onclick={() => start(current?.preflight_only ?? false)} disabled={starting}>{t('Try again')}</button>
        {/if}
      {/if}
      {#if !running}<span class="faint small">{t('finished {time}', { time: relTime(current.finished_at ?? current.created_at) })}</span>{/if}
    </div>
  </div>
{:else}
  <form class="stack" onsubmit={(e) => (e.preventDefault(), start(false))}>
    <p class="small muted">
      {t('The control plane logs in to the machine over SSH, checks it, runs the installer with a single-use join token and waits until the node is online.')}
      <b>{t('The key or password is used for this install only and never stored.')}</b>
    </p>
    <div class="grid-3">
      <label class="field">
        <span>{t('Host')}</span>
        <input bind:value={host} placeholder="203.0.113.10" autocomplete="off" required oninput={() => (confirmed = false)} />
      </label>
      <label class="field">
        <span>{t('Port')}</span>
        <input type="number" min="1" max="65535" bind:value={port} />
      </label>
      <label class="field">
        <span>{t('User')}</span>
        <input bind:value={user} autocomplete="off" />
        <span class="hint">{t('root, or a user with sudo')}</span>
      </label>
    </div>

    <div class="hostkey">
      <div class="between">
        <span class="label">{t('Host key')}</span>
        <button type="button" class="sm" onclick={probe} disabled={probing || !host.trim()}><Icon name="key" size={13} />{hostKey ? t('Read again') : t('Read host key')}</button>
      </div>
      {#if hostKey && keyFor === target}
        <div class="mono small fp">{hostKey.key_type} {hostKey.fingerprint}</div>
        {#if hostKey.control_plane}
          <div class="alert warn small">
            {t('{host} is this control plane. Installing a node agent on it replaces its configuration.', { host })}
            <label class="check"><input type="checkbox" bind:checked={allowCP} />{t('Install on it anyway')}</label>
          </div>
        {/if}
        <label class="check"><input type="checkbox" bind:checked={confirmed} />{t("This is the server's key")} ({t('compare with')} <code>ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub</code> {t('on its console')})</label>
        <label class="field">
          <span>{t('…or paste the expected fingerprint')}</span>
          <input class="mono" bind:value={expected} placeholder="SHA256:…" autocomplete="off" />
          {#if expectedMismatch}<span class="hint err">{t('Does not match the key the server presented: do not continue.')}</span>{/if}
        </label>
      {:else}
        <p class="hint">{t("Read the server's SSH host key and confirm it before any credential is sent.")}</p>
      {/if}
    </div>

    <div class="row">
      <label class="check"><input type="radio" bind:group={auth} value="key" />{t('Private key')}</label>
      <label class="check"><input type="radio" bind:group={auth} value="password" />{t('Password')}</label>
    </div>
    {#if auth === 'key'}
      <label class="field">
        <span>{t('Private key')}</span>
        <textarea class="mono" rows="5" bind:value={privateKey} placeholder="-----BEGIN OPENSSH PRIVATE KEY-----" autocomplete="off" spellcheck="false"></textarea>
      </label>
      <div class="grid-2">
        <label class="field">
          <span>{t('Key passphrase (if encrypted)')}</span>
          <input type="password" bind:value={passphrase} autocomplete="off" />
        </label>
        {#if user.trim() !== 'root'}
          <label class="field">
            <span>{t('sudo password (if sudo asks for one)')}</span>
            <input type="password" bind:value={sudoPassword} autocomplete="off" />
          </label>
        {/if}
      </div>
    {:else}
      <label class="field">
        <span>{t('Password')}</span>
        <input type="password" bind:value={password} autocomplete="off" />
        {#if user.trim() !== 'root'}<span class="hint">{t('Also used for sudo.')}</span>{/if}
      </label>
    {/if}

    <div class="grid-2">
      <div class="field">
        <span>{t('Roles')}</span>
        <div class="row">
          {#each ['worker', 'build', 'edge'] as r (r)}
            <label class="check"><input type="checkbox" checked={roles.includes(r)} onchange={() => toggleRole(r)} />{r}</label>
          {/each}
        </div>
      </div>
      <label class="field">
        <span>{t('Node name (optional)')}</span>
        <input bind:value={name} placeholder={t('its hostname')} autocomplete="off" />
      </label>
    </div>
    <label class="check"><input type="checkbox" bind:checked={reinstall} />{t('Reinstall if paasd is already installed')}</label>

    <ErrorMsg error={formError} />
    <div class="row">
      <button type="button" onclick={() => start(true)} disabled={starting || !keyOK || !credsOK}>{t('Run checks only')}</button>
      <button type="submit" class="primary" disabled={starting || !keyOK || !credsOK}>{t('Install')}</button>
    </div>
  </form>
{/if}

{#if history.length}
  <details class="history">
    <summary class="small muted">{t('Recent SSH installs ({n})', { n: history.length })}</summary>
    <table>
      <thead><tr><th>{t('Machine')}</th><th>{t('Status')}</th><th>{t('By')}</th><th>{t('When')}</th><th></th></tr></thead>
      <tbody>
        {#each history.slice(0, 10) as h (h.id)}
          <tr>
            <td class="mono small">{h.user}@{h.host}:{h.port}<div class="faint">{h.roles.join(', ')}{h.preflight_only ? ` · ${t('checks only')}` : ''}</div></td>
            <td><Status status={h.status === 'succeeded' ? 'online' : h.status === 'running' ? 'deploying' : 'failed'} label="{stateText(h.status)} · {stateText(h.step)}" />{#if h.error}<div class="faint small trunc" title={h.error}>{h.error}</div>{/if}</td>
            <td class="small">{h.created_by_email}</td>
            <td class="small" title={absTime(h.created_at)}>{relTime(h.created_at)}</td>
            <td class="right"><button class="sm" onclick={() => attach(h)}>{t('Log')}</button></td>
          </tr>
        {/each}
      </tbody>
    </table>
  </details>
{/if}

<style>
  .grid-3 {
    display: grid;
    grid-template-columns: minmax(0, 2fr) minmax(0, 0.7fr) minmax(0, 1fr);
    gap: 12px;
  }
  .hostkey {
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 12px;
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .fp {
    word-break: break-all;
  }
  .err {
    color: var(--red);
  }
  .steps {
    display: flex;
    gap: 6px;
    list-style: none;
    padding: 0;
    margin: 0;
    flex-wrap: wrap;
  }
  .steps li {
    font-size: 12px;
    padding: 3px 10px;
    border-radius: 999px;
    border: 1px solid var(--border);
    color: var(--text-faint);
  }
  .steps li.done {
    color: var(--green);
    border-color: var(--green);
  }
  .steps li.now {
    color: var(--text);
    border-color: var(--accent);
  }
  .steps li.bad {
    color: var(--red);
    border-color: var(--red);
  }
  .checks {
    list-style: none;
    padding: 0;
    margin: 0;
    display: flex;
    flex-direction: column;
    gap: 4px;
    font-size: 13px;
  }
  .checks li {
    display: flex;
    gap: 8px;
    align-items: baseline;
    flex-wrap: wrap;
  }
  .history {
    margin-top: 14px;
  }
  .trunc {
    max-width: 280px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  @media (max-width: 600px) {
    .grid-3 {
      grid-template-columns: 1fr;
    }
  }
</style>
