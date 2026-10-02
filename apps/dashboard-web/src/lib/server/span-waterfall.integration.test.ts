import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomBytes, randomUUID } from 'node:crypto';

/**
 * sentinel-span-ingest D8/D9 — the waterfall's span query and the span retention, against a REAL migrated
 * Postgres, because every claim is a claim about SQL: that the org scope, the project set and the environment
 * grants compose into the same WHERE as the trace id, that pulled spans (which have no project) follow the
 * environment rule of D8, and that retention is a DROP of whole day partitions.
 *
 * One trace id is seeded in org A (mobile spans in two projects x two environments, pulled core spans in two
 * environments) AND in org B (trace ids are client-asserted, so another org can send the same one).
 *
 * Point DATABASE_URL at a DISPOSABLE database — never the shared dev one (CLAUDE.md):
 *
 *   cd packages/db-migrations && GOWORK=off DB_URL_DASHBOARD='postgres://sentinel:<pw>@127.0.0.1:<port>/sentinel?sslmode=disable' \
 *     go run ./cmd/migrate up -target dashboard
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:<pw>@127.0.0.1:<port>/sentinel \
 *     SPAN_WATERFALL_INTEGRATION_REQUIRED=1 pnpm vitest run src/lib/server/span-waterfall.integration.test.ts
 *
 * Skipped when nothing answers; SPAN_WATERFALL_INTEGRATION_REQUIRED=1 makes that a hard failure.
 */
const connectionString = process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.SPAN_WATERFALL_INTEGRATION_REQUIRED === '1';

let dbReachable = false;
const probeSql = postgres(connectionString, { connect_timeout: 3, max: 1 });
try {
	await probeSql`select 1`;
	dbReachable = true;
} catch {
	dbReachable = false;
} finally {
	await probeSql.end({ timeout: 1 }).catch(() => {});
}
if (required && !dbReachable) {
	throw new Error(`SPAN_WATERFALL_INTEGRATION_REQUIRED=1 but no database answered at ${connectionString}.`);
}

const suffix = randomUUID().slice(0, 8);
const hex32 = () => randomBytes(16).toString('hex');
const hex16 = () => randomBytes(8).toString('hex');

const TRACE = hex32();
const TRACE_B_ONLY = hex32();

const orgA = randomUUID();
const orgB = randomUUID();
const projA1 = randomUUID();
const projA2 = randomUUID();
const projB = randomUUID();

const member = randomUUID(); // org A engineer, no grant
const narrowOne = randomUUID(); // narrowed to {production} in projA1 only
const narrowAll = randomUUID(); // narrowed to {production} in BOTH org A projects
const stagingCore = randomUUID(); // narrowed to {staging} on CORE (projA1); the mobile project projA2 unrestricted
const outsider = randomUUID(); // in no org

const NOW = Date.now();
const iso = (ms: number) => new Date(NOW - 60 * 60 * 1000 + ms).toISOString();

const id = {
	m1: hex16(), // mobile  projA1 production
	m2: hex16(), // mobile  projA1 staging
	m3: hex16(), // mobile  projA2 production
	p1: hex16(), // pulled  org A  production
	p2: hex16(), // pulled  org A  staging
	bm: hex16(), // mobile  projB  production  (org B)
	bp: hex16(), // pulled  org B  production  (org B)
};

