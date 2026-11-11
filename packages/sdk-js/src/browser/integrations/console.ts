// The console breadcrumb integration (spec §5.2, TASK-03).

import { isErrorLike } from '../../core/event.js';
import { scrubPII } from '../../core/pii.js';
import type { JsonValue } from '../../core/serialize.js';
import { sanitize } from '../../core/serialize.js';
import type { InternalClient } from '../../core/state.js';
import { filterUrlsInText } from '../../core/url.js';
import { isRecording, record, wrapMethod } from './shared.js';

const METHODS = ['debug', 'info', 'log', 'warn', 'error', 'assert', 'trace'] as const;
type ConsoleMethod = (typeof METHODS)[number];

const LEVEL: Record<ConsoleMethod, string> = {
  debug: 'debug',
  info: 'info',
  log: 'info',
  warn: 'warning',
  error: 'error',
  assert: 'error',
  trace: 'debug',
};

const MAX_ARGS = 10;
const MAX_ARG_BYTES = 512;
const MAX_MESSAGE_BYTES = 2048;
const SDK_PREFIX = '[@sentinel/sdk-js]';

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function codePoints(s: string): string[] {
  return Array.from(s);
}

function cutUtf8Bytes(s: string, max: number): string {
  let cps = codePoints(s);
  while (new TextEncoder().encode(cps.join('')).byteLength > max) cps = cps.slice(0, -1);
  return cps.join('');
}

/** One argument rendered per §5.2's table. */
function renderArg(value: unknown): string {
  if (typeof value === 'string') return value;
  if (value === null) return 'null';
  if (value === undefined) return 'undefined';
  if (typeof value === 'number' || typeof value === 'boolean' || typeof value === 'bigint') return String(value);
  if (isErrorLike(value)) {
    let name = 'Error';
    let message = '';
    try {
      name = String((value as { name?: unknown }).name ?? 'Error');
    } catch {
      name = 'Error';
    }
    try {
      message = String((value as { message?: unknown }).message ?? '');
    } catch {
      message = '';
    }
    return `${name}: ${message}`;
  }
  try {
    const { value: sanitized } = sanitize(value);
    const scrubbed = isPlainObject(sanitized) ? scrubPII(sanitized as { [key: string]: JsonValue }) : sanitized;
    return JSON.stringify(scrubbed) ?? '[unserializable]';
  } catch {
    return '[unserializable]';
  }
}

function buildMessage(args: unknown[]): { message: string; omitted: number } {
  const kept = args.slice(0, MAX_ARGS);
  const omitted = Math.max(0, args.length - MAX_ARGS);
  const rendered = kept.map((a) => cutUtf8Bytes(filterUrlsInText(renderArg(a)), MAX_ARG_BYTES));
  return { message: cutUtf8Bytes(rendered.join(' '), MAX_MESSAGE_BYTES), omitted };
}

/** Installs the console integration; returns its uninstall function. */
export function installConsoleIntegration(client: InternalClient): () => void {
  const target = (globalThis as { console?: unknown }).console;
  if (typeof target !== 'object' || target === null) return () => undefined;
  const consoleObj = target as unknown as Record<string, unknown>;

  const uninstalls: Array<() => void> = [];
  for (const name of METHODS) {
    const uninstall = wrapMethod<(...args: unknown[]) => unknown>(consoleObj, name, (original, active) => {
      return function (this: unknown, ...args: unknown[]): unknown {
        const result: unknown = original.apply(this, args);
        try {
          if (active() && !isRecording()) {
            const first = args[0];
            if (typeof first === 'string' && first.startsWith(SDK_PREFIX)) return result;
            if (name === 'assert') {
              if (args[0]) return result; // console.assert records only when the first argument is falsy.
              const rest = args.slice(1);
              const { message, omitted } = buildMessage(rest);
              const data: Record<string, unknown> = { logger: 'console' };
              if (omitted > 0) data.arguments_omitted = omitted;
              record(client, { type: 'default', category: 'console', level: LEVEL.assert, message: rest.length > 0 ? `Assertion failed: ${message}` : 'Assertion failed', data }, { input: args, level: name });
              return result;
            }
            const { message, omitted } = buildMessage(args);
            const data: Record<string, unknown> = { logger: 'console' };
            if (omitted > 0) data.arguments_omitted = omitted;
            record(client, { type: 'default', category: 'console', level: LEVEL[name], message, data }, { input: args, level: name });
          }
        } catch {
          // A recording failure never reaches the page.
        }
        return result;
      };
    });
    uninstalls.push(uninstall);
  }
  return () => {
    for (const u of uninstalls) u();
  };
}
