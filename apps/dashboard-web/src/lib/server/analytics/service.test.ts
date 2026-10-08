// @vitest-environment node
import { describe, it, expect, beforeAll, beforeEach, vi } from 'vitest';
import { DatabendUnavailableError, type Run } from './databend';
import { resetSchema, seed } from './test-support';
import * as svc from './service';
import type { ServiceSli } from '../sli/config';

// sentinel-reporting-and-dashboard D13/D14/D11: the page's sections, over a REAL Databend (the same seed shape as
// metrics.integration.test.ts), plus the failure-isolation and cache rules with a stub `run`.
const url = process.env.DATABEND_TEST_URL ?? '';
const required = process.env.DATABEND_INTEGRATION_REQUIRED === '1';
let real: Run | null = null;
if (url) {
	try {
		real = await resetSchema(url, 'sentinel_svc');
	} catch (e) {
		if (required) throw e;
	}
}
if (!real && required) throw new Error('DATABEND_INTEGRATION_REQUIRED=1 but no Databend answered');

const P1 = '11111111-1111-1111-1111-111111111111';
const P2 = '22222222-2222-2222-2222-222222222222';
const NOW = new Date('2026-10-07T12:00:00Z');
const H = (h: number) => new Date(NOW.getTime() - h * 3_600_000);
const D = (d: number) => new Date(NOW.getTime() - d * 86_400_000);

const slis: ServiceSli[] = [
	{ id: 'core-api', kind: 'http', project: 'core', source: 'api_core_80', availabilityTarget: 0.995, latency: { le: '1000.0', target: 0.94 }, service: 'api', windowDays: 28 },
	{ id: 'core-jobs', kind: 'jobs', project: 'core', source: 'j', windowDays: 28 },
	{ id: 'core-nats', kind: 'nats', project: 'core', source: 'n', windowDays: 28 },
];

const input = (run: Run, over: Partial<svc.DashboardInput> = {}): svc.DashboardInput => ({
	range: '7d', project: 'all', environment: 'all', now: NOW,
	projects: [{ id: P1, name: 'core' }], scopes: [{ projectId: P1, environments: null }],
	knownEnvironments: ['production', 'staging'], slis, sliEnvironment: 'production', run, ...over,
});

beforeEach(() => svc.clearAnalyticsCache());

describe.skipIf(!real)('dashboard sections over Databend', () => {
	beforeAll(async () => {
		await seed(real!, {
			issues: [
				{ id: 'i-api', projectId: P1, environment: 'production', firstSeen: D(2), firstRelease: 'v2', assignedTo: 'u1' },
				{ id: 'i-job', projectId: P1, environment: 'production', firstSeen: D(10), firstRelease: 'v1' },
				{ id: 'i-other', projectId: P2, environment: 'production', firstSeen: D(1) },
			],
			occs: [
				...[0, 1, 2, 3, 4].map((k) => ({ id: `a${k}`, issueId: 'i-api', environment: 'production', createdAt: H(10 + k), release: 'v2', service: 'api', tagSource: 'http_handler', requestScoped: true, userId: `u${k % 3}` })),
				...[0, 1, 2].map((k) => ({ id: `j${k}`, issueId: 'i-job', environment: 'production', createdAt: H(20 + k), release: 'v1', service: 'jobs worker', tagSource: 'job' })),
				{ id: 'x0', issueId: 'i-other', environment: 'production', createdAt: H(5), service: 'api', tagSource: 'http_handler', requestScoped: true },
				// previous period (7-14 days ago): 2 api events
				{ id: 'p0', issueId: 'i-api', environment: 'production', createdAt: D(9), service: 'api', tagSource: 'http_handler', requestScoped: true },
				{ id: 'p1', issueId: 'i-api', environment: 'production', createdAt: D(10), service: 'api', tagSource: 'http_handler', requestScoped: true },
			],
			activity: [{ issueId: 'i-job', eventType: 'status_changed', createdAt: D(3), newValue: { status: 'resolved' } }],
			sli: [['core-api', 'production', H(3), 1000, 20, 900], ['core-jobs', 'production', H(3), 100, 3, null]],
		});
	});

	it('health: groups against the previous period, SLI tiles, errors per 1k, no data for core-nats', async () => {
		const r = await svc.health(input(real!));
		expect(r.status).toBe('ok');
		if (r.status !== 'ok') return;
		expect(r.data.events.current).toMatchObject({ api: 5, jobs: 3, nats: 0 });
		expect(r.data.events.previous.api).toBe(2);
		expect(r.data.backlog.open).toBe(2);
		const api = r.data.slis.find((s) => s.id === 'core-api')!;
		expect(api).toMatchObject({ total: 1000, errors: 20, availabilityTarget: 0.995, latencyTarget: 0.94 });
		expect(api.availability).toBeCloseTo(0.98);
		expect(api.errorsPer1k).toBe(5); // 5 request-scoped api events / 1000 requests * 1000
		expect(r.data.slis.find((s) => s.id === 'core-jobs')!.errorsPer1k).toBe(30); // 3 job events / 100 runs
		const nats = r.data.slis.find((s) => s.id === 'core-nats');
		expect(nats?.errorsPer1k ?? null).toBeNull(); // no rows until core ships the counter: "no data"
	});

	it('D11: a viewer with no access to project core sees no SLI tile and no counts from it', async () => {
		const r = await svc.health(input(real!, { scopes: [{ projectId: P2, environments: null }], projects: [{ id: P2, name: 'other' }] }));
		if (r.status !== 'ok') throw new Error('unavailable');
		expect(r.data.slis).toEqual([]);
		expect(r.data.events.current.api).toBe(1); // only P2's own event
	});

	it('timeline carries release markers only when one project is selected', async () => {
		const all = await svc.timeline(input(real!));
		const one = await svc.timeline(input(real!, { project: P1 }));
		if (all.status !== 'ok' || one.status !== 'ok') throw new Error('unavailable');
		expect(all.data.releaseMarkers).toEqual([]);
		expect(all.data.bucket).toBe('hour');
		expect(one.data.byService).toBe(true);
		expect(one.data.releaseMarkers.map((x) => x.version).sort()).toEqual(['v1', 'v2']);
		const long = await svc.timeline(input(real!, { range: '28d' }));
		expect(long.status === 'ok' && long.data.bucket).toBe('day');
	});

	it('release table needs a project, then lists first-seen windows newest first with the change from the previous', async () => {
		const none = await svc.releases(input(real!));
		expect(none.status === 'ok' && none.data.needsProject).toBe(true);
		const r = await svc.releases(input(real!, { project: P1 }));
		if (r.status !== 'ok') throw new Error('unavailable');
		expect(r.data.rows.map((x) => x.version)).toEqual(['v2', 'v1']);
		expect(r.data.rows[0].events.api).toBe(5);
		expect(r.data.rows[0].change).not.toBeNull();
		expect(r.data.rows[1].change).toBeNull(); // the oldest has nothing before it
	});

	it('backlog and response sections', async () => {
		const b = await svc.backlog(input(real!));
		const r = await svc.response(input(real!));
		if (b.status !== 'ok' || r.status !== 'ok') throw new Error('unavailable');
		expect(b.data.top[0].id).toBe('i-api');
		expect(b.data.age['0-7d']).toBe(1);
		expect(r.data.regressionRate).toBe(0); // one resolution, no regression
		expect(r.data.changeFailureRate).toBe(0.5); // v2 introduced i-api reaching 3 users; v1's issue has none
	});

	it('freshness: as-of is the newest accessible received_at (never another project\'s), and it warns past 10 minutes of lag', async () => {
		const asOf = H(10); // a0 is the newest occurrence of project core; x0 (H5) belongs to P2 and must not count
		const late = await svc.freshness(input(real!), async () => new Date(asOf.getTime() + 15 * 60_000));
		const ok = await svc.freshness(input(real!), async () => new Date(asOf.getTime() + 5 * 60_000));
		const unknown = await svc.freshness(input(real!), async () => null);
		if (late.status !== 'ok' || ok.status !== 'ok' || unknown.status !== 'ok') throw new Error('unavailable');
		expect(late.data).toMatchObject({ analyticsAsOf: asOf.toISOString(), lagMinutes: 15, warn: true });
		expect(ok.data).toMatchObject({ lagMinutes: 5, warn: false });
		expect(unknown.data).toMatchObject({ lagMinutes: null, warn: false });
	});
});

