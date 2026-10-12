// Parsing helpers for .env text.

export interface EnvRow {
  key: string;
  value: string;
}

const keyRe = /^[A-Za-z_][A-Za-z0-9_]*$/;

export function validEnvKey(k: string): boolean {
  return keyRe.test(k);
}

/** Parses KEY=VALUE lines (supports `export`, comments, single/double quotes). */
export function parseDotEnv(text: string): EnvRow[] {
  const out: EnvRow[] = [];
  for (const raw of text.split(/\r?\n/)) {
    let line = raw.trim();
    if (!line || line.startsWith('#')) continue;
    if (line.startsWith('export ')) line = line.slice(7).trim();
    const eq = line.indexOf('=');
    if (eq <= 0) continue;
    const key = line.slice(0, eq).trim();
    let value = line.slice(eq + 1).trim();
    if ((value.startsWith('"') && value.endsWith('"') && value.length >= 2) || (value.startsWith("'") && value.endsWith("'") && value.length >= 2)) {
      const q = value[0];
      value = value.slice(1, -1);
      if (q === '"') value = value.replace(/\\n/g, '\n').replace(/\\"/g, '"');
    } else {
      const hash = value.search(/\s#/);
      if (hash >= 0) value = value.slice(0, hash).trim();
    }
    if (validEnvKey(key)) out.push({ key, value });
  }
  return out;
}

/** Merges rows into existing rows (later keys win, order preserved). */
export function mergeRows(rows: EnvRow[], add: EnvRow[]): EnvRow[] {
  const res = rows.filter((r) => r.key.trim() !== '' || r.value !== '');
  for (const a of add) {
    const i = res.findIndex((r) => r.key === a.key);
    if (i >= 0) res[i] = { ...res[i], value: a.value };
    else res.push({ ...a });
  }
  return res;
}

export function rowsToMap(rows: EnvRow[]): Record<string, string> {
  const m: Record<string, string> = {};
  for (const r of rows) {
    const k = r.key.trim();
    if (k) m[k] = r.value;
  }
  return m;
}

/** Returns an error message for the first invalid or duplicated key, or "". */
export function validateRows(rows: EnvRow[]): string {
  const seen = new Set<string>();
  for (const r of rows) {
    const k = r.key.trim();
    if (!k) {
      if (r.value) return 'A variable has a value but no name';
      continue;
    }
    if (!validEnvKey(k)) return `Invalid variable name "${k}" (letters, digits and _; must not start with a digit)`;
    if (seen.has(k)) return `Duplicate variable "${k}"`;
    seen.add(k);
  }
  return '';
}
