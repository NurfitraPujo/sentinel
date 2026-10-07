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
	reapExpiredCliDeviceAuthorizations: vi.fn(),
	maintainSpans: vi.fn(),
};
vi.mock('$lib/server/retention', () => retention);
vi.mock('$lib/db/queries/organizations', () => ({ reapAllExpiredInvitations: vi.fn(async () => 0) }));
vi.mock('$lib/server/attachment-reaper', () => ({ reapAllOrphanAttachments: vi.fn(async () => 0) }));
const logInfo = vi.fn();
const logError = vi.fn();
vi.mock('$lib/server/observability/log', () => ({
	log: { info: logInfo, error: logError, warn: vi.fn(), debug: vi.fn() },
}));

// sentinel-issue-tag-facets §5 "One run, one connection, one deadline": the route opens its own per-run
// client (openRetentionRunClient) before any step, takes the route's advisory lock on it and runs every
// step through run.db. Unmocked, db.ts would fall back to localhost:5432 and every authorised case would
// fail or hang. The stub records the route's own tagged statements AND anything sent through unsafe
// into one list, so a statement written with unsafe is caught by the exact-list assertion.
const lockState = {
	granted: true,
	holder: [] as { pid: number; state: string; application_name: string; held_ms: number }[],
};
const runStatements: string[] = [];
const runClient = Object.assign(
	vi.fn(async (strings: TemplateStringsArray) => {
		const text = strings.join('$');
		runStatements.push(text);
		if (text.includes('pg_try_advisory_lock')) return [{ locked: lockState.granted }];
		if (text.includes('pg_locks')) return lockState.holder;
		return [];
	}),
	{
		unsafe: vi.fn(async (text: string) => {
			runStatements.push(text);
			return [];
		}),
		end: vi.fn(async () => {}),
		options: { parsers: {}, serializers: {} },
	}
);
const openRetentionRunClient = vi.fn((_onclose: () => void) => runClient);
const mockedDb = {};
vi.mock('$lib/server/db', () => ({ db: mockedDb, openRetentionRunClient }));
const recordRetentionRun = vi.fn();
vi.mock('$lib/server/metrics', () => ({ recordRetentionRun }));
const organizations = await import('$lib/db/queries/organizations');
const attachmentReaper = await import('$lib/server/attachment-reaper');
const { RetentionDeadlineError, RetentionConnectionLost } = await import('$lib/server/retention-run');

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
	lockState.granted = true;
	lockState.holder = [];
	runStatements.length = 0;
	retention.cleanupRetainedData.mockImplementation(
		async (retentionDays: number, manualRetentionDays: number, tombstoneRetentionDays: number) => ({
			occurrenceStopped: 'drained',
			occurrenceBacklogSeconds: 0,
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
	retention.reapExpiredCliDeviceAuthorizations.mockResolvedValue(0);
	retention.maintainSpans.mockResolvedValue({ createdPartitions: 0, droppedPartitions: 0, reapedPullQueue: 0, failed: false });
});

describe('POST /api/cron/retention — retentionDays comes from the shared helper (decision 21)', () => {
	it('hands the sweep 30 days when DATA_RETENTION_DAYS is unset, read through getRetentionDays', async () => {
		const response = await POST(cronRequest('s3cret'));

		expect(response.status).toBe(200);
		expect(getRetentionDaysSpy).toHaveBeenCalledTimes(1);
		// The fourth argument is the run (sentinel-issue-tag-facets §5); the three numbers are unchanged.
		expect(retention.cleanupRetainedData).toHaveBeenCalledWith(30, 365, 30, expect.anything());
		expect((await response.json()).result.retentionDays).toBe(30);

		// One run: one client, the try-lock then the unlock and nothing else of the route's own, end once.
		expect(openRetentionRunClient).toHaveBeenCalledTimes(1);
		expect(runStatements).toEqual([
			expect.stringContaining('pg_try_advisory_lock'),
			expect.stringContaining('pg_advisory_unlock'),
		]);
		expect(runClient.end).toHaveBeenCalledTimes(1);
		// Every step got the SAME run.db, and it is not the pool.
		const run = retention.cleanupRetainedData.mock.calls[0][3] as { db: unknown };
		const stepDbs = [
			run.db,
			vi.mocked(organizations.reapAllExpiredInvitations).mock.calls[0][0],
			vi.mocked(attachmentReaper.reapAllOrphanAttachments).mock.calls[0][0],
			retention.reapStaleClaims.mock.calls[0][1],
			retention.reapExpiredIdempotencyKeys.mock.calls[0][0],
			retention.reapExpiredReportIdempotencyKeys.mock.calls[0][0],
			// sentinel-cli-browser-login: expired device authorizations, same run connection.
			retention.reapExpiredCliDeviceAuthorizations.mock.calls[0][0],
			// sentinel-span-ingest D1: span partitions are kept on the same run connection.
			retention.maintainSpans.mock.calls[0][0],
		];
		for (const d of stepDbs) expect(d).toBe(run.db);
		expect(run.db).toBeDefined();
		expect(run.db).not.toBe(mockedDb);
		expect(recordRetentionRun).toHaveBeenCalledTimes(1);
		expect(recordRetentionRun).toHaveBeenCalledWith('completed');
	});

	it('hands the sweep the parsed DATA_RETENTION_DAYS — the same number the loaders put on page data', async () => {
		dynamicEnv.DATA_RETENTION_DAYS = '45';

		const response = await POST(cronRequest('s3cret'));

		expect(retention.cleanupRetainedData).toHaveBeenCalledWith(45, 365, 30, expect.anything());
		expect((await response.json()).result.retentionDays).toBe(45);
		expect(recordRetentionRun).toHaveBeenCalledTimes(1);
		expect(recordRetentionRun).toHaveBeenCalledWith('completed');
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

const baseResult = {
	deletedAffectedUsers: 0,
	deletedAffectedTenants: 0,
	deletedOccurrenceBuckets: 0,
	deletedOrphanedIssues: 0,
	tombstonesWritten: 0,
	deletedTombstones: 0,
	retentionDays: 30,
	manualRetentionDays: 365,
	tombstoneRetentionDays: 30,
	cutoffDate: new Date('2026-01-01T00:00:00Z'),
};

describe('POST /api/cron/retention — one run, one connection, one deadline (sentinel-issue-tag-facets §5)', () => {
	it('behind: a run that finished but stopped deleting early records behind, not completed', async () => {
		retention.cleanupRetainedData.mockResolvedValueOnce({
			...baseResult,
			occurrenceStopped: 'lock_timeout',
			deletedOccurrences: 0,
			occurrenceBacklogSeconds: 50000,
		});
		const response = await POST(cronRequest('s3cret'));
		expect(response.status).toBe(200);
		expect(recordRetentionRun).toHaveBeenCalledTimes(1);
		expect(recordRetentionRun).toHaveBeenCalledWith('behind');
		expect(logInfo).toHaveBeenCalledWith('retention_cron.completed', expect.objectContaining({ outcome: 'behind' }));
	});

	it('behind: an early stop past the backlog bound is behind; a drained run past it is completed', async () => {
		retention.cleanupRetainedData.mockResolvedValueOnce({
			...baseResult,
			occurrenceStopped: 'budget',
			deletedOccurrences: 10,
			occurrenceBacklogSeconds: 7201,
		});
		await POST(cronRequest('s3cret'));
		expect(recordRetentionRun).toHaveBeenCalledTimes(1);
		expect(recordRetentionRun).toHaveBeenLastCalledWith('behind');

		retention.cleanupRetainedData.mockResolvedValueOnce({
			...baseResult,
			occurrenceStopped: 'drained',
			deletedOccurrences: 10,
			occurrenceBacklogSeconds: 7201,
		});
		await POST(cronRequest('s3cret'));
		expect(recordRetentionRun).toHaveBeenCalledTimes(2);
		expect(recordRetentionRun).toHaveBeenLastCalledWith('completed');
	});

	it('skip: a live holder -> 200 skipped, no step, no unlock, end once', async () => {
		lockState.granted = false;
		lockState.holder = [{ pid: 4242, state: 'active', application_name: 'sentinel.retention.run', held_ms: 5000 }];
		const response = await POST(cronRequest('s3cret'));
		expect(response.status).toBe(200);
		expect(await response.json()).toEqual({ success: true, skipped: 'already_running' });
		expect(retention.cleanupRetainedData).not.toHaveBeenCalled();
		expect(runStatements.some((t) => t.includes('pg_advisory_unlock'))).toBe(false);
		expect(runClient.end).toHaveBeenCalledTimes(1);
		expect(logInfo).toHaveBeenCalledWith(
			'retention_cron.skipped_concurrent',
			expect.objectContaining({ holderPid: 4242, heldMs: 5000 })
		);
		expect(recordRetentionRun).toHaveBeenCalledTimes(1);
		expect(recordRetentionRun).toHaveBeenCalledWith('skipped');
	});

	it('stale: a holder older than the stale bound -> 503 retention_lock_stale', async () => {
		lockState.granted = false;
		lockState.holder = [{ pid: 4242, state: 'active', application_name: 'sentinel.retention.run', held_ms: 240001 }];
		const response = await POST(cronRequest('s3cret'));
		expect(response.status).toBe(503);
		expect(await response.json()).toEqual({ error: 'retention_lock_stale', holderPid: 4242, heldMs: 240001 });
		expect(logError).toHaveBeenCalledWith('retention_cron.lock_stale', expect.objectContaining({ holderPid: 4242 }));
		expect(retention.cleanupRetainedData).not.toHaveBeenCalled();
		expect(runClient.end).toHaveBeenCalledTimes(1);
		expect(recordRetentionRun).toHaveBeenCalledTimes(1);
		expect(recordRetentionRun).toHaveBeenCalledWith('lock_stale');
	});

	it('release on failure: a failing step still unlocks and ends the client (the finally)', async () => {
		retention.cleanupRetainedData.mockRejectedValueOnce(new Error('boom'));
		const response = await POST(cronRequest('s3cret'));
		expect(response.status).toBe(500);
		expect(runStatements).toContainEqual(expect.stringContaining('pg_advisory_unlock'));
		expect(runClient.end).toHaveBeenCalledTimes(1);
		expect(recordRetentionRun).toHaveBeenCalledTimes(1);
		expect(recordRetentionRun).toHaveBeenCalledWith('failed');
	});

	it('deadline: a run that reached its deadline -> 500 retention_deadline_exceeded, unlock and end', async () => {
		retention.cleanupRetainedData.mockRejectedValueOnce(new RetentionDeadlineError('SELECT …'));
		const response = await POST(cronRequest('s3cret'));
		expect(response.status).toBe(500);
		expect(await response.json()).toEqual({ error: 'retention_deadline_exceeded' });
		expect(logError).toHaveBeenCalledWith('retention_cron.deadline_exceeded', expect.anything());
		expect(runStatements).toContainEqual(expect.stringContaining('pg_advisory_unlock'));
		expect(runClient.end).toHaveBeenCalledTimes(1);
		expect(recordRetentionRun).toHaveBeenCalledTimes(1);
		expect(recordRetentionRun).toHaveBeenCalledWith('deadline_exceeded');
	});

	it('connection lost: 500 retention_connection_lost, no unlock, end once', async () => {
		retention.cleanupRetainedData.mockImplementationOnce(async () => {
			openRetentionRunClient.mock.calls[0][0]();
			throw new RetentionConnectionLost();
		});
		const response = await POST(cronRequest('s3cret'));
		expect(response.status).toBe(500);
		expect(await response.json()).toEqual({ error: 'retention_connection_lost' });
		expect(logError).toHaveBeenCalledWith('retention_cron.connection_lost', expect.anything());
		expect(runStatements.some((t) => t.includes('pg_advisory_unlock'))).toBe(false);
		expect(runClient.end).toHaveBeenCalledTimes(1);
		expect(recordRetentionRun).toHaveBeenCalledTimes(1);
		expect(recordRetentionRun).toHaveBeenCalledWith('connection_lost');
	});
});

describe('POST /api/cron/retention — span partition upkeep is independent (sentinel-span-ingest D1, repair m7)', () => {
	it('runs BEFORE the occurrence sweep, so a throw there cannot stop partition create/drop', async () => {
		retention.cleanupRetainedData.mockRejectedValueOnce(new Error('sweep blew up'));
		const response = await POST(cronRequest('s3cret'));
		expect(response.status).toBe(500);
		expect(retention.maintainSpans).toHaveBeenCalledTimes(1);
		expect(retention.maintainSpans.mock.invocationCallOrder[0]).toBeLessThan(retention.cleanupRetainedData.mock.invocationCallOrder[0]);
	});

	it('a failed maintenance is reported in the result and does not fail the run', async () => {
		retention.maintainSpans.mockResolvedValueOnce({ createdPartitions: 0, droppedPartitions: 0, reapedPullQueue: 0, failed: true });
		const response = await POST(cronRequest('s3cret'));
		expect(response.status).toBe(200);
		expect((await response.json()).result.spanMaintenance.failed).toBe(true);
	});
});
