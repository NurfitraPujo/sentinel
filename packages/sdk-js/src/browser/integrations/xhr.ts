// The XMLHttpRequest breadcrumb integration (spec §5.4, TASK-03).

import { filterUrl } from '../../core/url.js';
import type { InternalClient } from '../../core/state.js';
import { isRecording, originAndPath, record, wrapMethod } from './shared.js';

function codePoints(s: string): string[] {
  return Array.from(s);
}

function cutCodePoints(s: string, max: number): string {
  const cps = codePoints(s);
  return cps.length > max ? cps.slice(0, max).join('') : s;
}

function now(): number {
  try {
    const perf = (globalThis as { performance?: { now?: () => number } }).performance;
    if (typeof perf?.now === 'function') return perf.now();
  } catch {
    // Fall through to Date.now().
  }
  return Date.now();
}

function levelFor(status: number): string {
  if (status === 0) return 'error';
  if (status >= 500) return 'error';
  if (status >= 400) return 'warning';
  return 'info';
}

interface XhrState {
  method: string;
  url: string;
  started?: number;
}

const STATE = new WeakMap<object, XhrState>();

/** Installs the XHR integration; returns its uninstall function. */
export function installXhrIntegration(client: InternalClient, selfTarget: string | undefined): () => void {
  const proto = (globalThis as { XMLHttpRequest?: { prototype?: unknown } }).XMLHttpRequest?.prototype;
  if (proto === null || typeof proto !== 'object') return () => undefined;
  const target = proto as unknown as Record<string, unknown>;

  const uninstallOpen = wrapMethod<(...args: unknown[]) => unknown>(target, 'open', (original, active) => {
    return function (this: unknown, ...args: unknown[]): unknown {
      const result: unknown = original.apply(this, args);
      try {
        if (active() && !isRecording()) {
          const method = typeof args[0] === 'string' && args[0] !== '' ? cutCodePoints(String(args[0]).toUpperCase(), 16) : 'GET';
          const rawUrl = typeof args[1] === 'string' ? args[1] : String(args[1] ?? '');
          let resolved = rawUrl;
          try {
            const href = (globalThis as { location?: { href?: unknown } }).location?.href;
            resolved = new URL(rawUrl, typeof href === 'string' ? href : undefined).href;
          } catch {
            // Kept as given.
          }
          STATE.set(this as object, { method, url: resolved });
        }
      } catch {
        // A recording failure never reaches the page.
      }
      return result;
    };
  });

  const uninstallSend = wrapMethod<(...args: unknown[]) => unknown>(target, 'send', (original, active) => {
    return function (this: unknown, ...args: unknown[]): unknown {
      const result: unknown = original.apply(this, args);
      try {
        if (active() && !isRecording()) {
          const state = STATE.get(this as object);
          if (state !== undefined) {
            state.started = now();
            const xhr = this as unknown as EventTarget;
            const onLoadend = (): void => {
              try {
                xhr.removeEventListener('loadend', onLoadend);
                const excluded = selfTarget !== undefined && originAndPath(state.url) === selfTarget;
                if (!excluded) {
                  const status = Number((this as { status?: unknown }).status ?? 0);
                  const duration = Math.round(now() - (state.started ?? now()));
                  const data: Record<string, unknown> = { method: state.method, url: filterUrl(state.url), duration_ms: duration };
                  if (status !== 0) data.status_code = status;
                  record(client, { type: 'http', category: 'xhr', level: levelFor(status), data }, { xhr: this });
                }
              } catch {
                // A recording failure never reaches the page.
              }
            };
            xhr.addEventListener('loadend', onLoadend);
          }
        }
      } catch {
        // A recording failure never reaches the page.
      }
      return result;
    };
  });

  return () => {
    uninstallOpen();
    uninstallSend();
  };
}
