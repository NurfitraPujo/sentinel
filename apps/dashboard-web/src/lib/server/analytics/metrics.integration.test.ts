// @vitest-environment node
import { describe, it, expect, beforeAll } from 'vitest';
import type { Run } from './databend';
import { resetSchema, seed } from './test-support';
import * as m from './metrics';
import type { AccessFilter } from './access';

// sentinel-reporting-and-dashboard D10/D11/D3 against a REAL Databend (v1.2.881, the production version). Skipped unless
// DATABEND_TEST_URL answers; DATABEND_INTEGRATION_REQUIRED=1 turns "unreachable" into a hard failure.
//
//   podman run -d -p 18000:8000 datafuselabs/databend:v1.2.881
//   DATABEND_TEST_URL=http://127.0.0.1:18000 DATABEND_INTEGRATION_REQUIRED=1 pnpm vitest run src/lib/server/analytics

const url = process.env.DATABEND_TEST_URL ?? '';
const required = process.env.DATABEND_INTEGRATION_REQUIRED === '1';
let run: Run | null = null;
if (url) {
	try {
		run = await resetSchema(url);
	} catch (e) {
		if (required) throw e;
	}
}
if (!run && required) throw new Error('DATABEND_INTEGRATION_REQUIRED=1 but no Databend answered');

const P1 = '11111111-1111-1111-1111-111111111111'; // core
const P2 = '22222222-2222-2222-2222-222222222222'; // other project the viewer cannot reach
const NOW = new Date('2026-10-07T12:00:00Z');
const H = (h: number) => new Date(NOW.getTime() - h * 3_600_000);
const D = (d: number) => new Date(NOW.getTime() - d * 86_400_000);
const win7: m.Window = { from: D(7), to: NOW };

const ctx = (access: Partial<AccessFilter> = {}): m.MetricCtx => ({
	run: run!,
	httpServices: ['api'],
	access: { scopes: [{ projectId: P1, environments: null }], environment: 'all', knownEnvironments: ['production', 'staging'], ...access },
});
const sum = (rows: { n: number }[]) => rows.reduce((a, r) => a + r.n, 0);

