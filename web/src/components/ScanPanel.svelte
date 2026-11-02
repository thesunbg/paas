<script lang="ts">
  import type { ImageScan, ScanSeverity } from '../lib/api';
  import { relTime, absTime, shortId } from '../lib/format';
  import { href } from '../lib/router.svelte';

  // Severity counts and the findings table of one image scan.
  let { scan, limit = 50 }: { scan: ImageScan; limit?: number } = $props();

  let filter = $state<ScanSeverity | ''>('');
  let showAll = $state(false);

  const sevBadge: Record<ScanSeverity, string> = { CRITICAL: 'red', HIGH: 'red', MEDIUM: 'yellow', LOW: 'blue', UNKNOWN: '' };
  const counts = $derived<[ScanSeverity, number][]>([
    ['CRITICAL', scan.critical],
    ['HIGH', scan.high],
    ['MEDIUM', scan.medium],
    ['LOW', scan.low],
    ['UNKNOWN', scan.unknown],
  ]);
  const total = $derived(scan.critical + scan.high + scan.medium + scan.low + scan.unknown);
  const filtered = $derived(filter ? scan.findings.filter((f) => f.severity === filter) : scan.findings);
  const shown = $derived(showAll ? filtered : filtered.slice(0, limit));
  const safeURL = (u?: string) => (u && /^https:\/\//.test(u) ? u : '');
</script>

<div class="stack">
  <div class="row meta small muted">
    <span>Deployment <a class="mono" href={href(`/services/${scan.service_id}/deployments/${scan.deployment_id}`)}>#{shortId(scan.deployment_id)}</a></span>
    <span class="mono truncate" title={scan.image}>{scan.image}</span>
    <span title={absTime(scan.scanned_at)}>scanned {relTime(scan.scanned_at)}</span>
    {#if scan.scanner_version}<span>trivy {scan.scanner_version}</span>{/if}
    {#if scan.db_updated_at}<span title={absTime(scan.db_updated_at)}>DB {relTime(scan.db_updated_at)}</span>{/if}
    {#if scan.os}<span>base {scan.os}</span>{/if}
  </div>

  {#if scan.message}
    <div class="alert {scan.blocked ? 'error' : scan.status !== 'scanned' ? 'warn' : ''}">{scan.message}</div>
  {/if}

  {#if scan.status === 'scanned'}
    <div class="sev" role="group" aria-label="Findings by severity">
      {#each counts as [sev, n] (sev)}
        {#if sev !== 'UNKNOWN' || n > 0}
          <button
            class="sev-box"
            class:active={filter === sev}
            aria-pressed={filter === sev}
            disabled={n === 0}
            onclick={() => (filter = filter === sev ? '' : sev)}
          >
            <span class="n {n > 0 ? sevBadge[sev] : ''}">{n}</span>
            <span class="small muted">{sev.toLowerCase()}</span>
          </button>
        {/if}
      {/each}
    </div>

    {#if total === 0}
      <p class="muted small">No known vulnerabilities in this image.</p>
    {:else}
      <div class="card table-wrap">
        <table>
          <thead>
            <tr><th>Severity</th><th>ID</th><th>Package</th><th>Installed</th><th>Fixed in</th><th>Title</th></tr>
          </thead>
          <tbody>
            {#each shown as f (f.id + f.pkg + f.installed)}
              <tr>
                <td><span class="badge {sevBadge[f.severity]}">{f.severity}</span></td>
                <td class="mono nowrap">
                  {#if safeURL(f.url)}<a href={safeURL(f.url)} target="_blank" rel="noreferrer noopener">{f.id}</a>{:else}{f.id}{/if}
                </td>
                <td><span class="mono">{f.pkg}</span>{#if f.target}<div class="faint small truncate tgt" title={f.target}>{f.target}</div>{/if}</td>
                <td class="mono small">{f.installed}</td>
                <td class="mono small">{#if f.fixed}{f.fixed}{:else}<span class="faint">—</span>{/if}</td>
                <td class="small title">{f.title ?? ''}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
      <div class="row small faint">
        <span>
          {shown.length} of {filter ? filtered.length : total} shown{#if scan.findings.length < total && !filter}; the {scan.findings.length} most severe are stored{/if}
        </span>
        {#if !showAll && filtered.length > shown.length}<button class="sm" onclick={() => (showAll = true)}>Show all</button>{/if}
      </div>
    {/if}
  {/if}
</div>

<style>
  .meta {
    gap: 14px;
    flex-wrap: wrap;
  }
  .meta .truncate {
    max-width: 360px;
  }
  .sev {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(110px, 1fr));
    gap: 10px;
  }
  .sev-box {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 2px;
    padding: 10px 12px;
    height: auto;
    background: var(--bg-elev);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    text-align: left;
  }
  .sev-box.active {
    border-color: var(--text);
  }
  .sev-box:disabled {
    opacity: 1;
    cursor: default;
  }
  .n {
    font-size: 22px;
    font-weight: 600;
    line-height: 1.1;
  }
  .n.red {
    color: var(--red);
  }
  .n.yellow {
    color: var(--yellow);
  }
  .n.blue {
    color: var(--accent);
  }
  .tgt {
    max-width: 220px;
  }
  .title {
    min-width: 220px;
  }
</style>
