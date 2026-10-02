// `traceparent` propagation on outgoing fetch / XHR (spec *Trace propagation*, cross-repo-trace-propagation D4): the page's
// trace id and a fresh span id per request go out as `traceparent: 00-<trace>-<span>-01` to the URLs
// `tracePropagationTargets` matches — by default the page's own origin only. Never `tracestate`, `baggage` or
// `sentry-trace`; never overwrites a `traceparent` the caller (or another SDK) already set. Independent of the
// breadcrumb integrations, and every failure degrades to sending the request untouched.

import type { InternalClient } from '../core/state.js';
import { reportError } from '../core/report.js';
import { originAndPath, wrapMethod } from './integrations/shared.js';
import { newSpanId } from './trace.js';

export type PropagationTarget = string | RegExp;

/** Where a RELATIVE request URL resolves: `document.baseURI` (a `<base href>` moves it), else the page URL. */
function baseHref(): string | undefined {
  // Bare globals: the es-compat scan (spec §5.1 F6) admits no new file naming the global object.
  const base = typeof document === 'undefined' ? undefined : document.baseURI;
  if (typeof base === 'string' && base !== '') return base;
  const href = typeof location === 'undefined' ? undefined : location.href;
  return typeof href === 'string' ? href : undefined;
}

function pageOrigin(): string | undefined {
  try {
    return typeof location === 'undefined' ? undefined : new URL(location.href).origin;
  } catch {
    return undefined;
  }
}

function absolute(raw: string): string | undefined {
  try {
    return new URL(raw, baseHref()).href;
  } catch {
    return undefined;
  }
}

/** A string target is `/path` (the page's own origin) or an absolute http(s) URL; anything else never matches. */
function parseStringTarget(t: string): { origin: string; path: string } | undefined {
  try {
    const u = t.startsWith('/') && !t.startsWith('//') ? new URL(t, pageOrigin()) : new URL(t);
    if (u.protocol !== 'http:' && u.protocol !== 'https:') return undefined;
    return { origin: u.origin, path: u.pathname };
  } catch {
    return undefined;
  }
}

/**
 * The option as resolved: `undefined` = the default (same origin). A non-array reports `invalid_option` and falls
 * back to the default; an entry that is neither a string nor a RegExp, or a string that is neither `/path` nor an
 * absolute http(s) URL (`'api/'` would silently never match), is dropped with one report each.
 */
export function resolveTargets(client: InternalClient, given: unknown): PropagationTarget[] | undefined {
  if (given === undefined) return undefined;
  if (!Array.isArray(given)) {
    reportError(client, 'invalid_option', `tracePropagationTargets ${JSON.stringify(given)} is not an array; using same origin`, { option: 'tracePropagationTargets' });
    return undefined;
  }
  const out: PropagationTarget[] = [];
  for (const t of given as unknown[]) {
    if (t instanceof RegExp) out.push(t);
    else if (typeof t === 'string' && parseStringTarget(t) !== undefined) out.push(t);
    else reportError(client, 'invalid_option', `tracePropagationTargets entry ${String(t)} is not a RegExp, a "/path" or an absolute http(s) URL; ignored`, { option: 'tracePropagationTargets' });
  }
  return out;
}

/**
 * Whether `url` (resolved against the document base) is a propagation target. Default (no option): the PAGE's
 * origin. With the option, it REPLACES the default: a string target matches when the request's ORIGIN equals the
 * target's exactly and the request path equals the target's path or lies under it, segment-aware (`'/api'` matches
 * `/api` and `/api/x`, not `/apiary`); a `/path` target is on the page's origin. A RegExp is tested against the
 * absolute URL. An empty array propagates nowhere.
 */