describe.skipIf(!run)('D10 metrics against Databend', () => {
	beforeAll(async () => {
		await seed(run!, {
			issues: [
				{ id: 'i-api', projectId: P1, environment: 'production', firstSeen: D(2), firstRelease: 'v2', assignedTo: 'u1' },
				{ id: 'i-job', projectId: P1, environment: 'production', firstSeen: D(10), firstRelease: 'v1' },
				{ id: 'i-old', projectId: P1, environment: 'production', firstSeen: D(100) },
				{ id: 'i-stg', projectId: P1, environment: 'staging', firstSeen: D(1) },
				{ id: 'i-dead', projectId: P1, environment: 'production', firstSeen: D(3), deletedAt: D(1), status: 'resolved' },
				{ id: 'i-rep', projectId: P1, environment: 'production', firstSeen: D(1), issueType: 'user_report' },
				{ id: 'i-other', projectId: P2, environment: 'production', firstSeen: D(1) },
			],
			occs: [
				// API: 5 request-scoped events from service api, 3 distinct users, release v2 (starts 2 days ago)
				...[0, 1, 2, 3, 4].map((k) => ({ id: `a${k}`, issueId: 'i-api', environment: 'production', createdAt: H(10 + k), release: 'v2', service: 'api', tagSource: 'http_handler', requestScoped: true, userId: `u${k % 3}` })),
				// a logged-only api error is group "other"
				{ id: 'a-logged', issueId: 'i-api', environment: 'production', createdAt: H(9), release: 'v2', service: 'api', tagSource: 'logger', requestScoped: false },
				// jobs and nats
				...[0, 1, 2].map((k) => ({ id: `j${k}`, issueId: 'i-job', environment: 'production', createdAt: H(20 + k), release: 'v1', service: 'jobs worker', tagSource: 'job' })),
				{ id: 'n0', issueId: 'i-job', environment: 'production', createdAt: H(30), release: 'v1', service: 'worker', tagSource: 'worker_handler' },
				{ id: 'u0', issueId: 'i-job', environment: 'production', createdAt: H(31), release: 'v1' },
				// staging and another project
				{ id: 's0', issueId: 'i-stg', environment: 'staging', createdAt: H(5), service: 'api', tagSource: 'http_handler', requestScoped: true },
				{ id: 'x0', issueId: 'i-other', environment: 'production', createdAt: H(5), service: 'api', tagSource: 'http_handler', requestScoped: true },
				// outside the 7-day range, and a deleted-issue occurrence (D3: history is kept)
				{ id: 'old0', issueId: 'i-old', environment: 'production', createdAt: D(40), service: 'api', tagSource: 'http_handler', requestScoped: true },
				{ id: 'dead0', issueId: 'i-dead', environment: 'production', createdAt: D(3), service: 'api', tagSource: 'http_handler', requestScoped: true },
			],
			activity: [
				{ issueId: 'i-api', eventType: 'assigned', createdAt: new Date(D(2).getTime() + 600_000) },
				{ issueId: 'i-api', eventType: 'claimed', createdAt: new Date(D(2).getTime() + 3_600_000) },
				{ issueId: 'i-job', eventType: 'regressed', createdAt: H(5), newValue: { releaseVersion: 'v2', status: 'unresolved' } },
				{ issueId: 'i-job', eventType: 'status_changed', createdAt: D(3), newValue: { status: 'resolved', resolvedInVersion: 'v1' }, oldValue: { status: 'unresolved' } },
				{ issueId: 'i-api', eventType: 'status_changed', createdAt: D(1), newValue: { status: 'ignored' } },
				{ issueId: 'i-dead', eventType: 'status_changed', createdAt: D(2), newValue: { status: 'resolved' } },
			],
			sli: [
				['core-api', 'production', H(3), 1000, 20, 900], ['core-api', 'production', H(2), 1000, 30, 950],
				['core-jobs', 'production', H(2), 400, 4, null], ['core-api', 'production', D(60), 500, 500, 0],
			],
		});
	});

	it('error events: D19 groups, with a logged-only api error in "other", nothing from another project', async () => {
		const rows = await m.errorEvents(ctx(), win7);
		const by = (g: m.Group) => sum(rows.filter((r) => r.group === g));
		expect(by('api')).toBe(7); // 5 + staging s0 + the deleted issue's occurrence (D3: history is kept)
		expect(by('jobs')).toBe(3);
		expect(by('nats')).toBe(1);
		expect(by('other')).toBe(1);
		expect(by('unattributed')).toBe(1);
		expect(rows.every((r) => r.projectId === P1)).toBe(true); // P2's x0 contributes nothing, not even a count
	});

	it('D11: a restricted environment scope contributes 0 for the environment it excludes', async () => {
		const prodOnly = await m.errorEvents(ctx({ scopes: [{ projectId: P1, environments: ['production'] }] }), win7);
		expect(prodOnly.some((r) => r.environment === 'staging')).toBe(false);
		expect(sum(prodOnly)).toBe(12);
	});

	it('D11: an empty set, or a scope with no environments, is the empty state', async () => {
		expect(sum(await m.errorEvents(ctx({ scopes: [] }), win7))).toBe(0);
		expect(sum(await m.errorEvents(ctx({ scopes: [{ projectId: P1, environments: [] }] }), win7))).toBe(0);
		expect(await m.openBacklog(ctx({ scopes: [] }), NOW)).toEqual([]);
		expect(await m.topIssues(ctx({ scopes: [] }), win7)).toEqual([]);
		expect(await m.userReportsWaiting(ctx({ scopes: [] }))).toEqual([]);
	});

	it('the environment filter narrows to one environment', async () => {
		expect(sum(await m.errorEvents(ctx({ environment: 'staging' }), win7))).toBe(1);
	});

	it('new issues, regressions and resolutions: counted in range; resolutions read new_value.status; history survives deletion', async () => {
		expect(sum(await m.newIssues(ctx(), win7))).toBe(3); // i-api, i-stg, i-dead (a user_report is not a system_error)
		expect(sum(await m.regressions(ctx(), win7))).toBe(1);
		expect(sum(await m.resolutions(ctx(), win7))).toBe(2); // i-job and the deleted i-dead; "ignored" is not a resolution
		expect(m.regressionRate(1, 2)).toBe(0.5);
		expect(m.regressionRate(1, 0)).toBeNull();
	});

	it('open backlog: unresolved and not deleted (D3), by age of first_seen, with the unassigned share', async () => {
		const rows = await m.openBacklog(ctx(), NOW);
		const age = (a: string) => sum(rows.filter((r) => r.age === a));
		expect(age('0-7d')).toBe(2);
		expect(age('7-30d')).toBe(1);
		expect(age('30-90d')).toBe(0);
		expect(age('90d+')).toBe(1);
		expect(rows.reduce((a, r) => a + r.unassigned, 0)).toBe(3); // i-api is assigned
	});

	it('user reports waiting', async () => {
		expect(await m.userReportsWaiting(ctx())).toEqual([{ projectId: P1, n: 1 }]);
	});

	it('top issues: most distinct users first, ties broken by event count', async () => {
		const top = await m.topIssues(ctx(), win7);
		expect(top[0]).toMatchObject({ id: 'i-api', users: 3, events: 6 });
		expect(top[1]).toMatchObject({ id: 'i-job', users: 0, events: 5 });
		expect(top.length).toBeLessThanOrEqual(10);
	});

	it('MTTA is the first assigned/claimed activity minus first_seen; MTTR the resolved activity minus first_seen', async () => {
		const a = await m.mtta(ctx(), win7, false);
		expect(a[0]).toMatchObject({ count: 1, p50: 600 });
		const r = await m.mttr(ctx(), win7, false);
		expect(r[0].count).toBe(2);
		expect(r[0].p50).toBeCloseTo(345600, 0);
		expect(r[0].p90).toBeCloseTo(552960, 0);
		const g = await m.mttr(ctx(), win7, true);
		expect(g[0]).toMatchObject({ projectId: P1, environment: 'production', count: 2 });
	});

	it('change failure: a release introducing an issue with >=3 users in 24 h fails; one with fewer does not', async () => {
		const rows = await m.changeFailure(ctx(), win7);
		expect(rows).toEqual([{ projectId: P1, environment: 'production', n: 2, failed: 1 }]);
	});

	it('release windows (D9): start = first seen, end = the next release or now; counts per window', async () => {
		const wins = await m.releaseWindows(ctx(), NOW, 10);
		expect(wins.map((w) => w.version)).toEqual(['v2', 'v1']); // newest first
		const v2 = wins[0], v1 = wins[1];
		expect(v2.start.toISOString()).toBe(H(14).toISOString().slice(0, 19) + '.000Z');
		expect(v1.end.toISOString()).toBe(v2.start.toISOString());
		expect(v2.events).toEqual({ api: 5, jobs: 0, nats: 0, other: 1, unattributed: 0 });
		expect(v1.events).toEqual({ api: 0, jobs: 3, nats: 1, other: 0, unattributed: 1 });
		expect(v2.regressions).toBe(1);
		expect((await m.releaseWindows(ctx(), NOW, 1)).map((w) => w.version)).toEqual(['v2']);
	});

	it('weekly flow: opened (new + regressed) vs resolved over 12 weeks', async () => {
		const weeks = await m.weeklyFlow(ctx(), NOW);
		expect(weeks.reduce((a, w) => a + w.opened, 0)).toBe(5);
		expect(weeks.reduce((a, w) => a + w.resolved, 0)).toBe(2);
	});

	it('SLI: good ratios, budget floored at -100%, errors per 1k, untargeted rates', async () => {
		const sums = await m.sliSums(ctx(), ['core-api', 'core-jobs'], win7, 'production');
		const api = sums.find((s) => s.sliId === 'core-api')!;
		expect(api).toMatchObject({ total: 2000, errors: 50, fast: 1850 }); // the 60-day-old row is outside the window
		expect(m.availability(api)).toBeCloseTo(0.975);
		expect(m.latencyRatio(api)).toBeCloseTo(0.925);
		expect(m.errorBudgetRemaining(api, 0.995)).toBe(-1);
		const jobs = sums.find((s) => s.sliId === 'core-jobs')!;
		expect(m.latencyRatio(jobs)).toBeNull();
		const ev = await m.sliMatchingEvents(ctx(), { id: 'core-api', kind: 'http', project: 'core', source: 'x', service: 'api', windowDays: 28 }, P1, 'production', win7);
		expect(ev).toBe(6);
		expect(m.errorsPer1k(ev, api.total)).toBe(3);
		const jobEvents = await m.sliMatchingEvents(ctx(), { id: 'core-jobs', kind: 'jobs', project: 'core', source: 'x', windowDays: 28 }, P1, 'production', win7);
		expect(m.errorsPer1k(jobEvents, jobs.total)).toBe(7.5);
		expect(await m.sliMatchingEvents(ctx(), { id: 'web', kind: 'http', project: 'web', source: 'x', windowDays: 28 }, P1, 'production', win7)).toBeNull();
		expect(await m.sliSums(ctx(), [], win7)).toEqual([]);
	});

	it('SLI latency denominator: latency_total where present, total for rows without it, availability keeps total', async () => {
		await run!(`DELETE FROM sli_hourly WHERE sli_id = 'core-stream'`);
		await seed(run!, { sli: [
			['core-stream', 'production', H(5), 1000, 10, 810, 900], // 100 of the 1000 were SSE
			['core-stream', 'production', H(6), 1000, 10, 940, null], // written before latency_total existed
		] });
		const [s] = await m.sliSums(ctx(), ['core-stream'], win7, 'production');
		expect(s).toMatchObject({ total: 2000, errors: 20, fast: 1750, totalWithFast: 1900 });
		expect(m.availability(s)).toBeCloseTo(0.99);
		const pts = await m.sliSeries(ctx(), 'core-stream', win7, 'hour', 'production');
		expect(pts.map((p) => p.totalWithFast).sort()).toEqual([1000, 900]);
		await run!(`DELETE FROM sli_hourly WHERE sli_id = 'core-stream'`);
	});

	it('SLI series buckets by hour', async () => {
		const pts = await m.sliSeries(ctx(), 'core-api', win7, 'hour', 'production');
		expect(pts.map((p) => p.total)).toEqual([1000, 1000]);
	});

	it('series by D19 group and freshness', async () => {
		const series = await m.errorEventsSeries(ctx(), win7, 'day');
		expect(series.reduce((a, r) => a + r.n, 0)).toBe(13);
		const latest = await m.latestReceivedAt(ctx());
		expect(latest?.toISOString()).toBe(H(5).toISOString());
		expect(await m.latestReceivedAt(ctx({ scopes: [] }))).toBeNull();
	});
});

