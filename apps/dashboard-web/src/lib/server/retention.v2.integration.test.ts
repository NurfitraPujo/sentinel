import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

// sentinel-event-schema-v2 §5.6 / §9.2 V4b -- runs the real `cleanupRetainedData` over occurrences
// that carry every v2 column (migration 1725300000). Retention needs no code change: it deletes whole
// rows by `created_at`, so the new columns go with the row and `error_search_index` still cascades.
// This is the proof that nothing on those columns (a foreign key, a trigger, a constraint) blocks the
// delete. The Go twin, `TestEventSchemaV2RetentionDeletesWholeRow` (tests/integration), runs the same
// DELETE statement on a disposable clone.
//
// Same shape as retention.claims.integration.test.ts: seeds its own organization/project/issue by
// suffix, removes them in `afterAll`, serialises with the other retention.*.integration suites on
// their shared advisory lock, and SKIPS unless a migrated Postgres answers. Set
// RETENTION_V2_INTEGRATION_REQUIRED=1 to make "no database" a failure:
//
//   cd apps/dashboard-web && \
//   DATABASE_URL=postgres://sentinel:changeme@localhost:5432/sentinel \
//   RETENTION_V2_INTEGRATION_REQUIRED=1 \
//   pnpm vitest run src/lib/server/retention.v2.integration.test.ts

const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.RETENTION_V2_INTEGRATION_REQUIRED === '1';

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
	throw new Error(
		`RETENTION_V2_INTEGRATION_REQUIRED=1 but no database answered at ${connectionString}. ` +
			'This test is the dashboard-side proof that retention deletes occurrences carrying the ' +
			'sentinel-event-schema-v2 columns; skipping it silently would leave that unproven.'
	);
}

const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const projectId = randomUUID();

function daysAgo(d: number): Date {
	const date = new Date();
	date.setDate(date.getDate() - d);
	return date;
}

describe.skipIf(!dbReachable)('cleanupRetainedData over v2 occurrence columns (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });
		await sql`select pg_advisory_lock(hashtext('sentinel.retention.integration'))`;
		await sql`
			insert into organizations (id, name, slug)
			values (${orgId}, ${'retention-v2-it-' + suffix}, ${'retention-v2-it-' + suffix})
		`;
		await sql`
			insert into projects (id, name, organization_id, api_key, api_key_hash)
			values (${projectId}, ${'retention-v2-it-' + suffix}, ${orgId}, ${'k_' + suffix}, ${'h_' + suffix})
		`;
	}, 120_000);

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from error_occurrences where issue_id in (select id from issues where project_id = ${projectId})`;
			await sql`delete from issues where project_id = ${projectId}`;
			await sql`delete from projects where id = ${projectId}`;
			await sql`delete from organizations where id = ${orgId}`;
		} finally {
			await sql`select pg_advisory_unlock_all()`.catch(() => {});
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	async function seedOccurrence(issueId: string, createdAt: Date) {
		const id = randomUUID();
		await sql`
			insert into error_occurrences (
				id, issue_id, project_id, environment, platform, created_at,
				level, message, error_class, logger, server_name, transaction, dist, source, trace_flags,
				received_at, sdk, event_user, tags, contexts, request, breadcrumbs, exception
			) values (
				${id}, ${issueId}, ${projectId}, 'production', 'javascript', ${createdAt},
				'warning', 'charge [REDACTED_PAN] failed', 'TypeError', 'checkout', 'web-1', '/checkout/pay',
				'4127', 'onerror', 1, ${createdAt},
				${sql.json({ name: 'sentinel.javascript', version: '1.8.0' })},
				${sql.json({ id: 'u1', email: 'a@b.co' })},
				${sql.json({ tenant_id: 'acme' })},
				${sql.json({ device: { model: 'Pixel 8' } })},
				${sql.json({ method: 'POST', headers: { Cookie: '***REDACTED***' } })},
				${sql.json([{ message: '/cart -> /checkout' }])},
				${sql.json({ values: [{ type: 'TypeError', value: 'x' }] })}
			)
		`;
		await sql`insert into error_search_index (occurrence_id, user_id, tenant_id) values (${id}, 'u1', 'acme')`;
		return id;
	}

	it('deletes an old occurrence with every v2 column populated, cascades its index row, keeps a newer one', async () => {
		const { cleanupRetainedData } = await import('./retention');

		const issueId = randomUUID();
		await sql`
			insert into issues (id, project_id, environment, fingerprint, message, error_class, status)
			values (${issueId}, ${projectId}, 'production', ${'v2-' + suffix}, 'seed', 'SeedError', 'unresolved')
		`;
		const oldId = await seedOccurrence(issueId, daysAgo(40));
		const newId = await seedOccurrence(issueId, new Date());

		await cleanupRetainedData(30);

		const remaining = await sql`select id from error_occurrences where id in (${oldId}, ${newId})`;
		expect(remaining.map((r) => r.id)).toEqual([newId]);

		const index = await sql`select occurrence_id from error_search_index where occurrence_id in (${oldId}, ${newId})`;
		expect(index.map((r) => r.occurrence_id)).toEqual([newId]);

		const [kept] = await sql`select level, tags, event_user from error_occurrences where id = ${newId}`;
		expect(kept.level).toBe('warning');
		expect(kept.tags).toEqual({ tenant_id: 'acme' });
		expect(kept.event_user).toEqual({ id: 'u1', email: 'a@b.co' });
	});
});
