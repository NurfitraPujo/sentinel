import { describe, it, expect, vi, beforeEach } from 'vitest';
import { PgDialect } from 'drizzle-orm/pg-core';
import type { SQL } from 'drizzle-orm';

/**
 * sentinel-user-report-issue §4.5 + SR-5: the per-organization rolling-24 h byte budget for
 * external-reporter uploads, summed in POSTGRES.
 *
 * SR-5's rationale is a behaviour, not a comment: "an unfiltered org-scoped sum would count
 * org-member and agent uploads ... so a dashboard team's own screenshots could start returning 429
 * to a service that uploaded nothing." So the test that matters most below is the one asserting the
 * `uploader_type = 'external_reporter'` predicate is actually in the WHERE clause -- a sum with the
 * right total and the wrong filter is the failure this decision exists to prevent.
 */

const captured: { where?: unknown; from?: unknown } = {};
let sumResult: Array<{ total: string; oldest: Date | null }> = [{ total: '0', oldest: null }];

const dbMock: any = {
	select: vi.fn(() => dbMock),
	from: vi.fn((table: unknown) => {
		captured.from = table;
		return dbMock;
	}),
	where: vi.fn((clause: unknown) => {
		captured.where = clause;
		return dbMock;
	}),
	then: vi.fn((resolve: any) => resolve(sumResult)),
};

vi.mock('$lib/server/db', () => ({ db: dbMock }));
vi.mock('$lib/server/storage', () => ({
	isStorageConfigured: () => true,
	putObject: vi.fn(),
	createPresignedPutUrl: vi.fn(),
	headObject: vi.fn(),
	getObjectRangeBytes: vi.fn(),
	deleteObject: vi.fn(),
}));
vi.mock('$lib/server/attachment-reaper', () => ({ reapOrgOrphanAttachments: vi.fn(async () => {}) }));

const {
	checkReportUploadBudget,
	sumExternalReporterUploadBytes,
	resolveReportUploadBytesPerDay,
	DEFAULT_REPORT_UPLOAD_BYTES_PER_DAY,
	REPORT_UPLOAD_WINDOW_MS,
} = await import('./upload-core');

/**
 * Renders the captured drizzle WHERE clause to real SQL + bound parameters, so the predicates can
 * be asserted as SQL rather than as an object graph.
 */
const dialect = new PgDialect();
function whereSql(): { sql: string; params: unknown[] } {
	const { sql, params } = dialect.sqlToQuery(captured.where as SQL);
	return { sql, params };
}
function whereText(): string {
	const { sql, params } = whereSql();
	return sql + ' -- ' + params.map((p) => (p instanceof Date ? p.toISOString() : String(p))).join(', ');
}

beforeEach(() => {
	vi.clearAllMocks();
	sumResult = [{ total: '0', oldest: null }];
});

describe('resolveReportUploadBytesPerDay', () => {
	it('defaults to 250 MB when unset or blank', () => {
		expect(resolveReportUploadBytesPerDay(undefined)).toBe(DEFAULT_REPORT_UPLOAD_BYTES_PER_DAY);
		expect(resolveReportUploadBytesPerDay('   ')).toBe(DEFAULT_REPORT_UPLOAD_BYTES_PER_DAY);
		expect(DEFAULT_REPORT_UPLOAD_BYTES_PER_DAY).toBe(250 * 1024 * 1024);
	});

	it('falls back to the default (never to "unlimited") on garbage or a negative value', () => {
		expect(resolveReportUploadBytesPerDay('not-a-number')).toBe(DEFAULT_REPORT_UPLOAD_BYTES_PER_DAY);
		expect(resolveReportUploadBytesPerDay('-1')).toBe(DEFAULT_REPORT_UPLOAD_BYTES_PER_DAY);
	});

	it('honours a lowered value, and honours 0 as a real setting', () => {
		expect(resolveReportUploadBytesPerDay('1024')).toBe(1024);
		expect(resolveReportUploadBytesPerDay('0')).toBe(0);
	});
});

describe('sumExternalReporterUploadBytes (SR-5)', () => {
	it("filters to uploader_type='external_reporter' as well as the org and the window", async () => {
		await sumExternalReporterUploadBytes('org-1', new Date('2026-09-03T00:00:00Z'));
		const { sql, params } = whereSql();
		// The org + created_at shape is what idx_attachments_org_created serves; the uploader_type
		// equality is SR-5's filter, without which an org member's own uploads would be counted.
		expect(sql).toMatch(/"org_id" = \$?\d*\??/);
		expect(sql).toContain('"uploader_type" =');
		expect(sql).toContain('"created_at" >=');
		expect(params.map(String)).toContain('org-1');
		expect(params.map(String)).toContain('external_reporter');
		expect(whereText()).toContain('2026-09-03T00:00:00.000Z');
	});

	it('coalesces an empty window to 0 rather than NULL', async () => {
		sumResult = [{ total: '0', oldest: null }];
		const usage = await sumExternalReporterUploadBytes('org-1', new Date());
		expect(usage).toEqual({ usedBytes: 0, oldestCreatedAt: null });
	});

	it('returns the sum as a number (Postgres sum() of bigint arrives as a string)', async () => {
		sumResult = [{ total: '104857600', oldest: new Date('2026-09-04T01:00:00Z') }];
		const usage = await sumExternalReporterUploadBytes('org-1', new Date());
		expect(usage.usedBytes).toBe(104857600);
		expect(usage.oldestCreatedAt?.toISOString()).toBe('2026-09-04T01:00:00.000Z');
	});
});

describe('checkReportUploadBudget', () => {
	const now = new Date('2026-09-04T12:00:00Z');

	it('counts the incoming file, so the request being decided cannot be the one that steps over', async () => {
		sumResult = [{ total: '900', oldest: new Date('2026-09-04T06:00:00Z') }];
		expect((await checkReportUploadBudget('org-1', 100, 1000, now)).allowed).toBe(true);
		expect((await checkReportUploadBudget('org-1', 101, 1000, now)).allowed).toBe(false);
	});

	it('opens a rolling 24 h window ending now, not a calendar day', async () => {
		await checkReportUploadBudget('org-1', 1, 1000, now);
		expect(whereText()).toContain(new Date(now.getTime() - REPORT_UPLOAD_WINDOW_MS).toISOString());
		expect(REPORT_UPLOAD_WINDOW_MS).toBe(24 * 60 * 60 * 1000);
	});

	it('derives Retry-After from when the oldest counted row leaves the window', async () => {
		// Oldest row at 06:00; at 12:00 it leaves the window in 18 h.
		sumResult = [{ total: '5000', oldest: new Date('2026-09-04T06:00:00Z') }];
		const verdict = await checkReportUploadBudget('org-1', 1, 1000, now);
		expect(verdict.allowed).toBe(false);
		expect(verdict.retryAfter).toBe(18 * 60 * 60);
	});

	it('falls back to a full window when nothing is counted, and never returns 0', async () => {
		sumResult = [{ total: '0', oldest: null }];
		const verdict = await checkReportUploadBudget('org-1', 1, 0, now);
		expect(verdict.allowed).toBe(false);
		expect(verdict.retryAfter).toBe(24 * 60 * 60);
	});

	it('reports the usage it measured alongside the verdict', async () => {
		sumResult = [{ total: '4096', oldest: new Date('2026-09-04T11:00:00Z') }];
		const verdict = await checkReportUploadBudget('org-1', 1, 1_000_000, now);
		expect(verdict).toMatchObject({ allowed: true, usedBytes: 4096, budgetBytes: 1_000_000 });
	});
});
