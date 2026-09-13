import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * sentinel-affected-users decision 21 — the retention window is read in ONE place,
 * `$lib/server/retention-window`, and this route is the third reader beside the `/` and
 * `/issues/{id}` loaders. Pinned here so the number on every count label ("in the last N days") is
 * by construction the number this sweep enforces: a route that went back to its own
 * `parseInt(env.DATA_RETENTION_DAYS ?? '30', 10)` would still work today and drift the day someone
 * changed one of the two.
 *
 * The sweep itself is mocked on purpose — `cleanupRetainedData` is proved against real Postgres in
 * the `retention.*.integration.test.ts` family; what is proved HERE is which number it is handed.
 */

const retention = {
	cleanupRetainedData: vi.fn(),
	reapStaleClaims: vi.fn(),
	reapExpiredIdempotencyKeys: vi.fn(),
	reapExpiredReportIdempotencyKeys: vi.fn(),
};
vi.mock('$lib/server/retention', () => retention);
vi.mock('$lib/db/queries/organizations', () => ({ reapAllExpiredInvitations: vi.fn(async () => 0) }));
vi.mock('$lib/server/attachment-reaper', () => ({ reapAllOrphanAttachments: vi.fn(async () => 0) }));
const logInfo = vi.fn();
vi.mock('$lib/server/observability/log', () => ({
	log: { info: logInfo, error: vi.fn(), warn: vi.fn(), debug: vi.fn() },
}));

const dynamicEnv: Record<string, string | undefined> = {};
vi.mock('$env/dynamic/private', () => ({ env: dynamicEnv }));

const getRetentionDaysSpy = vi.fn();
vi.mock('$lib/server/retention-window', async () => {
	const actual = await vi.importActual<typeof import('$lib/server/retention-window')>(
		'$lib/server/retention-window'
	);
	getRetentionDaysSpy.mockImplementation(actual.getRetentionDays);
	return { ...actual, getRetentionDays: getRetentionDaysSpy };
});

const { POST } = await import('./+server');

function cronRequest(secret: string) {
	return {
		request: new Request('http://x/api/cron/retention', {
			method: 'POST',
			headers: { 'x-cron-secret': secret },
		}),
	} as never;
}

beforeEach(() => {
	vi.clearAllMocks();
	for (const key of Object.keys(dynamicEnv)) delete dynamicEnv[key];
	dynamicEnv.CRON_SECRET = 's3cret';
	retention.cleanupRetainedData.mockImplementation(
		async (retentionDays: number, manualRetentionDays: number, tombstoneRetentionDays: number) => ({
			deletedOccurrences: 0,
			deletedAffectedUsers: 0,
			deletedAffectedTenants: 0,
			deletedOccurrenceBuckets: 0,
			deletedOrphanedIssues: 0,
			tombstonesWritten: 0,
			deletedTombstones: 0,
			retentionDays,
			manualRetentionDays,
			tombstoneRetentionDays,
			cutoffDate: new Date('2026-01-01T00:00:00Z'),
		})
	);
	retention.reapStaleClaims.mockResolvedValue({ releasedClaims: 0 });
	retention.reapExpiredIdempotencyKeys.mockResolvedValue(0);
	retention.reapExpiredReportIdempotencyKeys.mockResolvedValue(0);
});

describe('POST /api/cron/retention — retentionDays comes from the shared helper (decision 21)', () => {
	it('hands the sweep 30 days when DATA_RETENTION_DAYS is unset, read through getRetentionDays', async () => {
		const response = await POST(cronRequest('s3cret'));

		expect(response.status).toBe(200);
		expect(getRetentionDaysSpy).toHaveBeenCalledTimes(1);
		expect(retention.cleanupRetainedData).toHaveBeenCalledWith(30, 365, 30);
		expect((await response.json()).result.retentionDays).toBe(30);
	});

	it('hands the sweep the parsed DATA_RETENTION_DAYS — the same number the loaders put on page data', async () => {
		dynamicEnv.DATA_RETENTION_DAYS = '45';

		const response = await POST(cronRequest('s3cret'));

		expect(retention.cleanupRetainedData).toHaveBeenCalledWith(45, 365, 30);
		expect((await response.json()).result.retentionDays).toBe(45);
	});
});

describe('POST /api/cron/retention — the side-table counts reach the response and the log line (§4.5, flow R3)', () => {
	it('carries deletedAffectedUsers / deletedAffectedTenants as the integers the sweep returned', async () => {
		retention.cleanupRetainedData.mockResolvedValueOnce({
			deletedOccurrences: 11,
			deletedAffectedUsers: 5,
			deletedAffectedTenants: 3,
			deletedOccurrenceBuckets: 0,
			deletedOrphanedIssues: 0,
			tombstonesWritten: 0,
			deletedTombstones: 0,
			retentionDays: 30,
			manualRetentionDays: 365,
			tombstoneRetentionDays: 30,
			cutoffDate: new Date('2026-01-01T00:00:00Z'),
		});

		const response = await POST(cronRequest('s3cret'));

		expect(response.status).toBe(200);
		const { result } = await response.json();
		expect(result.deletedAffectedUsers).toBe(5);
		expect(result.deletedAffectedTenants).toBe(3);
		// Beside deletedOccurrences, as the contract puts them -- and distinct from it, so a route
		// that copied the wrong field would fail here.
		expect(result.deletedOccurrences).toBe(11);

		expect(logInfo).toHaveBeenCalledWith(
			'retention_cron.completed',
			expect.objectContaining({
				deletedOccurrences: 11,
				deletedAffectedUsers: 5,
				deletedAffectedTenants: 3,
			})
		);
	});
});

describe('POST /api/cron/retention — deletedOccurrenceBuckets reaches the response and the log line (sentinel-issue-trend §4.5, flow R3)', () => {
	it('carries deletedOccurrenceBuckets as the integer the sweep returned, beside deletedAffectedUsers / deletedAffectedTenants', async () => {
		retention.cleanupRetainedData.mockResolvedValueOnce({
			deletedOccurrences: 11,
			deletedAffectedUsers: 5,
			deletedAffectedTenants: 3,
			deletedOccurrenceBuckets: 7,
			deletedOrphanedIssues: 0,
			tombstonesWritten: 0,
			deletedTombstones: 0,
			retentionDays: 30,
			manualRetentionDays: 365,
			tombstoneRetentionDays: 30,
			cutoffDate: new Date('2026-01-01T00:00:00Z'),
		});

		const response = await POST(cronRequest('s3cret'));

		expect(response.status).toBe(200);
		const { result } = await response.json();
		expect(result.deletedOccurrenceBuckets).toBe(7);
		expect(Number.isInteger(result.deletedOccurrenceBuckets)).toBe(true);
		// Distinct from every neighbour it sits beside, so a route that copied the wrong field
		// (deletedOccurrences, or either side-table count) would fail here.
		expect(result.deletedOccurrences).toBe(11);
		expect(result.deletedAffectedUsers).toBe(5);
		expect(result.deletedAffectedTenants).toBe(3);

		expect(logInfo).toHaveBeenCalledWith(
			'retention_cron.completed',
			expect.objectContaining({
				deletedOccurrences: 11,
				deletedAffectedUsers: 5,
				deletedAffectedTenants: 3,
				deletedOccurrenceBuckets: 7,
			})
		);
	});
});
