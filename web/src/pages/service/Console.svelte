<script lang="ts">
  import { api, errorMessage, type ExecResult, type ServiceDetail } from '../../lib/api';
  import { duration } from '../../lib/format';
  import Icon from '../../components/Icon.svelte';
  import ErrorMsg from '../../components/ErrorMsg.svelte';

  let { detail }: { detail: ServiceDetail } = $props();

  interface Run {
    command: string;
    result?: ExecResult;
    error?: string;
    ms: number;
  }

  let command = $state('');
  let busy = $state(false);
  let history = $state<Run[]>([]);
  let histIdx = $state(-1);

  const running = $derived(detail.instances.filter((i) => i.status === 'running'));

  async function run(e: SubmitEvent) {
    e.preventDefault();
    const cmd = command.trim();
    if (!cmd || busy) return;
    busy = true;
    const t0 = performance.now();
    const entry: Run = { command: cmd, ms: 0 };
    try {
      entry.result = await api.exec(detail.service.id, cmd);
    } catch (err) {
      entry.error = errorMessage(err);
    } finally {
      entry.ms = performance.now() - t0;
      history.unshift(entry);
      if (history.length > 30) history.pop();
      busy = false;
      command = '';
      histIdx = -1;
    }
  }

  function onkeydown(e: KeyboardEvent) {
    if (e.key === 'ArrowUp' && history.length) {
      e.preventDefault();
      histIdx = Math.min(histIdx + 1, history.length - 1);
      command = history[histIdx].command;
    } else if (e.key === 'ArrowDown') {
      e.preventDefault();
      histIdx = Math.max(histIdx - 1, -1);
      command = histIdx >= 0 ? history[histIdx].command : '';
    }
  }
</script>

<div class="stack">
  <p class="muted small">
    Runs a one-off command with <code>/bin/sh -c</code> inside a running instance{running[0] ? ` (${running[0].container_id})` : ''}. Times out after 120 s. Commands are recorded in the audit log.
  </p>
  {#if running.length === 0}
    <div class="alert warn">No running instance{detail.service.status === 'sleeping' ? ' — the service is sleeping; send it a request to wake it' : ''}.</div>
  {/if}
  <form class="cmd" onsubmit={run}>
    <span class="prompt mono" aria-hidden="true">$</span>
    <label class="sr-only" for="cmd">Command</label>
    <input id="cmd" class="mono" bind:value={command} {onkeydown} placeholder="ls -la /app" autocomplete="off" spellcheck="false" disabled={busy} />
    <button class="primary" type="submit" disabled={busy || !command.trim() || running.length === 0}>{#if busy}<span class="spinner"></span>{:else}<Icon name="terminal" size={14} />{/if}Run</button>
  </form>

  {#each history as h, i (history.length - i)}
    <div class="card run">
      <div class="between run-head">
        <code class="truncate">$ {h.command}</code>
        <span class="row nowrap small">
          {#if h.result}
            <span class="badge {h.result.exit_code === 0 && !h.result.error ? 'green' : 'red'}">exit {h.result.exit_code}</span>
          {:else}
            <span class="badge red">error</span>
          {/if}
          <span class="faint">{duration(h.ms)}</span>
        </span>
      </div>
      {#if h.error}
        <div class="card-body"><ErrorMsg error={h.error} /></div>
      {:else if h.result}
        {#if h.result.error}<div class="card-body"><ErrorMsg error={h.result.error} /></div>{/if}
        {#if h.result.stdout}
          <div class="label out-label">stdout</div>
          <pre class="log out">{h.result.stdout}</pre>
        {/if}
        {#if h.result.stderr}
          <div class="label out-label">stderr</div>
          <pre class="log out err">{h.result.stderr}</pre>
        {/if}
        {#if !h.result.stdout && !h.result.stderr}<div class="card-body faint small">(no output)</div>{/if}
      {/if}
    </div>
  {/each}
</div>

<style>
  .cmd {
    display: flex;
    align-items: center;
    gap: 8px;
    background: var(--log-bg);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    padding: 6px 6px 6px 12px;
  }
  .cmd input {
    background: transparent;
    border: 0;
    color: #e4e4e7;
    box-shadow: none;
  }
  .prompt {
    color: var(--green);
  }
  .run-head {
    padding: 8px 12px;
    border-bottom: 1px solid var(--border);
  }
  .run-head code {
    background: none;
    border: 0;
    padding: 0;
  }
  .out-label {
    padding: 8px 12px 0;
  }
  .out {
    margin: 6px 12px 12px;
    max-height: 360px;
  }
  .out.err {
    color: #fca5a5;
  }
</style>
