// Minimal hash router: "#/services/<id>/logs" → { path: ['services', id, 'logs'] }.

function parse(): string[] {
  const h = location.hash.replace(/^#\/?/, '').split('?')[0];
  return h.split('/').filter(Boolean).map(decodeURIComponent);
}

export const route = $state<{ parts: string[] }>({ parts: parse() });

if (typeof window !== 'undefined') {
  window.addEventListener('hashchange', () => {
    route.parts = parse();
  });
}

export function navigate(path: string, replace = false) {
  const h = '#' + (path.startsWith('/') ? path : '/' + path);
  if (replace) {
    history.replaceState(null, '', h);
    route.parts = parse();
  } else {
    location.hash = h;
  }
}

export function href(path: string): string {
  return '#' + (path.startsWith('/') ? path : '/' + path);
}
