import { describe, it, expect } from 'vitest';
import {
	readReportBody,
	parseReportRequest,
	parseOccurredAt,
	MAX_REPORT_BODY_BYTES,
	MAX_CONTEXT_BYTES,
	MAX_ATTACHMENTS,
} from './report-intake';

/**
 * sentinel-user-report-issue §4.1's request table and §3's caps, asserted below HTTP.
 *
 * Every failure must be §7's envelope with the offending path in `details.field` -- that is what
 * makes "`400 invalid_request` (empty `text`, `details.field` populated)" observable at all, and it
 * is the half of TASK-03's criterion 1 that does not need a database.
 */

const ATTACHMENT_ID = '3b1f0d2c-9a4e-4b77-8f21-6d5c4b3a2019';

function minimal(overrides: Record<string, unknown> = {}) {
	return {
		text: 'The save button spins forever.',
		reporter: { external_id: 'u_8f3c41' },
		...overrides,
	};
}

/** Runs `fn` and returns the §7 envelope it threw, failing the test if it did not throw one. */
async function envelope(fn: () => unknown): Promise<{ status: number; body: any }> {
	try {
		await fn();
	} catch (err) {
		if (!(err instanceof Response)) throw err;
		return { status: err.status, body: await err.json() };
	}
	throw new Error('expected a §7 Response to be thrown, but the call succeeded');
}

function jsonRequest(body: string, headers: Record<string, string> = {}): Request {
	return new Request('http://localhost/api/report', {
		method: 'POST',
		headers: { 'Content-Type': 'application/json', ...headers },
		body,
	});
}

describe('§4.1 readReportBody', () => {
	it('accepts application/json, with or without a charset parameter', async () => {
		await expect(readReportBody(jsonRequest('{"a":1}'))).resolves.toMatchObject({ raw: { a: 1 } });
		await expect(
			readReportBody(jsonRequest('{"a":1}', { 'Content-Type': 'application/json; charset=utf-8' }))
		).resolves.toMatchObject({ raw: { a: 1 } });
	});

	it.each(['text/plain', 'application/x-www-form-urlencoded', 'multipart/form-data', ''])(
		'400 invalid_request for Content-Type %s',
		async (contentType) => {
			const req = new Request('http://localhost/api/report', { method: 'POST', body: '{}' });
			if (contentType) req.headers.set('content-type', contentType);
			else req.headers.delete('content-type');
			const res = await envelope(() => readReportBody(req));
			expect(res.status).toBe(400);
			expect(res.body.error).toMatchObject({ code: 'invalid_request', details: { field: 'Content-Type' } });
		}
	);

	it('413 payload_too_large on a declared Content-Length over 1 MiB, without reading the body', async () => {
		const req = jsonRequest('{}', { 'Content-Length': String(MAX_REPORT_BODY_BYTES + 1) });
		const res = await envelope(() => readReportBody(req));
		expect(res.status).toBe(413);
		expect(res.body.error).toMatchObject({ code: 'payload_too_large', details: {} });
	});

	it('413 payload_too_large on a body over 1 MiB whose Content-Length lied', async () => {
		// `Content-Length` is caller-supplied; the decoded length is what actually bounds the read.
		const oversized = JSON.stringify({ text: 'x'.repeat(MAX_REPORT_BODY_BYTES) });
		const req = jsonRequest(oversized, { 'Content-Length': '10' });
		const res = await envelope(() => readReportBody(req));
		expect(res.status).toBe(413);
		expect(res.body.error.code).toBe('payload_too_large');
	});

	it('accepts a body exactly at the 1 MiB cap', async () => {
		const filler = 'x'.repeat(MAX_REPORT_BODY_BYTES - '{"text":""}'.length);
		const body = `{"text":"${filler}"}`;
		expect(new TextEncoder().encode(body).byteLength).toBe(MAX_REPORT_BODY_BYTES);
		await expect(readReportBody(jsonRequest(body))).resolves.toBeTruthy();
	});

	it.each([
		['malformed JSON', '{"text":'],
		['a JSON array', '[]'],
		['a JSON scalar', '"hello"'],
		['JSON null', 'null'],
	])('400 invalid_request for %s', async (_label, body) => {
		const res = await envelope(() => readReportBody(jsonRequest(body)));
		expect(res.status).toBe(400);
		expect(res.body.error).toMatchObject({ code: 'invalid_request', details: { field: 'body' } });
	});
});

