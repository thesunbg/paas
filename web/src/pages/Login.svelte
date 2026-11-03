<script lang="ts">
  import { api, ApiError, errorMessage, type SetupStatus } from '../lib/api';
  import ErrorMsg from '../components/ErrorMsg.svelte';
  import LangSwitch from '../components/LangSwitch.svelte';
  import { t } from '../lib/i18n.svelte';

  let { setup, onauthed }: { setup: SetupStatus; onauthed: () => Promise<void> } = $props();

  // "#/signup" opens the registration form directly (e.g. links from a landing page).
  const wantsSignup = /^#\/(signup|register)\b/.test(location.hash);
  const initialMode = (): 'login' | 'register' => (setup.needs_setup || (wantsSignup && setup.allow_signup) ? 'register' : 'login');
  let mode = $state(initialMode());
  let email = $state('');
  let password = $state('');
  let confirmPw = $state('');
  let name = $state('');
  let busy = $state(false);
  let needCode = $state(false);
  let code = $state('');
  let codeInput: HTMLInputElement | undefined = $state();
  let error = $state('');

  // Results of "Continue with GitHub" come back as #/login?github_error=… or ?github_2fa=…
  // Single sign-on results come back as ?sso_error=… or ?sso_2fa=… the same way.
  const params = new URLSearchParams(location.hash.split('?')[1] ?? '');
  const ghPending = params.get('github_2fa') ?? params.get('sso_2fa') ?? '';
  const ssoErr = params.get('github_error') ?? params.get('sso_error');
  if (ssoErr) error = ssoErr;
  if (ghPending) needCode = true;
  if (ssoErr || ghPending) history.replaceState(null, '', '#/');
  const sso = $derived(setup.needs_setup ? [] : (setup.sso ?? []));

  const isSetup = $derived(setup.needs_setup);

  async function submit(e: SubmitEvent) {
    e.preventDefault();
    error = '';
    if (mode === 'register' && password !== confirmPw) {
      error = t('Passwords do not match');
      return;
    }
    if (mode === 'register' && password.length < 8) {
      error = t('Password must be at least 8 characters');
      return;
    }
    busy = true;
    try {
      if (ghPending) await api.login2FA(ghPending, code.trim());
      else if (mode === 'register') await api.register(email.trim(), password, name.trim());
      else await api.login(email.trim(), password, needCode ? code.trim() : undefined);
      if (wantsSignup) history.replaceState(null, '', '#/');
      await onauthed();
    } catch (err) {
      if (err instanceof ApiError && err.data?.two_factor) {
        // First step accepted the password; ask for the authenticator code.
        if (needCode) error = errorMessage(err);
        needCode = true;
        code = '';
        queueMicrotask(() => codeInput?.focus());
        return;
      }
      error = errorMessage(err);
    } finally {
      busy = false;
    }
  }
</script>

