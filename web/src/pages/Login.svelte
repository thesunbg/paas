<script lang="ts">
  import { api, ApiError, errorMessage, type SetupStatus } from '../lib/api';
  import ErrorMsg from '../components/ErrorMsg.svelte';

  let { setup, onauthed }: { setup: SetupStatus; onauthed: () => Promise<void> } = $props();

  const initialMode = (): 'login' | 'register' => (setup.needs_setup ? 'register' : 'login');
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

  const isSetup = $derived(setup.needs_setup);

  async function submit(e: SubmitEvent) {
    e.preventDefault();
    error = '';
    if (mode === 'register' && password !== confirmPw) {
      error = 'Passwords do not match';
      return;
    }
    if (mode === 'register' && password.length < 8) {
      error = 'Password must be at least 8 characters';
      return;
    }
    busy = true;
    try {
      if (mode === 'register') await api.register(email.trim(), password, name.trim());
      else await api.login(email.trim(), password, needCode ? code.trim() : undefined);
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
  <div class="box">
    <div class="brand">
      <span class="logo" aria-hidden="true"><svg viewBox="0 0 16 16" width="18" height="18"><path d="M2 13 8 2.5 14 13z" fill="currentColor" /></svg></span>
    </div>
    <h1>
      {#if isSetup}Create the administrator account{:else if mode === 'register'}Create an account{:else}Sign in to PaaS{/if}
    </h1>
    <p class="muted sub">
      {#if isSetup}
        This is a fresh installation. The first account becomes the platform administrator.
      {:else if mode === 'register'}
        Sign up for an account on {setup.base_domain || 'this platform'}.
      {:else}
        Deploy and manage your apps{setup.base_domain ? ` on ${setup.base_domain}` : ''}.
      {/if}
    </p>

    <form class="stack card card-pad" onsubmit={submit}>
      {#if mode === 'register'}
        <label class="field">
          <span>Name</span>
          <input bind:value={name} autocomplete="name" placeholder="Ada Lovelace" />
        </label>
      {/if}
      <label class="field">
        <span>Email</span>
        <input type="email" bind:value={email} required autocomplete="email" placeholder="you@example.com" readonly={needCode} />
      </label>
      <label class="field">
        <span>Password</span>
        <input type="password" bind:value={password} required autocomplete={mode === 'login' ? 'current-password' : 'new-password'} minlength={mode === 'register' ? 8 : undefined} />
      </label>
      {#if needCode && mode === 'login'}
        <label class="field">
          <span>Authentication code</span>
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
          <span class="hint">Two-factor authentication is on. Enter the 6-digit code from your authenticator app.</span>
        </label>
      {/if}
      {#if mode === 'register'}
        <label class="field">
          <span>Confirm password</span>
          <input type="password" bind:value={confirmPw} required autocomplete="new-password" />
          <span class="hint">At least 8 characters.</span>
        </label>
      {/if}
      <ErrorMsg {error} />
      <button class="primary" type="submit" disabled={busy}>
        {#if busy}<span class="spinner"></span>{/if}
        {#if isSetup}Create administrator{:else if mode === 'register'}Create account{:else if needCode}Verify{:else}Sign in{/if}
      </button>
      {#if needCode}<button type="button" class="ghost sm" onclick={() => ((needCode = false), (code = ''), (error = ''))}>Use a different account</button>{/if}
    </form>

    {#if !isSetup && setup.allow_signup}
      <p class="muted switch">
        {#if mode === 'login'}
          No account? <button class="link" onclick={() => ((mode = 'register'), (error = ''))}>Sign up</button>
        {:else}
          Already have an account? <button class="link" onclick={() => ((mode = 'login'), (error = ''))}>Sign in</button>
        {/if}
      </p>
    {/if}
  </div>
</div>

<style>
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
  .switch {
    text-align: center;
    font-size: 13px;
  }
  .link {
    background: none;
    border: 0;
    color: var(--accent);
    padding: 0;
    height: auto;
    font-weight: 500;
  }
  .link:hover:not(:disabled) {
    background: none;
    text-decoration: underline;
  }
</style>
