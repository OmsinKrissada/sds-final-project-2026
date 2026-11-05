import { app, logTrace, appHops } from './state.svelte';
import { navigate } from './router.svelte';

export class ApiError extends Error {
  status: number;
  recovering?: string;
  constructor(status: number, message: string, recovering?: string) {
    super(message);
    this.status = status;
    this.recovering = recovering;
  }
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** Exponential backoff with jitter, capped (default 5s). */
export function backoff(attempt: number, cap = 5000, base = 250): number {
  const d = Math.min(cap, base * 2 ** attempt);
  return d / 2 + Math.random() * (d / 2);
}

interface Raw {
  status: number;
  data: any;
}

async function once(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<Raw> {
  let res: Response;
  try {
    res = await fetch(path, {
      method,
      credentials: 'same-origin',
      signal,
      headers: { Accept: 'application/json', ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}) },
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
  } catch (e) {
    if (signal?.aborted) throw e;
    logTrace({ kind: 'http', method, path, status: 'ERR', trace: '', hops: 0 });
    return { status: 0, data: null };
  }
  const trace = res.headers.get('X-Trace') || '';
  const hopsH = res.headers.get('X-App-Hops');
  logTrace({
    kind: 'http',
    method,
    path,
    status: res.status,
    trace,
    hops: hopsH !== null && hopsH !== '' ? parseInt(hopsH, 10) || 0 : appHops(trace),
  });
  let data: any = null;
  const text = await res.text().catch(() => '');
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = { error: text.slice(0, 200) };
    }
  }
  return { status: res.status, data };
}

function isDown(r: Raw) {
  return r.status === 0 || r.status === 502 || r.status === 503 || r.status === 504;
}

function svcOf(r: Raw): string {
  if (r.status === 0) return 'connection';
  return (r.data && r.data.recovering) || 'server';
}

function showBanner(r: Raw, retrying: boolean) {
  const svc = svcOf(r);
  if (svc === 'connection') {
    app.banner = retrying ? 'Connection lost — retrying…' : 'Connection lost — check your network and try again';
    app.bannerKind = 'offline';
  } else {
    app.banner = retrying ? `${svc} is recovering — retrying…` : `${svc} is recovering — please try again in a moment`;
    app.bannerKind = 'recovering';
  }
}

/**
 * JSON fetch wrapper. GETs retry forever (exponential backoff + jitter, <=5s)
 * while the backend is recovering; mutations raise the banner and throw so the
 * user can retry. 401 sends the user to the login page.
 */
export async function api<T = any>(
  method: string,
  path: string,
  body?: unknown,
  opt: { signal?: AbortSignal; retry?: boolean } = {},
): Promise<T> {
  const retry = opt.retry ?? method === 'GET';
  for (let attempt = 0; ; attempt++) {
    const r = await once(method, path, body, opt.signal);
    if (isDown(r)) {
      showBanner(r, retry);
      if (!retry) throw new ApiError(r.status, 'Service is recovering', svcOf(r));
      await sleep(backoff(attempt));
      if (opt.signal?.aborted) throw new DOMException('aborted', 'AbortError');
      continue;
    }
    app.banner = '';
    app.bannerKind = '';
    if (r.status === 401 && !path.startsWith('/api/auth/')) {
      app.user = null;
      navigate('/login?next=' + encodeURIComponent(location.pathname + location.search), true);
      throw new ApiError(401, 'login required');
    }
    if (r.status >= 400) throw new ApiError(r.status, (r.data && r.data.error) || `HTTP ${r.status}`);
    return r.data as T;
  }
}

export const get = <T = any>(path: string, signal?: AbortSignal) => api<T>('GET', path, undefined, { signal });
export const post = <T = any>(path: string, body?: unknown) => api<T>('POST', path, body ?? {});
export const put = <T = any>(path: string, body?: unknown) => api<T>('PUT', path, body ?? {});
export const del = <T = any>(path: string) => api<T>('DELETE', path);
