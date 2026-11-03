<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type OIDCProvider, type OIDCProviderInput, type OIDCGroupRole } from '../../lib/api';
  import { session } from '../../lib/session.svelte';
  import { toast } from '../../lib/toast.svelte';
  import { confirmDialog } from '../../lib/confirm.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import Loading from '../../components/Loading.svelte';
  import CopyField from '../../components/CopyField.svelte';
  import { t } from '../../lib/i18n.svelte';

  // OpenID Connect providers for "Sign in with …" (instance administrators).
  let providers = $state<OIDCProvider[] | null>(null);
  let redirectURI = $state('');
  let error = $state('');
  let editing = $state<string | null>(null); // provider id, 'new' or null
  let busy = $state(false);
  let formError = $state('');

  type Form = {
    name: string;
    slug: string;
    issuer: string;
    client_id: string;
    client_secret: string;
    scopes: string;
    enabled: boolean;
    trusted: string;
    allowed: string;
    enforce: string;
    auto_provision: boolean;
    default_team_id: string;
    default_role: OIDCGroupRole['role'];
    groups_claim: string;
    group_roles: OIDCGroupRole[];
    skip_totp: boolean;
  };
  const blank = (): Form => ({
    name: '', slug: '', issuer: '', client_id: '', client_secret: '', scopes: 'openid email profile', enabled: true, trusted: '', allowed: '',
    enforce: '', auto_provision: false, default_team_id: '', default_role: 'developer', groups_claim: 'groups', group_roles: [], skip_totp: true,
  });
  let form = $state<Form>(blank());
  const teams = $derived(session.me?.teams ?? []);
  const list = (s: string) => s.split(/[\s,]+/).map((x) => x.trim()).filter(Boolean);

  async function load() {
    try {
      const r = await api.oidcProviders();
      providers = r.providers;
      redirectURI = r.redirect_uri;
      error = '';
    } catch (e) {
      error = errorMessage(e);
    }
  }
  onMount(load);

  function edit(p?: OIDCProvider) {
    formError = '';
    if (!p) {
      form = blank();
      editing = 'new';
      return;
    }
    form = {
      name: p.name, slug: p.slug, issuer: p.issuer, client_id: p.client_id, client_secret: '', scopes: p.scopes.join(' '), enabled: p.enabled,
      trusted: p.trusted_domains.join(', '), allowed: p.allowed_domains.join(', '), enforce: p.enforce_domains.join(', '),
      auto_provision: p.auto_provision, default_team_id: p.default_team_id ?? '', default_role: p.default_role, groups_claim: p.groups_claim,
      group_roles: p.group_roles.map((g) => ({ ...g })), skip_totp: p.skip_totp,
    };
    editing = p.id;
  }

  async function save(e: SubmitEvent) {
    e.preventDefault();
    busy = true;
    formError = '';
    const input: OIDCProviderInput = {
      name: form.name, slug: form.slug, issuer: form.issuer, client_id: form.client_id, client_secret: form.client_secret, scopes: list(form.scopes),
      enabled: form.enabled, trusted_domains: list(form.trusted), allowed_domains: list(form.allowed), enforce_domains: list(form.enforce),
      auto_provision: form.auto_provision, default_team_id: form.default_team_id, default_role: form.default_role, groups_claim: form.groups_claim,
      group_roles: form.group_roles.filter((g) => g.group.trim()), skip_totp: form.skip_totp,
    };
    try {
      if (editing === 'new') await api.createOIDCProvider(input);
      else if (editing) await api.updateOIDCProvider(editing, input);
      toast.success(t('Provider saved'));
      editing = null;
      await load();
    } catch (err) {
      formError = errorMessage(err);
    } finally {
      busy = false;
    }
  }

  async function remove(p: OIDCProvider) {
    if (!(await confirmDialog({ title: t('Delete {name}', { name: p.name }), message: t('Users linked to {name} will have to sign in with a password.', { name: p.name }), confirmLabel: t('Delete'), danger: true }))) return;
    try {
      await api.deleteOIDCProvider(p.id);
      await load();
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }
</script>

<div class="stack-lg">
  <div class="card">
    <div class="card-head">
      <div>
        <h3>{t('Single sign-on (OpenID Connect)')}</h3>
        <p class="hint">{t('Google Workspace, Microsoft Entra ID, Keycloak, Authentik… anything with')} <code>/.well-known/openid-configuration</code>.</p>
      </div>
      {#if editing === null}<button class="primary sm" onclick={() => edit()}>{t('Add provider')}</button>{/if}
    </div>
    <div class="card-body stack">
      {#if redirectURI}<CopyField value={redirectURI} label={t('Redirect URI to register at the provider')} />{/if}
      {#if error}<ErrorMsg {error} onretry={load} />{:else if providers === null}<Loading />{:else if providers.length === 0}<p class="muted small">{t('No providers yet.')}</p>{/if}
      {#each providers ?? [] as p (p.id)}
        <div class="row between">
          <div>
            <b>{p.name}</b> <span class="faint small mono">{p.issuer}</span>
            {#if !p.enabled}<span class="badge">{t('disabled')}</span>{/if}
            {#if p.enforce_domains.length}<span class="badge yellow">{t('enforced: {domains}', { domains: p.enforce_domains.join(', ') })}</span>{/if}
          </div>
          <div class="row">
            <button class="sm" onclick={() => edit(p)}>{t('Edit')}</button>
            <button class="sm danger" onclick={() => remove(p)}>{t('Delete')}</button>
          </div>
        </div>
      {/each}
    </div>
  </div>

  {#if editing !== null}
    <form class="card" onsubmit={save}>
      <div class="card-head"><h3>{editing === 'new' ? t('Add provider') : t('Edit {name}', { name: form.name })}</h3></div>
      <div class="card-body stack">
        <div class="grid-2">
          <label class="field"><span>{t('Display name')}</span><input bind:value={form.name} required placeholder="Google" /></label>
          <label class="field"><span>{t('Slug')}</span><input bind:value={form.slug} placeholder={t('from the name')} /><span class="hint">{t('Used in the sign-in URL.')}</span></label>
          <label class="field"><span>{t('Issuer URL')}</span><input bind:value={form.issuer} required placeholder="https://accounts.google.com" /></label>
          <label class="field"><span>{t('Scopes')}</span><input bind:value={form.scopes} /></label>
          <label class="field"><span>{t('Client ID')}</span><input bind:value={form.client_id} required /></label>
          <label class="field">
            <span>{t('Client secret')}</span><input type="password" bind:value={form.client_secret} autocomplete="new-password" placeholder={editing === 'new' ? '' : t('unchanged')} />
            <span class="hint">{t('Stored encrypted. Leave empty to keep it.')}</span>
          </label>
        </div>
        <label class="check"><input type="checkbox" bind:checked={form.enabled} />{t('Enabled (shown on the sign-in page)')}</label>
        <label class="check"><input type="checkbox" bind:checked={form.skip_totp} />{t("Skip this platform's two-factor code (the provider enforces MFA)")}</label>

        <h4>{t('Accounts')}</h4>
        <div class="grid-2">
          <label class="field">
            <span>{t('Trusted domains')}</span><input bind:value={form.trusted} placeholder="example.com" />
            <span class="hint">{t('A verified email here signs in to the existing account with that email. Elsewhere users link the provider themselves under Security. Administrators always link themselves.')}</span>
          </label>
          <label class="field">
            <span>{t('Allowed domains')}</span><input bind:value={form.allowed} placeholder={t('any')} />
            <span class="hint">{t('Only verified emails in these domains may sign in with this provider.')}</span>
          </label>
          <label class="field">
            <span>{t('Enforce SSO for domains')}</span><input bind:value={form.enforce} placeholder={t('none')} />
            <span class="hint">{t('Password and GitHub sign-in are refused for these domains. The instance owner (first administrator) keeps password sign-in.')}</span>
          </label>
          <label class="field"><span>{t('Groups claim')}</span><input bind:value={form.groups_claim} /></label>
        </div>
        <label class="check"><input type="checkbox" bind:checked={form.auto_provision} />{t('Create accounts for new users on their first sign-in')}</label>
        {#if form.auto_provision}
          <div class="grid-2">
            <label class="field">
              <span>{t('Add new users to team')}</span>
              <select bind:value={form.default_team_id}>
                <option value="">{t('none (personal team only)')}</option>
                {#each teams as tm (tm.id)}<option value={tm.id}>{tm.name}</option>{/each}
              </select>
            </label>
            <label class="field">
              <span>{t('With role')}</span>
              <select bind:value={form.default_role}><option value="viewer">{t('viewer')}</option><option value="developer">{t('developer')}</option><option value="admin">{t('admin')}</option></select>
            </label>
          </div>
        {/if}

        <h4>{t('Group → team role')}</h4>
        <p class="hint">{t('At each sign-in, members of a group get the role in the team (the highest one wins). Team owners are never changed.')}</p>
        {#each form.group_roles as g, i (i)}
          <div class="row nowrap">
            <input bind:value={g.group} placeholder={t('group')} aria-label={t('Group')} />
            <select bind:value={g.team_id} aria-label={t('Team')}>
              <option value="">{t('team…')}</option>
              {#each teams as tm (tm.id)}<option value={tm.id}>{tm.name}</option>{/each}
            </select>
            <select bind:value={g.role} aria-label={t('Role')}><option value="viewer">{t('viewer')}</option><option value="developer">{t('developer')}</option><option value="admin">{t('admin')}</option></select>
            <button type="button" class="sm danger" onclick={() => form.group_roles.splice(i, 1)}>{t('Remove')}</button>
          </div>
        {/each}
        <div><button type="button" class="sm" onclick={() => form.group_roles.push({ group: '', team_id: '', role: 'developer' })}>{t('Add mapping')}</button></div>

        <ErrorMsg error={formError} />
        <div class="row">
          <button class="primary" type="submit" disabled={busy}>{#if busy}<span class="spinner"></span>{/if}{t('Save')}</button>
          <button type="button" class="ghost" onclick={() => (editing = null)}>{t('Cancel')}</button>
        </div>
      </div>
    </form>
  {/if}
</div>
