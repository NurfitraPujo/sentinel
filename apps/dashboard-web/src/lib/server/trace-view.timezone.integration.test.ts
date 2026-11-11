import { describe, it, expect, vi, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { drizzle } from 'drizzle-orm/postgres-js';
import { randomBytes, randomUUID } from 'node:crypto';

/**
 * sentinel-cross-repo-trace-view m2 — row times and the skew hint under a NON-UTC session TimeZone.
 *
 * The Drizzle mirror declares `error_occurrences.created_at` as `timestamp` (no tz) while the
 * migrated column is `timestamptz`, so reading it through the mirror re-interprets the instant in
 * the Node process's zone: with `TimeZone=Asia/Jakarta` a 30 s skew read back as −7 h. The trace
 * queries therefore render `created_at` and compute the skew in SQL. This file points the app's
 * `$lib/server/db` at a client whose SESSION is Asia/Jakarta (a per-connection setting, so it cannot
 * disturb the other suites sharing the database — ALTER DATABASE would).
 *
 * Same database and flag as trace-view.integration.test.ts (TRACE_VIEW_INTEGRATION_REQUIRED=1).
 */
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.TRACE_VIEW_INTEGRATION_REQUIRED === '1';

let dbReachable = false;
const probe = postgres(connectionString, { connect_timeout: 3, max: 1 });
try {
	await probe`select 1`;
	dbReachable = true;
} catch {
	dbReachable = false;
} finally {
	await probe.end({ timeout: 1 }).catch(() => {});
}
if (required && !dbReachable) {
	throw new Error(`TRACE_VIEW_INTEGRATION_REQUIRED=1 but no database answered at ${connectionString}.`);
}

vi.mock('$lib/server/db', () => {
	const client = postgres(connectionString, { connection: { TimeZone: 'Asia/Jakarta' } });
	return { db: drizzle(client) };
});

const suffix = randomUUID().slice(0, 8);
const org = randomUUID();
const project = randomUUID();
const issue = randomUUID();
const trace = randomBytes(16).toString('hex');
const occSmall = randomUUID(); // 30 s skew
const occBig = randomUUID(); // 2 min skew

const T0 = Math.floor(Date.now() / 1000) * 1000 - 3 * 60 * 60 * 1000;
const iso = (ms: number) => new Date(T0 + ms).toISOString();

describe.skipIf(!dbReachable)('trace view under a non-UTC session TimeZone (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;

	beforeAll(async () => {
		sql = postgres(connectionString, { max: 1 });
		await sql`insert into organizations (id, name, slug) values (${org}, ${`tz-${suffix}`}, ${`tz-${suffix}`})`;
		await sql`insert into projects (id, name, organization_id, api_key, api_key_hash) values (${project}, ${`tz-${suffix}`}, ${org}, ${`k_tz_${suffix}`}, ${`h_tz_${suffix}`})`;
		await sql`insert into issues (id, project_id, environment, fingerprint, message, error_class) values (${issue}, ${project}, 'production', ${`fp-${issue}`}, 'tz', 'E')`;
		for (const [id, offsetMs, skewMs] of [[occSmall, 0, 30_000], [occBig, 1000, 120_000]] as const) {
			await sql`
				insert into error_occurrences (id, issue_id, environment, platform, trace_id, created_at, received_at)
				values (${id}, ${issue}, 'production', 'node', ${trace}, ${iso(offsetMs)}::timestamptz, ${iso(offsetMs + skewMs)}::timestamptz)
			`;
		}
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from error_occurrences where issue_id = ${issue}`;
			await sql`delete from issues where id = ${issue}`;
			await sql`delete from projects where id = ${project}`;
			await sql`delete from organizations where id = ${org}`;
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	it('the session really is Asia/Jakarta (otherwise this test proves nothing)', async () => {
		const { db } = await import('$lib/server/db');
		const { sql: s } = await import('drizzle-orm');
		const rows = (await db.execute(s`show timezone`)) as unknown as Array<{ TimeZone: string }>;
		expect(rows[0].TimeZone).toBe('Asia/Jakarta');
	});

	it('row times are the stored instants, and skew is 30 s / 120 s — no hint for 30 s, a hint for 2 min', async () => {
		const { listTraceOccurrences, traceRowColumns } = await import('./queries/trace-queries');
		const { rows } = await listTraceOccurrences({
			traceId: trace, scope: { projectIds: [project] }, limit: 10, columns: traceRowColumns,
		});
		const byId = new Map(rows.map((r: any) => [r.id, r]));

		expect(new Date((byId.get(occSmall) as any).createdAt).toISOString()).toBe(iso(0));
		expect(new Date((byId.get(occBig) as any).createdAt).toISOString()).toBe(iso(1000));
		expect((byId.get(occSmall) as any).skewSeconds).toBe(30);
		expect((byId.get(occBig) as any).skewSeconds).toBe(120);
	});

	it('the agent read returns the same instants', async () => {
		const { listAgentTraceOccurrences } = await import('$lib/db/queries/agent-reads');
		const { occurrences } = await listAgentTraceOccurrences({ organizationId: org, traceId: trace });
		expect(occurrences.map((o) => new Date(o.createdAt as any).toISOString())).toEqual([iso(0), iso(1000)]);
	});
});
