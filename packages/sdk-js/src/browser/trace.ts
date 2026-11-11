// The browser's page trace (spec *Trace propagation*, cross-repo-trace-propagation D4): one trace id per page load, renewed on
// each navigation to a new pathname (pushState, replaceState, popstate) and once it is older than `pageTraceMaxAgeMs`
// (checked lazily whenever it is read — see `currentPageTrace`; no timer), held on the client so a captured event
// without an explicit `traceparent` carries it and the propagation wrappers send it. Independent of the
// breadcrumb switches — `breadcrumbs: false` does not turn tracing off.

import { reportError } from '../core/report.js';
import type { InternalClient, PageTrace } from '../core/state.js';
import { wrapMethod } from './integrations/shared.js';

function hex(bytes: number): string {
  const out = new Uint8Array(bytes);
  // Bare globals: the es-compat scan (spec §5.1 F6) admits no new file naming the global object.
  const c = typeof crypto === 'undefined' ? undefined : crypto;
  for (let attempt = 0; attempt < 4; attempt++) {
    if (typeof c?.getRandomValues === 'function') c.getRandomValues(out);
    else for (let i = 0; i < bytes; i++) out[i] = Math.floor(Math.random() * 256);
    // W3C: an all-zero id is invalid.
    if (out.some((b) => b !== 0)) break;
    out[bytes - 1] = 1;
  }
  return Array.from(out, (b) => b.toString(16).padStart(2, '0')).join('');
}

/** A fresh 16-hex span id (one per propagated request). */
export function newSpanId(): string {
  return hex(8);
}

export const DEFAULT_PAGE_TRACE_MAX_AGE_MS = 1_800_000;
export const MIN_PAGE_TRACE_MAX_AGE_MS = 60_000;

function newPageTrace(): PageTrace {
  return { trace_id: hex(16), span_id: newSpanId() };
}

/** `pageTraceMaxAgeMs` as resolved: default 30 min, `0` / `Infinity` = off, invalid → default, below the 60 s floor → clamped (each reported). */
function resolveMaxAge(client: InternalClient, given: unknown): number {
  if (given === undefined) return DEFAULT_PAGE_TRACE_MAX_AGE_MS;
  if (typeof given !== 'number' || Number.isNaN(given) || given < 0) {
    reportError(client, 'invalid_option', `pageTraceMaxAgeMs ${String(given)} is not a number >= 0 (0 or Infinity = never renew by age); using ${DEFAULT_PAGE_TRACE_MAX_AGE_MS}`, { option: 'pageTraceMaxAgeMs' });
    return DEFAULT_PAGE_TRACE_MAX_AGE_MS;
  }
  if (given > 0 && given < MIN_PAGE_TRACE_MAX_AGE_MS) {
    reportError(client, 'invalid_option', `pageTraceMaxAgeMs ${given} is below ${MIN_PAGE_TRACE_MAX_AGE_MS}; clamped to ${MIN_PAGE_TRACE_MAX_AGE_MS}`, { option: 'pageTraceMaxAgeMs' });
    return MIN_PAGE_TRACE_MAX_AGE_MS;
  }
  return given === Infinity ? 0 : given;
}

function pathKey(): string {
  try {
    return typeof location === 'undefined' ? '' : `${String(location.origin)}${String(location.pathname)}`;
  } catch {
    return '';
  }
}

/** Starts the page trace now and renews it on each pathname change; returns the uninstall function. */
export function installPageTrace(client: InternalClient, maxAgeGiven?: unknown, clock: () => number = () => Date.now()): () => void {
  const maxAge = resolveMaxAge(client, maxAgeGiven);
  // A throwing clock never stops the trace installing: the trace then simply never ages.
  const now = (): number | undefined => {
    try {
      return clock();
    } catch {
      return undefined;
    }
  };
  let startedAt = now();
  const start = (): void => {
    client.pageTrace = newPageTrace();
    startedAt = now();
  };
  start();
  // Lazy renewal at read time — no timer (it would keep nothing alive and wake background tabs).
  client.renewPageTrace = () => {
    try {
      const n = now();
      // A wall clock stepped backwards (n < startedAt) renews too, rather than freezing the trace until it catches up.
      if (maxAge > 0 && client.pageTrace !== undefined && n !== undefined && startedAt !== undefined && (n < startedAt || n - startedAt >= maxAge)) start();
    } catch {
      // A tracing failure never reaches the page.
    }
    return client.pageTrace;
  };
  let last = pathKey();
  const check = (): void => {
    try {
      const now = pathKey();
      if (now !== last) {
        last = now;
        start();
      }
    } catch {
      // A tracing failure never reaches the page.
    }
  };

  const history = typeof window === 'undefined' ? undefined : window.history;
  const uninstalls: Array<() => void> = [];
  if (history !== null && typeof history === 'object') {
    for (const name of ['pushState', 'replaceState']) {
      uninstalls.push(
        wrapMethod<(...args: unknown[]) => unknown>(history as unknown as Record<string, unknown>, name, (original, active) => {
          return function (this: unknown, ...args: unknown[]): unknown {
            const result: unknown = original.apply(this, args);
            if (active()) check();
            return result;
          };
        }),
      );
    }
  }
  const win = typeof window === 'undefined' ? undefined : (window as EventTarget);
  let live = true;
  const onPopstate = (): void => {
    if (live) check();
  };
  try {
    win?.addEventListener?.('popstate', onPopstate);
  } catch {
    // No window with addEventListener: nothing to install.
  }
  return () => {
    live = false;
    for (const u of uninstalls) u();
    try {
      win?.removeEventListener?.('popstate', onPopstate);
    } catch {
      // Nothing more to undo.
    }
    client.pageTrace = undefined;
    client.renewPageTrace = undefined;
  };
}
