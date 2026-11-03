<script lang="ts">
  import { onMount } from 'svelte';
  import { api, errorMessage, type SetupChecklist } from '../../lib/api';
  import { navigate } from '../../lib/router.svelte';
  import { relTime } from '../../lib/format';
  import ErrorMsg from '../../components/ErrorMsg.svelte';
  import Loading from '../../components/Loading.svelte';
  import SetupMark from '../../components/SetupMark.svelte';
  import Icon from '../../components/Icon.svelte';

  let list = $state<SetupChecklist | null>(null);
  let error = $state('');
  let busy = $state(false);

  async function load(refresh = false) {
    busy = true;
    try {
      list = await api.setupChecklist(refresh);
      error = '';
    } catch (e) {
      error = errorMessage(e);
    } finally {
      busy = false;
    }
  }
  onMount(() => load());

  async function openWizard(step?: string) {
    try {
      await api.putSetupChecklist({ dismissed: false, finished: false });
    } catch {
      /* the wizard still opens */
    }
    navigate(step ? `/setup/${step}` : '/setup');
  }

  const counts = $derived.by(() => {
    const c = { ok: 0, warn: 0, fail: 0 };
    for (const s of list?.steps ?? []) c[s.status]++;
    return c;
  });
  const checkedAt = $derived(list?.steps.map((s) => s.checked_at).sort()[0]);
</script>

<div class="card">
  <div class="card-head">
    <div>
      <h3>Setup checklist</h3>
      <p class="hint">
        DNS, HTTPS, ports, git, off-site backups and notifications, checked from the server. Also available over SSH as <code>paas doctor</code>.
      </p>
    </div>
    <div class="row nowrap">
      <button class="sm" onclick={() => load(true)} disabled={busy} aria-label="Check again">
        {#if busy}<span class="spinner"></span>{:else}<Icon name="refresh" size={13} />{/if}<span class="hide-sm">Check again</span>
      </button>
      <button class="sm primary" onclick={() => openWizard()}>Open wizard</button>
    </div>
  </div>
  {#if !list}
    <div class="card-body">{#if error}<ErrorMsg {error} />{:else}<Loading label="Running checks…" />{/if}</div>
  {:else}
    <ul class="steps">
      {#each list.steps as s (s.id)}
        <li>
          <button type="button" class="step" onclick={() => openWizard(s.id)}>
            <SetupMark status={s.status} skipped={s.skipped} />
            <span class="text">
              <b>{s.title}</b>{#if s.skipped && s.status !== 'ok'} <span class="faint small">skipped</span>{/if}
              <span class="muted small">{s.summary}</span>
            </span>
            <Icon name="chevron" size={14} />
          </button>
        </li>
      {/each}
    </ul>
    <div class="card-body foot faint small">
      {counts.ok} ok · {counts.warn} to review · {counts.fail} failing{#if checkedAt} · checked {relTime(checkedAt)}{/if}
      {#if list.finished}· wizard finished{:else if list.dismissed}· wizard skipped{/if}
    </div>
  {/if}
</div>

<style>
  .steps {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .steps li + li {
    border-top: 1px solid var(--border);
  }
  .step {
    width: 100%;
    height: auto;
    display: flex;
    align-items: center;
    justify-content: flex-start;
    gap: 12px;
    padding: 10px 16px;
    border: 0;
    border-radius: 0;
    background: transparent;
    text-align: left;
    white-space: normal;
    font-weight: 400;
    color: var(--text-faint);
  }
  .text {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 2px;
    color: var(--text);
  }
  .foot {
    border-top: 1px solid var(--border);
  }
</style>
