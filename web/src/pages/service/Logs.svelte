<script lang="ts">
  import { onMount } from 'svelte';
  import { api } from '../../lib/api';
  import LogView from '../../components/LogView.svelte';
  import Icon from '../../components/Icon.svelte';

  let { serviceId }: { serviceId: string } = $props();

  const MAX = 5000;
  let lines = $state<string[]>([]);
  let paused = $state(false);
  let buffered = $state(0);
  let filter = $state('');
  let conn = $state<'connecting' | 'live' | 'error'>('connecting');
  let pending: string[] = [];
  let es: EventSource | null = null;

  function append(batch: string[]) {
    lines.push(...batch);
    if (lines.length > MAX) lines.splice(0, lines.length - MAX);
  }

  function connect() {
    es?.close();
    conn = 'connecting';
    es = new EventSource(api.serviceLogStreamURL(serviceId));
    es.addEventListener('open', () => {
      conn = 'live';
      // The server replays the last 100 lines per instance on every connect.
      lines = [];
      pending = [];
      buffered = 0;
    });
    es.addEventListener('log', (ev) => {
      const l = (ev as MessageEvent).data as string;
      if (paused) {
        pending.push(l);
        if (pending.length > MAX) pending.shift();
        buffered = pending.length;
      } else append([l]);
    });
    es.onerror = () => {
      conn = es?.readyState === EventSource.CLOSED ? 'error' : 'connecting';
    };
  }

  onMount(() => {
    connect();
    return () => es?.close();
  });

  function togglePause() {
    paused = !paused;
    if (!paused && pending.length) {
      append(pending);
      pending = [];
      buffered = 0;
    }
  }
  function clear() {
    lines = [];
    pending = [];
    buffered = 0;
  }
</script>

<div class="stack">
  <div class="row toolbar">
    <span class="row nowrap small" style="gap:6px">
      <span class="dot {conn === 'live' ? (paused ? 'sleeping' : 'running') : conn === 'error' ? 'failed' : 'deploying'}"></span>
      {conn === 'live' ? (paused ? 'Paused' : 'Live') : conn === 'error' ? 'Disconnected' : 'Connecting…'}
    </span>
    <label class="sr-only" for="logfilter">Filter logs</label>
    <input id="logfilter" type="search" class="filter" placeholder="Filter…" bind:value={filter} />
    <div class="spacer"></div>
    <button class="sm" onclick={togglePause} aria-pressed={paused}>
      <Icon name={paused ? 'play' : 'pause'} size={12} />{paused ? `Resume${buffered ? ` (${buffered} new)` : ''}` : 'Pause'}
    </button>
    <button class="sm" onclick={clear}>Clear</button>
    {#if conn === 'error'}<button class="sm" onclick={connect}><Icon name="refresh" size={12} />Reconnect</button>{/if}
  </div>
  <LogView {lines} {filter} height="calc(100vh - 300px)" empty={conn === 'live' ? 'Waiting for log lines… (sleeping or stopped services produce no output)' : 'Connecting…'} />
  <p class="hint">Each line is prefixed with the last 6 characters of the container id. Keeps the latest {MAX} lines.</p>
</div>

<style>
  .filter {
    max-width: 260px;
  }
  .toolbar {
    gap: 10px;
  }
</style>
