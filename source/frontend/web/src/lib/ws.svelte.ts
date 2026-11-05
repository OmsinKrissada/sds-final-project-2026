import type { GameState } from './types';
import { backoff } from './api';
import { logTrace } from './state.svelte';
import { toMs } from './clock.svelte';

const FATAL = [4404, 4401, 4400];

export function wsBase(): string {
  return (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host;
}

/**
 * Resilient game WebSocket. App-level ping every 5s (reconnect if nothing is
 * received for 10s), exponential backoff on close, immediate reconnect on
 * 4503 / tab becoming visible / network coming back. State messages with a
 * version lower than the last one seen on this connection are ignored.
 */
export class GameConn {
  game = $state<GameState | null>(null);
  online = $state(false);
  everOpen = $state(false);
  fatal = $state<{ code: number; reason: string } | null>(null);
  lastError = $state('');
  hello = $state<any>(null);
  /** serverNow - clientNow, ms */
  offset = 0;

  private ws: WebSocket | null = null;
  private lastRx = 0;
  private lastV = -1;
  private attempt = 0;
  private stopped = true;
  private retryTimer: ReturnType<typeof setTimeout> | undefined;
  private pingTimer: ReturnType<typeof setInterval> | undefined;
  private urlFn: () => string;
  private onHello?: (m: any) => void;
  private label: string;

  constructor(urlFn: () => string, opts: { onHello?: (m: any) => void; label?: string } = {}) {
    this.urlFn = urlFn;
    this.onHello = opts.onHello;
    this.label = opts.label || 'ws';
  }

  start() {
    this.stopped = false;
    this.fatal = null;
    document.addEventListener('visibilitychange', this.onVisible);
    window.addEventListener('online', this.onOnline);
    this.pingTimer = setInterval(() => this.tick(), 5000);
    this.connect();
  }

  stop() {
    this.stopped = true;
    clearTimeout(this.retryTimer);
    clearInterval(this.pingTimer);
    document.removeEventListener('visibilitychange', this.onVisible);
    window.removeEventListener('online', this.onOnline);
    this.drop();
  }

  send(obj: unknown): boolean {
    if (this.ws && this.ws.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify(obj));
      return true;
    }
    return false;
  }

  private onVisible = () => {
    if (this.stopped || document.visibilityState !== 'visible') return;
    if (!this.online) {
      this.attempt = 0;
      this.connect();
      return;
    }
    // Phones may silently kill sockets while backgrounded: verify liveness.
    const mark = Date.now();
    this.send({ type: 'ping' });
    setTimeout(() => {
      if (!this.stopped && this.lastRx < mark) {
        this.drop();
        this.attempt = 0;
        this.connect();
      }
    }, 3000);
  };

  private onOnline = () => {
    if (this.stopped || this.online) return;
    this.attempt = 0;
    this.connect();
  };

  private tick() {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return;
    if (Date.now() - this.lastRx > 10000) {
      this.drop();
      this.connect();
      return;
    }
    this.send({ type: 'ping' });
  }

  private drop() {
    const ws = this.ws;
    this.ws = null;
    this.online = false;
    if (ws) {
      ws.onopen = ws.onmessage = ws.onclose = ws.onerror = null;
      try {
        ws.close();
      } catch {
        /* ignore */
      }
    }
  }

  private schedule(ms: number) {
    clearTimeout(this.retryTimer);
    this.retryTimer = setTimeout(() => this.connect(), ms);
  }

  private connect() {
    clearTimeout(this.retryTimer);
    if (this.stopped) return;
    this.drop();
    let ws: WebSocket;
    try {
      ws = new WebSocket(this.urlFn());
    } catch {
      this.schedule(backoff(this.attempt++));
      return;
    }
    this.ws = ws;
    this.lastRx = Date.now();
    this.lastV = -1;
    ws.onmessage = (ev) => {
      if (this.ws !== ws) return;
      this.lastRx = Date.now();
      let m: any;
      try {
        m = JSON.parse(ev.data);
      } catch {
        return;
      }
      switch (m.type) {
        case 'hello':
          this.attempt = 0;
          this.online = true;
          this.everOpen = true;
          this.hello = m;
          logTrace({
            kind: 'ws',
            method: 'WS',
            path: this.label,
            status: 101,
            trace: m.trace ? 'gateway,' + m.trace : '',
            hops: typeof m.hops === 'number' ? m.hops : 0,
          });
          this.onHello?.(m);
          break;
        case 'state':
          if (typeof m.v === 'number') {
            if (m.v < this.lastV) return;
            this.lastV = m.v;
          }
          this.online = true;
          this.everOpen = true;
          if (m.game?.question?.serverNow) this.offset = toMs(m.game.question.serverNow) - Date.now();
          this.game = m.game;
          this.lastError = '';
          break;
        case 'error':
          this.lastError = m.error || 'error';
          break;
      }
    };
    ws.onclose = (ev) => {
      if (this.ws !== ws) return;
      this.ws = null;
      this.online = false;
      if (FATAL.includes(ev.code)) {
        this.fatal = { code: ev.code, reason: ev.reason };
        return;
      }
      if (ev.code === 4503) {
        this.attempt = 0;
        this.schedule(50 + Math.random() * 250);
        return;
      }
      this.schedule(backoff(this.attempt++));
    };
    ws.onerror = () => {
      /* onclose follows */
    };
  }
}
