// The navigation breadcrumb integration (spec §5.5, TASK-03): history.pushState / replaceState and popstate.

import { filterUrl } from '../../core/url.js';
import type { InternalClient } from '../../core/state.js';
import { isRecording, record, wrapMethod } from './shared.js';

function currentHref(): string {
  try {
    const href = (globalThis as { location?: { href?: unknown } }).location?.href;
    return typeof href === 'string' ? href : '';
  } catch {
    return '';
  }
}

/** A same-origin URL is recorded relative (path + query); a cross-origin one absolute (Sentry's rule). */
function relativeOrAbsolute(url: string): string {
  try {
    const u = new URL(url);
    const base = new URL(currentHref());
    return u.origin === base.origin ? u.pathname + u.search : u.href;
  } catch {
    return url;
  }
}

function resolveArg(arg: unknown): string | undefined {
  if (typeof arg === 'string') return arg;
  if (arg instanceof URL) return arg.href;
  return undefined;
}

function resolveAgainstHref(raw: string | undefined): string {
  if (raw === undefined) return currentHref();
  try {
    return new URL(raw, currentHref()).href;
  } catch {
    return currentHref();
  }
}

/** Installs the navigation integration; returns its uninstall function. */
export function installNavigationIntegration(client: InternalClient): () => void {
  const history = (globalThis as { history?: unknown }).history;
  const win = (globalThis as { window?: unknown }).window;
  if (history === null || typeof history !== 'object') return () => undefined;
  const historyObj = history as unknown as Record<string, unknown>;

  let lastHref = currentHref();

  function maybeRecord(rawTo: string, kind: 'pushState' | 'replaceState' | 'popstate'): void {
    try {
      if (!isRecording()) {
        const from = filterUrl(relativeOrAbsolute(lastHref));
        const to = filterUrl(relativeOrAbsolute(rawTo));
        if (from !== to) {
          record(client, { type: 'navigation', category: 'navigation', level: 'info', data: { from, to } }, { from: lastHref, to: rawTo, kind });
        }
      }
    } catch {
      // A recording failure never reaches the page.
    } finally {
      lastHref = rawTo;
    }
  }

  const uninstallPush = wrapMethod<(...args: unknown[]) => unknown>(historyObj, 'pushState', (original, active) => {
    return function (this: unknown, ...args: unknown[]): unknown {
      const result: unknown = original.apply(this, args);
      if (active()) {
        const resolved = resolveArg(args[2]);
        maybeRecord(resolveAgainstHref(resolved), 'pushState');
      }
      return result;
    };
  });

  const uninstallReplace = wrapMethod<(...args: unknown[]) => unknown>(historyObj, 'replaceState', (original, active) => {
    return function (this: unknown, ...args: unknown[]): unknown {
      const result: unknown = original.apply(this, args);
      if (active()) {
        const resolved = resolveArg(args[2]);
        maybeRecord(resolveAgainstHref(resolved), 'replaceState');
      }
      return result;
    };
  });

  let popstateActive = true;
  const onPopstate = (): void => {
    if (popstateActive) maybeRecord(currentHref(), 'popstate');
  };
  let popstateInstalled = false;
  try {
    (win as EventTarget | undefined)?.addEventListener?.('popstate', onPopstate);
    popstateInstalled = true;
  } catch {
    // No window with addEventListener: nothing to install.
  }

  return () => {
    uninstallPush();
    uninstallReplace();
    popstateActive = false;
    if (popstateInstalled) {
      try {
        (win as EventTarget | undefined)?.removeEventListener?.('popstate', onPopstate);
      } catch {
        // Nothing more to undo.
      }
    }
  };
}
