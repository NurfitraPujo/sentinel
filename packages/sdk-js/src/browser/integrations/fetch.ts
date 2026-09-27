// The fetch breadcrumb integration (spec §5.3, §5 *No wrapper marks a page promise handled*, TASK-03).

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

function levelFor(status: number | undefined): string {
  if (status === undefined) return 'error';
  if (status >= 500) return 'error';
  if (status >= 400) return 'warning';
  return 'info';
}

function methodOf(input: unknown, init: unknown): string {
  const fromInit = (init as { method?: unknown } | undefined)?.method;
  if (typeof fromInit === 'string' && fromInit !== '') return cutCodePoints(fromInit.toUpperCase(), 16);
  const fromRequest = (input as { method?: unknown } | undefined)?.method;
  if (typeof fromRequest === 'string' && fromRequest !== '') return cutCodePoints(fromRequest.toUpperCase(), 16);
  return 'GET';
}

function urlOf(input: unknown): string {
  const fromRequest = (input as { url?: unknown } | undefined)?.url;
  const raw = typeof fromRequest === 'string' ? fromRequest : String(input);
  try {
    const href = (globalThis as { location?: { href?: unknown } }).location?.href;
    return new URL(raw, typeof href === 'string' ? href : undefined).href;
  } catch {
    return raw;
  }
}

/** Installs the fetch integration; returns its uninstall function. */
export function installFetchIntegration(client: InternalClient, selfTarget: string | undefined): () => void {
  const g = globalThis as unknown as Record<string, unknown>;
  return wrapMethod<(...args: unknown[]) => unknown>(g, 'fetch', (original, active) => {
    return function (this: unknown, ...args: unknown[]): unknown {
      const [input, init] = args;
      const result: unknown = original.apply(this, args);
      if (!active() || isRecording()) return result;
      if (typeof (result as { then?: unknown } | null)?.then !== 'function') return result;
      const method = methodOf(input, init);
      const rawUrl = urlOf(input);
      const url = filterUrl(rawUrl);
      const excluded = selfTarget !== undefined && originAndPath(rawUrl) === selfTarget;
      const started = now();
      const hint = { input: args };
      return (result as Promise<unknown>).then(
        (response) => {
          if (!excluded) {
            try {
              const status = (response as { status?: unknown } | null)?.status;
              const statusCode = typeof status === 'number' ? status : undefined;
              const duration = Math.round(now() - started);
              const data: Record<string, unknown> = { method, url, duration_ms: duration };
              if (statusCode !== undefined) data.status_code = statusCode;
              record(client, { type: 'http', category: 'fetch', level: levelFor(statusCode), data }, { ...hint, response });
            } catch {
              // A recording failure never reaches the page.
            }
          }
          return response;
        },
        (error) => {
          if (!excluded) {
            try {
              const duration = Math.round(now() - started);
              record(client, { type: 'http', category: 'fetch', level: 'error', data: { method, url, duration_ms: duration } }, { ...hint, error });
            } catch {
              // A recording failure never reaches the page.
            }
          }
          throw error;
        },
      );
    };
  });
}