// Pin suite for the occurrence-aggregation rewrite (sentinel-reporting-and-dashboard BUG-01): every query that reads
// error_occurrences together with issues, over a fixture that exercises multi-project and per-environment access,
// deleted issues, user reports (not system errors), null releases, 24 h change-failure edges and release windows, under
// several access contexts. The snapshot in __snapshots__ was recorded from the pre-rewrite JOIN implementation; the
// rewrite must reproduce it byte for byte. It lives in its own database so the file-parallel service tests cannot clobber it.
const P3 = '33333333-3333-3333-3333-333333333333'; // a project the viewer never reaches
let pinRun: Run | null = null;
if (url && run) pinRun = await resetSchema(url, 'sentinel_test_pin');

const win28: m.Window = { from: D(28), to: NOW };
const win24h: m.Window = { from: H(24), to: NOW };

describe.skipIf(!pinRun)('BUG-01: occurrence aggregation pins every rewritten query', () => {
	beforeAll(async () => {
		const occs: Parameters<typeof seed>[1]['occs'] = [];
		let k = 0;
		const occ = (issueId: string, environment: string, createdAt: Date, o: Partial<NonNullable<typeof occs>[number]> = {}) =>
			occs!.push({ id: `o${k++}`, issueId, environment, createdAt, ...o });
		// r1 (P1 production): p1-a emits 10 events inside 24 h of the release start -> fails on count alone, one user
		for (let j = 0; j < 10; j++) occ('p1-a', 'production', H(100 - j), { release: 'r1', service: 'api', tagSource: 'http_handler', requestScoped: true, userId: 'u1', level: j % 2 ? 'error' : 'warning' });
		// ... and p1-a keeps emitting under the later release r2, which p1-a did not introduce
		for (let j = 0; j < 4; j++) occ('p1-a', 'production', H(40 - j), { release: 'r2', service: 'api', tagSource: 'logger', userId: `u${j}` });
		// r2 introduced by p1-c: 12 events but only 2 inside the first 24 h, 1 user -> does not fail
		occ('p1-c', 'production', H(40), { release: 'r2', service: 'jobs worker', tagSource: 'job' });
		occ('p1-c', 'production', H(39), { release: 'r2', service: 'jobs worker', tagSource: 'job' });
		for (let j = 0; j < 10; j++) occ('p1-c', 'production', H(10 - j / 2), { release: 'r2', service: 'jobs worker', tagSource: 'job', userId: 'u9' });
		// r3 introduced by p1-d: three distinct users inside 24 h -> fails on users; also nats + unattributed
		for (const u of ['x', 'y', 'z']) occ('p1-d', 'production', H(8), { release: 'r3', service: 'worker', tagSource: 'worker_handler', userId: u });
		occ('p1-d', 'production', H(7), { release: 'r3' });
		// occurrences without a release, a user_report issue that "introduces" r2 with 10 events inside 24 h (it must not make r2 fail), and a deleted issue
		for (let j = 0; j < 3; j++) occ('p1-a', 'production', H(20 + j), { service: 'api', tagSource: 'http_handler', requestScoped: true, userId: `n${j}` });
		occ('p1-a', 'production', H(21.5), { service: 'api', tagSource: 'http_handler', requestScoped: true, userId: 'n0' }); // breaks a top-issues tie with p1-d
		for (let j = 0; j < 10; j++) occ('p1-rep', 'production', H(30), { release: 'r2', service: 'api', tagSource: 'http_handler', requestScoped: true });
		occ('p1-dead', 'production', H(12), { release: 'r1', service: 'api', tagSource: 'http_handler', requestScoped: true, userId: 'd' });
		// P1 staging has its own r1 window
		for (let j = 0; j < 3; j++) occ('p1-s', 'staging', H(30 - j), { release: 'r1', service: 'api', tagSource: 'http_handler', requestScoped: true, userId: `s${j}`, receivedAt: H(1) });
		// P2 production: release w1 with 10 events -> fails; P2 staging data the scope below hides
		for (let j = 0; j < 13; j++) occ('p2-a', 'production', H(50 - j), { release: 'w1', service: 'api', tagSource: 'http_handler', requestScoped: true, userId: `p${j % 2}` });
		occ('p2-s', 'staging', H(9), { release: 'w1', service: 'api', tagSource: 'http_handler', requestScoped: true });
		// P3 is never reachable
		for (let j = 0; j < 12; j++) occ('p3-a', 'production', H(3), { release: 'q1', service: 'api', tagSource: 'http_handler', requestScoped: true, userId: `q${j}` });
		// outside the 28-day window, and one 24 h-old and one just-inside-the-window event
		occ('p1-a', 'production', D(60), { release: 'r0', service: 'api', tagSource: 'http_handler', requestScoped: true, receivedAt: D(60) });
		occ('p1-a', 'production', H(24), { release: 'r2', service: 'api', tagSource: 'logger' });
		// twelve small issues so the top-10 cut and its tie-break matter
		const small = [] as { id: string; projectId: string; environment: string; firstSeen: Date }[];
		for (let j = 0; j < 12; j++) {
			small.push({ id: `p1-t${j}`, projectId: P1, environment: 'production', firstSeen: D(3) });
			for (let e = 0; e <= j; e++) occ(`p1-t${j}`, 'production', H(5 + e), { userId: `t${j % 4}-${e % 2}` });
		}
		await seed(pinRun!, {
			issues: [
				{ id: 'p1-a', projectId: P1, environment: 'production', firstSeen: D(5), firstRelease: 'r1' },
				{ id: 'p1-c', projectId: P1, environment: 'production', firstSeen: D(2), firstRelease: 'r2' },
				{ id: 'p1-d', projectId: P1, environment: 'production', firstSeen: D(1), firstRelease: 'r3' },
				{ id: 'p1-s', projectId: P1, environment: 'staging', firstSeen: D(2), firstRelease: 'r1' },
				{ id: 'p1-rep', projectId: P1, environment: 'production', firstSeen: D(1), issueType: 'user_report', firstRelease: 'r2' },
				{ id: 'p1-dead', projectId: P1, environment: 'production', firstSeen: D(5), firstRelease: 'r1', deletedAt: D(1), status: 'resolved' },
				{ id: 'p2-a', projectId: P2, environment: 'production', firstSeen: D(3), firstRelease: 'w1' },
				{ id: 'p2-s', projectId: P2, environment: 'staging', firstSeen: D(1), firstRelease: 'w1' },
				{ id: 'p3-a', projectId: P3, environment: 'production', firstSeen: D(1), firstRelease: 'q1' },
				...small,
			],
			occs,
			activity: [{ issueId: 'p1-d', eventType: 'regressed', createdAt: H(4), newValue: { releaseVersion: 'r3' } }],
		});}, 120_000); // the seeder inserts row by row

	const at = (access: Partial<AccessFilter>): m.MetricCtx => ({
		run: pinRun!,
		httpServices: ['api'],
		access: { scopes: [{ projectId: P1, environments: null }], environment: 'all', knownEnvironments: ['production', 'staging'], ...access },
	});
	const norm = (rows: unknown[]) => rows.map((r) => JSON.stringify(r)).sort();
	const contexts: Record<string, m.MetricCtx> = {
		'P1 unrestricted': at({}),
		'P1 + P2, P2 production only': at({ scopes: [{ projectId: P1, environments: null }, { projectId: P2, environments: ['production'] }] }),
		'P1 production only': at({ scopes: [{ projectId: P1, environments: ['production'] }] }),
		'P1 + P2 unrestricted, environment=staging': at({ scopes: [{ projectId: P1, environments: null }, { projectId: P2, environments: null }], environment: 'staging' }),
		'no scopes': at({ scopes: [] }),
		'empty environments': at({ scopes: [{ projectId: P1, environments: [] }] }),
	};

	for (const [label, c] of Object.entries(contexts)) {
		it(`output is unchanged: ${label}`, async () => {
			const sli = { id: 'x', project: 'core', source: 'x', windowDays: 28 } as const;
			const out = {
				errorEvents28: norm(await m.errorEvents(c, win28)),
				errorEvents24h: norm(await m.errorEvents(c, win24h)),
				series28d: norm(await m.errorEventsSeries(c, win28, 'day')),
				series24h: norm(await m.errorEventsSeries(c, win24h, 'hour')),
				topIssues: (await m.topIssues(c, win28)).map((r) => JSON.stringify(r)), // ordered: users, then events
				topIssues24h: (await m.topIssues(c, win24h)).map((r) => JSON.stringify(r)),
				changeFailure: norm(await m.changeFailure(c, win28)),
				changeFailureNarrow: norm(await m.changeFailure(c, { from: D(3), to: D(1) })),
				releaseWindows: (await m.releaseWindows(c, NOW, 10)).map((r) => JSON.stringify(r)),
				releaseWindows1: (await m.releaseWindows(c, NOW, 1)).map((r) => JSON.stringify(r)),
				sliHttp: await m.sliMatchingEvents(c, { ...sli, kind: 'http', service: 'api' }, P1, 'production', win28),
				sliHttpStaging: await m.sliMatchingEvents(c, { ...sli, kind: 'http', service: 'api' }, P1, 'staging', win28),
				sliHttpP2: await m.sliMatchingEvents(c, { ...sli, kind: 'http', service: 'api' }, P2, 'production', win28),
				sliJobs: await m.sliMatchingEvents(c, { ...sli, kind: 'jobs' }, P1, 'production', win28),
				sliNats: await m.sliMatchingEvents(c, { ...sli, kind: 'nats' }, P1, 'production', win28),
				latest: (await m.latestReceivedAt(c))?.toISOString() ?? null,
			};
			expect(out).toMatchSnapshot();
		});
	}

	it('the fixture is not vacuous: change failure fails on count (r1) and on users (r3) but not on a spread-out r2', async () => {
		const rows = await m.changeFailure(contexts['P1 unrestricted'], win28);
		expect(rows.find((r) => r.environment === 'production')).toMatchObject({ n: 3, failed: 2 });
		expect(sum(await m.errorEvents(contexts['P1 unrestricted'], win28))).toBeGreaterThan(50);
	});
});