<div class="wrap">
  <div class="lang-corner"><LangSwitch /></div>
  <div class="box">
    <div class="brand">
      <span class="logo" aria-hidden="true"><svg viewBox="0 0 16 16" width="18" height="18"><path d="M2 13 8 2.5 14 13z" fill="currentColor" /></svg></span>
    </div>
    <h1>
      {#if isSetup}{t('Create the administrator account')}{:else if mode === 'register'}{t('Create an account')}{:else}{t('Sign in to PaaS')}{/if}
    </h1>
    <p class="muted sub">
      {#if isSetup}
        {t('This is a fresh installation. The first account becomes the platform administrator; a short setup wizard follows.')}
      {:else if mode === 'register'}
        {setup.base_domain ? t('Sign up for an account on {domain}.', { domain: setup.base_domain }) : t('Sign up for an account on this platform.')}
      {:else}
        {setup.base_domain ? t('Deploy and manage your apps on {domain}.', { domain: setup.base_domain }) : t('Deploy and manage your apps.')}
      {/if}
    </p>

    {#if !isSetup && setup.allow_signup}
      <div class="tabs" role="tablist">
        <button type="button" role="tab" aria-selected={mode === 'login'} class:on={mode === 'login'} onclick={() => ((mode = 'login'), (error = ''))}>{t('Sign in')}</button>
        <button type="button" role="tab" aria-selected={mode === 'register'} class:on={mode === 'register'} onclick={() => ((mode = 'register'), (error = ''), (needCode = false))}>{t('Create account')}</button>
      </div>
    {:else if wantsSignup && !isSetup}
      <p class="alert">{t('Sign-up is closed on this server. Ask an administrator to invite you.')}</p>
    {/if}

    <form class="stack card card-pad" onsubmit={submit}>
      {#if mode === 'register'}
        <label class="field">
          <span>{t('Name')}</span>
          <input bind:value={name} autocomplete="name" placeholder="Ada Lovelace" />
        </label>
      {/if}
      {#if !ghPending}
      <label class="field">
        <span>{t('Email')}</span>
        <input type="email" bind:value={email} required autocomplete="email" placeholder="you@example.com" readonly={needCode} />
      </label>
      <label class="field">
        <span>{t('Password')}</span>
        <input type="password" bind:value={password} required autocomplete={mode === 'login' ? 'current-password' : 'new-password'} minlength={mode === 'register' ? 8 : undefined} />
      </label>
      {/if}
      {#if needCode && mode === 'login'}
        <label class="field">
          <span>{t('Authentication code')}</span>
          <input
            bind:this={codeInput}
            class="mono code"
            bind:value={code}
            inputmode="numeric"
            autocomplete="one-time-code"
            pattern="[0-9]*" minlength="6"
            maxlength="6"
            placeholder="123456"
            required
          />
          <span class="hint">{t('Two-factor authentication is on. Enter the 6-digit code from your authenticator app.')}</span>
        </label>
      {/if}
      {#if mode === 'register'}
        <label class="field">
          <span>{t('Confirm password')}</span>
          <input type="password" bind:value={confirmPw} required autocomplete="new-password" />
          <span class="hint">{t('At least 8 characters.')}</span>
        </label>
      {/if}
      <ErrorMsg {error} />
      <button class="primary" type="submit" disabled={busy}>
        {#if busy}<span class="spinner"></span>{/if}
        {#if isSetup}{t('Create administrator')}{:else if mode === 'register'}{t('Create account')}{:else if needCode}{t('Verify')}{:else}{t('Sign in')}{/if}
      </button>
      {#if needCode && !ghPending}<button type="button" class="ghost sm" onclick={() => ((needCode = false), (code = ''), (error = ''))}>{t('Use a different account')}</button>{/if}
      {#if (setup.github_login || sso.length) && !needCode}
        <div class="or"><span>{t('or')}</span></div>
      {/if}
      {#if !needCode}
        {#each sso as p (p.slug)}
          <a class="btn gh" href={`/api/v1/auth/oidc/${encodeURIComponent(p.slug)}`}>{t('Sign in with {name}', { name: p.name })}</a>
        {/each}
      {/if}
      {#if setup.github_login && !needCode}
        <a class="btn gh" href="/api/v1/auth/github">
          <svg viewBox="0 0 16 16" width="16" height="16" aria-hidden="true"><path fill="currentColor" d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0 0 16 8c0-4.42-3.58-8-8-8z"/></svg>
          {t('Continue with GitHub')}
        </a>
      {/if}
    </form>

  </div>
</div>

<style>
  .or {
    display: flex;
    align-items: center;
    gap: 10px;
    color: var(--text-faint);
    font-size: 12px;
  }
  .or::before,
  .or::after {
    content: '';
    flex: 1;
    border-top: 1px solid var(--border);
  }
  .gh {
    justify-content: center;
    gap: 8px;
    width: 100%;
  }
  .tabs {
    display: flex;
    gap: 4px;
    padding: 3px;
    margin-bottom: 12px;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius-sm);
  }
  .tabs button {
    flex: 1;
    border: 0;
    background: transparent;
    color: var(--text-dim);
    font-weight: 500;
  }
  .tabs button.on {
    background: var(--bg-hover);
    color: var(--text);
  }
  .lang-corner {
    position: fixed;
    top: 12px;
    right: 16px;
    z-index: 2;
  }
  .wrap {
    min-height: 100vh;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 24px 16px;
    background: radial-gradient(1200px 600px at 50% -10%, var(--accent-soft), transparent 60%);
  }
  .box {
    width: 100%;
    max-width: 380px;
    display: flex;
    flex-direction: column;
    gap: 10px;
  }
  .brand {
    display: flex;
    justify-content: center;
    margin-bottom: 8px;
  }
  .logo {
    width: 40px;
    height: 40px;
    border-radius: 10px;
    background: var(--text);
    color: var(--bg);
    display: inline-flex;
    align-items: center;
    justify-content: center;
  }
  h1 {
    text-align: center;
    font-size: 20px;
  }
  .sub {
    text-align: center;
    margin-bottom: 12px;
  }
  .code {
    letter-spacing: 0.3em;
    font-size: 16px;
  }
</style>