export function matchesTarget(url: string, targets: PropagationTarget[] | undefined): boolean {
  const abs = absolute(url);
  if (abs === undefined) return false;
  let u: URL;
  try {
    u = new URL(abs);
  } catch {
    return false;
  }
  if (targets === undefined) return u.origin === pageOrigin();
  for (const t of targets) {
    if (typeof t === 'string') {
      const target = parseStringTarget(t);
      if (target === undefined || u.origin !== target.origin) continue;
      const prefix = target.path.endsWith('/') ? target.path : `${target.path}/`;
      if (u.pathname === target.path || u.pathname.startsWith(prefix)) return true;
    } else {
      t.lastIndex = 0;
      const hit = t.test(abs);
      t.lastIndex = 0;
      if (hit) return true;
    }
  }
  return false;
}

function headerValue(client: InternalClient): string | undefined {
  const t = client.pageTrace;
  return t === undefined ? undefined : `00-${t.trace_id}-${newSpanId()}-01`;
}

function hasTraceparent(headers: unknown): boolean {
  if (headers === undefined || headers === null) return false;
  // Duck-typed: a Request's Headers can come from another realm (a polyfill, an iframe), failing `instanceof`.
  if (typeof (headers as { has?: unknown }).has === 'function') return (headers as { has: (n: string) => boolean }).has('traceparent');
  if (Array.isArray(headers)) return headers.some((p) => Array.isArray(p) && String(p[0]).toLowerCase() === 'traceparent');
  if (typeof headers === 'object') return Object.keys(headers).some((k) => k.toLowerCase() === 'traceparent');
  return false;
}

function isPlainObject(v: unknown): boolean {
  if (typeof v !== 'object' || v === null) return false;
  const proto = Object.getPrototypeOf(v) as unknown;
  return proto === Object.prototype || proto === null;
}

function isRequest(v: unknown): v is { url: string; headers: unknown; clone: () => { headers: { set: (n: string, v: string) => void } } } {
  const r = v as { url?: unknown; clone?: unknown; headers?: unknown } | null;
  return typeof r === 'object' && r !== null && typeof r.url === 'string' && typeof r.clone === 'function' && typeof r.headers === 'object';
}

function installFetch(client: InternalClient, targets: PropagationTarget[] | undefined, selfTarget: string | undefined): () => void {
  if (typeof window === 'undefined') return () => undefined;
  return wrapMethod<(...args: unknown[]) => unknown>(window as unknown as Record<string, unknown>, 'fetch', (original, active) => {
    return function (this: unknown, ...args: unknown[]): unknown {
      let callArgs = args;
      try {
        if (active() && typeof Headers !== 'undefined') {
          const [input, init] = args;
          // An init that is not a plain object (a Request passed as init, say) is not safe to spread: its method /
          // body are prototype getters. Such a call goes out untouched.
          if (init === undefined || isPlainObject(init)) {
            const request = isRequest(input) ? input : undefined;
            const raw = request !== undefined ? request.url : String(input);
            const initHeaders = (init as { headers?: unknown } | undefined)?.headers;
            // Per the Fetch spec `init.headers` REPLACES a Request's headers, so only the headers the call will
            // actually send are checked for a caller's traceparent.
            const effective = initHeaders !== undefined ? initHeaders : request?.headers;
            const abs = absolute(raw);
            const excluded = selfTarget !== undefined && abs !== undefined && originAndPath(abs) === selfTarget;
            if (!excluded && matchesTarget(raw, targets) && !hasTraceparent(effective)) {
              const value = headerValue(client);
              if (value !== undefined) {
                if (initHeaders !== undefined || request === undefined) {
                  const merged = new Headers(initHeaders as HeadersInit | undefined);
                  merged.set('traceparent', value);
                  callArgs = [input, { ...(init as object | undefined), headers: merged }, ...args.slice(2)];
                } else {
                  // A Request with no init.headers: a non-empty init would reset its referrer and referrerPolicy
                  // (Fetch spec), so send a clone carrying the header instead — everything else is preserved.
                  // sentinel-sdkjs-request-clone-unread BUG-01: a Request WITH a body is not cloned. clone() tees the
                  // stream and the caller's original is never read, so `bodyUsed` stays false (a second fetch(req)
                  // succeeds where the platform throws) and a streaming upload is buffered. Constructing from the
                  // Request transfers its body and disturbs the source, exactly as a plain fetch(req) would.
                  // referrer/referrerPolicy are passed explicitly because a non-empty init resets them (Fetch spec).
                  const rq = request as unknown as Request;
                  const headers = new Headers(rq.headers);
                  headers.set('traceparent', value);
                  const copy =
                    rq.body === null
                      ? rq.clone()
                      : new Request(rq, { headers, referrer: rq.referrer, referrerPolicy: rq.referrerPolicy, duplex: 'half' } as RequestInit);
                  if (rq.body === null) copy.headers.set('traceparent', value);
                  callArgs = [copy, ...args.slice(1)];
                }
              }
            }
          }
        }
      } catch {
        callArgs = args;
      }
      return original.apply(this, callArgs);
    };
  });
}

