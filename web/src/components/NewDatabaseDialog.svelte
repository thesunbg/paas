<script lang="ts">
  import { onMount } from 'svelte';
  import { api, catalog, defaultEnvFor, errorMessage, type Engine, type ProjectService, type SizesResponse } from '../lib/api';
  import { mb, cpu } from '../lib/format';
  import { toast } from '../lib/toast.svelte';
  import { navigate } from '../lib/router.svelte';
  import Modal from './Modal.svelte';
  import ErrorMsg from './ErrorMsg.svelte';
  import { t, tn } from '../lib/i18n.svelte';

  let { open = $bindable(false), projectId, services }: { open: boolean; projectId: string; services: ProjectService[] } = $props();

  let cat = $state<SizesResponse | null>(null);
  onMount(() => {
    catalog().then((c) => (cat = c)).catch(() => {});
  });
  const dbSizes = $derived(cat?.database_sizes ?? []);

  const engineList: { id: Engine; label: string; shared: boolean }[] = [
    { id: 'postgres', label: 'PostgreSQL', shared: true },
    { id: 'mysql', label: 'MySQL', shared: true },
    { id: 'mariadb', label: 'MariaDB', shared: true },
    { id: 'redis', label: 'Redis', shared: false },
    { id: 'mongo', label: 'MongoDB', shared: false },
  ];

  let name = $state('main');
  let engine = $state<Engine>('postgres');
  let mode = $state<'shared' | 'dedicated'>('shared');
  let version = $state('');
  // End-of-life versions cannot be created (the API answers 409).
  const versionInfos = $derived((cat?.engines?.[engine]?.versions ?? []).filter((v) => v.status !== 'eol'));
  const versions = $derived(versionInfos.map((v) => v.name));
  const defaultVersion = $derived(cat?.engines?.[engine]?.default_version ?? '');
  $effect(() => {
    // Reset to the engine's default when the engine changes.
    if (!versions.includes(version)) version = defaultVersion;
  });
  let size = $state('micro');
  let link = $state<string[]>([]);
  let envName = $state('');
  let backupHours = $state(24);
  let busy = $state(false);
  let error = $state('');

  // Prefer the server's engine catalog for shared-mode support.
  const engines = $derived(engineList.map((e) => ({ ...e, shared: cat?.engines?.[e.id]?.shared ?? e.shared })));
  const sharedOK = $derived(engines.find((e) => e.id === engine)?.shared ?? false);
  $effect(() => {
    if (!sharedOK) mode = 'dedicated';
  });

  async function submit(e: SubmitEvent) {
    e.preventDefault();
    busy = true;
    error = '';
    try {
      const db = await api.createDatabase(projectId, {
        name: name.trim(),
        engine,
        version: version || undefined,
        mode,
        size: mode === 'dedicated' ? size : undefined,
        link_services: link.length ? link : undefined,
        env_name: envName.trim() || undefined,
        backup_schedule_hours: backupHours,
      });
      toast.success(t('Database {name} is provisioning', { name: db.name }));
      open = false;
      navigate(`/databases/${db.id}`);
    } catch (err) {
      error = errorMessage(err);
    } finally {
      busy = false;
    }
  }
</script>

<Modal bind:open title={t('New database')} width={560}>
  <form id="new-db" class="stack" onsubmit={submit}>
    <div class="grid-2">
      <label class="field">
        <span>{t('Name')}</span>
        <input bind:value={name} required maxlength="40" />
      </label>
      <label class="field">
        <span>{t('Engine')}</span>
        <select bind:value={engine}>
          {#each engines as e (e.id)}<option value={e.id}>{e.label}</option>{/each}
        </select>
      </label>
    </div>
    {#if versions.length}
      <label class="field">
        <span>{t('Version')}</span>
        <select bind:value={version}>
          {#each versionInfos as v (v.name)}<option value={v.name}>{v.name}{v.name === defaultVersion ? ` (${t('recommended')})` : ''}{v.status === 'ending' ? ` (${t('end of life {date}', { date: v.eol })})` : ''}</option>{/each}
        </select>
        <span class="hint">{engine === 'mariadb' ? t('MariaDB (MySQL-compatible).') + ' ' : ''}{t('Shared databases of the same version share one server; minor updates are applied on restart.')}</span>
      </label>
    {/if}
    <fieldset class="field">
      <legend class="label">{t('Mode')}</legend>
      <div class="modes">
        <label class="opt" class:sel={mode === 'shared'} class:dis={!sharedOK}>
          <input type="radio" bind:group={mode} value="shared" disabled={!sharedOK} />
          <span><b>{t('Shared')}</b><br /><span class="faint small">{t("A logical database on the cluster's shared server. Cheapest; upgrade later.")}</span></span>
        </label>
        <label class="opt" class:sel={mode === 'dedicated'}>
          <input type="radio" bind:group={mode} value="dedicated" />
          <span><b>{t('Dedicated')}</b><br /><span class="faint small">{t('Its own server container with reserved memory.')}</span></span>
        </label>
      </div>
    </fieldset>
    {#if mode === 'dedicated'}
      <label class="field">
        <span>{t('Size')}</span>
        <select bind:value={size}>
          {#each dbSizes as s (s.name)}<option value={s.name}>{s.name} — {mb(s.memory_mb)} RAM, {cpu(s.cpu_millis)}, {tn(s.max_connections, '{n} connection', '{n} connections')}</option>{/each}
        </select>
      </label>
    {/if}
    {#if services.length}
      <fieldset class="field">
        <legend class="label">{t('Link to services')}</legend>
        <div class="row" style="gap:14px">
          {#each services as s (s.id)}
            <label class="check"><input type="checkbox" bind:group={link} value={s.id} />{s.name}</label>
          {/each}
        </div>
      </fieldset>
    {/if}
    <div class="grid-2">
      <label class="field">
        <span>{t('Env variable name')}</span>
        <input class="mono" bind:value={envName} placeholder={defaultEnvFor(engine, cat)} />
        <span class="hint">{t('Injected into linked services.')}</span>
      </label>
      <label class="field">
        <span>{t('Backup every (hours)')}</span>
        <input type="number" min="0" max="720" bind:value={backupHours} />
        <span class="hint">{t('0 disables scheduled backups.')}</span>
      </label>
    </div>
    <ErrorMsg {error} />
  </form>
  {#snippet footer()}
    <button type="button" onclick={() => (open = false)}>{t('Cancel')}</button>
    <button type="submit" form="new-db" class="primary" disabled={busy || !name.trim()}>{#if busy}<span class="spinner"></span>{/if}{t('Create database')}</button>
  {/snippet}
</Modal>

<style>
  fieldset {
    border: 0;
    padding: 0;
    margin: 0;
  }
  legend {
    padding: 0;
    margin-bottom: 6px;
  }
  .modes {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 8px;
  }
  .opt {
    display: flex;
    gap: 10px;
    align-items: flex-start;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius-sm);
    padding: 10px;
    cursor: pointer;
    font-size: 13px;
  }
  .opt input {
    margin-top: 2px;
  }
  .opt.sel {
    border-color: var(--accent);
    background: var(--accent-soft);
  }
  .opt.dis {
    opacity: 0.5;
    cursor: not-allowed;
  }
  @media (max-width: 560px) {
    .modes {
      grid-template-columns: 1fr;
    }
  }
</style>
