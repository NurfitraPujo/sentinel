// @vitest-environment node
import { describe, it, expect } from 'vitest';
import { DatabendUnavailableError, type Run } from './databend';
import { resetSchema, seed } from './test-support';
import { databendCutoff, purgeDatabend } from './retention';

// sentinel-reporting-and-dashboard D5: 13-month deletes in Databend, over a REAL Databend, and the isolation rule.
const url = process.env.DATABEND_TEST_URL ?? '';
const required = process.env.DATABEND_INTEGRATION_REQUIRED === '1';
let real: Run | null = null;
if (url) {
	try {
		real = await resetSchema(url, 'sentinel_ret');
	} catch (e) {
		if (required) throw e;
	}
}
if (!real && required) throw new Error('DATABEND_INTEGRATION_REQUIRED=1 but no Databend answered');

const NOW = new Date('2026-10-07T12:00:00Z');
const P = '11111111-1111-1111-1111-111111111111';

describe('databendCutoff', () => {
	it('is 13 calendar months back', () => {
		expect(databendCutoff(NOW).toISOString()).toBe('2025-09-07T12:00:00.000Z');
	});
});

describe.skipIf(!real)('purgeDatabend over Databend', () => {
	it('deletes occurrences and activity older than 13 months and soft-deleted old issues, and keeps everything else', async () => {
		const old = new Date('2025-08-01T00:00:00Z');
		const fresh = new Date('2026-09-01T00:00:00Z');
		await seed(real!, {
			issues: [
				{ id: 'live-old', projectId: P, environment: 'production', firstSeen: old },
				{ id: 'dead-old', projectId: P, environment: 'production', firstSeen: old, deletedAt: old },
				{ id: 'dead-fresh', projectId: P, environment: 'production', firstSeen: fresh, deletedAt: fresh },
			],
			occs: [
				{ id: 'o-old', issueId: 'live-old', environment: 'production', createdAt: old },
				{ id: 'o-fresh', issueId: 'live-old', environment: 'production', createdAt: fresh },
			],
			activity: [
				{ issueId: 'live-old', eventType: 'assigned', createdAt: old },
				{ issueId: 'live-old', eventType: 'assigned', createdAt: fresh },
			],
			sli: [['core-api', 'production', old, 10, 1, 9]],
		});
		const r = await purgeDatabend(real!, NOW);
		expect(r.outcome).toBe('completed');
		const ids = async (sql: string) => (await real!(sql)).map((x) => x[0]);
		expect(await ids('SELECT id FROM error_occurrences ORDER BY id')).toEqual(['o-fresh']);
		expect(await ids('SELECT count(*) FROM issue_activity')).toEqual(['1']);
		// a LIVE old issue stays (only soft-deleted ones age out); a fresh deleted one stays
		expect(await ids('SELECT id FROM issues ORDER BY id')).toEqual(['dead-fresh', 'live-old']);
		// sli_hourly is kept forever
		expect(await ids('SELECT count(*) FROM sli_hourly')).toEqual(['1']);
	});
});

describe('isolation', () => {
	it('a Databend failure is returned, never thrown, so the Postgres run is untouched', async () => {
		const run: Run = async () => { throw new DatabendUnavailableError('down'); };
		const r = await purgeDatabend(run, NOW);
		expect(r.outcome).toBe('failed');
		expect(r.error).toContain('down');
	});
	it('stops at the first failing statement and reports failure', async () => {
		const seen: string[] = [];
		const run: Run = async (sql) => { seen.push(sql); if (seen.length === 2) throw new Error('boom'); return []; };
		expect((await purgeDatabend(run, NOW)).outcome).toBe('failed');
		expect(seen).toHaveLength(2);
	});
});
