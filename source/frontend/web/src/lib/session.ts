// sessionStorage helpers (may throw in private mode; always wrapped).
function get(k: string): string | null {
  try {
    return sessionStorage.getItem(k);
  } catch {
    return null;
  }
}
function set(k: string, v: string) {
  try {
    sessionStorage.setItem(k, v);
  } catch {
    /* ignore */
  }
}
function del(k: string) {
  try {
    sessionStorage.removeItem(k);
  } catch {
    /* ignore */
  }
}

export const hostToken = {
  get: (pin: string) => get(`qb.host.${pin}`),
  set: (pin: string, t: string) => set(`qb.host.${pin}`, t),
  clear: (pin: string) => del(`qb.host.${pin}`),
};

export interface PlayerSession {
  token: string;
  nick: string;
}
export const playerSession = {
  get(pin: string): PlayerSession | null {
    const raw = get(`qb.player.${pin}`);
    if (!raw) return null;
    try {
      return JSON.parse(raw);
    } catch {
      return null;
    }
  },
  set: (pin: string, s: PlayerSession) => set(`qb.player.${pin}`, JSON.stringify(s)),
  clear: (pin: string) => del(`qb.player.${pin}`),
};
