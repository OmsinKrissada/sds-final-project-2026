import type { TraceEntry, User } from './types';

declare global {
  interface Window {
    __BOOT__?: any;
  }
}

/** Bootstrap data injected by the Go SSR server (empty in vite dev). */
export const boot: any = (typeof window !== 'undefined' && window.__BOOT__) || {};

/** Boot data is only valid for the first page view; consumers flip these once used. */
export const bootFresh = { hallOfFame: true, game: true };

export const app = $state({
  user: ((boot.session && boot.session.user) || null) as User | null,
  banner: '',
  bannerKind: '' as '' | 'recovering' | 'offline',
  drawer: false,
  trace: [] as TraceEntry[],
});

let seq = 0;
export function logTrace(e: Omit<TraceEntry, 'id' | 'at'>) {
  app.trace.unshift({ ...e, id: ++seq, at: Date.now() });
  if (app.trace.length > 20) app.trace.length = 20;
}

/** Distinct services in a trace string, excluding gateway and postgres (mirrors common.AppHops). */
export function appHops(trace: string): number {
  const seen = new Set<string>();
  for (const tok of trace.split(',')) {
    const name = tok.trim().split('@')[0];
    if (!name || name === 'gateway' || name === 'postgres') continue;
    seen.add(name);
  }
  return seen.size;
}

// Record the page load itself. The browser cannot read navigation headers, so
// the Go server embeds its trace into the boot object. The gateway token is
// not part of it; show a generic "gateway" routing hop in front.
if (boot.trace) {
  logTrace({
    kind: 'page',
    method: 'GET',
    path: location.pathname + location.search,
    status: boot.recovering ? 503 : 200,
    trace: 'gateway,' + boot.trace,
    hops: appHops(boot.trace),
  });
}
