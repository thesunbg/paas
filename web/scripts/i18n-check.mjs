// Lists the strings passed to t() / tn() / plural() that have no Vietnamese
// translation in src/lib/i18n/vi/*.json, and dictionary entries no code uses.
// Exit code 1 when a translation is missing. Usage: npm run i18n:check [-- --unused]
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';

const root = new URL('../src/', import.meta.url).pathname;
const dictDir = join(root, 'lib/i18n/vi');

function walk(dir, out = []) {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) walk(p, out);
    else if (/\.(svelte|ts)$/.test(name) && !p.startsWith(dictDir) && !p.endsWith('i18n.svelte.ts')) out.push(p);
  }
  return out;
}

const str = String.raw`(['"\x60])((?:\\.|(?!\1)[^\\])*)\1`;
const unescape = (s) => s.replace(/\\(['"\x60\\])/g, '$1').replace(/\\n/g, '\n');
const used = new Map(); // key -> first file:line
function add(key, file, src, index) {
  if (key.includes('${')) return; // a template literal with expressions cannot be a key
  if (!used.has(key)) used.set(key, `${relative(root, file)}:${src.slice(0, index).split('\n').length}`);
}

for (const file of walk(root)) {
  const src = readFileSync(file, 'utf8');
  for (const m of src.matchAll(new RegExp(String.raw`\bt\(\s*` + str, 'g'))) add(unescape(m[2]), file, src, m.index);
  for (const m of src.matchAll(new RegExp(String.raw`\btn\(\s*[^,]+,\s*` + str + String.raw`\s*,\s*` + str.replaceAll('\\1', '\\3'), 'g'))) {
    add(unescape(m[2]), file, src, m.index);
    add(unescape(m[4]), file, src, m.index);
  }
  for (const m of src.matchAll(new RegExp(String.raw`\bplural\(\s*[^,]+,\s*` + str + String.raw`(?:\s*,\s*` + str.replaceAll('\\1', '\\3') + ')?', 'g'))) {
    const one = unescape(m[2]);
    add(one, file, src, m.index);
    add(m[4] !== undefined ? unescape(m[4]) : one + 's', file, src, m.index);
  }
}
// relTime's unit abbreviations
for (const u of ['s', 'min', 'h', 'd', 'mo', 'y']) used.set(u, 'lib/format.ts');

const dict = {};
const from = {}; // key -> dictionary file that defined it
for (const name of readdirSync(dictDir).filter((n) => n.endsWith('.json')).sort()) {
  const d = JSON.parse(readFileSync(join(dictDir, name), 'utf8'));
  for (const [k, v] of Object.entries(d)) {
    if (typeof v !== 'string' || v.trim() === '') {
      console.error(`${name}: empty translation for ${JSON.stringify(k)}`);
      process.exitCode = 1;
    }
    // The files are merged into one map: the same key translated differently in two
    // files would silently depend on the merge order.
    if (k in dict && dict[k] !== v) {
      console.log(`conflict ${JSON.stringify(k)}: ${from[k]} ${JSON.stringify(dict[k])} vs ${name} ${JSON.stringify(v)}`);
      process.exitCode = 1;
    }
    dict[k] = v;
    from[k] = name;
  }
}

const missing = [...used].filter(([k]) => !(k in dict));
for (const [k, where] of missing) console.log(`missing  ${where}  ${JSON.stringify(k)}`);
if (process.argv.includes('--unused')) {
  for (const k of Object.keys(dict)) if (!used.has(k)) console.log(`unused   ${JSON.stringify(k)}`);
}
// Placeholders must survive translation: {name} in the key must be in the value.
for (const [k, v] of Object.entries(dict)) {
  for (const p of k.match(/\{\w+\}/g) ?? []) {
    if (!v.includes(p)) {
      console.log(`placeholder ${p} missing in the translation of ${JSON.stringify(k)}`);
      process.exitCode = 1;
    }
  }
}
console.log(`${used.size} strings, ${Object.keys(dict).length} translations, ${missing.length} missing`);
if (missing.length) process.exitCode = 1;
