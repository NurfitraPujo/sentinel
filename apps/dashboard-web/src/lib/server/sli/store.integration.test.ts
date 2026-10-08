import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { collectSli, HOUR_MS } from './collect';
import { pgSliStore } from './store';
import type { ServiceSli } from './config';

// sentinel-reporting-and-dashboard D6 against a REAL, migrated Postgres: the three SLI kinds (http, jobs, nats), the
// 30-day first-run backfill, the 3-hour re-upsert, and "absent series writes no row", against a stub VictoriaMetrics.
// Skipped (not failed) unless a Postgres answers; SLI_INTEGRATION_REQUIRED=1 makes "unreachable" a hard failure.
//
//   DATABASE_URL=postgres://… SLI_INTEGRATION_REQUIRED=1 pnpm vitest run src/lib/server/sli/store.integration.test.ts

const connectionString = process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.SLI_INTEGRATION_REQUIRED === '1';
const suffix = `t${Date.now().toString(36)}`;

let reachable = false;
const sql = postgres(connectionString, { connect_timeout: 3, max: 1 });
try {
	await sql`select 1 from sli_hourly limit 1`;
	reachable = true;
} catch {
	reachable = false;
}
if (!reachable && required) throw new Error('SLI_INTEGRATION_REQUIRED=1 but no migrated Postgres answered');

// Every test owns its SLI ids (a fresh suffix per call), so no test depends on rows another one wrote: the file passes in any
// order, and under --sequence.shuffle (B13).
const created: string[] = [];
function slisFor(): { slis: ServiceSli[]; ids: { http: string; jobs: string; nats: string } } {
	const tag = `${suffix}-${created.length}-${Math.random().toString(36).slice(2, 6)}`;
	const ids = { http: `http-${tag}`, jobs: `jobs-${tag}`, nats: `nats-${tag}` };
	created.push(ids.http, ids.jobs, ids.nats);
	return {
		ids,
		slis: [
			{ id: ids.http, kind: 'http', project: 'core', source: 'api_core_80', latency: { le: '1000.0', target: 0.94 }, windowDays: 28 },
			{ id: ids.jobs, kind: 'jobs', project: 'core', source: 'j', windowDays: 28 },
			{ id: ids.nats, kind: 'nats', project: 'core', source: 'n', windowDays: 28 },
		],
	};
}
const NOW = new Date('2026-10-07T12:20:00Z');

// every series has a sample for every one of the last 720 hours except nats (absent)
function vm(valueFor: (kind: string, which: string, hourMs: number) => number | undefined): typeof fetch {
	return (async (u: URL | string) => {
		const q = new URL(String(u)).searchParams;
		const query = q.get('query')!;
		const kind = query.includes('envoy') ? 'http' : query.includes('job_handler') ? 'jobs' : 'nats';
		const which = query.includes('rq_time_bucket') ? 'fast' : query.includes('class="5"') || query.includes('status="error"') ? 'errors' : 'total';
		const start = Number(q.get('start')), end = Number(q.get('end'));
		const values: [number, string][] = [];
		for (let t = start; t <= end; t += 3600) {
			const v = valueFor(kind, which, t * 1000 - HOUR_MS);
			if (v !== undefined) values.push([t, String(v)]);
		}
		return new Response(JSON.stringify({ status: 'success', data: { result: values.length ? [{ metric: {}, values }] : [] } }));
	}) as unknown as typeof fetch;
}

describe.skipIf(!reachable)('sli-collect against real Postgres', () => {
	afterAll(async () => {
		for (const id of created) await sql`DELETE FROM sli_hourly WHERE sli_id = ${id}`;
		await sql.end({ timeout: 1 });
	});

	const rowsOf = async (id: string) => (await sql`SELECT count(*)::int AS n FROM sli_hourly WHERE sli_id = ${id}`)[0].n as number;

	it('backfills 30 days for http and jobs, writes nothing for the absent nats series', async () => {
		const { slis, ids } = slisFor();
		const fetchFn = vm((kind, which) => (kind === 'nats' ? undefined : which === 'total' ? 100 : which === 'errors' ? 3 : 90));
		await collectSli({ slis, cluster: 'tuju-production', vmUrl: 'http://vm', store: pgSliStore, now: NOW, fetchFn });
		expect(await rowsOf(ids.http)).toBe(720);
		expect(await rowsOf(ids.jobs)).toBe(720);
		expect(await rowsOf(ids.nats)).toBe(0); // an absent series writes no row, never zeros
		const [h] = await sql`SELECT min(fast) AS fast_min FROM sli_hourly WHERE sli_id = ${ids.http}`;
		expect(h.fast_min).toBe('90');
		const [j] = await sql`SELECT min(fast) AS fast_min FROM sli_hourly WHERE sli_id = ${ids.jobs}`;
		expect(j.fast_min).toBeNull(); // no latency objective: NULL, not 0
	});

	it('then re-upserts only the last 3 completed hours, replacing late-scrape values and leaving older rows alone', async () => {
		const { slis, ids } = slisFor();
		const first = vm((kind, which) => (kind === 'nats' ? undefined : which === 'total' ? 100 : which === 'errors' ? 3 : 90));
		await collectSli({ slis, cluster: 'tuju-production', vmUrl: 'http://vm', store: pgSliStore, now: NOW, fetchFn: first });
		const late = vm((kind, which) => (kind === 'nats' ? 40 : which === 'total' ? 200 : which === 'errors' ? 5 : 150));
		await collectSli({ slis, cluster: 'tuju-production', vmUrl: 'http://vm', store: pgSliStore, now: NOW, fetchFn: late });
		const [newest] = await sql`SELECT total, errors FROM sli_hourly WHERE sli_id = ${ids.http} AND hour_start = '2026-10-07T11:00:00Z'`;
		expect(newest).toMatchObject({ total: '200', errors: '5' });
		const [older] = await sql`SELECT total FROM sli_hourly WHERE sli_id = ${ids.http} AND hour_start = '2026-10-07T07:00:00Z'`;
		expect(older.total).toBe('100'); // outside the 3-hour window: untouched
		expect(await rowsOf(ids.nats)).toBe(720); // nats had no rows, so it backfilled once its series appeared
		expect(await rowsOf(ids.http)).toBe(720); // upsert, not insert
	});

	it('is idempotent: a second identical run changes nothing', async () => {
		const { slis, ids } = slisFor();
		const fetchFn = vm((kind, which) => (kind === 'nats' ? 7 : which === 'total' ? 10 : which === 'errors' ? 1 : 9));
		await collectSli({ slis, cluster: 'tuju-staging', vmUrl: 'http://vm', store: pgSliStore, now: NOW, fetchFn });
		const before = await sql`SELECT hour_start, total, errors FROM sli_hourly WHERE sli_id = ${ids.http} ORDER BY hour_start`;
		await collectSli({ slis, cluster: 'tuju-staging', vmUrl: 'http://vm', store: pgSliStore, now: NOW, fetchFn });
		const after = await sql`SELECT hour_start, total, errors FROM sli_hourly WHERE sli_id = ${ids.http} ORDER BY hour_start`;
		expect(after).toEqual(before);
		expect((await sql`SELECT DISTINCT environment FROM sli_hourly WHERE sli_id = ${ids.http}`).map((r) => r.environment)).toEqual(['staging']);
	});
});
