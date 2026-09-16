// The browser's global handlers (contract §4.6): `window.addEventListener('error', …)` and
// `window.addEventListener('unhandledrejection', …)` — `addEventListener`, never assignment to `window.onerror` /
// `window.onunhandledrejection`, so `@sentry/nextjs`'s wrapping of those properties and these listeners both fire;
// never `preventDefault()` or `stopImmediatePropagation()`, so neither swallows the other (B5, B6). The `error`
// listener is registered WITHOUT `capture: true`: a resource-load failure dispatches a plain `Event` named
// `error` that does not bubble, so it never reaches a bubbling listener on `window` — and a non-`ErrorEvent`
// arriving anyway is ignored (B26 (c)).
//
// The shape a global capture takes (decision 37, B26): an `ErrorEvent` with `error` set captures that value
// through the exception path (an `Error` by its class and stack; a thrown primitive as `NonError` — B22's
// "thrown primitive captured once"); an `ErrorEvent` without `error` (a cross-origin `"Script error."`) captures
// through `captureMessage(ev.message, { level: 'error' })` — `error_class: "Message"`, `stacktrace: []`,
// `beforeSend`'s `hint.originalException` is `ev.message`; a `PromiseRejectionEvent` captures its `reason`
// through the exception path. `metadata.source` is `'window.onerror'` / `'unhandledrejection'` on every capture
// here and on nothing else.
//
// Nothing in this module body touches `window`: the listeners are added by `installBrowserHandlers`, which the
// entry's `init` calls, and removed by the teardown hook it pushes on the client (a second `init` and `close`
// run it — contract §4.2, §4.3).

import { capture } from '../capture.js';
import { reportError } from '../report.js';
import type { InternalClient } from '../state.js';

interface Listener {
  (ev: unknown): void;
}

interface WindowLike {
  addEventListener(type: string, listener: Listener, options?: boolean | { capture?: boolean }): void;
  removeEventListener(type: string, listener: Listener, options?: boolean | { capture?: boolean }): void;
}

function windowTarget(): WindowLike | undefined {
  const win = (globalThis as { window?: unknown }).window;
  if (win === null || typeof win !== 'object') return undefined;
  const w = win as Partial<WindowLike>;
  return typeof w.addEventListener === 'function' && typeof w.removeEventListener === 'function' ? (w as WindowLike) : undefined;
}

/**
 * An `ErrorEvent` by construction where the constructor exists, else by its shape — a resource-load `Event` has
 * neither `message` nor `error`, so it fails both tests.
 */
function isErrorEvent(ev: unknown): ev is { message: unknown; error: unknown } {
  if (ev === null || typeof ev !== 'object') return false;
  const ctor = (globalThis as { ErrorEvent?: unknown }).ErrorEvent;
  if (typeof ctor === 'function' && ev instanceof (ctor as new () => object)) return true;
  return 'error' in ev && typeof (ev as { message?: unknown }).message === 'string';
}

function messageOf(ev: { message: unknown }): string {
  return typeof ev.message === 'string' ? ev.message : String(ev.message ?? '');
}

/**
 * Install the two listeners on `window` for the current client and push their removal on `client.teardown`.
 * Where no `window` with `addEventListener` exists (the browser build evaluated under node), `onError` reports
 * `handler_install_failed` once and nothing is installed — never a throw.
 */
export function installBrowserHandlers(client: InternalClient): void {
  const win = windowTarget();
  if (win === undefined) {
    reportError(client, 'handler_install_failed', 'no window with addEventListener: the error / unhandledrejection listeners were not installed');
    return;
  }
  const onError: Listener = (ev) => {
    if (!isErrorEvent(ev)) return;
    if (ev.error !== null && ev.error !== undefined) {
      capture({ kind: 'exception', value: ev.error, ctx: undefined, source: 'window.onerror' });
    } else {
      capture({ kind: 'message', value: messageOf(ev), ctx: { level: 'error' }, source: 'window.onerror' });
    }
  };
  const onRejection: Listener = (ev) => {
    const reason = ev !== null && typeof ev === 'object' ? (ev as { reason?: unknown }).reason : undefined;
    capture({ kind: 'exception', value: reason, ctx: undefined, source: 'unhandledrejection' });
  };
  try {
    win.addEventListener('error', onError);
    win.addEventListener('unhandledrejection', onRejection);
  } catch (cause) {
    reportError(client, 'handler_install_failed', `the error / unhandledrejection listeners could not be installed: ${String(cause)}`, { cause });
    try {
      win.removeEventListener('error', onError);
    } catch {
      // Nothing more to undo.
    }
    return;
  }
  client.teardown.push(() => {
    win.removeEventListener('error', onError);
    win.removeEventListener('unhandledrejection', onRejection);
  });
}
