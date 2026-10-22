// Formatting helpers for times, sizes and durations.

const rtf = typeof Intl !== 'undefined' && 'RelativeTimeFormat' in Intl ? new Intl.RelativeTimeFormat('en', { numeric: 'auto' }) : null;

/** "3 min ago", "in 2 h", "just now". */
export function relTime(iso: string | null | undefined, now = Date.now()): string {
  if (!iso) return '—';
  const t = Date.parse(iso);
  if (Number.isNaN(t) || t <= 0) return '—';
  const s = Math.round((t - now) / 1000);
  const a = Math.abs(s);
  if (a < 10) return 'just now';
  const units: [number, Intl.RelativeTimeFormatUnit, string][] = [
    [60, 'second', 's'],
    [3600, 'minute', 'min'],
    [86400, 'hour', 'h'],
    [86400 * 30, 'day', 'd'],
    [86400 * 365, 'month', 'mo'],
    [Infinity, 'year', 'y'],
  ];
  const div = [1, 60, 3600, 86400, 86400 * 30, 86400 * 365];
  for (let i = 0; i < units.length; i++) {
    if (a < units[i][0]) {
      const v = Math.round(s / div[i]);
      if (i === 0) return s < 0 ? `${a}s ago` : `in ${a}s`;
      const short = units[i][2];
      return s < 0 ? `${Math.abs(v)} ${short} ago` : rtf ? rtf.format(v, units[i][1]) : `in ${v} ${short}`;
    }
  }
  return new Date(t).toLocaleString();
}

export function absTime(iso: string | null | undefined): string {
  if (!iso) return '';
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleString();
}

/** Milliseconds → "1m 12s", "850ms", "2h 3m". */
export function duration(ms: number | null | undefined): string {
  if (ms == null || !Number.isFinite(ms) || ms < 0) return '—';
  if (ms < 1000) return `${Math.round(ms)}ms`;
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

/** Duration between two ISO timestamps (end defaults to now when running). */
export function between(start: string | null | undefined, end: string | null | undefined, running = false): string {
  if (!start) return '—';
  const a = Date.parse(start);
  const b = end ? Date.parse(end) : running ? Date.now() : NaN;
  if (Number.isNaN(a) || Number.isNaN(b)) return '—';
  return duration(b - a);
}

/** Megabytes → "512 MB", "1.5 GB". */
export function mb(v: number | null | undefined): string {
  if (v == null || !Number.isFinite(v)) return '—';
  if (Math.abs(v) >= 1024) {
    const g = v / 1024;
    return `${g >= 10 ? g.toFixed(0) : g.toFixed(1).replace(/\.0$/, '')} GB`;
  }
  return `${Math.round(v)} MB`;
}

export function bytes(v: number | null | undefined): string {
  if (v == null || !Number.isFinite(v)) return '—';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  let n = v;
  while (n >= 1024 && i < u.length - 1) {
    n /= 1024;
    i++;
  }
  return `${i === 0 ? n : n.toFixed(n >= 10 ? 0 : 1)} ${u[i]}`;
}

export function gb(v: number | null | undefined): string {
  if (v == null || !Number.isFinite(v)) return '—';
  return `${v >= 100 ? v.toFixed(0) : v.toFixed(1)} GB`;
}

/** CPU millicores → "0.5 vCPU", "250m". */
export function cpu(millis: number | null | undefined): string {
  if (millis == null || !Number.isFinite(millis)) return '—';
  if (millis < 1000) return `${millis}m`;
  const c = millis / 1000;
  return `${Number.isInteger(c) ? c : c.toFixed(2).replace(/0$/, '')} vCPU`;
}

export function pct(ratio: number | null | undefined, digits = 0): string {
  if (ratio == null || !Number.isFinite(ratio)) return '—';
  return `${(ratio * 100).toFixed(digits)}%`;
}

export function shortSha(sha: string | null | undefined): string {
  return sha ? sha.slice(0, 7) : '';
}

export function shortId(id: string | null | undefined, n = 8): string {
  return id ? id.slice(0, n) : '';
}

export function plural(n: number, word: string, pluralWord = word + 's'): string {
  return `${n} ${n === 1 ? word : pluralWord}`;
}

/** A rate in bytes per second, e.g. "1.2 MB/s". */
export function rate(bytesPerSec: number | null | undefined): string {
  if (bytesPerSec == null || !Number.isFinite(bytesPerSec)) return '—';
  return `${bytes(Math.round(bytesPerSec))}/s`;
}
