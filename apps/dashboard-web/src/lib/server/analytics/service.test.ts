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
		expect(api).toMatchObject({ total: 1000 });
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
		expect(one.data.split).toBe('group');
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

describe('SLO sections over a stub run (D2, D3, D5)', () => {
	const hourStart = (h: number) => new Date(NOW.getTime() - h * 3_600_000).toISOString().slice(0, 19).replace('T', ' ');
	const stub = () => vi.fn<Run>(async (sql) => {
		if (sql.includes('FROM sli_hourly') && sql.includes('GROUP BY sli_id')) {
			return [['core-api', 'production', '9000', '90', '8500', '9000'], ['core-nats', 'production', '5000', '500', null, '0']];
		}
		if (sql.includes('FROM sli_hourly')) {
			return [['core-api', hourStart(1), '1000', '10', '900'], ['core-nats', hourStart(1), '1000', '100', null], ['core-nats', hourStart(2), '1000', '100', null]];
		}
		return [];
	});
	const withTargets = [...slis.slice(0, 1), { ...slis[1], availabilityTarget: 0.995 }, { ...slis[2], availabilityTarget: 0.995, windowDays: 14 }];

	it('returns a row per objective with the status computed from sli_hourly', async () => {
		const r = await svc.sloStatus(input(stub(), { slis: withTargets }));
		if (r.status !== 'ok') throw new Error('unavailable');
		expect(r.data.rows.map((x) => x.key)).toEqual(['core-api:availability', 'core-api:latency', 'core-jobs:availability', 'core-nats:availability']);
		const nats = r.data.rows.at(-1)!;
		expect(nats).toMatchObject({ windowDays: 14, budgetLeft: -1 });
		expect(nats.status.state).toBe('breached');
		expect(nats.range).toMatchObject({ failed: 500, total: 5000 });
		expect(r.data.rows.find((x) => x.key === 'core-jobs:availability')!.status.state).toBe('no-data');
	});

	it('reads the hourly window once for every visible SLI, and a second section reuses it', async () => {
		const run = stub();
		await svc.sloStatus(input(run, { slis: withTargets }));
		const hourlyCalls = () => run.mock.calls.filter(([q]) => q.includes('FROM sli_hourly') && !q.includes('GROUP BY')).length;
		expect(hourlyCalls()).toBe(1); // not one per SLI
		expect(run.mock.calls.filter(([q]) => q.includes('FROM sli_hourly')).length).toBe(2); // the range sums and the hourly rows
	});

	it('every latency denominator is latency_total, falling back to total for older rows', async () => {
		const run = stub();
		await svc.sloStatus(input(run, { slis: withTargets }));
		const sums = run.mock.calls.map(([sql]) => sql).filter((sql) => sql.includes('FROM sli_hourly') && sql.includes('GROUP BY'));
		expect(sums.length).toBeGreaterThan(0);
		for (const sql of sums) expect(sql).toContain('sum(CASE WHEN fast IS NULL THEN 0 ELSE coalesce(latency_total, total) END)');
		const hourly = run.mock.calls.map(([sql]) => sql).find((sql) => sql.includes('FROM sli_hourly') && !sql.includes('GROUP BY'))!;
		expect(hourly).toContain('latency_total');
	});

	it('the longest budget window sets how far back the hourly rows are read (D5)', async () => {
		const run = stub();
		await svc.sloStatus(input(run, { slis: withTargets.map((x) => ({ ...x, windowDays: 7 })) }));
		const q = run.mock.calls.map(([sql]) => sql).find((sql) => sql.includes('FROM sli_hourly') && !sql.includes('GROUP BY'))!;
		expect(q).toContain(new Date(NOW.getTime() - 7 * 86_400_000).toISOString().slice(0, 19).replace('T', ' '));
	});

	it('the burn-down section shares the table\'s queries and ends at its budget left', async () => {
		const run = stub();
		const inp = input(run, { slis: withTargets });
		const t = await svc.sloStatus(inp);
		const b = await svc.budgetBurndown(inp);
		if (t.status !== 'ok' || b.status !== 'ok') throw new Error('unavailable');
		expect(run.mock.calls.filter(([q]) => q.includes('FROM sli_hourly')).length).toBe(2); // not 4
		for (const row of t.data.rows.filter((r) => r.target !== null && r.budgetLeft !== null)) {
			expect(b.data.charts.find((c) => c.key === row.key)!.points!.at(-1)!.v).toBe(row.budgetLeft);
		}
	});

	it('VM down: the table is intact and only the NATS list says "breakdown unavailable"', async () => {
		const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
		const vm = { url: 'http://vm', cluster: 'tuju-production', fetchFn: (async () => { throw new Error('down'); }) as never };
		const inp = input(stub(), { slis: withTargets, vm });
		const r = await svc.sloStatus(inp);
		const nb = await svc.natsBreakdown(inp);
		warn.mockRestore();
		if (r.status !== 'ok') throw new Error('unavailable');
		expect(r.data.rows).toHaveLength(4);
		expect(nb).toEqual({ status: 'unavailable' });
	});

	it('a slow VictoriaMetrics delays only the breakdown, never the SLO table (D8)', async () => {
		let release!: () => void;
		const gate = new Promise<void>((res) => (release = res));
		const fetchFn = vi.fn(async () => { await gate; return { ok: true, json: async () => ({ status: 'success', data: { result: [] } }) } as never; });
		const inp = input(stub(), { slis: withTargets, vm: { url: 'http://vm', cluster: 'c', fetchFn: fetchFn as never } });
		const pending = svc.natsBreakdown(inp);
		const table = await svc.sloStatus(inp); // resolves while the VM call is still parked
		expect(table.status).toBe('ok');
		expect(fetchFn).toHaveBeenCalled();
		let settled = false;
		void pending.then(() => (settled = true));
		await Promise.resolve();
		expect(settled).toBe(false);
		release();
		await pending;
	});

	it('no VM env: the same, never a 500; no nats SLI visible: no breakdown at all', async () => {
		expect(await svc.natsBreakdown(input(stub(), { slis: withTargets }))).toEqual({ status: 'unavailable' });
		svc.clearAnalyticsCache();
		expect(await svc.natsBreakdown(input(stub(), { slis: withTargets.slice(0, 2) }))).toBeNull();
	});

	// rev-d8-breakdown-access-gate-untested: the nats SLI stays CONFIGURED; it is the viewer who loses it. The breakdown is
	// its own streamed key, so a gate on the raw config would send core's (stream, consumer) list to a viewer without core.
	it.each([
		['no project scopes', { scopes: [] }],
		['a selected project that is not the nats SLI\'s', { project: P2 }],
	])('the nats SLI is configured but the viewer cannot see it (%s): no breakdown and no VictoriaMetrics call', async (_name, over) => {
		const fetchFn = vi.fn(async () => ({ ok: true, json: async () => ({ status: 'success', data: { result: [] } }) }) as never);
		const sliWeb: ServiceSli = { id: 'web-api', kind: 'http', project: 'web', source: 'api_web_80', availabilityTarget: 0.995, service: 'api', windowDays: 28 };
		const inp = input(stub(), {
			slis: [...withTargets, sliWeb],
			projects: [{ id: P1, name: 'core' }, { id: P2, name: 'web' }],
			scopes: [{ projectId: P1, environments: null }, { projectId: P2, environments: null }],
			vm: { url: 'http://vm', cluster: 'c', fetchFn: fetchFn as never },
			...over,
		});
		expect(inp.slis.some((s) => s.kind === 'nats')).toBe(true);
		expect(await svc.natsBreakdown(inp)).toBeNull();
		expect(fetchFn).not.toHaveBeenCalled();
	});

	it('switching the range inside 60 s returns the new range\'s counts, not the cached ones (rev-slo-cache-key-range)', async () => {
		const run = vi.fn<Run>(async (sql) => {
			if (sql.includes('FROM sli_hourly') && sql.includes('GROUP BY sli_id')) {
				const sevenDay = sql.includes(new Date(NOW.getTime() - 7 * 86_400_000).toISOString().slice(0, 19).replace('T', ' '));
				return [['core-api', 'production', sevenDay ? '7000' : '1000', '0', '0', '0']];
			}
			return [];
		});
		const a = await svc.sloStatus(input(run, { slis: withTargets.slice(0, 1), range: '7d' }));
		const b = await svc.sloStatus(input(run, { slis: withTargets.slice(0, 1), range: '24h' }));
		if (a.status !== 'ok' || b.status !== 'ok') throw new Error('unavailable');
		expect(a.data.rows[0].range.total).toBe(7000);
		expect(b.data.rows[0].range.total).toBe(1000);
	});

	it('the health backlog counts only the 30-90d and 90d+ age rows as "older than 30 days" (D14)', async () => {
		const run = vi.fn<Run>(async (sql) => {
			if (sql.includes('count_if(i.assigned_to IS NULL)')) {
				return [[P1, 'production', '0-7d', '1', '1'], [P1, 'production', '7-30d', '10', '2'], [P1, 'production', '30-90d', '100', '4'], [P1, 'production', '90d+', '1000', '8']];
			}
			return [];
		});
		const r = await svc.health(input(run, { slis: [] }));
		if (r.status !== 'ok') throw new Error('unavailable');
		expect(r.data.backlog).toEqual({ open: 1111, unassigned: 15, olderThan30d: 1100 });
	});

	it('an unfloored budget excludes hours outside the SLI\'s own window (D5)', async () => {
		const run = vi.fn<Run>(async (sql) => {
			if (sql.includes('FROM sli_hourly') && sql.includes('GROUP BY sli_id')) return [];
			if (sql.includes('FROM sli_hourly')) {
				const old = new Date(NOW.getTime() - 20 * 86_400_000).toISOString().slice(0, 19).replace('T', ' ');
				return [['core-nats', hourStart(1), '1000', '1', null], ['core-nats', old, '1000', '4', null]];
			}
			return [];
		});
		const r = await svc.sloStatus(input(run, { slis: [{ ...withTargets[2] }] }));
		if (r.status !== 'ok') throw new Error('unavailable');
		expect(r.data.rows[0].budgetLeft).toBeCloseTo(0.8);
	});

	it('is empty (no query) when the viewer sees no SLI', async () => {
		const run = stub();
		const r = await svc.sloStatus(input(run, { scopes: [] }));
		expect(r).toEqual({ status: 'ok', data: { rows: [] } });
		expect(run).not.toHaveBeenCalled();
	});
});

