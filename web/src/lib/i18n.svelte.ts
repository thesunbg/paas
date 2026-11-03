// Dashboard translations (English source, Vietnamese).
//
// Strings stay in English in the components, wrapped in t(): t('Deploy'),
// t('Delete {name}?', { name }). The English text is the key; i18n/vi/*.json map
// it to Vietnamese, and a missing entry falls back to the English text. Run
// `npm run i18n:check` to list strings without a translation.
//
// t() reads the current locale from reactive state, so every string re-renders
// when the language changes. Messages that come from the API (errors) stay in
// English.
import { vi } from './i18n/vi';

export type Locale = 'en' | 'vi';
export const locales: { id: Locale; label: string; short: string }[] = [
  { id: 'en', label: 'English', short: 'EN' },
  { id: 'vi', label: 'Tiếng Việt', short: 'VI' },
];

const storageKey = 'paas.locale';

function initialLocale(): Locale {
  try {
    const v = localStorage.getItem(storageKey);
    if (v === 'en' || v === 'vi') return v;
  } catch {
    // storage blocked: fall back to the browser language
  }
  if (typeof navigator !== 'undefined' && navigator.language?.toLowerCase().startsWith('vi')) return 'vi';
  return 'en';
}

export const i18n = $state({ locale: initialLocale() as Locale });

if (typeof document !== 'undefined') document.documentElement.lang = i18n.locale;

export function setLocale(l: Locale) {
  i18n.locale = l;
  try {
    localStorage.setItem(storageKey, l);
  } catch {
    // not persisted: still applies to this page
  }
  if (typeof document !== 'undefined') document.documentElement.lang = l;
}

/** BCP 47 tag for Intl and toLocaleString. */
export function intlLocale(): string {
  return i18n.locale === 'vi' ? 'vi-VN' : 'en';
}

type Params = Record<string, string | number | null | undefined>;

/** Translates an English string; {name} placeholders are filled from params. */
export function t(s: string, params?: Params): string {
  let out = i18n.locale === 'vi' ? (vi[s] ?? s) : s;
  if (params) out = out.replace(/\{(\w+)\}/g, (m, k: string) => (k in params ? String(params[k] ?? '') : m));
  return out;
}

/** Picks the singular or plural English form, then translates it: tn(3, '{n} service', '{n} services'). */
export function tn(n: number, one: string, other: string, params?: Params): string {
  return t(n === 1 ? one : other, { n, ...params });
}
