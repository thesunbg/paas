<script lang="ts">
  import { onMount } from 'svelte';
  import { api, catalog, errorMessage, type DetectResult, type GitHubRepo, type ProjectDetail, type ServiceInput, type ServiceType, type Size, type TeamGitHub } from '../lib/api';
  import { href, navigate } from '../lib/router.svelte';
  import { session } from '../lib/session.svelte';
  import { mb, cpu, relTime, plural } from '../lib/format';
  import { rowsToMap, validateRows, type EnvRow } from '../lib/env';
  import { toast } from '../lib/toast.svelte';
  import ErrorMsg from '../components/ErrorMsg.svelte';
  import Loading from '../components/Loading.svelte';
  import Icon from '../components/Icon.svelte';
  import EnvEditor from '../components/EnvEditor.svelte';
  import EnvInputs from '../components/EnvInputs.svelte';
  import CronField from '../components/CronField.svelte';
  import AutoscaleFields from '../components/AutoscaleFields.svelte';
  import { parseCron } from '../lib/cron';
  import { t, tn } from '../lib/i18n.svelte';

  let { projectId }: { projectId: string } = $props();

  let project = $state<ProjectDetail | null>(null);
  let sizes = $state<Size[]>([]);
  let loadError = $state('');

  // Form state
  let step = $state(0);
  let sourceKind = $state<'git' | 'image'>('git');
  let repo = $state('');
  let branch = $state('main');
  let rootDir = $state('');
  let gitToken = $state('');
  let image = $state('');
  let type = $state<ServiceType>('web');
  let name = $state('');
  let nameTouched = $state(false);
  let size = $state('small');
  let port = $state(8080);
  let sleepEnabled = $state(false);
  let idleMinutes = $state(15);
  let envRows = $state<EnvRow[]>([]);
  let domain = $state('');
  let autoDeploy = $state(true);
  let schedule = $state('*/15 * * * *');
  let jobCommand = $state('');
  let jobTimeout = $state(3600);
  let previews = $state(false);
  let autoscale = $state({ enabled: false, min: 1, max: 3, target_cpu_percent: 70, target_rps: 0 });

  let busy = $state(false);
  let error = $state('');

  // GitHub App: pick a repository instead of typing a URL.
  let gh = $state<TeamGitHub | null>(null);
  let gitMode = $state<'github' | 'url'>('github');
  let repos = $state<GitHubRepo[] | null>(null);
  let reposError = $state('');
  let repoQuery = $state('');
  let picked = $state<GitHubRepo | null>(null);
  let branches = $state<string[]>([]);

  // Auto-detection of language, framework, type and port.
  let detected = $state<DetectResult | null>(null);
  let detectKey = $state('');
  let detecting = $state(false);
  let detectError = $state('');
  let typeTouched = $state(false);
  let portTouched = $state(false);

  onMount(async () => {
    try {
      const [p, s] = await Promise.all([api.project(projectId), catalog()]);
      project = p;
      sizes = s.sizes;
    } catch (e) {
      loadError = errorMessage(e);
      return;
    }
    try {
      gh = await api.teamGitHub(project.project.team_id, `/#/projects/${projectId}/new-service`);
      if (gh.installations.length) await loadRepos();
      else if (!gh.configured) gitMode = 'url';
    } catch {
      gitMode = 'url';
    }
    const params = new URLSearchParams(location.hash.split('?')[1] ?? '');
    if (params.get('github') === 'connected') toast.success(t('GitHub account connected'));
    if (params.get('github_error')) error = params.get('github_error')!;
  });

  async function loadRepos() {
    try {
      const r = await api.githubRepos(project!.project.team_id);
      repos = r.repos;
      reposError = (r.errors ?? []).join('; ');
    } catch (e) {
      reposError = errorMessage(e);
      repos = [];
    }
  }

  const filteredRepos = $derived(
    (repos ?? [])
      .filter((r) => !repoQuery || r.full_name.toLowerCase().includes(repoQuery.toLowerCase()))
      .sort((a, b) => (b.pushed_at ?? '').localeCompare(a.pushed_at ?? ''))
      .slice(0, 50),
  );

  async function pick(r: GitHubRepo) {
    picked = r;
    repo = r.clone_url;
    branch = r.default_branch || 'main';
    branches = [branch];
    error = '';
    detect();
    try {
      const b = await api.githubBranches(project!.project.team_id, r.installation_id, r.full_name);
      branches = b.branches.length ? b.branches : [branch];
    } catch {
      /* keep the default branch */
    }
  }

  function unpick() {
    picked = null;
    repo = '';
    detected = null;
    detectKey = '';
  }

  const installationId = $derived(sourceKind === 'git' && gitMode === 'github' && picked ? picked.installation_id : undefined);

  async function detect() {
    if (sourceKind !== 'git' || !repo.trim()) return;
    const key = [repo.trim(), branch.trim(), rootDir.trim(), installationId ?? '', gitToken ? 't' : ''].join('|');
    if (key === detectKey && (detected || detecting)) return;
    detectKey = key;
    detecting = true;
    detectError = '';
    try {
      const r = await api.detect(projectId, {
        repo: repo.trim(),
        branch: branch.trim() || 'main',
        root_dir: rootDir.trim() || undefined,
        git_token: !installationId && gitToken.trim() ? gitToken.trim() : undefined,
        installation_id: installationId,
      });
      if (key !== detectKey) return;
      detected = r;
      if (!typeTouched && r.suggested_type) type = r.suggested_type;
      if (!portTouched && r.port) port = r.port;
    } catch (e) {
      if (key !== detectKey) return;
      detected = null;
      detectError = errorMessage(e);
    } finally {
      if (key === detectKey) detecting = false;
    }
  }

  // Suggest a service name from the repo or image.
  $effect(() => {
    if (nameTouched) return;
    let base = '';
    if (sourceKind === 'git' && repo) {
      base = repo.replace(/\.git$/, '').replace(/\/+$/, '').split('/').pop() ?? '';
      if (rootDir) base = rootDir.replace(/\/+$/, '').split('/').pop() || base;
    } else if (sourceKind === 'image' && image) {
      base = (image.split('/').pop() ?? '').split(':')[0].split('@')[0];
    }
    name = base.toLowerCase().replace(/[^a-z0-9-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 40);
  });

  const steps = ['Source', 'Configure', 'Environment'];
  const stepLabels = $derived([t('Source'), t('Configure'), t('Environment')]);
  const selectedSize = $derived(sizes.find((s) => s.name === size));

  function stepError(i: number): string {
    if (i === 0) {
      if (sourceKind === 'git') {
        if (gitMode === 'github' && !picked) return t('Choose a repository');
        if (!repo.trim()) return t('Enter a Git repository URL');
        if (!/^(https?:\/\/|git@|ssh:\/\/|file:\/\/)/.test(repo.trim())) return t('Repository URL should start with https://, git@ or ssh://');
      } else if (!image.trim()) return t('Enter a Docker image reference');
    }
    if (i === 1) {
      if (!name.trim()) return t('Service name is required');
      if (type === 'web' && (port < 1 || port > 65535)) return t('Port must be between 1 and 65535');
      if (type === 'web' && sleepEnabled && idleMinutes < 1) return t('Idle minutes must be at least 1');
      if (type === 'cron') {
        try {
          parseCron(schedule);
        } catch (err) {
          return t('Schedule: {error}', { error: (err as Error).message });
        }
        if (jobTimeout < 1) return t('Timeout must be at least 1 second');
      }
      if (type === 'web' && autoscale.enabled) {
        if (autoscale.min < 1 || autoscale.max < autoscale.min || autoscale.max > 50) return t('Autoscaling needs 1 ≤ min ≤ max ≤ 50');
        if (!autoscale.target_cpu_percent && !autoscale.target_rps) return t('Set a CPU or request-rate target for autoscaling');
      }
    }
    if (i === 2) {
      const v = validateRows(envRows);
      if (v) return v;
      if (domain && !/^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9-]{2,63}$/i.test(domain.trim())) return t('Invalid domain name');
    }
    return '';
  }

  function next() {
    error = stepError(step);
    if (error) return;
    if (step === 0) detect();
    step = Math.min(step + 1, steps.length - 1);
  }

  // A docker-compose file describing several services: create them all at once.
  let importing = $state(false);
  let importWarnings = $state<string[]>([]);
  let composeEnv = $state<Record<string, Record<string, string>>>({});
  const composeServices = $derived(detected?.compose_plan?.services ?? []);
  $effect(() => {
    for (const s of composeServices) if (!composeEnv[s.name]) composeEnv[s.name] = {};
  });
  // Names already used in the project: rename, or reuse an existing database.
  let renames = $state<Record<string, string>>({});
  let dbChoice = $state<Record<string, 'new' | 'existing'>>({});
  let conflictsFor = $state('');
  $effect(() => {
    const cp = detected?.compose_plan;
    if (!cp || conflictsFor === detectKey) return;
    conflictsFor = detectKey;
    const r: Record<string, string> = {};
    const c: Record<string, 'new' | 'existing'> = {};
    for (const s of cp.services) if (s.taken) r[s.name] = s.suggested ?? '';
    for (const d of cp.databases) {
      if (!d.taken) continue;
      r[d.name] = d.suggested ?? '';
      c[d.name] = 'new';
    }
    renames = r;
    dbChoice = c;
  });
  async function importAll() {
    if (!detected?.compose) return;
    importing = true;
    error = '';
    try {
      const r = await api.importRepo(projectId, {
        repo: repo.trim(),
        branch: branch.trim() || 'main',
        installation_id: installationId,
        git_token: !installationId && gitToken.trim() ? gitToken.trim() : undefined,
        compose_path: detected.compose.path,
        env: composeEnv,
        rename: Object.fromEntries(Object.entries(renames).filter(([k]) => dbChoice[k] !== 'existing')),
        existing: Object.fromEntries(
          (detected.compose_plan?.databases ?? []).filter((d) => d.existing && dbChoice[d.name] === 'existing').map((d) => [d.name, d.existing!.id]),
        ),
      });
      const n = r.services?.length ?? 0;
      const d = r.databases?.length ?? 0;
      toast.success(
        d
          ? t('Created {services} and {databases}; deploying', { services: plural(n, 'service'), databases: plural(d, 'database') })
          : t('Created {services}; deploying', { services: plural(n, 'service') }),
      );
      if (r.warnings?.length) {
        importWarnings = r.warnings;
        return;
      }
      navigate(`/projects/${projectId}`);
    } catch (err) {
      error = errorMessage(err);
    } finally {
      importing = false;
    }
  }

  // Variables from the app's .env.example: defaults and generated secrets go
  // straight into the environment, the rest is asked for.
  let exampleValues = $state<Record<string, string>>({});
  let exampleApplied = $state('');
  const exampleInputs = $derived(
    (detected?.env ?? [])
      .filter((h) => h.kind === 'required' || h.kind === 'optional')
      .map((h) => ({ key: h.key, required: h.kind === 'required', placeholder: h.value, hint: h.hint })),
  );
  const exampleNotes = $derived((detected?.env ?? []).filter((h) => h.kind === 'database' || h.kind === 'url'));
  $effect(() => {
    const env = detected?.env;
    if (!env?.length || exampleApplied === detectKey) return;
    exampleApplied = detectKey;
    const add = env.filter((h) => (h.kind === 'value' || h.kind === 'generated') && !envRows.some((r) => r.key === h.key)).map((h) => ({ key: h.key, value: h.value ?? '' }));
    envRows = [...envRows.filter((r) => r.key || r.value), ...add];
  });

  async function submit(e: SubmitEvent) {
    e.preventDefault();
    if (step < steps.length - 1) {
      next();
      return;
    }
    for (let i = 0; i < steps.length; i++) {
      const m = stepError(i);
      if (m) {
        step = i;
        error = m;
        return;
      }
    }
    error = '';
    const input: ServiceInput = {
      name: name.trim(),
      type,
      source:
        sourceKind === 'git'
          ? { repo: repo.trim(), branch: branch.trim() || 'main', root_dir: rootDir.trim() || undefined, installation_id: installationId }
          : { image: image.trim() },
      size,
      sleep: type === 'web' && sleepEnabled && !autoscale.enabled ? { enabled: true, idle_minutes: idleMinutes } : { enabled: false },
      env: { ...Object.fromEntries(Object.entries(exampleValues).filter(([k, v]) => v.trim() !== '' && !envRows.some((r) => r.key === k))), ...rowsToMap(envRows) },
      auto_deploy: sourceKind === 'git' ? autoDeploy : undefined,
      deploy: true,
    };
    if (type === 'web') {
      input.port = port;
      if (autoscale.enabled)
        input.autoscale = {
          enabled: true,
          min: autoscale.min,
          max: autoscale.max,
          target_cpu_percent: autoscale.target_cpu_percent || undefined,
          target_rps: autoscale.target_rps || undefined,
        };
      if (sourceKind === 'git' && previews) input.previews = true;
    }
    if (type === 'cron') {
      input.schedule = schedule.trim();
      input.job_timeout_sec = jobTimeout;
      if (jobCommand.trim()) input.command = [jobCommand.trim()];
    }
    if (sourceKind === 'git' && !installationId && gitToken.trim()) input.git_token = gitToken.trim();
    if ((type === 'web' || type === 'static') && domain.trim()) input.domain = domain.trim().toLowerCase();
    busy = true;
    try {
      const res = await api.createService(projectId, input);
      toast.success(t('Service {name} created', { name: res.service.name }));
      if (res.deployment) navigate(`/services/${res.service.id}/deployments/${res.deployment.id}`);
      else navigate(`/services/${res.service.id}`);
    } catch (err) {
      error = errorMessage(err);
    } finally {
      busy = false;
    }
  }

  const types: { id: ServiceType; label: string; desc: string; icon: string }[] = $derived([
    { id: 'web', label: t('Web service'), desc: t('HTTP server on a port, gets a public URL'), icon: 'box' },
    { id: 'worker', label: t('Worker'), desc: t('Background process, no public URL'), icon: 'zap' },
    { id: 'static', label: t('Static site'), desc: t('Built files served from the edge'), icon: 'globe' },
    { id: 'cron', label: t('Cron job'), desc: t('Runs a command on a schedule'), icon: 'clock' },
  ]);
