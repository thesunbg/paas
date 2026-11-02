<script lang="ts">
  import { onMount, untrack } from 'svelte';
  import { api, ApiError, catalog, errorMessage, type HealthCheck, type QoS, type Resources, type ServiceDetail, type ServiceInput, type Size, type Volume } from '../../lib/api';
  import { mb, cpu } from '../../lib/format';
  import { navigate, href } from '../../lib/router.svelte';
  import { toast } from '../../lib/toast.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import CopyField from '../../components/CopyField.svelte';
  import Secret from '../../components/Secret.svelte';
  import Icon from '../../components/Icon.svelte';
  import CronField from '../../components/CronField.svelte';
  import AutoscaleFields from '../../components/AutoscaleFields.svelte';
  import { parseCron } from '../../lib/cron';
  import VolumeBackups from '../../components/VolumeBackups.svelte';
  import VolumeMove from '../../components/VolumeMove.svelte';

  let { detail, reload, canEdit }: { detail: ServiceDetail; reload: () => Promise<void>; canEdit: boolean } = $props();

  // Snapshot of the configuration when the tab opened; polling must not overwrite edits.
  const s0 = untrack(() => detail.service);
  const isStatic = s0.type === 'static';
  const isWeb = s0.type === 'web';
  const isCron = s0.type === 'cron';
  const isPreview = !!s0.parent_id;
  const provider = $derived(detail.git_provider || 'github');
  const providerName = $derived({ github: 'GitHub', gitlab: 'GitLab', gitea: 'Gitea / Forgejo', bitbucket: 'Bitbucket' }[provider]);
  // Version of the service the form was filled from; sent as If-Match so a change made elsewhere is not overwritten.
  let formVersion = s0.version;

  let sizes = $state<Size[]>([]);

  // Editable copies of the current configuration (taken once; polling does not overwrite edits).
  let name = $state(s0.name);
  let kind = $state<'git' | 'image'>(s0.source.kind === 'image' ? 'image' : 'git');
  let repo = $state(s0.source.repo ?? '');
  let branch = $state(s0.source.branch ?? 'main');
  let rootDir = $state(s0.source.root_dir ?? '');
  let dockerfile = $state(s0.source.dockerfile ?? '');
  let image = $state(s0.source.image ?? '');
  let autoDeploy = $state(s0.source.auto_deploy ?? true);
  let gitToken = $state('');
  let clearToken = $state(false);

  let builder = $state(s0.build.builder ?? '');
  let installCmd = $state(s0.build.install_cmd ?? '');
  let buildCmd = $state(s0.build.build_cmd ?? '');
  let startCmd = $state(s0.build.start_cmd ?? '');
  let outputDir = $state(s0.build.output_dir ?? '');

  let size = $state('');
  let qos = $state<QoS>(s0.spec.qos ?? 'burstable');
  let res = $state<Resources>({ ...s0.spec.resources });

  let port = $state(s0.spec.port ?? 8080);
  let replicas = $state(s0.spec.replicas || 1);
  let hcType = $state(s0.spec.health_check.type || '');
  let hcPath = $state(s0.spec.health_check.path ?? '');
  let hcTimeout = $state<number | undefined>(s0.spec.health_check.timeout_sec || undefined);
  let sleepEnabled = $state(s0.spec.sleep.enabled);
  let idleMinutes = $state(s0.spec.sleep.idle_minutes ?? 15);
  let runtimeClass = $state(s0.spec.runtime_class || 'standard');
  let volumes = $state<Volume[]>((s0.spec.volumes ?? []).map((v) => ({ ...v })));
  let volBackupHours = $state(s0.spec.volume_backup_hours ?? 0);
  let spa = $state(s0.spec.spa ?? false);
  // A single-element command runs through /bin/sh -c (internal/runtime withCommand).
  const cmdString = (c: string[] | undefined) =>
    !c?.length ? '' : c.length === 1 ? c[0] : c.length === 3 && c[0] === '/bin/sh' && c[1] === '-c' ? c[2] : c.join(' ');
  let command = $state(cmdString(s0.spec.command));
  let previews = $state(!!s0.spec.previews);
  const a0 = s0.spec.autoscale;
  let autoscale = $state({
    enabled: !!a0?.enabled,
    min: a0?.min ?? 1,
    max: a0?.max ?? 3,
    target_cpu_percent: a0?.target_cpu_percent ?? (a0?.enabled ? 0 : 70),
    target_rps: a0?.target_rps ?? 0,
  });
  let schedule = $state(s0.spec.schedule ?? '*/15 * * * *');
  let jobTimeout = $state<number | undefined>(s0.spec.job_timeout_sec || undefined);

  let saving = $state(false);
  let error = $state('');

  function presetOf(r: Resources, list: Size[]): string {
    return list.find((x) => x.memory_limit_mb === r.memory_limit_mb && x.cpu_limit_millis === r.cpu_limit_millis)?.name ?? 'custom';
  }

  onMount(async () => {
    try {
      sizes = (await catalog()).sizes;
      size = presetOf(s0.spec.resources, sizes);
    } catch (e) {
      error = errorMessage(e);
    }
  });

  const initialSize = $derived(presetOf(s0.spec.resources, sizes));
  const sizeObj = $derived(sizes.find((x) => x.name === size));

  // When a preset is chosen, preview the resources it implies.
  function pickSize() {
    if (!sizeObj) return;
    res.memory_limit_mb = sizeObj.memory_limit_mb;
    res.cpu_limit_millis = sizeObj.cpu_limit_millis;
    if (qos === 'guaranteed') {
      res.memory_request_mb = sizeObj.memory_limit_mb;
      res.cpu_request_millis = sizeObj.cpu_limit_millis / 2;
    } else if (qos === 'best-effort') {
      res.memory_request_mb = 0;
      res.cpu_request_millis = 0;
    } else {
      res.memory_request_mb = sizeObj.memory_request_mb;
      res.cpu_request_millis = sizeObj.cpu_request_millis;
    }
  }

  const same = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);

  function buildPatch(): ServiceInput {
    const s = detail.service;
    const p: ServiceInput = {};
    if (name.trim() !== s.name) p.name = name.trim();

    const src =
      kind === 'git'
        ? { kind: 'git' as const, repo: repo.trim(), branch: branch.trim() || 'main', root_dir: rootDir.trim() || undefined, dockerfile: dockerfile.trim() || undefined, installation_id: s0.source.installation_id || undefined }
        : { kind: 'image' as const, image: image.trim() };
    const curSrc =
      s.source.kind === 'image'
        ? { kind: 'image', image: s.source.image }
        : { kind: 'git', repo: s.source.repo, branch: s.source.branch, root_dir: s.source.root_dir || undefined, dockerfile: s.source.dockerfile || undefined, installation_id: s.source.installation_id || undefined };
    if (!same(src, curSrc)) p.source = src;
    if (kind === 'git' && autoDeploy !== (s.source.auto_deploy ?? true)) p.auto_deploy = autoDeploy;
    if (clearToken) p.git_token = '';
    else if (gitToken.trim()) p.git_token = gitToken.trim();

    const b = {
      builder: builder || undefined,
      install_cmd: installCmd.trim() || undefined,
      build_cmd: buildCmd.trim() || undefined,
      start_cmd: startCmd.trim() || undefined,
      output_dir: outputDir.trim() || undefined,
    };
    const cb = {
      builder: s.build.builder || undefined,
      install_cmd: s.build.install_cmd || undefined,
      build_cmd: s.build.build_cmd || undefined,
      start_cmd: s.build.start_cmd || undefined,
      output_dir: s.build.output_dir || undefined,
    };
    if (!same(b, cb)) p.build = b;

    // A preset change (or a QoS change) goes through `size` + `qos`; the server derives resources.
    // Note: the API resets the size to "small" if qos is sent without size, so always send both.
    if (size !== 'custom' && (size !== initialSize || qos !== s.spec.qos)) {
      p.size = size;
      p.qos = qos;
    } else if (!same(res, s.spec.resources)) {
      p.resources = { ...res };
    }

    if (isWeb && port !== s.spec.port) p.port = port;
    if (!isStatic && !isCron && !autoscale.enabled && replicas !== s.spec.replicas) p.replicas = replicas;
    if (isWeb && !isPreview && kind === 'git' && previews !== !!s.spec.previews) p.previews = previews;
    if (isWeb) {
      const a = autoscale.enabled
        ? { enabled: true, min: autoscale.min, max: autoscale.max, target_cpu_percent: autoscale.target_cpu_percent || undefined, target_rps: autoscale.target_rps || undefined }
        : { enabled: false };
      const ca = s.spec.autoscale?.enabled
        ? { enabled: true, min: s.spec.autoscale.min, max: s.spec.autoscale.max, target_cpu_percent: s.spec.autoscale.target_cpu_percent || undefined, target_rps: s.spec.autoscale.target_rps || undefined }
        : { enabled: false };
      if (!same(a, ca)) p.autoscale = a;
    }
    if (isCron) {
      if (schedule.trim() !== (s.spec.schedule ?? '')) p.schedule = schedule.trim();
      if ((jobTimeout || 0) !== (s.spec.job_timeout_sec || 0)) p.job_timeout_sec = jobTimeout || 0;
    }
    const hc: HealthCheck = { type: (hcType || undefined) as HealthCheck['type'], path: hcPath.trim() || undefined, timeout_sec: hcTimeout || undefined };
    const chc = { type: s.spec.health_check.type || undefined, path: s.spec.health_check.path || undefined, timeout_sec: s.spec.health_check.timeout_sec || undefined };
    if (!isStatic && !isCron && !same(hc, chc)) p.health_check = hc;
    const sl = sleepEnabled && !(isWeb && autoscale.enabled) ? { enabled: true, idle_minutes: idleMinutes } : { enabled: false };
    const csl = s.spec.sleep.enabled ? { enabled: true, idle_minutes: s.spec.sleep.idle_minutes ?? 15 } : { enabled: false };
    if (isWeb && !same(sl, csl)) p.sleep = sl;
    if (!isStatic && runtimeClass !== (s.spec.runtime_class || 'standard')) p.runtime_class = runtimeClass;
    const vols = volumes.filter((v) => v.name.trim() || v.path.trim()).map((v) => ({ name: v.name.trim(), path: v.path.trim() }));
    if (!isStatic && !same(vols, s.spec.volumes ?? [])) p.volumes = vols;
    if (!isStatic && volBackupHours !== (s.spec.volume_backup_hours ?? 0)) p.volume_backup_hours = volBackupHours;
    if (isStatic && spa !== (s.spec.spa ?? false)) p.spa = spa;
    if (!isStatic && command.trim() !== cmdString(s.spec.command)) p.command = command.trim() ? [command.trim()] : [];
    return p;
  }

  const patch = $derived(buildPatch());
  const changed = $derived(Object.keys(patch).length > 0);

  async function save(e: SubmitEvent) {
    e.preventDefault();
    error = '';
    if (kind === 'git' && !repo.trim()) return void (error = 'Repository URL is required');
    if (kind === 'image' && !image.trim()) return void (error = 'Image is required');
    if (volumes.some((v) => (v.name || v.path) && (!v.name.trim() || !v.path.startsWith('/')))) return void (error = 'Volumes need a name and an absolute path');
    if (volumes.filter((v) => v.name).length && replicas > 1 && !isCron) return void (error = 'Services with volumes cannot run more than one replica');
    if (isWeb && autoscale.enabled && (autoscale.min < 1 || autoscale.max < autoscale.min || autoscale.max > 50))
      return void (error = 'Autoscaling needs 1 ≤ min ≤ max ≤ 50');
    if (isCron) {
      try {
        parseCron(schedule);
      } catch (err) {
        return void (error = 'Schedule: ' + (err as Error).message);
      }
    }
    saving = true;
    try {
      const r = await api.updateService(detail.service.id, patch, formVersion);
      formVersion = r.service.version;
      gitToken = '';
      clearToken = false;
      await reload();
      if (r.deployment) {
        toast.success('Saved — rolling restart started');
        navigate(`/services/${detail.service.id}/deployments/${r.deployment.id}`);
      } else {
        toast.success('Settings saved' + (patch.source || patch.build ? ' — deploy to apply source/build changes' : ''));
      }
    } catch (err) {
      error = errorMessage(err);
      if (err instanceof ApiError && err.status === 412) await reload();
    } finally {
      saving = false;
    }
  }
