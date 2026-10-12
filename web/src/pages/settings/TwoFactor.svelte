<script lang="ts">
  import { api, errorMessage, type TwoFactorSetup } from '../../lib/api';
  import { session, loadMe } from '../../lib/session.svelte';
  import { qrMatrix, qrPath } from '../../lib/qr';
  import { toast } from '../../lib/toast.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import CopyField from '../../components/CopyField.svelte';
  import Icon from '../../components/Icon.svelte';

  let setup = $state<TwoFactorSetup | null>(null);
  let code = $state('');
  let password = $state('');
  let busy = $state(false);
  let error = $state('');

  const enabled = $derived(!!session.me?.user.totp_enabled);
  const qr = $derived.by(() => {
    if (!setup) return null;
    try {
      const m = qrMatrix(setup.otpauth_url);
      return { size: m.length + 8, d: qrPath(m) };
    } catch {
      return null;
    }
  });
  const grouped = $derived(setup ? setup.secret.replace(/(.{4})/g, '$1 ').trim() : '');

  async function start() {
    busy = true;
    error = '';
    try {
      setup = await api.twoFactorSetup();
      code = '';
    } catch (e) {
      error = errorMessage(e);
    } finally {
      busy = false;
    }
  }
  async function enable(e: SubmitEvent) {
    e.preventDefault();
    busy = true;
    error = '';
    try {
      await api.twoFactorEnable(code.trim());
      toast.success('Two-factor authentication enabled');
      setup = null;
      code = '';
      await loadMe();
    } catch (err) {
      error = errorMessage(err);
    } finally {
      busy = false;
    }
  }
  async function disable(e: SubmitEvent) {
    e.preventDefault();
    busy = true;
    error = '';
    try {
      await api.twoFactorDisable(code.trim(), password);
      toast.success('Two-factor authentication disabled');
      code = '';
      password = '';
      await loadMe();
    } catch (err) {
      error = errorMessage(err);
    } finally {
      busy = false;
    }
  }
  const codeOK = $derived(/^\d{6}$/.test(code.trim()));
</script>

<div class="card">
  <div class="card-head">
    <div>
      <h3>Two-factor authentication</h3>
      <p class="hint">Require a 6-digit code from an authenticator app (1Password, Authy, Google Authenticator…) when signing in.</p>
    </div>
    <span class="badge {enabled ? 'green' : ''}"><Icon name="shield" size={12} />{enabled ? 'enabled' : 'off'}</span>
  </div>
  <div class="card-body stack">
    {#if enabled}
      <form class="stack narrow" onsubmit={disable}>
        <p class="small muted">To turn it off, confirm with your password and a current code.</p>
        <label class="field"><span>Password</span><input type="password" bind:value={password} required autocomplete="current-password" /></label>
        <label class="field">
          <span>Authentication code</span>
          <input class="mono code" bind:value={code} inputmode="numeric" autocomplete="one-time-code" maxlength="6" pattern="[0-9]*" minlength="6" placeholder="123456" required />
        </label>
        <ErrorMsg {error} />
        <div><button class="danger" type="submit" disabled={busy || !codeOK || !password}>{#if busy}<span class="spinner"></span>{/if}Disable two-factor</button></div>
      </form>
    {:else if !setup}
      <ErrorMsg {error} />
      <div><button class="primary" onclick={start} disabled={busy}>{#if busy}<span class="spinner"></span>{/if}Set up two-factor</button></div>
    {:else}
      <div class="setup">
        {#if qr}
          <svg class="qr" viewBox="0 0 {qr.size} {qr.size}" width="200" height="200" role="img" aria-label="QR code for the authenticator app" shape-rendering="crispEdges">
            <rect width={qr.size} height={qr.size} fill="#fff" />
            <path d={qr.d} fill="#000" />
          </svg>
        {/if}
        <form class="stack" onsubmit={enable} style="flex:1;min-width:240px">
          <ol class="small muted steps">
            <li>Scan the QR code with your authenticator app, or enter the secret manually.</li>
            <li>Enter the 6-digit code it shows to finish.</li>
          </ol>
          <div class="field"><span>Secret</span><CopyField value={grouped} label="TOTP secret" /></div>
          <details>
            <summary class="small muted">otpauth:// link</summary>
            <div style="margin-top:6px"><CopyField value={setup.otpauth_url} label="otpauth URL" multiline /></div>
          </details>
          <label class="field">
            <span>Authentication code</span>
            <input class="mono code" bind:value={code} inputmode="numeric" autocomplete="one-time-code" maxlength="6" placeholder="123456" required />
          </label>
          <ErrorMsg {error} />
          <div class="row">
            <button class="primary" type="submit" disabled={busy || !codeOK}>{#if busy}<span class="spinner"></span>{/if}Enable</button>
            <button type="button" class="ghost" onclick={() => ((setup = null), (error = ''))}>Cancel</button>
          </div>
        </form>
      </div>
    {/if}
  </div>
</div>

<style>
  .narrow {
    max-width: 380px;
  }
  .code {
    letter-spacing: 0.3em;
    max-width: 180px;
    font-size: 16px;
  }
  .setup {
    display: flex;
    gap: 24px;
    align-items: flex-start;
    flex-wrap: wrap;
  }
  .qr {
    border-radius: 8px;
    border: 1px solid var(--border-strong);
    flex: none;
  }
  .steps {
    margin: 0;
    padding-left: 18px;
  }
</style>