describe('§4.1 parseReportRequest — text', () => {
	it('accepts a minimal body and trims text', () => {
		const parsed = parseReportRequest(minimal({ text: '  spins forever  ' }));
		expect(parsed.text).toBe('spins forever');
		expect(parsed.severity).toBe('medium');
		expect(parsed.title).toBeNull();
		expect(parsed.idempotencyKey).toBeNull();
		expect(parsed.attachments).toEqual([]);
		expect(parsed.context).toEqual({});
	});

	it.each([
		['absent', {}],
		['null', { text: null }],
		['a number', { text: 7 }],
	])('400 with details.field=text when text is %s', (_label, override) => {
		const body = { reporter: { external_id: 'u_1' }, ...override };
		return envelope(() => parseReportRequest(body)).then((res) => {
			expect(res.status).toBe(400);
			expect(res.body.error).toMatchObject({ code: 'invalid_request', details: { field: 'text' } });
		});
	});

	it.each([
		['empty', ''],
		['whitespace only', '   \t\n '],
	])('400 with details.field=text when text is %s after trim', async (_label, text) => {
		const res = await envelope(() => parseReportRequest(minimal({ text })));
		expect(res.status).toBe(400);
		expect(res.body.error).toMatchObject({ code: 'invalid_request', details: { field: 'text' } });
		expect(res.body.error.message).toContain('text must not be empty');
	});

	it('accepts 8000 characters and rejects 8001', async () => {
		expect(parseReportRequest(minimal({ text: 'x'.repeat(8000) })).text).toHaveLength(8000);
		const res = await envelope(() => parseReportRequest(minimal({ text: 'x'.repeat(8001) })));
		expect(res.status).toBe(400);
		expect(res.body.error.details).toEqual({ field: 'text' });
	});
});

describe('§4.1 parseReportRequest — title, severity, idempotency_key', () => {
	it('treats an absent, null or blank title as "derive one"', () => {
		for (const title of [undefined, null, '   ']) {
			expect(parseReportRequest(minimal({ title })).title).toBeNull();
		}
	});

	it('accepts 200 characters of title and rejects 201', async () => {
		expect(parseReportRequest(minimal({ title: 't'.repeat(200) })).title).toHaveLength(200);
		const res = await envelope(() => parseReportRequest(minimal({ title: 't'.repeat(201) })));
		expect(res.body.error.details).toEqual({ field: 'title' });
	});

	it.each(['low', 'medium', 'high', 'critical'])('accepts severity %s', (severity) => {
		expect(parseReportRequest(minimal({ severity })).severity).toBe(severity);
	});

	it.each([
		['absent', undefined],
		['null', null],
	])('defaults severity to medium when %s (§3: absent and null are the same)', (_label, severity) => {
		expect(parseReportRequest(minimal({ severity })).severity).toBe('medium');
	});

	it.each(['urgent', 'LOW', 3, {}])('400 details.field=severity for %s', async (severity) => {
		const res = await envelope(() => parseReportRequest(minimal({ severity })));
		expect(res.status).toBe(400);
		expect(res.body.error.details).toEqual({ field: 'severity' });
	});

	it('accepts 128 characters of idempotency_key and rejects 129', async () => {
		expect(parseReportRequest(minimal({ idempotency_key: 'k'.repeat(128) })).idempotencyKey).toHaveLength(128);
		const res = await envelope(() => parseReportRequest(minimal({ idempotency_key: 'k'.repeat(129) })));
		expect(res.body.error.details).toEqual({ field: 'idempotency_key' });
	});
});

