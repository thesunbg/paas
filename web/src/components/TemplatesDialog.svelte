<script lang="ts">
  import { api, errorMessage, type Template } from '../lib/api';
  import { toast } from '../lib/toast.svelte';
  import Modal from './Modal.svelte';
  import ErrorMsg from './ErrorMsg.svelte';
  import Loading from './Loading.svelte';
  import Icon from './Icon.svelte';
  import { t, tn } from '../lib/i18n.svelte';

  let { open = $bindable(false), projectId, ondone }: { open: boolean; projectId: string; ondone: () => void } = $props();

  let templates = $state<Template[] | null>(null);
  let loadError = $state('');
  let picked = $state<Template | null>(null);
  let domain = $state('');
  let category = $state('All');
  let query = $state('');
  let name = $state('');
  // Values of the picked template's inputs, by service then variable.
  let values = $state<Record<string, Record<string, string>>>({});
  let busy = $state(false);
  let error = $state('');

  $effect(() => {
    if (open && templates === null && !loadError) {
      api
        .templates()
        .then((list) => (templates = list ?? []))
        .catch((e) => (loadError = errorMessage(e)));
    }
    if (!open) {
      picked = null;
      error = '';
      domain = '';
      query = '';
    }
  });

  function pick(tpl: Template) {
    picked = tpl;
    name = tpl.services[0]?.name ?? '';
    values = Object.fromEntries(tpl.services.map((s) => [s.name, Object.fromEntries((s.inputs ?? []).map((i) => [i.key, '']))]));
  }

  const categories = $derived(['All', ...new Set((templates ?? []).map((tpl) => tpl.category))]);
  const shown = $derived.by(() => {
    const q = query.trim().toLowerCase();
    return (templates ?? []).filter(
      (tpl) => (category === 'All' || tpl.category === category) && (!q || `${tpl.title} ${tpl.description} ${tpl.category}`.toLowerCase().includes(q)),
    );
  });
  // Template database modes (shared | dedicated).
  const modeLabel = $derived<Record<string, string>>({ shared: t('shared'), dedicated: t('dedicated') });
  const inputs = $derived((picked?.services ?? []).flatMap((s) => (s.inputs ?? []).map((i) => ({ svc: s.name, ...i }))));

  async function install(e: SubmitEvent) {
    e.preventDefault();
    if (!picked) return;
    busy = true;
    error = '';
    try {
      const r = await api.installTemplate(projectId, picked.id, domain.trim() || undefined, inputs.length ? values : undefined, name.trim() || undefined);
      const ns = r.services?.length ?? 0;
      const nd = r.databases?.length ?? 0;
      const services = tn(ns, '{n} service', '{n} services');
      toast.success(
        nd
          ? t('{title}: {services} and {databases} created — deploying', { title: picked.title, services, databases: tn(nd, '{n} database', '{n} databases') })
          : t('{title}: {services} created — deploying', { title: picked.title, services }),
      );
      open = false;
      ondone();
    } catch (err) {
      error = errorMessage(err);
    } finally {
      busy = false;
    }
  }
</script>

