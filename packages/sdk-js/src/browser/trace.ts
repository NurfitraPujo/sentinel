// The browser's page trace (spec *Trace propagation*, cross-repo-trace-propagation D4): one trace id per page load, renewed on
// each navigation to a new pathname (pushState, replaceState, popstate), held on the client so a captured event
// without an explicit `traceparent` carries it and the propagation wrappers send it. Independent of the
// breadcrumb switches — `breadcrumbs: false` does not turn tracing off.

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

function newPageTrace(): PageTrace {
  return { trace_id: hex(16), span_id: newSpanId() };
}

function pathKey(): string {
  try {
    return typeof location === 'undefined' ? '' : `${String(location.origin)}${String(location.pathname)}`;
  } catch {
    return '';
  }
}

/** Starts the page trace now and renews it on each pathname change; returns the uninstall function. */
export function installPageTrace(client: InternalClient): () => void {
  client.pageTrace = newPageTrace();
  let last = pathKey();
  const check = (): void => {
    try {
      const now = pathKey();
      if (now !== last) {
        last = now;
        client.pageTrace = newPageTrace();
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
  };
}