describe('§4.1 parseReportRequest — reporter', () => {
	it.each([
		['absent', undefined],
		['null', null],
		['an array', []],
		['a string', 'u_1'],
	])('400 details.field=reporter when reporter is %s', async (_label, reporter) => {
		const res = await envelope(() => parseReportRequest({ text: 'x', reporter }));
		expect(res.status).toBe(400);
		expect(res.body.error.details).toEqual({ field: 'reporter' });
	});

	it.each([
		['absent', {}],
		['null', { external_id: null }],
		['blank', { external_id: '   ' }],
		['a number', { external_id: 5 }],
	])('400 details.field=reporter.external_id when external_id is %s', async (_label, reporter) => {
		const res = await envelope(() => parseReportRequest({ text: 'x', reporter }));
		expect(res.body.error.details).toEqual({ field: 'reporter.external_id' });
	});

	it('bounds external_id at 255, display_name at 255 and email at 320', async () => {
		const cases: Array<[string, Record<string, unknown>]> = [
			['reporter.external_id', { external_id: 'u'.repeat(256) }],
			['reporter.display_name', { external_id: 'u', display_name: 'd'.repeat(256) }],
			['reporter.email', { external_id: 'u', email: 'e'.repeat(321) }],
		];
		for (const [field, reporter] of cases) {
			const res = await envelope(() => parseReportRequest({ text: 'x', reporter }));
			expect(res.body.error.details).toEqual({ field });
		}
	});

	it('stores an odd-looking email rather than rejecting it — it is data, never identity (§2)', () => {
		const parsed = parseReportRequest(
			minimal({ reporter: { external_id: 'u_1', email: 'not really an address' } })
		);
		expect(parsed.reporter.email).toBe('not really an address');
	});
});

describe('§4.1 parseReportRequest — context', () => {
	it('keeps unknown keys verbatim rather than rejecting them (§3)', () => {
		const parsed = parseReportRequest(
			minimal({ context: { page: 'Visits', somethingNewer: { nested: [1, 2] } } })
		);
		expect(parsed.context).toEqual({ page: 'Visits', somethingNewer: { nested: [1, 2] } });
		expect(parsed.page).toBe('Visits');
	});

	it('400 details.field=context when context is over 16 KiB serialised', async () => {
		const oversized = { blob: 'x'.repeat(MAX_CONTEXT_BYTES) };
		const res = await envelope(() => parseReportRequest(minimal({ context: oversized })));
		expect(res.status).toBe(400);
		expect(res.body.error.details).toEqual({ field: 'context' });
	});

	it('accepts a context just under 16 KiB', () => {
		const filler = 'x'.repeat(MAX_CONTEXT_BYTES - 100);
		expect(parseReportRequest(minimal({ context: { blob: filler } })).context).toHaveProperty('blob');
	});

	it.each(['page', 'release', 'brand', 'environment'])('400 details.field=context.%s for a non-string', async (key) => {
		const res = await envelope(() => parseReportRequest(minimal({ context: { [key]: 12 } })));
		expect(res.body.error.details).toEqual({ field: `context.${key}` });
	});

	it.each(['trace_id', 'session_id'])('bounds context.%s at 128 characters', async (key) => {
		expect(parseReportRequest(minimal({ context: { [key]: 'a'.repeat(128) } })).context).toBeTruthy();
		const res = await envelope(() => parseReportRequest(minimal({ context: { [key]: 'a'.repeat(129) } })));
		expect(res.body.error.details).toEqual({ field: `context.${key}` });
	});

	it('400 details.field=context for a non-object context', async () => {
		const res = await envelope(() => parseReportRequest(minimal({ context: ['a'] })));
		expect(res.body.error.details).toEqual({ field: 'context' });
	});
});

