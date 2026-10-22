<script lang="ts" module>
  export interface Pt {
    t: number; // ms epoch
    v: number;
  }
</script>

<script lang="ts">
  // Single-series time chart in plain SVG: area+line (or bars), recessive grid,
  // crosshair with tooltip on hover/touch, keyboard-steppable.
  let {
    points,
    title,
    format = (v: number) => String(Math.round(v)),
    color = 'var(--accent)',
    kind = 'line',
    height = 150,
    max: fixedMax,
    minTop = 0,
    summary,
  }: {
    points: Pt[];
    title: string;
    format?: (v: number) => string;
    color?: string;
    kind?: 'line' | 'bars';
    height?: number;
    max?: number;
    /** Smallest axis maximum (e.g. 2 for integer counts so the mid tick stays whole). */
    minTop?: number;
    summary?: string;
  } = $props();

  let width = $state(400);
  let hover = $state<number | null>(null);

  const padL = 58;
  const padR = 8;
  const padT = 8;
  const padB = 20;

  const t0 = $derived(points.length ? points[0].t : 0);
  const t1 = $derived(points.length ? points[points.length - 1].t : 1);
  const vmax = $derived.by(() => {
    const m = Math.max(0, ...points.map((p) => p.v));
    const top = Math.max(fixedMax ?? m, minTop);
    return niceCeil(top || 1);
  });
  const innerW = $derived(Math.max(10, width - padL - padR));
  const innerH = $derived(height - padT - padB);
  const x = (t: number) => padL + (t1 === t0 ? innerW / 2 : ((t - t0) / (t1 - t0)) * innerW);
  const y = (v: number) => padT + innerH - (Math.min(v, vmax) / vmax) * innerH;

  function niceCeil(v: number): number {
    const p = Math.pow(10, Math.floor(Math.log10(v)));
    const n = v / p;
    // Steps whose halves are also round, so the middle grid label is exact.
    return (n <= 1 ? 1 : n <= 2 ? 2 : n <= 4 ? 4 : n <= 6 ? 6 : n <= 8 ? 8 : 10) * p;
  }

  const linePath = $derived(points.map((p, i) => `${i ? 'L' : 'M'}${x(p.t).toFixed(1)},${y(p.v).toFixed(1)}`).join(''));
  const areaPath = $derived(
    points.length ? `${linePath}L${x(t1).toFixed(1)},${(padT + innerH).toFixed(1)}L${x(t0).toFixed(1)},${(padT + innerH).toFixed(1)}Z` : '',
  );
  const barW = $derived(points.length ? Math.max(1, innerW / points.length - 1) : 0);
  const ticks = $derived([0, vmax / 2, vmax]);
  const xTicks = $derived.by(() => {
    if (!points.length) return [];
    const n = width < 360 ? 2 : 4;
    return Array.from({ length: n + 1 }, (_, i) => t0 + ((t1 - t0) * i) / n);
  });
  const spanH = $derived((t1 - t0) / 3.6e6);
  const fmtTime = (t: number) => {
    const d = new Date(t);
    const hm = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    return spanH > 20 ? `${d.toLocaleDateString([], { weekday: 'short' })} ${hm}` : hm;
  };

  function onmove(e: PointerEvent) {
    if (!points.length) return;
    const r = (e.currentTarget as SVGElement).getBoundingClientRect();
    const px = e.clientX - r.left;
    const t = t0 + ((px - padL) / innerW) * (t1 - t0);
    let best = 0;
    let bd = Infinity;
    for (let i = 0; i < points.length; i++) {
      const d = Math.abs(points[i].t - t);
      if (d < bd) {
        bd = d;
        best = i;
      }
    }
    hover = best;
  }
  function onkey(e: KeyboardEvent) {
    if (!points.length) return;
    if (e.key === 'ArrowRight') hover = Math.min(points.length - 1, (hover ?? -1) + 1);
    else if (e.key === 'ArrowLeft') hover = Math.max(0, (hover ?? points.length) - 1);
    else if (e.key === 'Escape') hover = null;
    else return;
    e.preventDefault();
  }
  const hp = $derived(hover != null ? points[hover] : null);
  const latest = $derived(points.length ? points[points.length - 1].v : null);
</script>