<Modal bind:open title={picked ? t('Install {name}', { name: picked.title }) : t('New from template')} width={760}>
  {#if loadError}
    <ErrorMsg error={loadError} />
  {:else if templates === null}
    <Loading />
  {:else if !picked}
    <div class="stack">
      <input type="search" bind:value={query} placeholder={t('Search {n} templates…', { n: templates.length })} aria-label={t('Search templates')} />
      <div class="row" style="flex-wrap:wrap" role="group" aria-label={t('Category')}>
        {#each categories as c (c)}
          <button type="button" class="sm" class:on={category === c} aria-pressed={category === c} onclick={() => (category = c)}>{c === 'All' ? t('All') : c}</button>
        {/each}
      </div>
      <div class="gallery">
        {#each shown as tpl (tpl.id)}
          <button type="button" class="tpl" onclick={() => pick(tpl)}>
            <span class="between nowrap" style="width:100%">
              <b>{tpl.title}</b>
              <span class="badge">{tpl.category}</span>
            </span>
            <span class="muted small desc">{tpl.description}</span>
            <span class="faint small">
              <Icon name="box" size={12} /> {tn(tpl.services.length, '{n} service', '{n} services')}
              {#if tpl.databases?.length}· <Icon name="database" size={12} /> {tpl.databases.map((d) => d.engine).join(', ')}{/if}
            </span>
          </button>
        {:else}
          <p class="muted">{t('No templates match.')}</p>
        {/each}
      </div>
    </div>
  {:else}
    <form id="tpl-form" class="stack" onsubmit={install}>
      <p class="muted">{picked.description}{#if picked.website}<a style="margin-left:6px" href={picked.website} target="_blank" rel="noopener noreferrer">{t('Website')} ↗</a>{/if}</p>
      <div class="card">
        <table>
          <thead><tr><th>{t('Creates')}</th><th>{t('Image / engine')}</th><th>{t('Details')}</th></tr></thead>
          <tbody>
            {#each picked.databases ?? [] as d (d.name)}
              <tr><td><Icon name="database" size={13} /> {d.name}</td><td>{d.engine}</td><td class="faint small">{t('{mode} database, provisioned first', { mode: modeLabel[d.mode ?? 'shared'] ?? d.mode })}</td></tr>
            {/each}
            {#each picked.services as sv (sv.name)}
              <tr>
                <td><Icon name="box" size={13} /> {sv.name}</td>
                <td class="mono small break">{sv.image}</td>
                <td class="faint small">{sv.type}{sv.port ? ` · ${t('port {port}', { port: sv.port })}` : ''}{sv.size ? ` · ${sv.size}` : ''}{sv.volumes?.length ? ` · ${tn(sv.volumes.length, '{n} volume', '{n} volumes')}` : ''}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
      <label class="field">
        <span>{t('Name')}</span>
        <input class="mono" bind:value={name} spellcheck="false" />
        <span class="hint">{picked.databases?.length ? t('Of the service and its database. If it is already taken in this project, -2, -3… is added.') : t('Of the service. If it is already taken in this project, -2, -3… is added.')}</span>
      </label>
      {#each inputs as i (i.svc + i.key)}
        <label class="field">
          <span>{i.key}{#if !i.required} <span class="faint">({t('optional')})</span>{/if}{#if picked.services.length > 1} <span class="faint">· {i.svc}</span>{/if}</span>
          <input
            class="mono"
            type={/PASS|SECRET|KEY|TOKEN/.test(i.key) ? 'password' : 'text'}
            autocomplete="new-password"
            bind:value={values[i.svc][i.key]}
            placeholder={i.placeholder ?? ''}
            required={i.required}
            spellcheck="false"
          />
          {#if i.hint}<span class="hint">{i.hint}</span>{/if}
        </label>
      {/each}
      <label class="field">
        <span>{t('Custom domain')} <span class="faint">({t('optional')})</span></span>
        <input class="mono" bind:value={domain} placeholder="blog.example.com" spellcheck="false" />
        <span class="hint">{t('Used for the first web service; a generated domain is always added.')}</span>
      </label>
      <ErrorMsg {error} />
    </form>
  {/if}
  {#snippet footer()}
    {#if picked}
      <button type="button" onclick={() => (picked = null)}><Icon name="arrowleft" size={14} />{t('Back')}</button>
      <button type="submit" form="tpl-form" class="primary" disabled={busy}>{#if busy}<span class="spinner"></span>{/if}{t('Install')}</button>
    {:else}
      <button type="button" onclick={() => (open = false)}>{t('Close')}</button>
    {/if}
  {/snippet}
</Modal>

<style>
  .gallery {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(210px, 1fr));
    gap: 8px;
  }
  .tpl {
    height: auto;
    flex-direction: column;
    align-items: flex-start;
    gap: 6px;
    padding: 12px;
    text-align: left;
    white-space: normal;
    font-weight: 400;
    background: var(--bg-elev);
  }
  .tpl:hover:not(:disabled) {
    border-color: var(--accent);
  }
  .desc {
    display: -webkit-box;
    -webkit-line-clamp: 3;
    line-clamp: 3;
    -webkit-box-orient: vertical;
    overflow: hidden;
  }
  .on {
    border-color: var(--accent);
    background: var(--accent-soft);
  }
</style>