describe('releases on all projects (D15)', () => {
	const P3 = '33333333-3333-3333-3333-333333333333';
	const ts = (d: number) => new Date(NOW.getTime() - d * 86_400_000).toISOString().slice(0, 19).replace('T', ' ');
	// five releases per project, newest first: v5 started 1 day ago ... v1 started 5 days ago
	const wins = (pid: string, prefix: string) => [1, 2, 3, 4, 5].map((d) => [pid, 'production', `${prefix}${6 - d}`, ts(d), d === 1 ? ts(0) : ts(d - 1)]);
	const run: Run = async (sql) => (sql.includes('WITH occ_rel') ? [...wins(P1, 'core-v'), ...wins(P2, 'web-v'), ...wins(P3, 'docs-v')] : []);
	const multi = (over: Partial<svc.DashboardInput> = {}) => input(run, {
		projects: [{ id: P1, name: 'core' }, { id: P2, name: 'web' }, { id: P3, name: 'docs' }],
		scopes: [P1, P2, P3].map((projectId) => ({ projectId, environments: null })),
		slis: [slis[0], { id: 'web', kind: 'http', project: 'web', source: 'w', availabilityTarget: 0.995, windowDays: 28 }],
		...over,
	});

	it('project=all lists at most 3 newest releases of each project that owns a service SLI, with the project', async () => {
		const r = await svc.releases(multi());
		if (r.status !== 'ok') throw new Error('unavailable');
		expect(r.data).toMatchObject({ needsProject: false, allProjects: true });
		expect(r.data.rows.map((x) => `${x.project}:${x.version}`)).toEqual(['core:core-v5', 'core:core-v4', 'core:core-v3', 'web:web-v5', 'web:web-v4', 'web:web-v3']);
		expect(r.data.rows.some((x) => x.project === 'docs')).toBe(false); // owns no SLI
		expect(r.data.rows[0].change).not.toBeNull(); // compared with the previous release of ITS project, not another's
	});

	it('one project selected is unchanged: up to 10 of that project, no all-projects mode', async () => {
		const r = await svc.releases(multi({ project: P1 }));
		if (r.status !== 'ok') throw new Error('unavailable');
		expect(r.data.allProjects).toBe(false);
		expect(r.data.rows.map((x) => x.version)).toEqual(['core-v5', 'core-v4', 'core-v3', 'core-v2', 'core-v1']);
		expect(r.data.rows.at(-1)!.change).toBeNull();
	});

	it('project=all with no SLI-owning project still asks for a project', async () => {
		const r = await svc.releases(multi({ slis: [] }));
		expect(r).toEqual({ status: 'ok', data: { rows: [], needsProject: true, allProjects: true } });
	});
});

describe('timeline split (D12)', () => {
	const run: Run = async (sql) => (sql.includes('error_occurrences') ? [['2026-10-07 10:00:00', 'api', 'api', '3'], ['2026-10-07 10:00:00', 'other', null, '1']] : []);
	it('works on project=all, echoes the split, and a split change is a different cache entry', async () => {
		const g = await svc.timeline(input(run));
		const sv = await svc.timeline(input(run, { split: 'service' }));
		if (g.status !== 'ok' || sv.status !== 'ok') throw new Error('unavailable');
		expect(g.data.split).toBe('group');
		expect(sv.data.split).toBe('service');
		expect(sv.data.byGroup).toHaveLength(2);
		expect(sv.data.releaseMarkers).toEqual([]); // markers stay a one-project feature
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
		for (const f of [svc.health, svc.timeline, svc.sloStatus, svc.budgetBurndown, svc.releases, svc.backlog, svc.response]) {
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
