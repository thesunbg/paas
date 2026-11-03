<script lang="ts">
  import { onMount } from 'svelte';
  import { api, ApiError, errorMessage, type Batch, type BatchEstimate, type Template } from '../lib/api';
  import { currentTeam } from '../lib/session.svelte';
  import { href, navigate } from '../lib/router.svelte';
  import { mb, relTime, absTime } from '../lib/format';
  import { poll } from '../lib/poll';
  import { toast } from '../lib/toast.svelte';
  import { confirmDialog } from '../lib/confirm.svelte';
  import ErrorMsg from '../components/ErrorMsg.svelte';
  import Loading from '../components/Loading.svelte';
  import Icon from '../components/Icon.svelte';
  import Status from '../components/Status.svelte';
  import CopyButton from '../components/CopyButton.svelte';
  import { t, tn } from '../lib/i18n.svelte';

  // Bulk installs: one template many times (a class of learners), see internal/server/batches.go.
  let { id }: { id?: string } = $props();

  let batches = $state<Batch[] | null>(null);
  let batch = $state<Batch | null>(null);
  let error = $state('');

  async function load() {
    try {
      if (id) batch = await api.batch(id);
      else batches = await api.batches();
      error = '';
    } catch (e) {
      error = errorMessage(e);
    }
  }
  onMount(() => {
    load();
    return poll(load, 5000);
  });

  // New batch form.
  let creating = $state(false);
  let templates = $state<Template[]>([]);
  let form = $state({ name: '', template: 'n8n', mode: 'instances' as 'instances' | 'accounts', count: 30, learners: '', prefix: '', parallel: 2, sleep: true, idle: 30 });
  let env = $state<Record<string, Record<string, string>>>({});
  let estimate = $state<BatchEstimate | null>(null);
  let formError = $state('');
  let busy = $state(false);
  let needForce = $state(false);

  async function openForm() {
    creating = true;
    if (!templates.length) {
      try {
        templates = (await api.templates()) ?? [];
      } catch (e) {
        formError = errorMessage(e);
      }
    }
  }
  const tpl = $derived(templates.find((x) => x.id === form.template));
  const inputs = $derived((tpl?.services ?? []).flatMap((s) => (s.inputs ?? []).map((i) => ({ svc: s.name, ...i }))));
  $effect(() => {
    for (const i of inputs) {
      env[i.svc] ??= {};
      env[i.svc][i.key] ??= '';
    }
  });
  // "Name, email" or just "email", one learner per line.
  const learners = $derived(
    form.learners
      .split('\n')
      .map((l) => l.trim())
      .filter(Boolean)
      .map((l) => {
        const parts = l.split(/[,;\t]/).map((x) => x.trim());
        const email = parts.find((x) => x.includes('@')) ?? '';
        const name = parts.find((x) => x && !x.includes('@')) ?? '';
        return { name, email };
      }),
  );
  const count = $derived(form.mode === 'instances' ? form.count : learners.length);

  let estTimer: ReturnType<typeof setTimeout>;
  $effect(() => {
    const tid = form.template, n = count, par = form.parallel, sl = form.sleep;
    clearTimeout(estTimer);
    if (!creating || !tid || !n || n < 1) return;
    estTimer = setTimeout(async () => {
      try {
        estimate = await api.batchEstimate(tid, n, par, sl);
      } catch {
        estimate = null;
      }
    }, 300);
  });

  async function create(e: SubmitEvent, force = false) {
    e.preventDefault();
    busy = true;
    formError = '';
    try {
      const r = await api.createBatch({
        name: form.name.trim(),
        template: form.template,
        mode: form.mode,
        count: form.mode === 'instances' ? form.count : undefined,
        learners: form.mode === 'accounts' ? learners : undefined,
        prefix: form.prefix.trim() || undefined,
        team_id: currentTeam()?.id,
        env: inputs.length ? env : undefined,
        force,
        parallel: form.parallel,
        sleep: form.sleep,
        idle_minutes: form.sleep ? form.idle : undefined,
      });
      toast.success(tn(r.items, 'Creating {n} copy of {name}', 'Creating {n} copies of {name}', { name: tpl?.title ?? form.template }));
      creating = false;
      needForce = false;
      navigate(`/batches/${r.id}`);
    } catch (err) {
      formError = errorMessage(err);
      needForce = err instanceof ApiError && err.status === 409 && !!err.data?.estimate && !(err.data.estimate as BatchEstimate).blocked;
    } finally {
      busy = false;
    }
  }

  async function remove(b: Batch) {
    const accounts = b.mode === 'accounts';
    if (
      !(await confirmDialog({
        title: t('Delete batch'),
        message: accounts
          ? t('Deletes the {n} learner accounts of "{name}", their projects, apps and databases. Their data is lost.', { n: Object.values(b.counts).reduce((a, c) => a + c, 0), name: b.name })
          : t('Deletes every copy of "{name}" with its database. Their data is lost.', { name: b.name }),
        confirmLabel: t('Delete everything'),
        danger: true,
        requireText: b.name,
      }))
    )
      return;
    try {
      await api.deleteBatch(b.id);
      toast.success(t('Deleting the batch'));
      if (id) navigate('/batches');
      else load();
    } catch (e) {
      toast.error(errorMessage(e));
    }
  }

  const statusOf = (s: string) => (s === 'creating' || s === 'created' || s === 'deploying' ? 'deploying' : s);
  const total = (b: Batch) => Object.values(b.counts).reduce((a, c) => a + c, 0);
  // Item statuses: failed, creating, or the service's status (created, deploying, running, sleeping, stopped).
  function statusLabel(s: string): string {
    const labels: Record<string, string> = {
      creating: t('creating'),
      created: t('created'),
      deploying: t('deploying'),
      running: t('running'),
      sleeping: t('sleeping'),
      stopped: t('stopped'),
      failed: t('failed'),
    };
    return labels[s] ?? s;
  }
