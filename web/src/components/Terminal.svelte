<script lang="ts">
  import { onMount } from 'svelte';
  import { Terminal } from '@xterm/xterm';
  import { FitAddon } from '@xterm/addon-fit';
  import '@xterm/xterm/css/xterm.css';

  // An interactive shell in a container, over GET /services/{id}/terminal (WebSocket):
  // binary frames are keystrokes and output, text frames JSON control messages.
  let { serviceId, instance = '' }: { serviceId: string; instance?: string } = $props();

  type State = 'connecting' | 'open' | 'closed';
  let el: HTMLDivElement;
  let st = $state<State>('connecting');
  let note = $state('');
  let reconnect = $state<() => void>(() => {});

  onMount(() => {
    const term = new Terminal({
      cursorBlink: true,
      fontSize: 13,
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
      theme: { background: '#0b0b0f' },
      scrollback: 5000,
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(el);
    const refit = () => {
      try {
        fit.fit();
      } catch {
        /* not laid out yet */
      }
    };
    refit();
    const enc = new TextEncoder();
    let ws: WebSocket | null = null;

    function connect() {
      st = 'connecting';
      note = '';
      term.reset();
      const proto = location.protocol === 'https:' ? 'wss' : 'ws';
      const q = new URLSearchParams({ cols: String(term.cols), rows: String(term.rows) });
      if (instance) q.set('instance', instance);
      const sock = new WebSocket(`${proto}://${location.host}/api/v1/services/${encodeURIComponent(serviceId)}/terminal?${q}`);
      sock.binaryType = 'arraybuffer';
      ws = sock;
      sock.onopen = () => {
        st = 'open';
        term.focus();
      };
      sock.onmessage = (ev) => {
        if (typeof ev.data === 'string') {
          try {
            const m = JSON.parse(ev.data) as { exit?: number; error?: string };
            if (m.error) note = m.error;
            else if (m.exit !== undefined) note = `Process exited with code ${m.exit}`;
          } catch {
            /* ignore */
          }
          return;
        }
        term.write(new Uint8Array(ev.data as ArrayBuffer));
      };
      sock.onclose = () => {
        if (ws === sock) {
          st = 'closed';
          if (!note) note = 'Disconnected';
        }
      };
    }

    const dataSub = term.onData((d) => {
      if (ws?.readyState === WebSocket.OPEN) ws.send(enc.encode(d));
    });
    const binSub = term.onBinary((d) => {
      if (ws?.readyState !== WebSocket.OPEN) return;
      const b = new Uint8Array(d.length);
      for (let i = 0; i < d.length; i++) b[i] = d.charCodeAt(i) & 255;
      ws.send(b);
    });
    const resizeSub = term.onResize(({ cols, rows }) => {
      if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ cols, rows }));
    });
    const ro = new ResizeObserver(() => requestAnimationFrame(refit));
    ro.observe(el);

    reconnect = () => {
      const old = ws;
      ws = null;
      old?.close();
      connect();
    };
    connect();

    return () => {
      ro.disconnect();
      dataSub.dispose();
      binSub.dispose();
      resizeSub.dispose();
      const s = ws;
      ws = null;
      s?.close();
      term.dispose();
    };
  });
</script>

<div class="wrap">
  <div class="tbar small">
    <span class="led {st}"></span>
    <span class="muted">{st === 'open' ? 'Connected' : st === 'connecting' ? 'Connecting…' : note}</span>
    {#if st === 'closed'}
      <button class="sm" onclick={() => reconnect()}>Reconnect</button>
    {/if}
  </div>
  <div class="term" bind:this={el}></div>
</div>

<style>
  .wrap {
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    overflow: hidden;
    background: #0b0b0f;
  }
  .tbar {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 6px 10px;
    border-bottom: 1px solid #27272a;
    color: #a1a1aa;
    min-height: 36px;
  }
  .led {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: #71717a;
  }
  .led.open {
    background: var(--green);
  }
  .led.connecting {
    background: var(--yellow, #eab308);
  }
  .term {
    height: min(65vh, 640px);
    padding: 6px 0 0 8px;
  }
</style>