<figure class="chart card">
  <figcaption class="between nowrap">
    <span class="ttl">{title}</span>
    <span class="cur">{hp ? format(hp.v) : summary ?? (latest != null ? format(latest) : '—')}</span>
  </figcaption>
  <div class="plot" bind:clientWidth={width}>
    {#if points.length < 2}
      <div class="nodata faint small" style:height="{height}px">Not enough data yet</div>
    {:else}
      <!-- svelte-ignore a11y_no_noninteractive_tabindex, a11y_no_noninteractive_element_interactions -->
      <svg
        {width}
        {height}
        viewBox="0 0 {width} {height}"
        role="img"
        aria-label="{title}: latest {latest != null ? format(latest) : 'n/a'}, peak {format(Math.max(...points.map((p) => p.v)))}"
        tabindex="0"
        onpointermove={onmove}
        onpointerdown={onmove}
        onpointerleave={() => (hover = null)}
        onkeydown={onkey}
        onblur={() => (hover = null)}
      >
        {#each ticks as tv (tv)}
          <line class="grid" x1={padL} x2={width - padR} y1={y(tv)} y2={y(tv)} />
          <text class="axis" x={padL - 6} y={y(tv) + 3.5} text-anchor="end">{format(tv)}</text>
        {/each}
        {#each xTicks as tt, i (i)}
          <text class="axis" x={x(tt)} y={height - 5} text-anchor={i === 0 ? 'start' : i === xTicks.length - 1 ? 'end' : 'middle'}>{fmtTime(tt)}</text>
        {/each}
        {#if kind === 'bars'}
          {#each points as p, i (i)}
            {#if p.v > 0}
              <rect x={x(p.t) - barW / 2} y={y(p.v)} width={barW} height={padT + innerH - y(p.v)} rx={Math.min(2, barW / 2)} fill={color} opacity={hover === i ? 1 : 0.85} />
            {/if}
          {/each}
        {:else}
          <path d={areaPath} fill={color} opacity="0.12" />
          <path d={linePath} fill="none" stroke={color} stroke-width="2" stroke-linejoin="round" stroke-linecap="round" />
        {/if}
        {#if hp}
          <line class="cross" x1={x(hp.t)} x2={x(hp.t)} y1={padT} y2={padT + innerH} />
          <circle cx={x(hp.t)} cy={y(hp.v)} r="4" fill={color} stroke="var(--bg-elev)" stroke-width="2" />
        {/if}
      </svg>
      {#if hp}
        {@const left = Math.min(Math.max(x(hp.t), 70), width - 70)}
        <div class="tip" style:left="{left}px">
          <div class="faint">{new Date(hp.t).toLocaleString([], { weekday: 'short', hour: '2-digit', minute: '2-digit' })}</div>
          <div><b>{format(hp.v)}</b></div>
        </div>
      {/if}
    {/if}
  </div>
</figure>

<style>
  .chart {
    margin: 0;
    padding: 10px 12px 6px;
    min-width: 0;
  }
  figcaption {
    margin-bottom: 4px;
  }
  .ttl {
    font-size: 12px;
    color: var(--text-dim);
    font-weight: 500;
  }
  .cur {
    font-size: 13px;
    font-weight: 600;
    font-variant-numeric: tabular-nums;
  }
  .plot {
    position: relative;
    width: 100%;
  }
  svg {
    display: block;
    touch-action: pan-y;
    outline-offset: 2px;
  }
  .grid {
    stroke: var(--border);
    stroke-width: 1;
  }
  .axis {
    fill: var(--text-faint);
    font-size: 10px;
    font-family: var(--font);
    font-variant-numeric: tabular-nums;
  }
  .cross {
    stroke: var(--text-faint);
    stroke-width: 1;
    stroke-dasharray: 3 3;
  }
  .tip {
    position: absolute;
    top: 0;
    transform: translateX(-50%);
    background: var(--bg-elev-2);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius-sm);
    padding: 4px 8px;
    font-size: 11.5px;
    pointer-events: none;
    white-space: nowrap;
    box-shadow: var(--shadow);
  }
  .nodata {
    display: flex;
    align-items: center;
    justify-content: center;
    border: 1px dashed var(--border);
    border-radius: var(--radius-sm);
  }
</style>