</script>

<div class="page stack-lg">
  {#if id}
    <div>
      <a class="small" href={href('/batches')}><Icon name="arrowleft" size={12} />{t('Batches')}</a>
    </div>
    {#if !batch}
      {#if error}<ErrorMsg {error} />{:else}<Loading />{/if}
    {:else}
      <div class="between">
        <div>
          <h1>{batch.name}</h1>
          <p class="muted small">
            {total(batch)} × {batch.template} · {batch.mode === 'accounts' ? t('one account per learner') : t('copies in one project')} · {t('created {time}', { time: relTime(batch.created_at) })}
            {#if batch.project_id}· <a href={href(`/projects/${batch.project_id}`)}>{t('open the project')}</a>{/if}
          </p>
        </div>
        <div class="row">
          <a class="btn" href={api.batchCSVURL(batch.id)} download><Icon name="upload" size={14} />{t('Download CSV')}</a>
          <button class="danger" onclick={() => batch && remove(batch)} disabled={batch.status === 'deleting'}><Icon name="trash" size={14} />{t('Delete batch')}</button>
        </div>
      </div>
      <div class="row small" style="gap:16px">
        {#each Object.entries(batch.counts) as [k, v] (k)}<span><Status status={statusOf(k)} label="{v} {statusLabel(k)}" /></span>{/each}
        {#if batch.status === 'deleting'}<span class="badge red">{t('deleting…')}</span>{/if}
      </div>
      {#if batch.mode === 'accounts'}
        <p class="hint">{t('Hand each learner their email and first password: they sign in at')} <code>{location.origin}</code> {t('and see only their own project. They can change the password in Settings.')}</p>
      {:else}
        <p class="hint">{t('Hand each learner one link: the app asks them to create its owner account on first visit.')}</p>
      {/if}
      <div class="card table-wrap">
        <table>
          <thead>
            <tr><th>#</th><th>{t('Name')}</th><th>{t('Status')}</th><th>{t('App')}</th>{#if batch.mode === 'accounts'}<th>{t('Email')}</th><th>{t('First password')}</th>{/if}<th></th></tr>
          </thead>
          <tbody>
            {#each batch.items ?? [] as it (it.index)}
              <tr>
                <td class="faint">{it.index + 1}</td>
                <td>{it.label}</td>
                <td>
                  <Status status={statusOf(it.status)} label={statusLabel(it.status)} />
                  {#if it.error}<div class="small" style="color:var(--red)">{it.error}</div>{/if}
                </td>
                <td class="mono small">{#if it.url}<a href={it.url} target="_blank" rel="noopener noreferrer">{it.url.replace(/^https?:\/\//, '')}</a>{/if}</td>
                {#if batch.mode === 'accounts'}
                  <td class="small">{it.email}</td>
                  <td class="mono small nowrap">{#if it.password}{it.password} <CopyButton text={it.password} label={t('Copy password')} />{/if}</td>
                {/if}
                <td class="right">{#if it.service_id}<a class="small" href={href(`/services/${it.service_id}`)}>{t('Open')}</a>{/if}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  {:else}
    <div class="between">
      <div>
        <h1>{t('Batches')}</h1>
        <p class="muted small">{t('Install one template many times at once: an n8n for each of 30 learners, a WordPress per customer. Delete the whole batch when the class is over.')}</p>
      </div>
      {#if !creating}<button class="primary" onclick={openForm}><Icon name="plus" size={14} />{t('New batch')}</button>{/if}
    </div>

    {#if creating}
      <form class="card" onsubmit={(e) => create(e)}>
        <div class="card-head"><h3>{t('New batch')}</h3></div>
        <div class="card-body stack">
          <div class="grid-2">
            <label class="field"><span>{t('Name')}</span><input bind:value={form.name} placeholder="Lớp n8n K1" required /></label>
            <label class="field">
              <span>{t('Template')}</span>
              <select bind:value={form.template}>
                {#each templates as tp (tp.id)}<option value={tp.id}>{tp.title}</option>{/each}
              </select>
            </label>
          </div>
          <div class="stack" style="gap:6px">
            <label class="check"><input type="radio" bind:group={form.mode} value="instances" />{t('Copies in one project: learners only get a link (the app has its own login, like n8n)')}</label>
            <label class="check"><input type="radio" bind:group={form.mode} value="accounts" />{t('One dashboard account per learner, with their own project')}</label>
          </div>
          {#if form.mode === 'instances'}
            <div class="grid-2">
              <label class="field"><span>{t('Number of copies')}</span><input type="number" min="1" max="200" bind:value={form.count} /></label>
              <label class="field">
                <span>{t('Name of the copies')} <span class="faint">{t('(optional)')}</span></span>
                <input bind:value={form.prefix} placeholder={tpl?.services[0]?.name ?? 'app'} />
                <span class="hint">{t('{first} … {last}, in a new project named after the batch.', { first: `${(form.prefix || tpl?.services[0]?.name || 'app').toLowerCase()}-01`, last: `-${String(form.count).padStart(2, '0')}` })}</span>
              </label>
            </div>
          {:else}
            <label class="field">
              <span>{t('Learners')} <span class="faint">({learners.length})</span></span>
              <textarea rows="8" bind:value={form.learners} placeholder={'Nguyễn Văn An, an@example.com\nTrần Thị Bình, binh@example.com'}></textarea>
              <span class="hint">{t('One per line: name and email (comma, semicolon or tab), or just the email. Paste straight from a spreadsheet.')}</span>
            </label>
          {/if}
          {#each inputs as i (i.svc + i.key)}
            <label class="field">
              <span>{i.key}{#if !i.required} <span class="faint">{t('(optional)')}</span>{/if}</span>
              <input class="mono" bind:value={env[i.svc][i.key]} required={i.required} autocomplete="off" />
              <span class="hint">{i.hint ?? ''} {t('The same value is used for every copy.')}</span>
            </label>
          {/each}
          <div class="grid-2">
            <label class="field">
              <span>{t('Start at the same time')}</span>
              <input type="number" min="1" max="10" bind:value={form.parallel} />
              <span class="hint">{t('Copies start {n} at a time; the next ones wait until these are up. Lower is gentler on a small server.', { n: form.parallel })}</span>
            </label>
            <div class="field">
              <label class="check"><input type="checkbox" bind:checked={form.sleep} />{t('Sleep when unused')}</label>
              {#if form.sleep}
                <label class="row small" style="gap:6px">{t('after')} <input type="number" min="1" max="1440" bind:value={form.idle} style="width:80px" /> {t('minutes without a visit')}</label>
              {/if}
              <span class="hint">{t('Each copy goes to sleep once it is up and wakes in a couple of seconds on its first visit, so only the copies in use take memory.')}</span>
            </div>
          </div>
          {#if estimate}
            <div class="alert {estimate.blocked || !estimate.fits ? 'error' : estimate.warning ? 'warn' : 'info'} small">
              <div>
                {#if estimate.sleep}
                  {t('Asleep the copies reserve')} <b>{mb(estimate.need_request_mb)}</b> {t('(databases and the {n} starting) of {free} free on the workers', { n: estimate.parallel, free: mb(estimate.free_schedulable_mb) })}{#if estimate.max_awake !== undefined} · {t('about {n} can be awake at once', { n: estimate.max_awake })}{/if}.
                {:else}
                  {t('{count} × {size} reserved =', { count, size: mb(estimate.per_copy_request_mb) })} <b>{mb(estimate.need_request_mb)}</b> {t('of {free} free on the workers · up to {limit} if all are busy.', { free: mb(estimate.free_schedulable_mb), limit: mb(estimate.need_limit_mb) })}
                {/if}
                {t('Start-up peak {peak} · {free} of memory free now.', { peak: mb(estimate.startup_peak_mb), free: mb(estimate.free_memory_mb) })}
                {#if estimate.warning}<div style="margin-top:4px">{estimate.warning}</div>{/if}
              </div>
            </div>
          {/if}
          <ErrorMsg error={formError} />
          <div class="row">
            <button class="primary" type="submit" disabled={busy || !form.name.trim() || count < 1}>{#if busy}<span class="spinner"></span>{/if}{tn(count, 'Create {n} copy', 'Create {n} copies', { n: count || '' })}</button>
            {#if needForce}<button type="button" class="danger" onclick={(e) => create(e as unknown as SubmitEvent, true)} disabled={busy}>{t('Create anyway')}</button>{/if}
            <button type="button" onclick={() => ((creating = false), (needForce = false))}>{t('Cancel')}</button>
          </div>
        </div>
      </form>
    {/if}

    {#if batches === null}
      {#if error}<ErrorMsg {error} />{:else}<Loading />{/if}
    {:else if batches.length === 0 && !creating}
      <div class="card card-body muted">{t('No batches yet.')}</div>
    {:else if batches.length}
      <div class="card table-wrap">
        <table>
          <thead><tr><th>{t('Name')}</th><th>{t('Template')}</th><th>{t('Mode')}</th><th>{t('Copies')}</th><th>{t('Created')}</th><th></th></tr></thead>
          <tbody>
            {#each batches as b (b.id)}
              <tr>
                <td><a href={href(`/batches/${b.id}`)}><b>{b.name}</b></a></td>
                <td>{b.template}</td>
                <td class="small">{b.mode === 'accounts' ? t('accounts') : t('copies')}</td>
                <td class="small">
                  {total(b)}
                  {#each Object.entries(b.counts) as [k, v] (k)}<span class="faint"> · {v} {statusLabel(k)}</span>{/each}
                  {#if b.status === 'deleting'}<span class="badge red">{t('deleting…')}</span>{/if}
                </td>
                <td class="small" title={absTime(b.created_at)}>{relTime(b.created_at)}</td>
                <td class="right"><button class="sm danger" onclick={() => remove(b)} disabled={b.status === 'deleting'}>{t('Delete')}</button></td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  {/if}
</div>
