<script lang="ts">
  // Tiny single-series sparkline with a hover readout (no axes).
  let {
    values,
    times = [],
    label,
    format = (v: number) => String(Math.round(v)),
    max: fixedMax,
    color = 'var(--accent)',
    width = 120,
    height = 28,
  }: {
    values: number[];
    times?: number[];
    label: string;
    format?: (v: number) => string;
    max?: number;
    color?: string;
    width?: number;
    height?: number;
  } = $props();

  let hover = $state<number | null>(null);
  const top = $derived(Math.max(fixedMax ?? 0, ...values, 1e-9));
  const x = (i: number) => (values.length < 2 ? width / 2 : 1 + (i / (values.length - 1)) * (width - 2));
  const y = (v: number) => height - 2 - (Math.min(v, top) / top) * (height - 4);
  const d = $derived(values.map((v, i) => `${i ? 'L' : 'M'}${x(i).toFixed(1)},${y(v).toFixed(1)}`).join(''));

  function onmove(e: PointerEvent) {
    if (!values.length) return;
    const r = (e.currentTarget as SVGElement).getBoundingClientRect();
    const i = Math.round(((e.clientX - r.left) / r.width) * (values.length - 1));
    hover = Math.max(0, Math.min(values.length - 1, i));
  }
  const last = $derived(values.length ? values[values.length - 1] : null);
  const shown = $derived(hover != null ? values[hover] : last);
</script>

<span class="spark">
  {#if values.length < 2}
    <span class="faint small" style:width="{width}px" style:display="inline-block">no data</span>
  {:else}
    <svg
      {width}
      {height}
      viewBox="0 0 {width} {height}"
      role="img"
      aria-label="{label}: latest {last != null ? format(last) : 'n/a'}, peak {format(Math.max(...values))}"
      onpointermove={onmove}
      onpointerleave={() => (hover = null)}
    >
      <line x1="0" x2={width} y1={height - 1.5} y2={height - 1.5} stroke="var(--border)" stroke-width="1" />
      <path {d} fill="none" stroke={color} stroke-width="1.5" stroke-linejoin="round" stroke-linecap="round" />
      {#if hover != null}
        <circle cx={x(hover)} cy={y(values[hover])} r="2.5" fill={color} />
      {/if}
    </svg>
  {/if}
  <span class="val small">
    {shown != null ? format(shown) : '—'}
    {#if hover != null && times[hover]}<span class="faint">{new Date(times[hover]).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}</span>{/if}
  </span>
</span>

<style>
  .spark {
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }
  svg {
    display: block;
    flex: none;
  }
  .val {
    font-variant-numeric: tabular-nums;
    min-width: 72px;
    display: inline-flex;
    flex-direction: column;
    line-height: 1.2;
  }
</style>
