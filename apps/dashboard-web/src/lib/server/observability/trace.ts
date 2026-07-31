// Request-scoped trace context for the dashboard's SvelteKit server. There is no framework-level
// "context object" threaded through every function call the way apps/*-go thread a context.Context, so
// this uses Node's AsyncLocalStorage to get the same effect: whatever runs (directly or via
// await/promise chains) inside `runWithTraceContext`'s callback can read the active trace/span id
// without it being passed as an explicit parameter — mirrors how packages/shared-go/obs.Handler reads
// trace/span ids off a context.Context automatically (see log.go).
//
// Server-only: uses Node's node:async_hooks and node:crypto. Do not import this from a .svelte
// component or any module reachable from the client bundle — SvelteKit's `$lib/server/` convention
// already guards this (Vite fails the client build if a client module reaches in here).

import { AsyncLocalStorage } from 'node:async_hooks';
import { randomBytes } from 'node:crypto';
import { trace } from '@opentelemetry/api';

export interface TraceContext {
	/** 32 lowercase hex chars (128-bit), matching OTel's trace id encoding. */
	traceId: string;
	/** 16 lowercase hex chars (64-bit) — this request's OWN span id, not the caller's. */
	spanId: string;
}

const storage = new AsyncLocalStorage<TraceContext>();

const TRACEPARENT_RE = /^([0-9a-f]{2})-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})$/i;
const ALL_ZERO_TRACE_ID = '0'.repeat(32);
const ALL_ZERO_SPAN_ID = '0'.repeat(16);

/**
 * Parses an inbound W3C `traceparent` header (https://www.w3.org/TR/trace-context/#traceparent-header).
 * Returns null for anything missing/malformed/all-zero so callers fall back to starting a fresh trace —
 * a bad header must never crash the request.
 */
export function parseTraceparent(header: string | null | undefined): { traceId: string } | null {
	if (!header) return null;
	const match = TRACEPARENT_RE.exec(header.trim());
	if (!match) return null;

	const [, version, traceId, spanId] = match;
	// version "ff" is explicitly reserved/invalid per the W3C spec; a future version format isn't
	// something we can honour, so it degrades the same as "no header at all".
	if (version.toLowerCase() === 'ff') return null;
	if (traceId.toLowerCase() === ALL_ZERO_TRACE_ID) return null;
	if (spanId.toLowerCase() === ALL_ZERO_SPAN_ID) return null;

	return { traceId: traceId.toLowerCase() };
}

/** Generates a fresh 128-bit trace id, hex-encoded — used when no valid inbound traceparent exists. */
export function generateTraceId(): string {
	return randomBytes(16).toString('hex');
}

/** Generates this request's own 64-bit span id, hex-encoded. */
export function generateSpanId(): string {
	return randomBytes(8).toString('hex');
}

/**
 * Builds the TraceContext for an inbound request.
 *
 * D-d (docs/plans/OBSERVABILITY_PLAN.md) is explicit: the correlation id IS the OTel trace id, not a
 * bespoke scheme living alongside it. `instrumentation.mjs`'s `HttpInstrumentation` already builds a
 * SERVER span for this exact request before hooks.server.ts ever runs, and it does so by extracting any
 * inbound `traceparent` itself via the W3C propagator — so `trace.getActiveSpan()` is the ground truth
 * for both this request's trace id (the caller's, if a valid header was honoured, else one the SDK
 * minted) and its own span id. Reading that span, instead of independently re-parsing the same header
 * here, is what keeps the two in agreement; parsing it a second time in a separate, unconnected code path
 * is exactly how they drifted (three different trace ids logged for one request, reproduced in review).
 *
 * `getActiveSpan()` (and `isSpanContextValid`, guarding against a stray no-op/all-zero context) returns
 * nothing usable when this runs outside an OTel span: tracing disabled entirely (no
 * OTEL_EXPORTER_OTLP_(TRACES_)?ENDPOINT, OTEL_SDK_DISABLED=true, or instrumentation.mjs's bootstrap
 * itself failed — all degrade the same way, see that file), or a caller of this function in a test. That
 * is the D-f fallback ("no spans, but still honour/emit traceparent"), so this still needs to honour a
 * valid inbound header and otherwise mint a fresh id on its own — the original behaviour, kept verbatim
 * below as the fallback rather than replaced.
 *
 * This diverges only for real, un-instrumented traffic that also sends no `traceparent` — which is
 * exactly the case a caller-supplied header on both sides cannot catch, per the bug report. Tests MUST
 * cover the no-span/no-header path, not just the header-agrees-by-coincidence one.
 */
export function traceContextForRequest(traceparentHeader: string | null | undefined): TraceContext {
	const activeSpanContext = trace.getActiveSpan()?.spanContext();
	if (activeSpanContext && trace.isSpanContextValid(activeSpanContext)) {
		return { traceId: activeSpanContext.traceId, spanId: activeSpanContext.spanId };
	}

	const inbound = parseTraceparent(traceparentHeader);
	return {
		traceId: inbound?.traceId ?? generateTraceId(),
		spanId: generateSpanId(),
	};
}

/** Runs `fn` with `ctx` as the active trace context for anything it calls, directly or via async/await. */
export function runWithTraceContext<T>(ctx: TraceContext, fn: () => T): T {
	return storage.run(ctx, fn);
}

/** The active trace context, if any code path reached here via `runWithTraceContext` (e.g. a script
 * invoked outside a request, or a test that didn't set one up) — undefined otherwise. */
export function getTraceContext(): TraceContext | undefined {
	return storage.getStore();
}