describe('§4.1 parseOccurredAt', () => {
	const now = new Date('2026-09-04T12:00:00Z');

	it('accepts an RFC 3339 instant inside the 24 h window', () => {
		expect(parseOccurredAt('2026-09-04T09:12:03Z', now)?.toISOString()).toBe('2026-09-04T09:12:03.000Z');
	});

	it.each([
		['absent', undefined],
		['null', null],
	])('returns null when %s — the server then uses receipt time', (_label, value) => {
		expect(parseOccurredAt(value, now)).toBeNull();
	});

	it('400 for any time in the future, with no skew allowance', async () => {
		const res = await envelope(() => parseOccurredAt('2026-09-04T12:00:01Z', now));
		expect(res.status).toBe(400);
		expect(res.body.error.details).toEqual({ field: 'context.occurred_at' });
	});

	it('accepts exactly 24 h in the past and rejects one second more', async () => {
		expect(parseOccurredAt('2026-09-03T12:00:00Z', now)).toBeInstanceOf(Date);
		const res = await envelope(() => parseOccurredAt('2026-09-03T11:59:59Z', now));
		expect(res.body.error.details).toEqual({ field: 'context.occurred_at' });
	});

	it.each(['yesterday', '', 5, '2026-13-45T00:00:00Z'])('400 for the unparseable value %s', async (value) => {
		const res = await envelope(() => parseOccurredAt(value, now));
		expect(res.body.error.details).toEqual({ field: 'context.occurred_at' });
	});

	it('is reached through parseReportRequest, not only directly', async () => {
		const res = await envelope(() =>
			parseReportRequest(minimal({ context: { occurred_at: '2030-01-01T00:00:00Z' } }), now)
		);
		expect(res.body.error.details).toEqual({ field: 'context.occurred_at' });
	});
});

describe('§4.1 parseReportRequest — attachments', () => {
	it('accepts up to 5 ids and rejects 6', async () => {
		const five = Array(MAX_ATTACHMENTS).fill(ATTACHMENT_ID);
		expect(parseReportRequest(minimal({ attachments: five })).attachments).toHaveLength(5);
		const res = await envelope(() => parseReportRequest(minimal({ attachments: [...five, ATTACHMENT_ID] })));
		expect(res.status).toBe(400);
		expect(res.body.error).toMatchObject({ code: 'invalid_request', details: { field: 'attachments' } });
	});

	it('a malformed id is invalid_request, NOT attachment_not_found', async () => {
		// attachment_not_found is reserved for a well-formed but unknown/claimed/foreign id, and must
		// stay one undifferentiated answer so the endpoint cannot probe another tenant's ids.
		const res = await envelope(() => parseReportRequest(minimal({ attachments: ['not-a-uuid'] })));
		expect(res.body.error.code).toBe('invalid_request');
		expect(res.body.error.details).toEqual({ field: 'attachments' });
	});

	it.each([
		['a string', 'id'],
		['an object', {}],
	])('400 details.field=attachments when attachments is %s', async (_label, attachments) => {
		const res = await envelope(() => parseReportRequest(minimal({ attachments })));
		expect(res.body.error.details).toEqual({ field: 'attachments' });
	});

	it.each([
		['absent', undefined],
		['null', null],
	])('treats %s attachments as an empty list', (_label, attachments) => {
		expect(parseReportRequest(minimal({ attachments })).attachments).toEqual([]);
	});
});

describe('§3 unknown top-level keys', () => {
	it('are ignored, not rejected, so a newer SDK does not fail closed', () => {
		const parsed = parseReportRequest(minimal({ somethingTheServerHasNotLearned: { a: 1 } }));
		expect(parsed.text).toBe('The save button spins forever.');
	});

	it('are still carried on `raw`, which is what the idempotency digest reads', () => {
		const parsed = parseReportRequest(minimal({ somethingTheServerHasNotLearned: 1 }));
		expect(parsed.raw).toHaveProperty('somethingTheServerHasNotLearned', 1);
	});
});
