// The node process hooks (contract §4.6) — the exit behaviour is the contract:
//
// - uncaught exceptions are observed through `process.on('uncaughtExceptionMonitor')`, which runs before the
//   default handler and does not stop the process from exiting — behaviour-preserving by construction (N5); the
//   capture carries `metadata.source: 'uncaughtException'` (decision 37);
// - unhandled rejections: `process.on('unhandledRejection')` captures with `metadata.source:
//   'unhandledRejection'`, then re-raises the reason on the next tick when this listener is the only one
//   (`process.listenerCount('unhandledRejection') === 1`) — unconditionally, whatever `--unhandled-rejections`
//   mode the process runs in (no mode detection; under `warn` / `none` the SDK's presence makes the rejection
//   fatal, N16, documented) — so the default `throw` mode still crashes the process exactly as before, with the
//   event captured first (N6); when the host already has a listener the SDK captures and does nothing else (N7).
//   The re-raised value reaches the monitor and, being the same object, is deduplicated (contract §4.3), so the
//   event keeps `'unhandledRejection'`; a non-`Error` reason is re-raised as is (`throw reason`);
// - `beforeSend` is called synchronously on both captures and a promise-returning hook's result is ignored
//   (decision 22, `syncHookOnly`); `flush` is not awaited and there is NO fire-and-forget `fetch` here — on the
//   sole-listener path the crash event is enqueued and lost when node exits without another loop turn, which
//   is the stated behaviour (contract §1 *NOT building*, §4.6), not a bug to patch.
//
// Nothing in this module body touches `process`: `installNodeHandlers` runs from the entry's `init`, and the
// teardown hook it pushes removes both listeners (a second `init` and `close` run it).

import { capture } from '../capture.js';
import { reportError } from '../report.js';
import type { InternalClient } from '../state.js';

interface ProcessLike {
  on(event: string, listener: (...args: unknown[]) => void): unknown;
  removeListener(event: string, listener: (...args: unknown[]) => void): unknown;
  listenerCount(event: string): number;
  nextTick(fn: () => void): void;
}

function processTarget(): ProcessLike | undefined {
  const proc = (globalThis as { process?: unknown }).process;
  if (proc === null || typeof proc !== 'object') return undefined;
  const p = proc as Partial<ProcessLike>;
  return typeof p.on === 'function' && typeof p.removeListener === 'function' && typeof p.listenerCount === 'function' && typeof p.nextTick === 'function'
    ? (p as ProcessLike)
    : undefined;
}

/**
 * Install the monitor and the rejection listener on `process` for the current client and push their removal on
 * `client.teardown`. With no usable `process` (the node build evaluated where there is none), `onError` reports
 * `handler_install_failed` once and nothing is installed — never a throw.
 */
export function installNodeHandlers(client: InternalClient): void {
  const proc = processTarget();
  if (proc === undefined) {
    reportError(client, 'handler_install_failed', 'no process with on/nextTick: the uncaughtExceptionMonitor / unhandledRejection listeners were not installed');
    return;
  }
  const onMonitor = (err: unknown): void => {
    capture({ kind: 'exception', value: err, ctx: undefined, source: 'uncaughtException', syncHookOnly: true });
  };
  const onRejection = (reason: unknown): void => {
    capture({ kind: 'exception', value: reason, ctx: undefined, source: 'unhandledRejection', syncHookOnly: true });
    if (proc.listenerCount('unhandledRejection') === 1) {
      // The sole listener: re-raise so the default `throw` mode crashes exactly as it would have. The header of
      // node's crash output names this line; everything from the first `Error:` line down is the reason's own.
      proc.nextTick(() => {
        throw reason;
      });
    }
  };
  try {
    proc.on('uncaughtExceptionMonitor', onMonitor);
    proc.on('unhandledRejection', onRejection);
  } catch (cause) {
    reportError(client, 'handler_install_failed', `the process listeners could not be installed: ${String(cause)}`, { cause });
    try {
      proc.removeListener('uncaughtExceptionMonitor', onMonitor);
    } catch {
      // Nothing more to undo.
    }
    return;
  }
  client.teardown.push(() => {
    proc.removeListener('uncaughtExceptionMonitor', onMonitor);
    proc.removeListener('unhandledRejection', onRejection);
  });
}