describe('D11 SLI visibility (no database needed)', () => {
	it('D11: an SLI tile needs an unrestricted or production scope on its project', () => {
	const base = input((async () => []) as Run);
	expect(svc.visibleSlis(base).map((v) => v.sli.id)).toEqual(['core-api', 'core-jobs', 'core-nats']);
	expect(svc.visibleSlis({ ...base, scopes: [{ projectId: P1, environments: ['production'] }] })).toHaveLength(3);
	expect(svc.visibleSlis({ ...base, scopes: [{ projectId: P1, environments: ['staging'] }] })).toHaveLength(0);
	expect(svc.visibleSlis({ ...base, scopes: [] })).toHaveLength(0);
	expect(svc.visibleSlis({ ...base, sliEnvironment: null })).toHaveLength(0);
});
});

describe('failure isolation (D14) and the 60 s cache', () => {
	it('a failing query makes only its section unavailable', async () => {
		const run: Run = async (sql) => {
			if (sql.includes('FROM sli_hourly')) throw new DatabendUnavailableError('down');
			return [];
		};
		const h = await svc.health(input(run));
		expect(h).toEqual({ status: 'unavailable' });
		const b = await svc.backlog(input(run));
		expect(b.status).toBe('ok'); // backlog does not touch sli_hourly
	});

	it('Databend unconfigured: every section is unavailable and nothing throws', async () => {
		const run: Run = async () => { throw new DatabendUnavailableError('Databend is not configured'); };
		for (const f of [svc.health, svc.timeline, svc.sliSection, svc.releases, svc.backlog, svc.response]) {
			expect(await f(input(run, { project: P1 }))).toEqual({ status: 'unavailable' });
		}
	});

	it('caches per (accessible set, filters) for 60 s, and never across viewers', async () => {
		const run = vi.fn<Run>(async () => []);
		await svc.backlog(input(run));
		const calls = run.mock.calls.length;
		await svc.backlog(input(run));
		expect(run.mock.calls.length).toBe(calls); // served from cache
		await svc.backlog(input(run, { scopes: [{ projectId: P1, environments: ['production'] }] }));
		expect(run.mock.calls.length).toBeGreaterThan(calls); // a different accessible set is a different entry
		await svc.backlog(input(run, { now: new Date(NOW.getTime() + 61_000) }));
		expect(run.mock.calls.length).toBeGreaterThan(calls * 2 - 1);
	});

	it('does not cache a failure', async () => {
		let fail = true;
		const run: Run = async () => { if (fail) throw new DatabendUnavailableError('x'); return []; };
		expect((await svc.backlog(input(run))).status).toBe('unavailable');
		fail = false;
		expect((await svc.backlog(input(run))).status).toBe('ok');
	});
});
