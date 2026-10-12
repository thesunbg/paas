<script lang="ts">
  import { onMount, untrack } from 'svelte';
  import { api, errorMessage, type HealthCheck, type QoS, type Resources, type ServiceDetail, type ServiceInput, type Size, type Volume } from '../../lib/api';
  import { mb, cpu } from '../../lib/format';
  import { navigate } from '../../lib/router.svelte';
  import { toast } from '../../lib/toast.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import CopyField from '../../components/CopyField.svelte';
  import Secret from '../../components/Secret.svelte';
  import Icon from '../../components/Icon.svelte';

  let { detail, reload, canEdit }: { detail: ServiceDetail; reload: () => Promise<void>; canEdit: boolean } = $props();

  // Snapshot of the configuration when the tab opened; polling must not overwrite edits.
  const s0 = untrack(() => detail.service);
  const isStatic = s0.type === 'static';
  const isWeb = s0.type === 'web';

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
  let spa = $state(s0.spec.spa ?? false);
  let command = $state((s0.spec.command ?? []).join(' '));

  let saving = $state(false);
  let error = $state('');

  function presetOf(r: Resources, list: Size[]): string {
    return list.find((x) => x.memory_limit_mb === r.memory_limit_mb && x.cpu_limit_millis === r.cpu_limit_millis)?.name ?? 'custom';
  }

  onMount(async () => {
    try {
      sizes = (await api.sizes()).sizes;
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
        ? { kind: 'git' as const, repo: repo.trim(), branch: branch.trim() || 'main', root_dir: rootDir.trim() || undefined, dockerfile: dockerfile.trim() || undefined }
        : { kind: 'image' as const, image: image.trim() };
    const curSrc =
      s.source.kind === 'image'
        ? { kind: 'image', image: s.source.image }
        : { kind: 'git', repo: s.source.repo, branch: s.source.branch, root_dir: s.source.root_dir || undefined, dockerfile: s.source.dockerfile || undefined };
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
    if (!isStatic && replicas !== s.spec.replicas) p.replicas = replicas;
    const hc: HealthCheck = { type: (hcType || undefined) as HealthCheck['type'], path: hcPath.trim() || undefined, timeout_sec: hcTimeout || undefined };
    const chc = { type: s.spec.health_check.type || undefined, path: s.spec.health_check.path || undefined, timeout_sec: s.spec.health_check.timeout_sec || undefined };
    if (!isStatic && !same(hc, chc)) p.health_check = hc;
    const sl = sleepEnabled ? { enabled: true, idle_minutes: idleMinutes } : { enabled: false };
    const csl = s.spec.sleep.enabled ? { enabled: true, idle_minutes: s.spec.sleep.idle_minutes ?? 15 } : { enabled: false };
    if (isWeb && !same(sl, csl)) p.sleep = sl;
    if (!isStatic && runtimeClass !== (s.spec.runtime_class || 'standard')) p.runtime_class = runtimeClass;
    const vols = volumes.filter((v) => v.name.trim() || v.path.trim()).map((v) => ({ name: v.name.trim(), path: v.path.trim() }));
    if (!isStatic && !same(vols, s.spec.volumes ?? [])) p.volumes = vols;
    if (isStatic && spa !== (s.spec.spa ?? false)) p.spa = spa;
    const cmd = command.trim() ? ['/bin/sh', '-c', command.trim()] : [];
    const curCmd = s.spec.command ?? [];
    const curCmdStr = curCmd.length === 3 && curCmd[0] === '/bin/sh' && curCmd[1] === '-c' ? curCmd[2] : curCmd.join(' ');
    if (!isStatic && command.trim() !== curCmdStr) p.command = cmd;
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
    if (volumes.filter((v) => v.name).length && replicas > 1) return void (error = 'Services with volumes cannot run more than one replica');
    saving = true;
    try {
      const r = await api.updateService(detail.service.id, patch);
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
          <label class="field">
            <span>Access token</span>
            <input type="password" class="mono" bind:value={gitToken} placeholder={detail.service.has_git_token ? '•••••••• (set — leave blank to keep)' : 'not set'} autocomplete="off" disabled={clearToken} />
          </label>
          {#if detail.service.has_git_token}<label class="check small"><input type="checkbox" bind:checked={clearToken} />Remove the stored token</label>{/if}
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
          <div class="grid-3">
            {#if isWeb}<label class="field"><span>Port</span><input type="number" min="1" max="65535" bind:value={port} /></label>{/if}
            <label class="field"><span>Replicas</span><input type="number" min="1" max="50" bind:value={replicas} /></label>
            <label class="field">
              <span>Runtime class</span>
              <select bind:value={runtimeClass}>
                <option value="standard">standard (runc/crun)</option>
                <option value="sandboxed">sandboxed (gVisor)</option>
              </select>
            </label>
          </div>
          <label class="field">
            <span>Start command override <span class="faint">(optional)</span></span>
            <input class="mono" bind:value={command} placeholder="run with the image's default command" />
            <span class="hint">Runs as <code>/bin/sh -c "…"</code>.</span>
          </label>
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
          {#if isWeb}
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
    {#if s0.source.kind !== 'image'}
      <div class="card">
        <div class="card-head"><h3>GitHub webhook</h3></div>
        <div class="card-body stack">
          <div class="field"><span>Payload URL</span><CopyField value={detail.webhook_url} label="Webhook URL" /></div>
          <div class="field"><span>Secret</span><Secret value={detail.webhook_secret} /></div>
          <ol class="howto small muted">
            <li>Open your repository on GitHub → <b>Settings</b> → <b>Webhooks</b> → <b>Add webhook</b>.</li>
            <li>Paste the <b>Payload URL</b> above.</li>
            <li>Content type: <code>application/json</code>.</li>
            <li>Secret: the value above.</li>
            <li>Events: <b>Just the push event</b>. Save.</li>
          </ol>
          <p class="hint">Pushes to <code>{s0.source.branch || 'main'}</code>{s0.source.root_dir ? ` touching ${s0.source.root_dir}/` : ''} deploy automatically{detail.service.source.auto_deploy ? '' : ' (auto-deploy is currently off)'}.</p>
        </div>
      </div>
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
