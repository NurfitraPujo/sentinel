// Test plumbing for the event pipeline: a client installed through `initCore` whose queue the test reads back as
// parsed events, and the slot-clearing `beforeEach` every suite here uses (contract §4.1 *Implementer guidance
// for the test suite*: the slot lives on `globalThis`, so vitest's module isolation does not reset it).

import { vi } from 'vitest';
import { initCore } from '../../src/core/init.js';
import { clearSlot, getInternalClient } from '../../src/core/state.js';
import type { CoreOptions, Runtime, SentinelEvent, SentinelSdkError } from '../../src/core/types.js';

export const SLOT = Symbol.for('@sentinel/sdk-js');

/** Back to "before the first `init`": the slot deleted, every spy restored. */
export function resetPipeline(): void {
  clearSlot();
  vi.restoreAllMocks();
}

export interface Pipeline {
  /** The events the client was handed, parsed from the serialised strings, in enqueue order. */
  events(): SentinelEvent[];
  /** Every `onError` report, in order. */
  errors: SentinelSdkError[];
  /** Every `debug` line, in order (the client runs with `debug: true`). */
  debugLines: string[];
}

/** Install a client for `runtime` with `options` plus `debug: true` and an `onError` recorder. */
export function setup(options: CoreOptions = {}, runtime: Runtime = 'browser'): Pipeline {
  const errors: SentinelSdkError[] = [];
  const debugLines: string[] = [];
  vi.spyOn(console, 'debug').mockImplementation((line: unknown) => {
    debugLines.push(String(line));
  });
  const onError = options.onError;
  initCore(
    {
      ...options,
      debug: true,
      onError: (err) => {
        errors.push(err);
        onError?.(err);
      },
    },
    runtime,
  );
  return {
    events: () => (getInternalClient()?.queue ?? []).map((json) => JSON.parse(json) as SentinelEvent),
    errors,
    debugLines,
  };
}

/** The `<16-hex>.js` Turbopack production chunk name (contract §4.3, decision 27). */
export const TURBOPACK_CHUNK = 'https://app/_next/static/chunks/8f3a1c2b9d4e7f60.js';
