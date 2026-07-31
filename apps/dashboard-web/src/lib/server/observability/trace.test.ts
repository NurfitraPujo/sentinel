// Regression coverage for Defect 2 (D-d violation): the logged/returned trace id must always be the
// real OTel trace id, never a value independently re-derived from the same `traceparent` header. The
// bug this guards against hid itself precisely because the two paths agree whenever a caller supplies a
// `traceparent` — so every test here that matters covers the case where NO span is active and/or NO
// header is present, since that's the path real browser traffic takes and the one that was silently
// broken.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { trace, type Span, type SpanContext } from '@opentelemetry/api';
import { generateSpanId, generateTraceId, traceContextForRequest } from './trace';

function fakeSpan(spanContext: SpanContext): Span {
	// Only spanContext() is exercised by traceContextForRequest; the rest of the Span interface is
	// irrelevant here.
	return { spanContext: () => spanContext } as unknown as Span;
}

// 32/16 lowercase hex chars respectively — valid OTel trace/span id shapes.
const VALID_TRACE_ID = 'b829739fac5ecfd19873551891fd734a';
const VALID_SPAN_ID = 'abcdef0123456789';
const HEADER_TRACE_ID = '4bf92f3577b34da6a3ce929d0e0e4736';
const VALID_TRACEPARENT = `00-${HEADER_TRACE_ID}-00f067aa0ba902b7-01`;

describe('traceContextForRequest', () => {
	afterEach(() => {
		vi.restoreAllMocks();
	});

	it('prefers the active OTel span over the (absent) header — the path that hides the bug in prod', () => {
		vi.spyOn(trace, 'getActiveSpan').mockReturnValue(
			fakeSpan({ traceId: VALID_TRACE_ID, spanId: VALID_SPAN_ID, traceFlags: 1 }),
		);

		// No inbound traceparent at all — real browser traffic, per the bug report.
		const ctx = traceContextForRequest(null);

		expect(ctx.traceId).toBe(VALID_TRACE_ID);
		expect(ctx.spanId).toBe(VALID_SPAN_ID);
	});

	it('falls back to generating a fresh id when no span is active AND no header is present', () => {
		vi.spyOn(trace, 'getActiveSpan').mockReturnValue(undefined);

		const ctx = traceContextForRequest(null);

		// Must still produce well-formed ids (tracing-disabled / D-f fallback path), not throw or
		// return something malformed.
		expect(ctx.traceId).toMatch(/^[0-9a-f]{32}$/);
		expect(ctx.spanId).toMatch(/^[0-9a-f]{16}$/);
	});

	it('falls back to honouring a valid inbound traceparent when no span is active', () => {
		vi.spyOn(trace, 'getActiveSpan').mockReturnValue(undefined);

		const ctx = traceContextForRequest(VALID_TRACEPARENT);

		expect(ctx.traceId).toBe(HEADER_TRACE_ID);
	});

	it('ignores the header once a real active span exists — no independent re-derivation', () => {
		vi.spyOn(trace, 'getActiveSpan').mockReturnValue(
			fakeSpan({ traceId: VALID_TRACE_ID, spanId: VALID_SPAN_ID, traceFlags: 1 }),
		);

		// A header IS present, and even encodes a DIFFERENT trace id than the active span. The active
		// span must still win — proving this isn't accidentally doing both and getting lucky.
		const ctx = traceContextForRequest(VALID_TRACEPARENT);

		expect(ctx.traceId).toBe(VALID_TRACE_ID);
		expect(ctx.traceId).not.toBe(HEADER_TRACE_ID);
	});

	it('treats an invalid (all-zero) active span context as "no active span"', () => {
		vi.spyOn(trace, 'getActiveSpan').mockReturnValue(
			fakeSpan({ traceId: '0'.repeat(32), spanId: '0'.repeat(16), traceFlags: 0 }),
		);

		const ctx = traceContextForRequest(null);

		expect(ctx.traceId).toMatch(/^[0-9a-f]{32}$/);
		expect(ctx.traceId).not.toBe('0'.repeat(32));
	});
});

// Sanity checks on the raw generators these fallbacks build on — unchanged by Defect 2's fix, but
// otherwise uncovered.
describe('generateTraceId / generateSpanId', () => {
	it('produce correctly-sized lowercase hex ids', () => {
		expect(generateTraceId()).toMatch(/^[0-9a-f]{32}$/);
		expect(generateSpanId()).toMatch(/^[0-9a-f]{16}$/);
	});
});
