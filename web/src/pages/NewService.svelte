<script lang="ts">
  import { onMount } from 'svelte';
  import { api, catalog, errorMessage, type DetectResult, type GitHubRepo, type ProjectDetail, type ServiceInput, type ServiceType, type Size, type TeamGitHub } from '../lib/api';
  import { href, navigate } from '../lib/router.svelte';
  import { session } from '../lib/session.svelte';
  import { mb, cpu, relTime } from '../lib/format';
  import { rowsToMap, validateRows, type EnvRow } from '../lib/env';
  import { toast } from '../lib/toast.svelte';
  import ErrorMsg from '../components/ErrorMsg.svelte';
  import Loading from '../components/Loading.svelte';
  import Icon from '../components/Icon.svelte';
  import EnvEditor from '../components/EnvEditor.svelte';
  import CronField from '../components/CronField.svelte';
  import AutoscaleFields from '../components/AutoscaleFields.svelte';
  import { parseCron } from '../lib/cron';

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
    if (params.get('github') === 'connected') toast.success('GitHub account connected');
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
  const selectedSize = $derived(sizes.find((s) => s.name === size));

  function stepError(i: number): string {
    if (i === 0) {
      if (sourceKind === 'git') {
        if (gitMode === 'github' && !picked) return 'Choose a repository';
        if (!repo.trim()) return 'Enter a Git repository URL';
        if (!/^(https?:\/\/|git@|ssh:\/\/|file:\/\/)/.test(repo.trim())) return 'Repository URL should start with https://, git@ or ssh://';
      } else if (!image.trim()) return 'Enter a Docker image reference';
    }
    if (i === 1) {
      if (!name.trim()) return 'Service name is required';
      if (type === 'web' && (port < 1 || port > 65535)) return 'Port must be between 1 and 65535';
      if (type === 'web' && sleepEnabled && idleMinutes < 1) return 'Idle minutes must be at least 1';
      if (type === 'cron') {
        try {
          parseCron(schedule);
        } catch (err) {
          return 'Schedule: ' + (err as Error).message;
        }
        if (jobTimeout < 1) return 'Timeout must be at least 1 second';
      }
      if (type === 'web' && autoscale.enabled) {
        if (autoscale.min < 1 || autoscale.max < autoscale.min || autoscale.max > 50) return 'Autoscaling needs 1 ≤ min ≤ max ≤ 50';
        if (!autoscale.target_cpu_percent && !autoscale.target_rps) return 'Set a CPU or request-rate target for autoscaling';
      }
    }
    if (i === 2) {
      const v = validateRows(envRows);
      if (v) return v;
      if (domain && !/^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9-]{2,63}$/i.test(domain.trim())) return 'Invalid domain name';
    }
    return '';
  }

  function next() {
    error = stepError(step);
    if (error) return;
    if (step === 0) detect();
    step = Math.min(step + 1, steps.length - 1);
  }

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
      env: rowsToMap(envRows),
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
      toast.success(`Service ${res.service.name} created`);
      if (res.deployment) navigate(`/services/${res.service.id}/deployments/${res.deployment.id}`);
      else navigate(`/services/${res.service.id}`);
    } catch (err) {
      error = errorMessage(err);
    } finally {
      busy = false;
    }
  }

  const types: { id: ServiceType; label: string; desc: string; icon: string }[] = [
    { id: 'web', label: 'Web service', desc: 'HTTP server on a port, gets a public URL', icon: 'box' },
    { id: 'worker', label: 'Worker', desc: 'Background process, no public URL', icon: 'zap' },
    { id: 'static', label: 'Static site', desc: 'Built files served from the edge', icon: 'globe' },
    { id: 'cron', label: 'Cron job', desc: 'Runs a command on a schedule', icon: 'clock' },
  ];
</script>

