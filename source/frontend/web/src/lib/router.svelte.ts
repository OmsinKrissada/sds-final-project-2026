function norm(p: string) {
  return p.length > 1 && p.endsWith('/') ? p.slice(0, -1) : p;
}

export const route = $state({ path: norm(location.pathname), search: location.search });

export function navigate(to: string, replace = false) {
  const u = new URL(to, location.origin);
  const url = norm(u.pathname) + u.search;
  if (replace) history.replaceState(null, '', url);
  else history.pushState(null, '', url);
  route.path = norm(u.pathname);
  route.search = u.search;
  window.scrollTo(0, 0);
}

export function query(): URLSearchParams {
  return new URLSearchParams(route.search);
}

export function initRouter() {
  window.addEventListener('popstate', () => {
    route.path = norm(location.pathname);
    route.search = location.search;
  });
  document.addEventListener('click', (e) => {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    const a = (e.target as Element | null)?.closest?.('a');
    if (!a || a.target || a.hasAttribute('download') || a.origin !== location.origin) return;
    e.preventDefault();
    navigate(a.pathname + a.search);
  });
}
