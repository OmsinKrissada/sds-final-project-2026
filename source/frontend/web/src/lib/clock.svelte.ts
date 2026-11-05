/** Millisecond epoch from either seconds or milliseconds. */
export function toMs(n: number): number {
  return n < 1e11 ? n * 1000 : n;
}

/** Reactive "now" ticking every `ms`. Call during component init. */
export function useNow(ms = 100) {
  const s = $state({ v: Date.now() });
  $effect(() => {
    const t = setInterval(() => (s.v = Date.now()), ms);
    return () => clearInterval(t);
  });
  return s;
}