<div class="page narrow">
  {#if loadError}
    <ErrorMsg error={loadError} />
  {:else if !project}
    <Loading rows={3} />
  {:else}
    <div class="crumbs">
      <a href={href('/')}>Projects</a><Icon name="chevron" size={12} />
      <a href={href(`/projects/${projectId}`)}>{project.project.name}</a><Icon name="chevron" size={12} />
      <span>New service</span>
    </div>
    <div class="page-head">
      <div>
        <h1>New service</h1>
        <p class="sub">Deploy from a Git repository or a container image into <b>{project.project.name}</b>.</p>
      </div>
    </div>

    <ol class="steps" aria-label="Steps">
      {#each steps as s, i (s)}
        <li class:done={i < step} class:cur={i === step}>
          <button type="button" class="ghost" onclick={() => (i <= step || !stepError(step)) && (step = i)} aria-current={i === step ? 'step' : undefined}>
            <span class="n">{#if i < step}<Icon name="check" size={12} />{:else}{i + 1}{/if}</span>{s}
          </button>
        </li>
      {/each}
    </ol>

    <form class="card" onsubmit={submit} novalidate>
      <div class="card-body stack-lg">
        {#if step === 0}
          <div class="seg" role="radiogroup" aria-label="Source type">
            <label class:sel={sourceKind === 'git'}><input type="radio" bind:group={sourceKind} value="git" /><Icon name="git" />Git repository</label>
            <label class:sel={sourceKind === 'image'}><input type="radio" bind:group={sourceKind} value="image" /><Icon name="layers" />Docker image</label>
          </div>
          {#if sourceKind === 'git'}
            {#if gh}
              <div class="tabs-inline">
                <button type="button" class="ghost sm" class:on={gitMode === 'github'} onclick={() => (gitMode = 'github')}><Icon name="git" size={13} />From GitHub</button>
                <button type="button" class="ghost sm" class:on={gitMode === 'url'} onclick={() => ((gitMode = 'url'), unpick())}>Any Git URL</button>
              </div>
            {/if}
            {#if gitMode === 'github'}
              {#if !gh?.installations.length}
                <div class="connect">
                  <p><b>Connect your GitHub account</b> to pick repositories from a list. Private repositories, push deploys and PR previews then work automatically.</p>
                  {#if gh?.install_url}
                    <a class="btn primary" href={gh.install_url}><Icon name="git" size={14} />Connect GitHub</a>
                  {:else}
                    <p class="hint">First set up the GitHub App once in <a href={href('/settings/github')}>Settings → GitHub</a>{session.me?.user.is_admin ? '' : ' (administrator only)'}.</p>
                  {/if}
                </div>
              {:else if picked}
                <div class="picked">
                  <Icon name="git" size={16} />
                  <div class="grow">
                    <b>{picked.full_name}</b>{#if picked.private}<span class="badge">private</span>{/if}
                    {#if picked.description}<div class="faint small">{picked.description}</div>{/if}
                  </div>
                  <button type="button" class="sm" onclick={unpick}>Change</button>
                </div>
                <div class="grid-2">
                  <label class="field">
                    <span>Branch</span>
                    <select bind:value={branch} onchange={detect}>
                      {#each branches as b (b)}<option value={b}>{b}</option>{/each}
                    </select>
                  </label>
                  <label class="field">
                    <span>Root directory <span class="faint">(optional)</span></span>
                    <input class="mono" bind:value={rootDir} onchange={detect} placeholder="apps/web" spellcheck="false" />
                    <span class="hint">For monorepos. Only pushes touching this folder redeploy.</span>
                  </label>
                </div>
              {:else}
                <div class="repo-pick">
                  <input bind:value={repoQuery} placeholder="Search repositories…" autocomplete="off" spellcheck="false" />
                  {#if reposError}<ErrorMsg error={reposError} />{/if}
                  {#if repos === null}
                    <Loading rows={3} />
                  {:else if filteredRepos.length === 0}
                    <p class="muted small" style="padding:10px">No repositories{repoQuery ? ' match' : ' granted yet'}.</p>
                  {:else}
                    <ul>
                      {#each filteredRepos as r (r.installation_id + '/' + r.id)}
                        <li>
                          <button type="button" class="ghost" onclick={() => pick(r)}>
                            <Icon name="git" size={14} />
                            <span class="grow"><b>{r.full_name}</b>{#if r.private}<span class="badge">private</span>{/if}<br /><span class="faint small">{r.language || '—'}{r.pushed_at ? ' · updated ' + relTime(r.pushed_at) : ''}</span></span>
                            <span class="btn sm">Import</span>
                          </button>
                        </li>
                      {/each}
                    </ul>
                  {/if}
                  {#if gh?.install_url}
                    <p class="hint">Missing a repository? <a href={gh.install_url}>Adjust GitHub App permissions</a>.</p>
                  {/if}
                </div>
              {/if}
            {:else}
            <label class="field">
              <span>Repository URL</span>
              <input class="mono" bind:value={repo} placeholder="https://github.com/acme/app" autocomplete="off" spellcheck="false" />
            </label>
            <div class="grid-2">
              <label class="field">
                <span>Branch</span>
                <input class="mono" bind:value={branch} placeholder="main" spellcheck="false" />
              </label>
              <label class="field">
                <span>Root directory <span class="faint">(optional)</span></span>
                <input class="mono" bind:value={rootDir} placeholder="apps/web" spellcheck="false" />
                <span class="hint">For monorepos. Only pushes touching this folder redeploy.</span>
              </label>
            </div>
            <label class="field">
              <span>Access token <span class="faint">(optional)</span></span>
              <input type="password" class="mono" bind:value={gitToken} placeholder="ghp_…" autocomplete="off" />
              <span class="hint">Needed for private repositories; also used to post GitHub commit statuses.</span>
            </label>
            {/if}
            {#if detecting}
              <div class="detect"><span class="spinner"></span>Analyzing repository…</div>
            {:else if detected}
              <div class="detect ok"><Icon name="check" size={14} />Detected <b>{detected.summary || detected.language}</b></div>
            {:else if detectError}
              <div class="detect warn">Could not analyze the repository: {detectError}</div>
            {/if}
            <label class="check"><input type="checkbox" bind:checked={autoDeploy} />Deploy automatically on push{installationId ? '' : ' (webhook)'}</label>
          {:else}
            <label class="field">
              <span>Image</span>
              <input class="mono" bind:value={image} placeholder="ghcr.io/acme/app:1.2.0" autocomplete="off" spellcheck="false" />
              <span class="hint">Any public registry image reference.</span>
            </label>
          {/if}
        {:else if step === 1}
          {#if sourceKind === 'git'}
            {#if detecting}
              <div class="detect"><span class="spinner"></span>Analyzing repository to suggest settings…</div>
            {:else if detected}
              <div class="detect ok">
                <Icon name="zap" size={14} />
                <span>Detected <b>{detected.summary || detected.language}</b>{detected.commit_sha ? ` at ${detected.commit_sha.slice(0, 7)}` : ''} — type{detected.port && type === 'web' ? ' and port' : ''} preselected.{#if detected.prisma}<br />Prisma ({detected.prisma.provider}): <code>{detected.prisma.url_env || 'DATABASE_URL'}</code> is set up automatically{detected.prisma.provider === 'sqlite' ? ' with a persistent volume' : ' with a managed database'}, and {detected.prisma.migrations ? 'migrations run' : 'the schema is pushed'} before the app starts.{/if}</span>
              </div>
            {:else if detectError}
              <div class="detect warn">Could not analyze the repository: {detectError}</div>
            {/if}
          {/if}
          <fieldset>
            <legend class="label">Service type</legend>
            <div class="types">
              {#each types as t (t.id)}
                <label class="type" class:sel={type === t.id}>
                  <input type="radio" bind:group={type} value={t.id} onchange={() => (typeTouched = true)} />
                  <Icon name={t.icon} size={18} />
                  <span><b>{t.label}</b><br /><span class="faint small">{t.desc}</span></span>
                </label>
              {/each}
            </div>
          </fieldset>
          <div class="grid-2">
            <label class="field">
              <span>Name</span>
              <input bind:value={name} oninput={() => (nameTouched = true)} placeholder="api" maxlength="40" />
              {#if type !== 'cron'}<span class="hint">Internal host: <code>{(name || 'name').toLowerCase()}.{project.project.slug}.internal</code></span>{/if}
            </label>
            {#if type === 'web'}
              <label class="field">
                <span>Port</span>
                <input type="number" bind:value={port} min="1" max="65535" oninput={() => (portTouched = true)} />
                <span class="hint">The port your app listens on (also exported as <code>PORT</code>).</span>
              </label>
            {/if}
          </div>
          <fieldset>
            <legend class="label">Size</legend>
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
                Limit {mb(selectedSize.memory_limit_mb)} RAM / {cpu(selectedSize.cpu_limit_millis)} CPU · reserved {mb(selectedSize.memory_request_mb)} / {cpu(selectedSize.cpu_request_millis)} (burstable).
                {#if type === 'static'}Used for the build only.{/if}
              </p>
            {/if}
          </fieldset>
          {#if type === 'cron'}
            <div class="sleep stack">
              <CronField bind:value={schedule} />
              <label class="field">
                <span>Command <span class="faint">(optional)</span></span>
                <input class="mono" bind:value={jobCommand} placeholder="node scripts/report.js" spellcheck="false" />
                <span class="hint">Runs with <code>/bin/sh -c</code>. Leave empty to use the image's default command.</span>
              </label>
              <label class="field" style="max-width:220px">
                <span>Timeout (seconds)</span>
                <input type="number" min="1" max="86400" bind:value={jobTimeout} />
              </label>
            </div>
          {/if}
          {#if type === 'web'}
            <AutoscaleFields bind:value={autoscale} />
            {#if sourceKind === 'git'}
              <div class="sleep">
                <label class="check"><input type="checkbox" bind:checked={previews} />Preview deployments for pull requests</label>
                <p class="hint">Each PR targeting <code>{branch || 'main'}</code> gets its own service and URL, sleeps when idle and is deleted when the PR closes. {installationId ? 'Handled by the GitHub App.' : 'Requires the GitHub webhook with the pull_request event.'}</p>
              </div>
            {/if}
          {/if}
          {#if type === 'web' && !autoscale.enabled}
            <div class="sleep">
              <label class="check"><input type="checkbox" bind:checked={sleepEnabled} />Sleep when idle</label>
              <p class="hint">Stops instances after a period without requests and wakes on the next request (a cold start of a few seconds).</p>
              {#if sleepEnabled}
                <label class="field" style="max-width:200px;margin-top:8px">
                  <span>Idle minutes</span>
                  <input type="number" min="1" max="1440" bind:value={idleMinutes} />
                </label>
              {/if}
            </div>
          {/if}
        {:else}
          <div class="stack">
            <div>
              <h3>Environment variables</h3>
              <p class="hint">Encrypted at rest. Paste a .env file to import many at once.</p>
            </div>
            <EnvEditor bind:rows={envRows} masked />
          </div>
          {#if type === 'web' || type === 'static'}
            <hr style="margin:0" />
            <label class="field">
              <span>Custom domain <span class="faint">(optional)</span></span>
              <input class="mono" bind:value={domain} placeholder="app.example.com" spellcheck="false" />
              <span class="hint">A generated <code>*.{project.project.slug}…</code> domain is always added. For a custom one, point an A record to your edge node.</span>
            </label>
          {/if}
          <div class="summary">
            <div class="section-title" style="margin-bottom:6px">Summary</div>
            <dl class="kv">
              <dt>Source</dt><dd class="mono small">{sourceKind === 'git' ? `${picked ? picked.full_name : repo}@${branch || 'main'}${rootDir ? ' /' + rootDir : ''}` : image}</dd>
              {#if detected}<dt>Detected</dt><dd>{detected.summary}</dd>{/if}
              <dt>Type</dt><dd>{type}</dd>
              <dt>Name</dt><dd>{name}</dd>
              <dt>Size</dt><dd>{size}{selectedSize ? ` (${mb(selectedSize.memory_limit_mb)}, ${cpu(selectedSize.cpu_limit_millis)})` : ''}</dd>
              {#if type === 'web'}
                <dt>Port</dt><dd>{port}</dd>
                <dt>Scaling</dt><dd>{autoscale.enabled ? `autoscale ${autoscale.min}–${autoscale.max}` : sleepEnabled ? `sleep after ${idleMinutes} min idle` : '1 replica'}</dd>
                {#if previews && sourceKind === 'git'}<dt>PR previews</dt><dd>on</dd>{/if}
              {/if}
              {#if type === 'cron'}<dt>Schedule</dt><dd class="mono small">{schedule} (UTC)</dd>{/if}
              <dt>Variables</dt><dd>{Object.keys(rowsToMap(envRows)).length}</dd>
            </dl>
          </div>
        {/if}
        <ErrorMsg {error} />
      </div>
      <div class="actions">
        {#if step > 0}
          <button type="button" onclick={() => ((step -= 1), (error = ''))}><Icon name="arrowleft" size={14} />Back</button>
        {:else}
          <a class="btn" href={href(`/projects/${projectId}`)}>Cancel</a>
        {/if}
        <div class="spacer"></div>
        {#if step < steps.length - 1}
          <button type="submit" class="primary">Continue<Icon name="chevron" size={14} /></button>
        {:else}
          <button type="submit" class="primary" disabled={busy}>{#if busy}<span class="spinner"></span>{:else}<Icon name="rocket" size={14} />{/if}Create and deploy</button>
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
