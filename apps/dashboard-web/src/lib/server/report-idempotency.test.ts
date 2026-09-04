import { describe, it, expect } from 'vitest';
import {
	hashReportRequest,
	idempotencyCutoff,
	isDuplicateKeyViolation,
	REPORT_IDEMPOTENCY_RETENTION_DAYS,
} from './report-idempotency';

/**
 * sentinel-user-report-issue §4.1/§8 — the parts of the dedupe mechanism that are decidable
 * without a database. The stored-row behaviour (replay, `409 idempotency_conflict`, the expiry
 * delete and the concurrent-writer race) is asserted end to end in
 * `src/routes/api/report/report.intake.integration.test.ts` against a real Postgres, because it is
 * a property of the unique index and of transaction ordering, not of these functions.
 */

const BODY = {
	text: 'The save button spins forever.',
	reporter: { external_id: 'u_8f3c41', display_name: 'Sarah K.' },
	context: { page: 'Visits', release: 'v31.70' },
};

describe('§4.1 hashReportRequest', () => {
	it('is stable for the same body and project', () => {
		expect(hashReportRequest(BODY, 'proj-1')).toBe(hashReportRequest(BODY, 'proj-1'));
	});

	it('is a sha256 hex digest, so it fits request_hash VARCHAR(64)', () => {
		expect(hashReportRequest(BODY, 'proj-1')).toMatch(/^[0-9a-f]{64}$/);
	});

	it('ignores key ORDER at every depth — a re-serialised retry is not a "different body"', () => {
		const reordered = {
			context: { release: 'v31.70', page: 'Visits' },
			reporter: { display_name: 'Sarah K.', external_id: 'u_8f3c41' },
			text: 'The save button spins forever.',
		};
		expect(hashReportRequest(reordered, 'proj-1')).toBe(hashReportRequest(BODY, 'proj-1'));
	});

	it('does NOT ignore array order — order is meaning in `attachments`', () => {
		const a = { ...BODY, attachments: ['a', 'b'] };
		const b = { ...BODY, attachments: ['b', 'a'] };
		expect(hashReportRequest(a, 'proj-1')).not.toBe(hashReportRequest(b, 'proj-1'));
	});

	it.each([
		['a changed field', { ...BODY, text: 'something else' }],
		['an added known field', { ...BODY, severity: 'high' }],
		['an added UNKNOWN field', { ...BODY, somethingNewer: 1 }],
		['a removed field', { text: BODY.text, reporter: BODY.reporter }],
	])('changes when the body changes by %s', (_label, other) => {
		expect(hashReportRequest(other, 'proj-1')).not.toBe(hashReportRequest(BODY, 'proj-1'));
	});

	it('distinguishes null from absent, so clearing a field is a different body', () => {
		expect(hashReportRequest({ ...BODY, title: null }, 'proj-1')).not.toBe(
			hashReportRequest(BODY, 'proj-1')
		);
	});

	it('folds in the resolved project, so one key across two X-Project-Key values cannot collide', () => {
		expect(hashReportRequest(BODY, 'proj-1')).not.toBe(hashReportRequest(BODY, 'proj-2'));
	});

	it('hashes a nested empty object distinguishably from an absent one', () => {
		expect(hashReportRequest({ ...BODY, context: {} }, 'proj-1')).not.toBe(
			hashReportRequest({ text: BODY.text, reporter: BODY.reporter }, 'proj-1')
		);
	});
});

describe('§8 idempotencyCutoff', () => {
	it('is 7 days before now, matching the existing agent-side reaper', () => {
		expect(REPORT_IDEMPOTENCY_RETENTION_DAYS).toBe(7);
		const now = new Date('2026-09-04T12:00:00Z');
		expect(idempotencyCutoff(now).toISOString()).toBe('2026-08-28T12:00:00.000Z');
	});

	it('does not mutate the instant it is given', () => {
		const now = new Date('2026-09-04T12:00:00Z');
		idempotencyCutoff(now);
		expect(now.toISOString()).toBe('2026-09-04T12:00:00.000Z');
	});
});

describe('isDuplicateKeyViolation', () => {
	it('recognises the idempotency index by SQLSTATE and constraint name', () => {
		expect(
			isDuplicateKeyViolation({ code: '23505', constraint_name: 'report_idempotency_keys_key_unique' })
		).toBe(true);
	});

	it('accepts a driver that reports the constraint under `constraint`', () => {
		expect(
			isDuplicateKeyViolation({ code: '23505', constraint: 'report_idempotency_keys_key_unique' })
		).toBe(true);
	});

	it('accepts a 23505 with no constraint name at all rather than losing the race silently', () => {
		expect(isDuplicateKeyViolation({ code: '23505' })).toBe(true);
	});

	it('rejects a unique violation from a DIFFERENT index — that is a real error, not a replay', () => {
		expect(
			isDuplicateKeyViolation({ code: '23505', constraint_name: 'issues_project_id_fingerprint_key' })
		).toBe(false);
	});

	it.each([
		['a foreign-key violation', { code: '23503' }],
		['a check violation', { code: '23514' }],
		['a plain Error', new Error('boom')],
		['null', null],
		['undefined', undefined],
	])('rejects %s', (_label, err) => {
		expect(isDuplicateKeyViolation(err)).toBe(false);
	});
});
