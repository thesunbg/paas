// Port of internal/cron: parses 5-field cron expressions (UTC) for client-side hints.

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
  if (!/^\d+$/.test(s)) throw new Error(`bad value "${s}"`);
  return Number(s);
}

function field(s: string, lo: number, hi: number, names?: Record<string, number>): Set<number> {
  const out = new Set<number>();
  for (let part of s.split(',')) {
    let step = 1;
    const slash = part.indexOf('/');
    if (slash >= 0) {
      const n = Number(part.slice(slash + 1));
      if (!Number.isInteger(n) || n <= 0) throw new Error(`bad step in "${part}"`);
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
    if (start < lo || end > hi || start > end) throw new Error(`"${part}" out of range ${lo}-${hi}`);
    for (let v = start; v <= end; v += step) out.add(v);
  }
  return out;
}

export function parseCron(expr: string): Schedule {
  let e = expr.trim().toLowerCase();
  if (aliases[e]) e = aliases[e];
  const f = e.split(/\s+/).filter(Boolean);
  if (f.length !== 5) throw new Error(`cron expression needs 5 fields, got ${f.length}`);
  const wrap = (name: string, fn: () => Set<number>) => {
    try {
      return fn();
    } catch (err) {
      throw new Error(`${name}: ${(err as Error).message}`);
    }
  };
  const s: Schedule = {
    minute: wrap('minute', () => field(f[0], 0, 59)),
    hour: wrap('hour', () => field(f[1], 0, 23)),
    dom: wrap('day of month', () => field(f[2], 1, 31)),
    month: wrap('month', () => field(f[3], 1, 12, monthNames)),
    dow: wrap('day of week', () => field(f[4], 0, 7, dayNames)),
    domStar: f[2] === '*',
    dowStar: f[4] === '*',
    fields: f,
  };
  if (s.dow.has(7)) s.dow.add(0);
  return s;
}

function matches(s: Schedule, t: Date): boolean {
  if (!s.minute.has(t.getUTCMinutes()) || !s.hour.has(t.getUTCHours()) || !s.month.has(t.getUTCMonth() + 1)) return false;
  const domOK = s.dom.has(t.getUTCDate());
  const dowOK = s.dow.has(t.getUTCDay());
  if (s.domStar && s.dowStar) return true;
  if (s.domStar) return dowOK;
  if (s.dowStar) return domOK;
  return domOK || dowOK;
}

/** The next `count` scheduled times strictly after `from`. */
export function nextRuns(s: Schedule, count = 3, from = new Date()): Date[] {
  const out: Date[] = [];
  const t = new Date(from.getTime());
  t.setUTCSeconds(0, 0);
  t.setUTCMinutes(t.getUTCMinutes() + 1);
  // Skip whole hours/days quickly when they cannot match.
  for (let i = 0; i < 4 * 366 * 24 * 60 && out.length < count; i++) {
    if (!s.month.has(t.getUTCMonth() + 1) || !(s.domStar && s.dowStar ? true : matchesDay(s, t))) {
      t.setUTCHours(0, 0, 0, 0);
      t.setUTCDate(t.getUTCDate() + 1);
      continue;
    }
    if (!s.hour.has(t.getUTCHours())) {
      t.setUTCMinutes(0);
      t.setUTCHours(t.getUTCHours() + 1);
      continue;
    }
    if (matches(s, t)) out.push(new Date(t.getTime()));
    t.setUTCMinutes(t.getUTCMinutes() + 1);
  }
  return out;
}

function matchesDay(s: Schedule, t: Date): boolean {
  const domOK = s.dom.has(t.getUTCDate());
  const dowOK = s.dow.has(t.getUTCDay());
  if (s.domStar) return dowOK;
  if (s.dowStar) return domOK;
  return domOK || dowOK;
}

const pad = (n: number) => String(n).padStart(2, '0');
const dayFull = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];

/** A short English description for common shapes; falls back to the expression. */
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
  if (mi === '*' && h === '*' && dom === '*' && mo === '*' && dow === '*') return 'Every minute';
  if (everyN(mi) && h === '*' && dom === '*' && mo === '*' && dow === '*') return `Every ${everyN(mi)} minutes`;
  if (num(mi) !== null && h === '*' && dom === '*' && mo === '*' && dow === '*') return `Every hour at minute ${num(mi)}`;
  if (num(mi) !== null && everyN(h) && dom === '*' && mo === '*' && dow === '*') return `Every ${everyN(h)} hours at minute ${num(mi)}`;
  if (num(mi) !== null && num(h) !== null && mo === '*') {
    const at = `${pad(num(h)!)}:${pad(num(mi)!)} UTC`;
    if (dom === '*' && dow === '*') return `Every day at ${at}`;
    if (dom === '*' && dow === '1-5') return `Weekdays at ${at}`;
    if (dom === '*' && num(dow) !== null) return `Every ${dayFull[num(dow)! % 7]} at ${at}`;
    if (num(dom) !== null && dow === '*') return `On day ${num(dom)} of every month at ${at}`;
  }
  return '';
}

export const cronPresets = [
  { label: 'Every 5 min', expr: '*/5 * * * *' },
  { label: 'Every 15 min', expr: '*/15 * * * *' },
  { label: 'Hourly', expr: '@hourly' },
  { label: 'Daily 03:00', expr: '0 3 * * *' },
  { label: 'Weekdays 09:00', expr: '0 9 * * 1-5' },
  { label: 'Weekly (Sun)', expr: '@weekly' },
  { label: 'Monthly', expr: '@monthly' },
];