interface XhrState {
  url: string;
  callerSet: boolean;
}

const XHR = new WeakMap<object, XhrState>();

function installXhr(client: InternalClient, targets: PropagationTarget[] | undefined, selfTarget: string | undefined): () => void {
  const proto = typeof XMLHttpRequest === 'undefined' ? undefined : (XMLHttpRequest as { prototype?: unknown }).prototype;
  if (proto === null || typeof proto !== 'object') return () => undefined;
  const target = proto as unknown as Record<string, unknown>;
  const uninstalls: Array<() => void> = [];

  uninstalls.push(
    wrapMethod<(...args: unknown[]) => unknown>(target, 'open', (original, active) => {
      return function (this: unknown, ...args: unknown[]): unknown {
        const result: unknown = original.apply(this, args);
        try {
          if (active()) XHR.set(this as object, { url: typeof args[1] === 'string' ? args[1] : String(args[1] ?? ''), callerSet: false });
        } catch {
          // A tracing failure never reaches the page.
        }
        return result;
      };
    }),
  );
  uninstalls.push(
    wrapMethod<(...args: unknown[]) => unknown>(target, 'setRequestHeader', (original, active) => {
      return function (this: unknown, ...args: unknown[]): unknown {
        try {
          if (active() && String(args[0]).toLowerCase() === 'traceparent') {
            const state = XHR.get(this as object);
            if (state !== undefined) state.callerSet = true;
          }
        } catch {
          // A tracing failure never reaches the page.
        }
        return original.apply(this, args);
      };
    }),
  );
  uninstalls.push(
    wrapMethod<(...args: unknown[]) => unknown>(target, 'send', (original, active) => {
      return function (this: unknown, ...args: unknown[]): unknown {
        try {
          const state = active() ? XHR.get(this as object) : undefined;
          if (state !== undefined && !state.callerSet) {
            const abs = absolute(state.url);
            const excluded = selfTarget !== undefined && abs !== undefined && originAndPath(abs) === selfTarget;
            const value = excluded || !matchesTarget(state.url, targets) ? undefined : headerValue(client);
            // Set once; a re-sent XHR gets a fresh `open`, which resets the state.
            if (value !== undefined) {
              state.callerSet = true;
              (this as { setRequestHeader: (n: string, v: string) => void }).setRequestHeader('traceparent', value);
            }
          }
        } catch {
          // A tracing failure never reaches the page: the request goes out untouched.
        }
        return original.apply(this, args);
      };
    }),
  );
  return () => {
    for (const u of uninstalls) u();
  };
}

/** Installs fetch + XHR propagation; returns one combined uninstall. `selfTargetUrl` (the tunnel) is never given a header. */
export function installPropagation(client: InternalClient, given: unknown, selfTargetUrl: string | undefined): () => void {
  const targets = resolveTargets(client, given);
  const selfTarget = selfTargetUrl === undefined ? undefined : originAndPath(selfTargetUrl);
  const uninstalls = [installFetch(client, targets, selfTarget), installXhr(client, targets, selfTarget)];
  return () => {
    for (const u of uninstalls) u();
  };
}
