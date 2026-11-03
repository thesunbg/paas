// Port of internal/cron: parses 5-field cron expressions (UTC) for client-side hints.
import { t } from './i18n.svelte';

const aliases: Record<string, string> = {
  '@yearly': '0 0 1 1 *',
  '@annually': '0 0 1 1 *',
  '@monthly': '0 0 1 * *',
  '@weekly': '0 0 * * 0',
  '@daily': '0 0 * * *',
  '@midnight': '0 0 * * *',
  '@hourly': '0 * * * *',
};
const monthNames: Record<string, number> = { jan: 1, feb: 2, mar: 3, apr: 4, may: 5, jun: 6, jul: 7, aug: 8, sep: 9, oct: 10, nov: 11, dec: 12 };
const dayNames: Record<string, number> = { sun: 0, mon: 1, tue: 2, wed: 3, thu: 4, fri: 5, sat: 6 };

export interface Schedule {
  minute: Set<number>;
  hour: Set<number>;
  dom: Set<number>;
  month: Set<number>;
  dow: Set<number>;
  domStar: boolean;
  dowStar: boolean;
  fields: string[];
}

function value(s: string, names?: Record<string, number>): number {
  if (names && s in names) return names[s];
  if (!/^\d+$/.test(s)) throw new Error(t('bad value "{value}"', { value: s }));
  return Number(s);
}

function field(s: string, lo: number, hi: number, names?: Record<string, number>): Set<number> {
  const out = new Set<number>();
  for (let part of s.split(',')) {
    let step = 1;
    const slash = part.indexOf('/');
    if (slash >= 0) {
      const n = Number(part.slice(slash + 1));
      if (!Number.isInteger(n) || n <= 0) throw new Error(t('bad step in "{part}"', { part }));
      step = n;
      part = part.slice(0, slash);
    }
    let start = lo;
    let end = hi;
    if (part === '*') {
      // full range
    } else if (part.includes('-')) {
      const [a, b] = part.split('-', 2);
      start = value(a, names);
      end = value(b, names);
    } else {
      start = end = value(part, names);
      if (step > 1) end = hi;
    }
    if (start < lo || end > hi || start > end) throw new Error(t('"{part}" out of range {lo}-{hi}', { part, lo, hi }));
    for (let v = start; v <= end; v += step) out.add(v);
  }
  return out;
}

export function parseCron(expr: string): Schedule {
  let e = expr.trim().toLowerCase();
  if (aliases[e]) e = aliases[e];
  const f = e.split(/\s+/).filter(Boolean);
  if (f.length !== 5) throw new Error(t('cron expression needs 5 fields, got {n}', { n: f.length }));
  const wrap = (name: string, fn: () => Set<number>) => {
    try {
      return fn();
    } catch (err) {
      throw new Error(`${name}: ${(err as Error).message}`);
    }
  };
  const s: Schedule = {
    minute: wrap(t('minute'), () => field(f[0], 0, 59)),
    hour: wrap(t('hour'), () => field(f[1], 0, 23)),
    dom: wrap(t('day of month'), () => field(f[2], 1, 31)),
    month: wrap(t('month'), () => field(f[3], 1, 12, monthNames)),
    dow: wrap(t('day of week'), () => field(f[4], 0, 7, dayNames)),
    domStar: f[2] === '*',
    dowStar: f[4] === '*',
    fields: f,
  };
  if (s.dow.has(7)) s.dow.add(0);
  return s;
}

function matches(s: Schedule, d: Date): boolean {
  if (!s.minute.has(d.getUTCMinutes()) || !s.hour.has(d.getUTCHours()) || !s.month.has(d.getUTCMonth() + 1)) return false;
  const domOK = s.dom.has(d.getUTCDate());
  const dowOK = s.dow.has(d.getUTCDay());
  if (s.domStar && s.dowStar) return true;
  if (s.domStar) return dowOK;
  if (s.dowStar) return domOK;
  return domOK || dowOK;
}