</script>

<div class="layout">
  <form class="stack-lg" onsubmit={save}>
    <fieldset class="card" disabled={!canEdit}>
      <legend class="card-head"><h3>General</h3></legend>
      <div class="card-body stack">
        <label class="field">
          <span>Name</span>
          <input bind:value={name} maxlength="40" />
          <span class="hint">Renaming keeps the slug <code>{detail.service.slug}</code> and existing domains.</span>
        </label>
      </div>
    </fieldset>

    <fieldset class="card" disabled={!canEdit}>
      <legend class="card-head"><h3>Source</h3></legend>
      <div class="card-body stack">
        <div class="row" role="radiogroup" aria-label="Source type">
          <label class="check"><input type="radio" bind:group={kind} value="git" />Git repository</label>
          <label class="check"><input type="radio" bind:group={kind} value="image" />Docker image</label>
        </div>
        {#if kind === 'git'}
          <label class="field"><span>Repository URL</span><input class="mono" bind:value={repo} spellcheck="false" /></label>
          <div class="grid-3">
            <label class="field"><span>Branch</span><input class="mono" bind:value={branch} /></label>
            <label class="field"><span>Root directory</span><input class="mono" bind:value={rootDir} placeholder="/" /></label>
            <label class="field"><span>Dockerfile</span><input class="mono" bind:value={dockerfile} placeholder="auto-detect" /></label>
          </div>
          {#if s0.source.installation_id}
          <p class="hint">Connected through the GitHub App — no token needed.</p>
          {:else}
          <label class="field">
            <span>Access token</span>
            <input type="password" class="mono" bind:value={gitToken} placeholder={detail.service.has_git_token ? '•••••••• (set — leave blank to keep)' : 'not set'} autocomplete="off" disabled={clearToken} />
            <span class="hint">Optional when the team has a credential for this host (Settings → <a href={href('/settings/git')}>Git hosts</a>).</span>
          </label>
          {#if detail.service.has_git_token}<label class="check small"><input type="checkbox" bind:checked={clearToken} />Remove the stored token</label>{/if}
          {/if}
          <label class="check small"><input type="checkbox" bind:checked={autoDeploy} />Deploy automatically on push</label>
        {:else}
          <label class="field"><span>Image</span><input class="mono" bind:value={image} spellcheck="false" /></label>
        {/if}
      </div>
    </fieldset>

    {#if kind === 'git'}
      <fieldset class="card" disabled={!canEdit}>
        <legend class="card-head"><h3>Build</h3></legend>
        <div class="card-body stack">
          <label class="field">
            <span>Builder</span>
            <select bind:value={builder}>
              <option value="">Auto-detect (Dockerfile if present, else language buildpack)</option>
              <option value="auto">Language buildpack</option>
              <option value="dockerfile">Dockerfile</option>
            </select>
          </label>
          <div class="grid-2">
            <label class="field"><span>Install command</span><input class="mono" bind:value={installCmd} placeholder="npm ci" /></label>
            <label class="field"><span>Build command</span><input class="mono" bind:value={buildCmd} placeholder="npm run build" /></label>
            {#if !isStatic}<label class="field"><span>Start command</span><input class="mono" bind:value={startCmd} placeholder="npm start" /></label>{/if}
            {#if isStatic || s0.type === 'web'}<label class="field"><span>Output directory</span><input class="mono" bind:value={outputDir} placeholder="dist" /></label>{/if}
          </div>
          <p class="hint">Source and build changes apply on the next deployment.</p>
        </div>
      </fieldset>
    {/if}

    <fieldset class="card" disabled={!canEdit}>
      <legend class="card-head"><h3>Resources</h3></legend>
      <div class="card-body stack">
        <div class="grid-2">
          <label class="field">
            <span>Size</span>
            <select bind:value={size} onchange={pickSize}>
              {#each sizes as x (x.name)}<option value={x.name}>{x.name} — {mb(x.memory_limit_mb)}, {cpu(x.cpu_limit_millis)}</option>{/each}
              <option value="custom">custom</option>
            </select>
          </label>
          <label class="field">
            <span>QoS class</span>
            <select bind:value={qos} onchange={pickSize} disabled={size === 'custom'}>
              <option value="guaranteed">guaranteed — reserve the full limit</option>
              <option value="burstable">burstable — reserve less, burst to the limit</option>
              <option value="best-effort">best-effort — no reservation</option>
            </select>
          </label>
        </div>
        <details open={size === 'custom'}>
          <summary class="small muted">Custom requests and limits</summary>
          <div class="grid-2" style="margin-top:10px">
            <label class="field"><span>Memory limit (MB)</span><input type="number" min="32" bind:value={res.memory_limit_mb} oninput={() => (size = 'custom')} /></label>
            <label class="field"><span>Memory request (MB)</span><input type="number" min="0" bind:value={res.memory_request_mb} oninput={() => (size = 'custom')} /></label>
            <label class="field"><span>CPU limit (millicores)</span><input type="number" min="0" step="50" bind:value={res.cpu_limit_millis} oninput={() => (size = 'custom')} /></label>
            <label class="field"><span>CPU request (millicores)</span><input type="number" min="0" step="25" bind:value={res.cpu_request_millis} oninput={() => (size = 'custom')} /></label>
          </div>
        </details>
        <p class="hint">Requests are reserved on a node for scheduling; limits are hard caps. Changes trigger a rolling restart.</p>
      </div>
    </fieldset>

    {#if !isStatic}
      <fieldset class="card" disabled={!canEdit}>
        <legend class="card-head"><h3>Runtime</h3></legend>
        <div class="card-body stack">
          {#if isCron}
            <CronField bind:value={schedule} />
            <label class="field" style="max-width:220px">
              <span>Job timeout (seconds)</span>
              <input type="number" min="1" max="86400" bind:value={jobTimeout} placeholder="default" />
            </label>
          {/if}
          {#if isWeb}
            <AutoscaleFields bind:value={autoscale} disabled={!canEdit} />
            {#if !isPreview && kind === 'git'}
              <label class="check"><input type="checkbox" bind:checked={previews} />Preview deployments for pull requests</label>
              <p class="hint" style="margin-top:-6px">Needs the webhook with pull / merge request events (see the webhook panel).</p>
            {/if}
          {/if}
          <div class="grid-3">
            {#if isWeb}<label class="field"><span>Port</span><input type="number" min="1" max="65535" bind:value={port} /></label>{/if}
            {#if !isCron}
              <label class="field">
                <span>Replicas</span>
                <input type="number" min="1" max="50" bind:value={replicas} disabled={autoscale.enabled} title={autoscale.enabled ? 'Managed by autoscaling' : ''} />
              </label>
            {/if}
            <label class="field">
              <span>Runtime class</span>
              <select bind:value={runtimeClass}>
                <option value="standard">standard (runc/crun)</option>
                <option value="sandboxed">sandboxed (gVisor)</option>
              </select>
            </label>
          </div>
          <label class="field">
            <span>{isCron ? 'Job command' : 'Start command override'} <span class="faint">(optional)</span></span>
            <input class="mono" bind:value={command} placeholder="run with the image's default command" />
            <span class="hint">Runs as <code>/bin/sh -c "…"</code>.</span>
          </label>
          {#if !isCron}
          <div class="grid-3">
            <label class="field">
              <span>Health check</span>
              <select bind:value={hcType}>
                <option value="">auto (TCP, or HTTP when a path is set)</option>
                <option value="http">HTTP</option>
                <option value="tcp">TCP</option>
                <option value="none">none</option>
              </select>
            </label>
            {#if hcType === 'http' || hcType === ''}
              <label class="field"><span>Path</span><input class="mono" bind:value={hcPath} placeholder="/" /></label>
            {/if}
            <label class="field"><span>Timeout (s)</span><input type="number" min="1" max="600" bind:value={hcTimeout} placeholder="90" /></label>
          </div>
          {/if}
          {#if isWeb && !autoscale.enabled}
            <div>
              <label class="check"><input type="checkbox" bind:checked={sleepEnabled} />Sleep when idle</label>
              {#if sleepEnabled}
                <label class="field" style="max-width:200px;margin-top:8px"><span>Idle minutes</span><input type="number" min="1" max="1440" bind:value={idleMinutes} /></label>
              {/if}
            </div>
          {/if}
          <div class="stack" style="gap:6px">
            <span class="label">Volumes</span>
            {#each volumes as v, i (i)}
              <div class="row nowrap">
                <input class="mono" aria-label="Volume name" placeholder="data" bind:value={v.name} />
                <input class="mono" aria-label="Mount path" placeholder="/data" bind:value={v.path} />
                <button type="button" class="icon ghost" aria-label="Remove volume" onclick={() => volumes.splice(i, 1)}><Icon name="trash" size={14} /></button>
              </div>
            {/each}
            <div><button type="button" class="sm" onclick={() => volumes.push({ name: '', path: '' })}><Icon name="plus" size={12} />Add volume</button></div>
            <p class="hint">Persistent node-local storage. Services with volumes run a single replica.</p>
            {#if volumes.some((v) => v.name.trim())}
              <label class="field" style="max-width:480px;margin-top:8px">
                <span>Volume backups</span>
                <select bind:value={volBackupHours} style="max-width:260px">
                  <option value={6}>Every 6 hours</option>
                  <option value={12}>Every 12 hours</option>
                  <option value={0}>Daily</option>
                  <option value={168}>Weekly</option>
                  <option value={-1}>Off</option>
                </select>
                <span class="hint">Copied to off-site storage (the last 7 are kept), so the service can be recovered on another node if its node is lost.</span>
              </label>
            {/if}
          </div>
        </div>
      </fieldset>
    {:else}
      <fieldset class="card" disabled={!canEdit}>
        <legend class="card-head"><h3>Static site</h3></legend>
        <div class="card-body">
          <label class="check"><input type="checkbox" bind:checked={spa} />Single-page app (serve index.html for unknown paths)</label>
        </div>
      </fieldset>
    {/if}

    {#if canEdit}
      <div class="savebar">
        <ErrorMsg {error} />
        <div class="row">
          <span class="faint small">{changed ? `Changed: ${Object.keys(patch).join(', ')}` : 'No changes'}</span>
          <div class="spacer"></div>
          <button type="submit" class="primary" disabled={saving || !changed}>{#if saving}<span class="spinner"></span>{/if}Save changes</button>
        </div>
      </div>
    {/if}
  </form>

  <aside class="stack-lg">
    {#if s0.source.kind !== 'image' && s0.source.installation_id}
      <div class="card">
        <div class="card-head"><h3>GitHub</h3></div>
        <div class="card-body stack">
          <p class="small">Connected through the GitHub App. Pushes to <code>{s0.source.branch || 'main'}</code>{s0.source.root_dir ? ` touching ${s0.source.root_dir}/` : ''} deploy automatically{detail.service.source.auto_deploy ? '' : ' (auto-deploy is currently off)'}; commit statuses{previews ? ' and PR previews' : ''} are reported on GitHub.</p>
        </div>
      </div>
    {:else if s0.source.kind !== 'image'}
      <div class="card">
        <div class="card-head"><h3>{providerName} webhook</h3></div>
        <div class="card-body stack">
          <div class="field"><span>{provider === 'gitlab' ? 'URL' : provider === 'bitbucket' ? 'URL' : 'Payload URL'}</span><CopyField value={detail.webhook_url} label="Webhook URL" /></div>
          <div class="field"><span>{provider === 'gitlab' ? 'Secret token' : 'Secret'}</span><Secret value={detail.webhook_secret} /></div>
          <ol class="howto small muted">
            {#if provider === 'gitlab'}
              <li>Open your project on GitLab → <b>Settings</b> → <b>Webhooks</b> → <b>Add new webhook</b>.</li>
              <li>Paste the <b>URL</b> above and the secret as <b>Secret token</b>.</li>
              <li>Trigger: <b>Push events</b>{#if previews} and <b>Merge request events</b>{/if}. Save.</li>
            {:else if provider === 'gitea'}
              <li>Open your repository on Gitea / Forgejo → <b>Settings</b> → <b>Webhooks</b> → <b>Add webhook</b> → <b>Gitea</b> (or <b>Forgejo</b>).</li>
              <li>Paste the <b>Target URL</b> above; content type <code>application/json</code>; secret: the value above.</li>
              <li>Trigger on: <b>Push</b>{#if previews} and <b>Pull request</b>{/if} events. Save.</li>
            {:else if provider === 'bitbucket'}
              <li>Open your repository on Bitbucket → <b>Repository settings</b> → <b>Webhooks</b> → <b>Add webhook</b>.</li>
              <li>Paste the <b>URL</b> above and the secret (required: unsigned deliveries are rejected).</li>
              <li>Triggers: <b>Repository push</b>{#if previews} and Pull request <b>Created</b>, <b>Updated</b>, <b>Merged</b>, <b>Declined</b>{/if}. Save.</li>
            {:else}
              <li>Open your repository on GitHub → <b>Settings</b> → <b>Webhooks</b> → <b>Add webhook</b>.</li>
              <li>Paste the <b>Payload URL</b> above.</li>
              <li>Content type: <code>application/json</code>.</li>
              <li>Secret: the value above.</li>
              <li>Events: <b>Pushes</b>{previews ? ' and ' : ''}{#if previews}<b>Pull requests</b>{/if} (“Let me select individual events”). Save.</li>
            {/if}
          </ol>
          {#if !detail.git_provider}
            <p class="hint">Self-hosted GitLab or Gitea? Add a credential for its host under Settings → <a href={href('/settings/git')}>Git hosts</a> to get the matching webhook.</p>
          {/if}
          <p class="hint">Pushes to <code>{s0.source.branch || 'main'}</code>{s0.source.root_dir ? ` touching ${s0.source.root_dir}/` : ''} deploy automatically{detail.service.source.auto_deploy ? '' : ' (auto-deploy is currently off)'}.</p>
        </div>
      </div>
    {/if}
    {#if !isStatic && (s0.spec.volumes ?? []).length > 0}
      <VolumeMove serviceId={s0.id} />
      <VolumeBackups serviceId={s0.id} {canEdit} />
    {/if}
    <div class="card">
      <div class="card-head"><h3>Identifiers</h3></div>
      <dl class="kv card-body small">
        <dt>Service ID</dt><dd class="mono">{s0.id}</dd>
        <dt>Slug</dt><dd class="mono">{s0.slug}</dd>
        <dt>Type</dt><dd>{s0.type}</dd>
      </dl>
    </div>
  </aside>
</div>

<style>
  .layout {
    display: grid;
    grid-template-columns: minmax(0, 1fr) 340px;
    gap: 20px;
    align-items: start;
  }
  fieldset {
    margin: 0;
    padding: 0;
    min-width: 0;
  }
  legend.card-head {
    float: left;
    width: 100%;
    padding: 12px 16px;
  }
  legend + .card-body {
    clear: both;
  }
  details summary {
    cursor: pointer;
  }
  .savebar {
    position: sticky;
    bottom: 12px;
    background: var(--bg-elev);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    padding: 10px 12px;
    box-shadow: var(--shadow);
    display: flex;
    flex-direction: column;
    gap: 8px;
    z-index: 5;
  }
  .howto {
    margin: 0;
    padding-left: 18px;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  @media (max-width: 1000px) {
    .layout {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