</script>

<div class="page narrow">
  {#if loadError}
    <ErrorMsg error={loadError} />
  {:else if !project}
    <Loading rows={3} />
  {:else}
    <div class="crumbs">
      <a href={href('/')}>{t('Projects')}</a><Icon name="chevron" size={12} />
      <a href={href(`/projects/${projectId}`)}>{project.project.name}</a><Icon name="chevron" size={12} />
      <span>{t('New service')}</span>
    </div>
    <div class="page-head">
      <div>
        <h1>{t('New service')}</h1>
        <p class="sub">{t('Deploy from a Git repository or a container image into')} <b>{project.project.name}</b>.</p>
      </div>
    </div>

    <ol class="steps" aria-label={t('Steps')}>
      {#each steps as s, i (s)}
        <li class:done={i < step} class:cur={i === step}>
          <button type="button" class="ghost" onclick={() => (i <= step || !stepError(step)) && (step = i)} aria-current={i === step ? 'step' : undefined}>
            <span class="n">{#if i < step}<Icon name="check" size={12} />{:else}{i + 1}{/if}</span>{stepLabels[i]}
          </button>
        </li>
      {/each}
    </ol>

    <form class="card" onsubmit={submit} novalidate>
      <div class="card-body stack-lg">
        {#if step === 0}
          <div class="seg" role="radiogroup" aria-label={t('Source type')}>
            <label class:sel={sourceKind === 'git'}><input type="radio" bind:group={sourceKind} value="git" /><Icon name="git" />{t('Git repository')}</label>
            <label class:sel={sourceKind === 'image'}><input type="radio" bind:group={sourceKind} value="image" /><Icon name="layers" />{t('Docker image')}</label>
          </div>
          {#if sourceKind === 'git'}
            {#if gh}
              <div class="tabs-inline">
                <button type="button" class="ghost sm" class:on={gitMode === 'github'} onclick={() => (gitMode = 'github')}><Icon name="git" size={13} />{t('From GitHub')}</button>
                <button type="button" class="ghost sm" class:on={gitMode === 'url'} onclick={() => ((gitMode = 'url'), unpick())}>{t('Any Git URL')}</button>
              </div>
            {/if}
            {#if gitMode === 'github'}
              {#if !gh?.installations.length}
                <div class="connect">
                  <p><b>{t('Connect your GitHub account')}</b> {t('to pick repositories from a list. Private repositories, push deploys and PR previews then work automatically.')}</p>
                  {#if gh?.install_url}
                    <a class="btn primary" href={gh.install_url}><Icon name="git" size={14} />{t('Connect GitHub')}</a>
                  {:else}
                    <p class="hint">{t('First set up the GitHub App once in')} <a href={href('/settings/github')}>{t('Settings → GitHub')}</a>{session.me?.user.is_admin ? '' : ` ${t('(administrator only)')}`}.</p>
                  {/if}
                </div>
              {:else if picked}
                <div class="picked">
                  <Icon name="git" size={16} />
                  <div class="grow">
                    <b>{picked.full_name}</b>{#if picked.private}<span class="badge">{t('private')}</span>{/if}
                    {#if picked.description}<div class="faint small">{picked.description}</div>{/if}
                  </div>
                  <button type="button" class="sm" onclick={unpick}>{t('Change')}</button>
                </div>
                <div class="grid-2">
                  <label class="field">
                    <span>{t('Branch')}</span>
                    <select bind:value={branch} onchange={detect}>
                      {#each branches as b (b)}<option value={b}>{b}</option>{/each}
                    </select>
                  </label>
                  <label class="field">
                    <span>{t('Root directory')} <span class="faint">{t('(optional)')}</span></span>
                    <input class="mono" bind:value={rootDir} onchange={detect} placeholder="apps/web" spellcheck="false" />
                    <span class="hint">{t('For monorepos. Only pushes touching this folder redeploy.')}</span>
                  </label>
                </div>
              {:else}
                <div class="repo-pick">
                  <input bind:value={repoQuery} placeholder={t('Search repositories…')} autocomplete="off" spellcheck="false" />
                  {#if reposError}<ErrorMsg error={reposError} />{/if}
                  {#if repos === null}
                    <Loading rows={3} />
                  {:else if filteredRepos.length === 0}
                    <p class="muted small" style="padding:10px">{repoQuery ? t('No repositories match.') : t('No repositories granted yet.')}</p>
                  {:else}
                    <ul>
                      {#each filteredRepos as r (r.installation_id + '/' + r.id)}
                        <li>
                          <button type="button" class="ghost" onclick={() => pick(r)}>
                            <Icon name="git" size={14} />
                            <span class="grow"><b>{r.full_name}</b>{#if r.private}<span class="badge">{t('private')}</span>{/if}<br /><span class="faint small">{r.language || '—'}{r.pushed_at ? ' · ' + t('updated {time}', { time: relTime(r.pushed_at) }) : ''}</span></span>
                            <span class="btn sm">{t('Import')}</span>
                          </button>
                        </li>
                      {/each}
                    </ul>
                  {/if}
                  {#if gh?.install_url}
                    <p class="hint">{t('Missing a repository?')} <a href={gh.install_url}>{t('Adjust GitHub App permissions')}</a>.</p>
                  {/if}
                </div>
              {/if}
            {:else}
            <label class="field">
              <span>{t('Repository URL')}</span>
              <input class="mono" bind:value={repo} onchange={detect} placeholder="https://github.com/acme/app" autocomplete="off" spellcheck="false" />
            </label>
            <div class="grid-2">
              <label class="field">
                <span>{t('Branch')}</span>
                <input class="mono" bind:value={branch} onchange={detect} placeholder="main" spellcheck="false" />
              </label>
              <label class="field">
                <span>{t('Root directory')} <span class="faint">{t('(optional)')}</span></span>
                <input class="mono" bind:value={rootDir} placeholder="apps/web" spellcheck="false" />
                <span class="hint">{t('For monorepos. Only pushes touching this folder redeploy.')}</span>
              </label>
            </div>
            <label class="field">
              <span>{t('Access token')} <span class="faint">{t('(optional)')}</span></span>
              <input type="password" class="mono" bind:value={gitToken} placeholder="ghp_…" autocomplete="off" />
              <span class="hint">{t('Needed for private repositories; also used to post GitHub commit statuses.')}</span>
            </label>
            {/if}
            {#if detecting}
              <div class="detect"><span class="spinner"></span>{t('Analyzing repository…')}</div>
            {:else if detected}
              <div class="detect ok"><Icon name="check" size={14} />{t('Detected')} <b>{detected.summary || detected.language}</b></div>
              {#if detected.compose}
                <div class="compose">
                  <div>
                    <b>{detected.compose.path}</b> {tn(detected.compose.services.length, 'describes {n} service:', 'describes {n} services:')}
                    {#each detected.compose.services as s, i (s)}<code>{s}</code>{i < detected.compose.services.length - 1 ? ', ' : ''}{/each}.
                    <br /><span class="faint small">{t('Services with a')} <code>build</code> {t('section are built from this repository with their Dockerfile; database containers become managed databases, linked automatically. Secrets the file expects are generated.')}</span>
                  </div>
                  {#if detected.compose_plan}
                    {@const cp = detected.compose_plan}
                    <ul class="plan">
                      {#each cp.services as ps (ps.name)}
                        <li>
                          <div class="row nowrap">
                            <Icon name={ps.public ? 'globe' : 'box'} size={14} />
                            <b>{ps.name}</b>
                            <span class="faint small mono truncate">{ps.dockerfile ?? ps.image}{ps.port ? ` · :${ps.port}` : ''} · {ps.public ? t('public') : t('private')}{ps.depends_on?.length ? ` · ${t('after {names}', { names: ps.depends_on.join(', ') })}` : ''}</span>
                          </div>
                          {#if ps.taken}
                            <div class="conflict small">
                              <span>{t('A service named')} <b>{ps.name}</b> {t('already exists in this project. Create this one as')}</span>
                              <input class="mono name-in" bind:value={renames[ps.name]} aria-label={t('New name for {name}', { name: ps.name })} spellcheck="false" disabled={importing} />
                              {#if ps.reached_by?.length}<span class="faint">{tn(ps.reached_by.length, '{names} calls it', '{names} call it', { names: ps.reached_by.join(', ') })} <code>{ps.name}</code> {t('in their config: update that too, or delete the existing service first.')}</span>{/if}
                            </div>
                          {/if}
                          {#if ps.links && Object.keys(ps.links).length}
                            <div class="faint small">{#each Object.entries(ps.links) as [k, db] (k)}<code>{k}</code> ← {t('database')} <b>{db}</b> {/each}</div>
                          {/if}
                          {#if ps.inputs.length && composeEnv[ps.name]}
                            <EnvInputs inputs={ps.inputs} bind:values={composeEnv[ps.name]} disabled={importing} />
                          {/if}
                        </li>
                      {/each}
                      {#each cp.databases as d (d.name)}
                        <li>
                          <div class="row nowrap"><Icon name="database" size={14} /><b>{d.name}</b><span class="faint small">{t('managed {engine}', { engine: d.engine + (d.version ? ' ' + d.version : '') })}</span></div>
                          {#if d.taken}
                            <div class="conflict small">
                              <span>{t('A database named')} <b>{d.name}</b> {t('already exists in this project.')}</span>
                              {#if d.existing}
                                <label class="opt"><input type="radio" bind:group={dbChoice[d.name]} value="existing" disabled={importing} />{t('Use the existing')} <b>{d.existing.name}</b> ({d.existing.engine} {d.existing.version}) {t('— the services share its data')}</label>
                              {/if}
                              <label class="opt">
                                {#if d.existing}<input type="radio" bind:group={dbChoice[d.name]} value="new" disabled={importing} />{/if}
                                {t('Create a new one named')}
                                <input class="mono name-in" bind:value={renames[d.name]} aria-label={t('New name for database {name}', { name: d.name })} spellcheck="false" disabled={importing || dbChoice[d.name] === 'existing'} />
                              </label>
                            </div>
                          {/if}
                        </li>
                      {/each}
                    </ul>
                  {/if}
                  {#if importWarnings.length}
                    <div class="alert warn small">
                      <div><b>{t('Created, with warnings')}</b><ul>{#each importWarnings as w, i (i)}<li>{w}</li>{/each}</ul></div>
                    </div>
                    <a class="btn primary" href={href(`/projects/${projectId}`)}>{t('Open project')}</a>
                  {:else}
                    <button type="button" class="btn primary" onclick={importAll} disabled={importing}>
                      {#if importing}<span class="spinner"></span>{/if}{tn(detected.compose.services.length, 'Deploy {n} service', 'Deploy all {n} services')}
                    </button>
                  {/if}
                </div>
              {/if}
            {:else if detectError}
              <div class="detect warn">{t('Could not analyze the repository: {error}', { error: detectError })}</div>
            {/if}
            <label class="check"><input type="checkbox" bind:checked={autoDeploy} />{t('Deploy automatically on push')}{installationId ? '' : ' (webhook)'}</label>
          {:else}
            <label class="field">
              <span>{t('Image')}</span>
              <input class="mono" bind:value={image} placeholder="ghcr.io/acme/app:1.2.0" autocomplete="off" spellcheck="false" />
              <span class="hint">{t('Any public registry image reference.')}</span>
            </label>
          {/if}
        {:else if step === 1}
          {#if sourceKind === 'git'}
            {#if detecting}
              <div class="detect"><span class="spinner"></span>{t('Analyzing repository to suggest settings…')}</div>
            {:else if detected}
              <div class="detect ok">
                <Icon name="zap" size={14} />
                <span>{t('Detected')} <b>{detected.summary || detected.language}</b>{detected.commit_sha ? ` ${t('at {sha}', { sha: detected.commit_sha.slice(0, 7) })}` : ''} — {detected.port && type === 'web' ? t('type and port preselected.') : t('type preselected.')}{#if detected.prisma}<br />Prisma ({detected.prisma.provider}): <code>{detected.prisma.url_env || 'DATABASE_URL'}</code> {detected.prisma.provider === 'sqlite' ? t('is set up automatically with a persistent volume,') : t('is set up automatically with a managed database,')} {detected.prisma.migrations ? t('and migrations run before the app starts.') : t('and the schema is pushed before the app starts.')}{/if}</span>
              </div>
            {:else if detectError}
              <div class="detect warn">{t('Could not analyze the repository: {error}', { error: detectError })}</div>
            {/if}
          {/if}
          <fieldset>
            <legend class="label">{t('Service type')}</legend>
            <div class="types">
              {#each types as ty (ty.id)}
                <label class="type" class:sel={type === ty.id}>
                  <input type="radio" bind:group={type} value={ty.id} onchange={() => (typeTouched = true)} />
                  <Icon name={ty.icon} size={18} />
                  <span><b>{ty.label}</b><br /><span class="faint small">{ty.desc}</span></span>
                </label>
              {/each}
            </div>
          </fieldset>
          <div class="grid-2">
            <label class="field">
              <span>{t('Name')}</span>
              <input bind:value={name} oninput={() => (nameTouched = true)} placeholder="api" maxlength="40" />
              {#if type !== 'cron'}<span class="hint">{t('Internal host:')} <code>{(name || 'name').toLowerCase()}.{project.project.slug}.internal</code></span>{/if}
            </label>
            {#if type === 'web'}
              <label class="field">
                <span>{t('Port')}</span>
                <input type="number" bind:value={port} min="1" max="65535" oninput={() => (portTouched = true)} />
                <span class="hint">{t('The port your app listens on')} ({t('also exported as')} <code>PORT</code>).</span>
              </label>
            {/if}
          </div>
          <fieldset>
            <legend class="label">{t('Size')}</legend>
            <div class="sizes">
              {#each sizes as s (s.name)}
                <label class="size" class:sel={size === s.name}>
                  <input type="radio" bind:group={size} value={s.name} />
                  <b>{s.name}</b>
                  <span class="small">{mb(s.memory_limit_mb)}</span>
                  <span class="faint small">{cpu(s.cpu_limit_millis)}</span>
                </label>
              {/each}
            </div>
            {#if selectedSize}
              <p class="hint" style="margin-top:6px">
                {t('Limit {memory} RAM / {cpu} CPU · reserved {memoryReserved} / {cpuReserved} (burstable).', {
                  memory: mb(selectedSize.memory_limit_mb),
                  cpu: cpu(selectedSize.cpu_limit_millis),
                  memoryReserved: mb(selectedSize.memory_request_mb),
                  cpuReserved: cpu(selectedSize.cpu_request_millis),
                })}
                {#if type === 'static'}{t('Used for the build only.')}{/if}
              </p>
            {/if}
          </fieldset>
          {#if type === 'cron'}
            <div class="sleep stack">
              <CronField bind:value={schedule} />
              <label class="field">
                <span>{t('Command')} <span class="faint">{t('(optional)')}</span></span>
                <input class="mono" bind:value={jobCommand} placeholder="node scripts/report.js" spellcheck="false" />
                <span class="hint">{t('Runs with')} <code>/bin/sh -c</code>. {t("Leave empty to use the image's default command.")}</span>
              </label>
              <label class="field" style="max-width:220px">
                <span>{t('Timeout (seconds)')}</span>
                <input type="number" min="1" max="86400" bind:value={jobTimeout} />
              </label>
            </div>
          {/if}
          {#if type === 'web'}
            <AutoscaleFields bind:value={autoscale} />
            {#if sourceKind === 'git'}
              <div class="sleep">
                <label class="check"><input type="checkbox" bind:checked={previews} />{t('Preview deployments for pull requests')}</label>
                <p class="hint">{t('Each PR targeting')} <code>{branch || 'main'}</code> {t('gets its own service and URL, sleeps when idle and is deleted when the PR closes.')} {installationId ? t('Handled by the GitHub App.') : t('Requires the GitHub webhook with the pull_request event.')}</p>
              </div>
            {/if}
          {/if}
          {#if type === 'web' && !autoscale.enabled}
            <div class="sleep">
              <label class="check"><input type="checkbox" bind:checked={sleepEnabled} />{t('Sleep when idle')}</label>
              <p class="hint">{t('Stops instances after a period without requests and wakes on the next request (a cold start of a few seconds).')}</p>
              {#if sleepEnabled}
                <label class="field" style="max-width:200px;margin-top:8px">
                  <span>{t('Idle minutes')}</span>
                  <input type="number" min="1" max="1440" bind:value={idleMinutes} />
                </label>
              {/if}
            </div>
          {/if}
        {:else}
          <div class="stack">
            <div>
              <h3>{t('Environment variables')}</h3>
              <p class="hint">{t('Encrypted at rest. Paste a .env file to import many at once.')}</p>
            </div>
            {#if detected?.env?.length}
              <div class="compose">
                <div class="small">
                  {t('Read')} <code>{detected.env_file}</code>: {exampleInputs.length ? t('defaults and generated secrets are filled in below; fill in what only you know.') : t('defaults and generated secrets are filled in below.')}
                </div>
                {#if exampleInputs.length}<EnvInputs inputs={exampleInputs} bind:values={exampleValues} />{/if}
                {#each exampleNotes as n (n.key)}
                  <div class="faint small">
                    <code>{n.key}</code>
                    {#if n.kind === 'database'}{t('expects a {engine} database: create one in the project and link it as', { engine: n.value })} <code>{n.key}</code>{detected.prisma ? ` ${t('(Prisma: done automatically)')}` : ''}.{:else}{t("points at localhost: set it to this service's public URL{suffix} after it is created.", { suffix: n.value ? ` + ${n.value}` : '' })}{/if}
                  </div>
                {/each}
              </div>
            {/if}
            <EnvEditor bind:rows={envRows} masked />
          </div>
          {#if type === 'web' || type === 'static'}
            <hr style="margin:0" />
            <label class="field">
              <span>{t('Custom domain')} <span class="faint">{t('(optional)')}</span></span>
              <input class="mono" bind:value={domain} placeholder="app.example.com" spellcheck="false" />
              <span class="hint">{t('A generated')} <code>*.{project.project.slug}…</code> {t('domain is always added.')} {t('For a custom one, point an A record to your edge node.')}</span>
            </label>
          {/if}
          <div class="summary">
            <div class="section-title" style="margin-bottom:6px">{t('Summary')}</div>
            <dl class="kv">
              <dt>{t('Source')}</dt><dd class="mono small">{sourceKind === 'git' ? `${picked ? picked.full_name : repo}@${branch || 'main'}${rootDir ? ' /' + rootDir : ''}` : image}</dd>
              {#if detected}<dt>{t('Detected')}</dt><dd>{detected.summary}</dd>{/if}
              <dt>{t('Type')}</dt><dd>{types.find((ty) => ty.id === type)?.label ?? type}</dd>
              <dt>{t('Name')}</dt><dd>{name}</dd>
              <dt>{t('Size')}</dt><dd>{size}{selectedSize ? ` (${mb(selectedSize.memory_limit_mb)}, ${cpu(selectedSize.cpu_limit_millis)})` : ''}</dd>
              {#if type === 'web'}
                <dt>{t('Port')}</dt><dd>{port}</dd>
                <dt>{t('Scaling')}</dt><dd>{autoscale.enabled ? t('autoscale {min}–{max}', { min: autoscale.min, max: autoscale.max }) : sleepEnabled ? t('sleep after {n} min idle', { n: idleMinutes }) : t('1 replica')}</dd>
                {#if previews && sourceKind === 'git'}<dt>{t('PR previews')}</dt><dd>{t('enabled')}</dd>{/if}
              {/if}
              {#if type === 'cron'}<dt>{t('Schedule')}</dt><dd class="mono small">{schedule} (UTC)</dd>{/if}
              <dt>{t('Variables')}</dt><dd>{Object.keys(rowsToMap(envRows)).length}</dd>
            </dl>
          </div>
        {/if}
        <ErrorMsg {error} />
      </div>
      <div class="actions">
        {#if step > 0}
          <button type="button" onclick={() => ((step -= 1), (error = ''))}><Icon name="arrowleft" size={14} />{t('Back')}</button>
        {:else}
          <a class="btn" href={href(`/projects/${projectId}`)}>{t('Cancel')}</a>
        {/if}
        <div class="spacer"></div>
        {#if step < steps.length - 1}
          <button type="submit" class="primary">{t('Continue')}<Icon name="chevron" size={14} /></button>
        {:else}
          <button type="submit" class="primary" disabled={busy}>{#if busy}<span class="spinner"></span>{:else}<Icon name="rocket" size={14} />{/if}{t('Create and deploy')}</button>
        {/if}
      </div>
    </form>
  {/if}
</div>

<style>
  .narrow {
    max-width: 820px;
  }
  fieldset {
    border: 0;
    margin: 0;
    padding: 0;
  }
  legend {
    padding: 0;
    margin-bottom: 8px;
  }
  .steps {
    list-style: none;
    display: flex;
    gap: 4px;
    padding: 0;
    margin: 0 0 14px;
    flex-wrap: wrap;
  }
  .steps button {
    gap: 8px;
    color: var(--text-faint);
    padding: 0 10px 0 4px;
  }
  .steps li.cur button {
    color: var(--text);
  }
  .steps li.done button {
    color: var(--text-dim);
  }
  .n {
    width: 22px;
    height: 22px;
    border-radius: 50%;
    border: 1px solid var(--border-strong);
    display: inline-flex;
    align-items: center;
    justify-content: center;
    font-size: 11.5px;
  }
  .cur .n {
    background: var(--accent);
    border-color: var(--accent);
    color: #fff;
  }
  .done .n {
    background: var(--green-soft);
    border-color: transparent;
    color: var(--green);
  }
  .seg {
    display: inline-flex;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius-sm);
    padding: 3px;
    gap: 3px;
    align-self: flex-start;
    flex-wrap: wrap;
  }
  .seg label {
    display: inline-flex;
    align-items: center;
    gap: 7px;
    padding: 5px 12px;
    border-radius: 4px;
    cursor: pointer;
    color: var(--text-dim);
    font-weight: 500;
    font-size: 13px;
  }
  .seg label.sel {
    background: var(--bg-hover);
    color: var(--text);
  }
  .seg input,
  .types input,
  .sizes input {
    position: absolute;
    opacity: 0;
    pointer-events: none;
  }
  .seg label:has(input:focus-visible),
  .type:has(input:focus-visible),
  .size:has(input:focus-visible) {
    outline: 2px solid var(--accent);
    outline-offset: 1px;
  }
  .types {
    display: grid;
    grid-template-columns: repeat(4, 1fr);
    gap: 8px;
  }
  .type {
    position: relative;
    display: flex;
    gap: 10px;
    align-items: flex-start;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius-sm);
    padding: 12px;
    cursor: pointer;
    font-size: 13px;
  }
  .type.sel,
  .size.sel {
    border-color: var(--accent);
    background: var(--accent-soft);
  }
  .sizes {
    display: grid;
    grid-template-columns: repeat(6, 1fr);
    gap: 6px;
  }
  .size {
    position: relative;
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 1px;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius-sm);
    padding: 8px 4px;
    cursor: pointer;
    font-size: 13px;
  }
  .sleep {
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 12px;
    background: var(--bg-elev-2);
  }
  .tabs-inline {
    display: flex;
    gap: 4px;
  }
  .tabs-inline .on {
    background: var(--bg-hover);
    color: var(--text);
  }
  .connect,
  .picked {
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 14px;
    background: var(--bg-elev-2);
  }
  .picked {
    display: flex;
    gap: 10px;
    align-items: center;
  }
  .grow {
    flex: 1;
    min-width: 0;
    text-align: left;
  }
  .badge {
    margin-left: 6px;
  }
  .repo-pick {
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .repo-pick ul {
    list-style: none;
    margin: 0;
    padding: 0;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    max-height: 360px;
    overflow: auto;
  }
  .repo-pick li + li {
    border-top: 1px solid var(--border);
  }
  .repo-pick li button {
    width: 100%;
    height: auto;
    padding: 10px 12px;
    display: flex;
    gap: 10px;
    align-items: center;
    border-radius: 0;
    font-weight: normal;
  }
  .detect {
    display: flex;
    gap: 8px;
    align-items: center;
    font-size: 13px;
    padding: 9px 12px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border);
    background: var(--bg-elev-2);
  }
  .plan {
    list-style: none;
    margin: 0;
    padding: 0;
    width: 100%;
    display: flex;
    flex-direction: column;
    gap: 8px;
  }
  .plan li {
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding: 10px 12px;
    border: 1px solid var(--border);
    border-radius: 8px;
    background: var(--bg-input);
  }
  .conflict {
    display: flex;
    flex-direction: column;
    gap: 6px;
    padding: 8px 10px;
    border-radius: 6px;
    border: 1px solid color-mix(in srgb, var(--yellow, #eab308) 40%, var(--border));
    background: color-mix(in srgb, var(--yellow, #eab308) 8%, transparent);
  }
  .conflict .opt {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .name-in {
    width: 200px;
    height: 30px;
  }
  .compose {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 10px;
    padding: 12px 14px;
    border: 1px solid var(--border);
    border-radius: 8px;
    font-size: 13px;
  }
  .detect.ok {
    border-color: transparent;
    background: var(--green-soft);
  }
  .detect.warn {
    border-color: transparent;
    background: var(--bg-hover);
    color: var(--text-dim);
  }
  .summary {
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 12px;
    background: var(--bg-elev-2);
  }
  .actions {
    display: flex;
    gap: 8px;
    padding: 12px 16px;
    border-top: 1px solid var(--border);
    background: var(--bg-elev-2);
    border-radius: 0 0 var(--radius) var(--radius);
  }
  @media (max-width: 760px) {
    .types {
      grid-template-columns: 1fr 1fr;
    }
  }
  @media (max-width: 680px) {
    .sizes {
      grid-template-columns: repeat(3, 1fr);
    }
  }
</style>