/** The next `count` scheduled times strictly after `from`. */
export function nextRuns(s: Schedule, count = 3, from = new Date()): Date[] {
  const out: Date[] = [];
  const d = new Date(from.getTime());
  d.setUTCSeconds(0, 0);
  d.setUTCMinutes(d.getUTCMinutes() + 1);
  // Skip whole hours/days quickly when they cannot match.
  for (let i = 0; i < 4 * 366 * 24 * 60 && out.length < count; i++) {
    if (!s.month.has(d.getUTCMonth() + 1) || !(s.domStar && s.dowStar ? true : matchesDay(s, d))) {
      d.setUTCHours(0, 0, 0, 0);
      d.setUTCDate(d.getUTCDate() + 1);
      continue;
    }
    if (!s.hour.has(d.getUTCHours())) {
      d.setUTCMinutes(0);
      d.setUTCHours(d.getUTCHours() + 1);
      continue;
    }
    if (matches(s, d)) out.push(new Date(d.getTime()));
    d.setUTCMinutes(d.getUTCMinutes() + 1);
  }
  return out;
}

function matchesDay(s: Schedule, d: Date): boolean {
  const domOK = s.dom.has(d.getUTCDate());
  const dowOK = s.dow.has(d.getUTCDay());
  if (s.domStar) return dowOK;
  if (s.dowStar) return domOK;
  return domOK || dowOK;
}

const pad = (n: number) => String(n).padStart(2, '0');
// "Every <weekday> at <time>", one full sentence per day so translations can reorder it.
function everyWeekday(day: number, at: string): string {
  switch (day) {
    case 0: return t('Every Sunday at {time}', { time: at });
    case 1: return t('Every Monday at {time}', { time: at });
    case 2: return t('Every Tuesday at {time}', { time: at });
    case 3: return t('Every Wednesday at {time}', { time: at });
    case 4: return t('Every Thursday at {time}', { time: at });
    case 5: return t('Every Friday at {time}', { time: at });
    default: return t('Every Saturday at {time}', { time: at });
  }
}

/** A short description (in the current language) for common shapes; empty otherwise. */
export function describeCron(expr: string): string {
  let s: Schedule;
  try {
    s = parseCron(expr);
  } catch {
    return '';
  }
  const [mi, h, dom, mo, dow] = s.fields;
  const everyN = (f: string) => /^\*\/(\d+)$/.exec(f)?.[1];
  const num = (f: string) => (/^\d+$/.test(f) ? Number(f) : null);
  if (mi === '*' && h === '*' && dom === '*' && mo === '*' && dow === '*') return t('Every minute');
  if (everyN(mi) && h === '*' && dom === '*' && mo === '*' && dow === '*') return t('Every {n} minutes', { n: everyN(mi) });
  if (num(mi) !== null && h === '*' && dom === '*' && mo === '*' && dow === '*') return t('Every hour at minute {m}', { m: num(mi) });
  if (num(mi) !== null && everyN(h) && dom === '*' && mo === '*' && dow === '*') return t('Every {n} hours at minute {m}', { n: everyN(h), m: num(mi) });
  if (num(mi) !== null && num(h) !== null && mo === '*') {
    const at = `${pad(num(h)!)}:${pad(num(mi)!)} UTC`;
    if (dom === '*' && dow === '*') return t('Every day at {time}', { time: at });
    if (dom === '*' && dow === '1-5') return t('Weekdays at {time}', { time: at });
    if (dom === '*' && num(dow) !== null) return everyWeekday(num(dow)! % 7, at);
    if (num(dom) !== null && dow === '*') return t('On day {d} of every month at {time}', { d: num(dom), time: at });
  }
  return '';
}

/** Preset schedules; a function so the labels follow the current language. */
export const cronPresets = () => [
  { label: t('Every 5 min'), expr: '*/5 * * * *' },
  { label: t('Every 15 min'), expr: '*/15 * * * *' },
  { label: t('Hourly'), expr: '@hourly' },
  { label: t('Daily 03:00'), expr: '0 3 * * *' },
  { label: t('Weekdays 09:00'), expr: '0 9 * * 1-5' },
  { label: t('Weekly (Sun)'), expr: '@weekly' },
  { label: t('Monthly'), expr: '@monthly' },
];