describe.skipIf(!dbReachable)('the span waterfall query and span retention (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;

	const insertSpan = (
		org: string, project: string | null, source: 'mobile' | 'pulled', env: string, trace: string, spanId: string,
		startMs: number, over: { parent?: string; name?: string; attrs?: Record<string, unknown>; startTs?: string; scope?: string | null } = {}
	) => sql`
		insert into spans (org_id, project_id, environment, source, service_name, trace_id, span_id, parent_span_id, name, op, start_ts, duration_us, status, attributes, scope_project_id)
		values (${org}, ${project}, ${env}, ${source}, ${source === 'mobile' ? 'mobile-app' : 'core-api'}, ${trace}, ${spanId}, ${over.parent ?? ''},
		        ${over.name ?? `span ${spanId}`}, 'op', ${over.startTs ?? iso(startMs)}::timestamptz, 1500, 'ok', ${sql.json((over.attrs ?? {}) as never)}, ${over.scope ?? null})`;

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });
		await sql`select spans_ensure_partitions(1, 2)`;

		for (const [oid, label] of [[orgA, 'a'], [orgB, 'b']] as const) {
			await sql`insert into organizations (id, name, slug) values (${oid}, ${`sw-${label}-${suffix}`}, ${`sw-${label}-${suffix}`})`;
		}
		for (const [pid, org, label] of [[projA1, orgA, 'a1'], [projA2, orgA, 'a2'], [projB, orgB, 'b']] as const) {
			await sql`insert into projects (id, name, organization_id, api_key, api_key_hash)
				values (${pid}, ${`sw-${label}-${suffix}`}, ${org}, ${`k_${label}_${suffix}`}, ${`h_${label}_${suffix}`})`;
		}
		for (const [uid, label] of [[member, 'member'], [narrowOne, 'narrow1'], [narrowAll, 'narrowall'], [stagingCore, 'stagingcore'], [outsider, 'outsider']] as const) {
			await sql`insert into "user" (id, name, email) values (${uid}, ${label}, ${`sw-${label}-${suffix}@example.test`})`;
		}
		for (const uid of [member, narrowOne, narrowAll, stagingCore]) {
			await sql`insert into organization_members (organization_id, user_id, role) values (${orgA}, ${uid}, 'engineer')`;
		}
		await sql`insert into project_environment_grants (user_id, project_id, environments, created_by)
			values (${narrowOne}, ${projA1}, ${sql.array(['production'])}, ${member})`;
		await sql`insert into project_environment_grants (user_id, project_id, environments, created_by)
			values (${stagingCore}, ${projA1}, ${sql.array(['staging'])}, ${member})`;
		for (const p of [projA1, projA2]) {
			await sql`insert into project_environment_grants (user_id, project_id, environments, created_by)
				values (${narrowAll}, ${p}, ${sql.array(['production'])}, ${member})`;
		}

		await insertSpan(orgA, projA1, 'mobile', 'production', TRACE, id.m1, 0, { attrs: { 'http.method': 'GET', long: 'x'.repeat(500), nested: { a: 1 } } });
		await insertSpan(orgA, projA1, 'mobile', 'staging', TRACE, id.m2, 10, { parent: id.m1 });
		await insertSpan(orgA, projA2, 'mobile', 'production', TRACE, id.m3, 20, { parent: id.m1 });
		await insertSpan(orgA, null, 'pulled', 'production', TRACE, id.p1, 30, { parent: id.m1, scope: projA1 });
		await insertSpan(orgA, null, 'pulled', 'staging', TRACE, id.p2, 40, { parent: id.p1, scope: projA1 });
		await insertSpan(orgB, projB, 'mobile', 'production', TRACE, id.bm, 5);
		await insertSpan(orgB, null, 'pulled', 'production', TRACE, id.bp, 6);
		await insertSpan(orgB, null, 'pulled', 'production', TRACE_B_ONLY, hex16(), 7);
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from spans where org_id in ${sql([orgA, orgB])}`;
			await sql`delete from span_pull_queue where org_id in ${sql([orgA, orgB])}`;
			await sql`delete from project_environment_grants where user_id in ${sql([member, narrowOne, narrowAll, stagingCore])}`;
			await sql`delete from organization_members where organization_id in ${sql([orgA, orgB])}`;
			await sql`delete from projects where organization_id in ${sql([orgA, orgB])}`;
			await sql`delete from "user" where id in ${sql([member, narrowOne, narrowAll, stagingCore, outsider])}`;
			await sql`delete from organizations where id in ${sql([orgA, orgB])}`;
			// Leave no partition behind that this file created for the retention case.
			await sql`select spans_drop_expired(14)`;
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	async function scopeFor(userId: string, orgId = orgA) {
		const { getAccessibleProjects } = await import('./issue-access');
		const { resolveEnvironmentScopes } = await import('./environment-access');
		return {
			orgId,
			projects: await getAccessibleProjects(userId, orgId),
			environmentScopes: await resolveEnvironmentScopes(userId),
		};
	}
	async function spansFor(userId: string, trace = TRACE, orgId = orgA, cap?: number) {
		const { listTraceSpans } = await import('./queries/span-queries');
		return listTraceSpans(trace, await scopeFor(userId, orgId), cap);
	}
	const ids = (r: { spans: Array<{ spanId: string }> }) => r.spans.map((s) => s.spanId);
	const orgAHandle = { id: orgA, slug: `sw-a-${suffix}` };

	it('an org member sees the org’s mobile and pulled spans in time order, never the other org’s', async () => {
		const r = await spansFor(member);
		expect(ids(r)).toEqual([id.m1, id.m2, id.m3, id.p1, id.p2]);
		expect(ids(r)).not.toContain(id.bm);
		expect(ids(r)).not.toContain(id.bp);
		expect(r.truncated).toBe(false);
		expect(r.spans[0]).toMatchObject({ source: 'mobile', projectName: `sw-a1-${suffix}`, environment: 'production', durationUs: 1500 });
		expect(r.spans[3]).toMatchObject({ source: 'pulled', projectName: null });
		expect(r.spans[3].startUs).toBeGreaterThan(r.spans[0].startUs);
	});

	it('a project environment grant hides the other environment of ITS project’s mobile spans only', async () => {
		// narrowed to production on projA1 (core, the scope project of pulled spans): m2 (projA1/staging) and the staging pulled span p2 are
		// hidden; m3 (projA2, unrestricted) stays.
		expect(ids(await spansFor(narrowOne))).toEqual([id.m1, id.m3, id.p1]);
	});

	it('pulled spans follow the grant on the org’s CORE project (their scope project)', async () => {
		expect(ids(await spansFor(narrowAll))).toEqual([id.m1, id.m3, id.p1]); // m2 and p2 are staging
	});

	it('(a) staging-only on core and unrestricted on the mobile project sees NO production pulled spans', async () => {
		// stagingCore is unrestricted on projA2 (mobile): neither that nor a mobile error queueing the trace may open core's data.
		const r = await spansFor(stagingCore);
		expect(ids(r)).toEqual([id.m2, id.m3, id.p2]);
		expect(ids(r)).not.toContain(id.p1);
	});

	it('(b) a viewer unrestricted on core only sees every environment of the pulled spans, and none of the mobile project’s spans', async () => {
		expect(ids(await spansFor(member))).toEqual(expect.arrayContaining([id.p1, id.p2]));
		const scope = await scopeFor(member);
		const { listTraceSpans } = await import('./queries/span-queries');
		expect(ids(await listTraceSpans(TRACE, { ...scope, projects: [{ id: projA1 }] }))).toEqual([id.m1, id.m2, id.p1, id.p2]);
	});

	it('(c) a mobile-only viewer (cannot reach core) sees none of the pulled spans', async () => {
		const scope = await scopeFor(member);
		const { listTraceSpans } = await import('./queries/span-queries');
		const r = await listTraceSpans(TRACE, { ...scope, projects: [{ id: projA2 }] }); // core (projA1) not reachable
		expect(ids(r)).toEqual([id.m3]);
	});

	it('a pulled span with no scope project is never shown', async () => {
		const orphan = hex16();
		await insertSpan(orgA, null, 'pulled', 'production', TRACE, orphan, 50, { scope: null });
		expect(ids(await spansFor(member))).not.toContain(orphan);
	});

	it('a viewer who reaches no project sees nothing, pulled spans included', async () => {
		const r = await spansFor(outsider);
		expect(r.spans).toEqual([]);
	});

	it('another org’s trace is not revealed: a trace seen only in org B is empty for an org A viewer', async () => {
		const mine = await spansFor(member, TRACE_B_ONLY);
		expect(mine.spans).toEqual([]);
		const strip = (d: any) => ({ ...d, traceId: 'x' });
		const { load } = await import('../../routes/[orgSlug]/traces/[traceId]/+page.server');
		const loadPage = async (trace: string) =>
			strip(
				await (load as any)({
					params: { orgSlug: orgAHandle.slug, traceId: trace },
					locals: { auth: async () => ({ user: { id: member } }), currentOrg: orgAHandle },
					url: new URL(`http://localhost/x/traces/${trace}`),
				})
			);
		expect(await loadPage(TRACE_B_ONLY)).toEqual(await loadPage(hex32()));
	});

	it('the page loader returns the spans, the cap flag and the span retention window', async () => {
		const { load } = await import('../../routes/[orgSlug]/traces/[traceId]/+page.server');
		const data = await (load as any)({
			params: { orgSlug: orgAHandle.slug, traceId: TRACE },
			locals: { auth: async () => ({ user: { id: member } }), currentOrg: orgAHandle },
			url: new URL(`http://localhost/x/traces/${TRACE}`),
		});
		expect(data.spans.map((s: any) => s.spanId)).toEqual([id.m1, id.m2, id.m3, id.p1, id.p2]);
		expect(data.spansTruncated).toBe(false);
		expect(data.spanRetentionDays).toBe(14);
		expect(data.occurrences).toEqual([]); // spans without errors: the occurrence list is unchanged
	});

	it('caps the result and says so, keeping the earliest spans', async () => {
		const r = await spansFor(member, TRACE, orgA, 3);
		expect(ids(r)).toEqual([id.m1, id.m2, id.m3]);
		expect(r.truncated).toBe(true);
	});

	it('ships a trimmed attribute preview, not the whole 8 KiB', async () => {
		const [first] = (await spansFor(member)).spans;
		expect(Object.keys(first.attributes).sort()).toEqual(['http.method', 'long']); // the nested object is not a preview scalar
		expect(String(first.attributes.long)).toHaveLength(200);
	});

	it('retention drops whole expired day partitions, keeps the window, and reaps the pull queue on the same 14 days', async () => {
		const { maintainSpans } = await import('./retention');
		const day = (back: number) => new Date(Date.UTC(new Date().getUTCFullYear(), new Date().getUTCMonth(), new Date().getUTCDate() - back));
		const name = (d: Date) => `spans_p${d.toISOString().slice(0, 10).replaceAll('-', '')}`;
		const bound = (d: Date) => `${d.toISOString().slice(0, 10)} 00:00:00+00`;
		const old = day(16);
		const edge = day(13);
		for (const d of [old, edge]) {
			const next = new Date(d.getTime() + 86_400_000);
			await sql.unsafe(`create table if not exists ${name(d)} partition of spans for values from ('${bound(d)}') to ('${bound(next)}')`);
		}
		const retTrace = hex32();
		await insertSpan(orgA, null, 'pulled', 'production', retTrace, hex16(), 0, { startTs: new Date(old.getTime() + 3_600_000).toISOString() });
		await insertSpan(orgA, null, 'pulled', 'production', retTrace, hex16(), 0, { startTs: new Date(edge.getTime() + 3_600_000).toISOString() });
		await sql`insert into span_pull_queue (org_id, trace_id, next_fetch_at, created_at) values
			(${orgA}, ${hex32()}, now(), now() - interval '20 days'), (${orgA}, ${hex32()}, now(), now() - interval '2 days')`;

		const result = await maintainSpans();
		expect(result.failed).toBe(false);
		expect(result.droppedPartitions).toBeGreaterThanOrEqual(1);
		expect(result.reapedPullQueue).toBe(1);

		const [{ n }] = await sql`select count(*)::int as n from spans where trace_id = ${retTrace}`;
		expect(n).toBe(1); // the 16-day-old row went with its partition; the 13-day-old one stayed
		const parts = (await sql`select c.relname from pg_inherits i join pg_class c on c.oid = i.inhrelid where i.inhparent = 'spans'::regclass`).map((r) => r.relname);
		expect(parts).not.toContain(name(old));
		expect(parts).toContain(name(edge));
		const [{ q }] = await sql`select count(*)::int as q from span_pull_queue where org_id = ${orgA}`;
		expect(q).toBe(1);
		// The processor's and the cron's partition keeper is idempotent.
		expect((await maintainSpans()).createdPartitions).toBe(0);
	});

	it('m7: maintenance is bounded — a reader holding `spans` cannot make it wait out the run, and the run’s own caps come back', async () => {
		const { maintainSpans } = await import('./retention');
		const { openRetentionRun } = await import('./retention-run');
		const { openRetentionRunClient } = await import('$lib/server/db');
		const client = openRetentionRunClient(() => {});
		const run = openRetentionRun(client as never, { deadlineMs: 120_000, isLost: () => false });
		try {
			// An expired partition for spans_drop_expired to work on.
			const old = new Date(Date.UTC(new Date().getUTCFullYear(), new Date().getUTCMonth(), new Date().getUTCDate() - 17));
			const next = new Date(old.getTime() + 86_400_000);
			const pname = `spans_p${old.toISOString().slice(0, 10).replaceAll('-', '')}`;
			const day = (d: Date) => `${d.toISOString().slice(0, 10)} 00:00:00+00`;
			await sql.unsafe(`create table if not exists ${pname} partition of spans for values from ('${day(old)}') to ('${day(next)}')`);

			// A reader holds `spans` open (ACCESS SHARE blocks the ACCESS EXCLUSIVE a DROP needs).
			let release!: () => void;
			const held = new Promise<void>((r) => (release = r));
			let locked!: () => void;
			const lockedP = new Promise<void>((r) => (locked = r));
			const reader = postgres(connectionString, { connect_timeout: 5, max: 1 });
			const readerDone = reader.begin(async (tx) => {
				await tx`lock table spans in access share mode`;
				locked();
				await held;
			});
			await lockedP;

			const caps: number[] = [];
			const spy = { setCaps: (c: { lockMs: number } | null) => { caps.push(c ? c.lockMs : -1); run.setCaps(c as never); } };
			const t0 = performance.now();
			const result = await maintainSpans(run.db, 14, spy, { lockMs: 400, statementMs: 5_000 });
			const took = performance.now() - t0;
			expect(result.failed).toBe(true); // the drop timed out on the lock and was abandoned
			expect(took).toBeLessThan(3_000); // not the run's remaining ~120 s
			expect(caps).toEqual([400, -1]); // tightened for these statements, then returned to the run's own bound

			release();
			await readerDone;
			await reader.end({ timeout: 2 });

			// The run still works, and a second attempt (no reader, default caps) completes the drop.
			const again = await maintainSpans(run.db, 14, run);
			expect(again.failed).toBe(false);
			expect(again.droppedPartitions).toBeGreaterThanOrEqual(1);
		} finally {
			await (client as any).end({ timeout: 1 }).catch(() => {});
		}
	});
});
