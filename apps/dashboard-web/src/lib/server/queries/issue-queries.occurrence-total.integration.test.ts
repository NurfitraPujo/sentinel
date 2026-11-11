import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';
import { issueQueries } from '$lib/server/queries/issue-queries';

/**
 * sentinel-issue-page-slow-count BUG-01 — `countOccurrencesByIssueId`, the issue page's
 * `Occurrence History (N)` total, against a REAL migrated Postgres. The claim is that the hourly
 * bucket sum it reads for a system-error issue is the same number as `COUNT(*)` over the issue's
 * retained `error_occurrences` rows: when every occurrence is written the way the processor writes
 * it (the row plus `occurrenceBucketUpsertQuery`, processor-go store.go, verbatim below), after a
 * retention-style delete of both at one hour-aligned cutoff, and after the real
 * `cleanupRetainedData` sweep. And that a `user_report` issue is counted exactly, buckets or not.
 * A mocked `db` can say which table was read (page.server.occurrence-total.test.ts); only rows can
 * say the number is right.
 *
 * Seeds its OWN organization, project and issues under a per-run suffix and deletes them again.
 *
 * Point DATABASE_URL at a DISPOSABLE database — never the shared dev one (CLAUDE.md); the third
 * test runs the real, database-wide retention sweep:
 *
 *   podman run -d --name ipc-sentinel-pg -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme \
 *     -e POSTGRES_DB=sentinel -p 127.0.0.1:55501:5432 docker.io/library/postgres:16
 *   cd packages/db-migrations && GOWORK=off \
 *     DB_URL_PROCESSOR=postgres://sentinel:changeme@127.0.0.1:55501/sentinel?sslmode=disable \
 *     go run ./cmd/migrate up -target=processor
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:changeme@127.0.0.1:55501/sentinel \
 *     OCCURRENCE_TOTAL_INTEGRATION_REQUIRED=1 \
 *     pnpm vitest run src/lib/server/queries/issue-queries.occurrence-total.integration.test.ts
 *
 * Skipped when nothing answers. SCHEMA_DRIFT_REQUIRED=1 (CI's switch for the DB-backed suites) or
 * OCCURRENCE_TOTAL_INTEGRATION_REQUIRED=1 makes "no reachable database" a hard failure.
 */
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required =
	process.env.SCHEMA_DRIFT_REQUIRED === '1' ||
	process.env.OCCURRENCE_TOTAL_INTEGRATION_REQUIRED === '1';

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
		`SCHEMA_DRIFT_REQUIRED=1 / OCCURRENCE_TOTAL_INTEGRATION_REQUIRED=1 but no database answered at ${connectionString}. ` +
			'This test is the only coverage that the issue page occurrence total matches COUNT(*) on real Postgres.'
	);
}

// The retention.*.integration family's mutex (see retention.buckets.integration.test.ts): this file
// seeds aged occurrence rows a sibling's sweep would delete mid-test, and runs a sweep itself.
const RETENTION_FAMILY_LOCK = 'sentinel.retention.integration';
const RETENTION_DAYS = 30;

const HOUR = 3_600_000;
const DAY = 24 * HOUR;

// processor-go store.go `occurrenceBucketUpsertQuery`, verbatim; $2 is the occurrence's UTC hour.
const OCCURRENCE_BUCKET_UPSERT = `
		INSERT INTO issue_occurrence_buckets (issue_id, bucket_start, count)
		VALUES ($1, $2, 1)
		ON CONFLICT (issue_id, bucket_start) DO UPDATE
		   SET count = issue_occurrence_buckets.count + 1
	`;

const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const projectId = randomUUID();

function floorToHour(date: Date): Date {
	return new Date(Math.floor(date.getTime() / HOUR) * HOUR);
}

describe.skipIf(!dbReachable)('countOccurrencesByIssueId (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });
		await sql`select pg_advisory_lock(hashtext(${RETENTION_FAMILY_LOCK}))`;
		await sql`
			insert into organizations (id, name, slug)
			values (${orgId}, ${'occurrence-total-it-' + suffix}, ${'occurrence-total-it-' + suffix})
		`;
		await sql`
			insert into projects (id, name, organization_id, api_key, api_key_hash)
			values (${projectId}, ${'occurrence-total-it-' + suffix}, ${orgId}, ${'k_' + suffix}, ${'h_' + suffix})
		`;
	}, 120_000);

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from issue_tombstones where organization_id = ${orgId}`;
			// error_occurrences and issue_occurrence_buckets cascade from issues.
			await sql`delete from issues where project_id = ${projectId}`;
			await sql`delete from projects where id = ${projectId}`;
			await sql`delete from organizations where id = ${orgId}`;
		} finally {
			await sql`select pg_advisory_unlock_all()`.catch(() => {});
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	async function seedIssue(issueType: 'system_error' | 'user_report'): Promise<string> {
		const id = randomUUID();
		await sql`
			insert into issues (
				id, project_id, environment, fingerprint, message, error_class, status, issue_type,
				first_seen, last_seen
			) values (
				${id}, ${projectId}, ${'production'}, ${'occ-total-' + id}, ${'occurrence total seed'},
				${'SeedError'}, ${'unresolved'}, ${issueType}, ${new Date(Date.now() - 60 * DAY)}, ${new Date()}
			)
		`;
		return id;
	}

	// One occurrence the way StoreEvent writes it: the row, then its hour's bucket upsert, in one
	// transaction. `withBucket: false` is a row with no bucket (nothing writes one for a manual issue).
	async function storeOccurrence(issueId: string, createdAt: Date, withBucket = true): Promise<void> {
		await sql.begin(async (tx) => {
			await tx`
				insert into error_occurrences (issue_id, environment, platform, created_at)
				values (${issueId}, ${'production'}, ${'go'}, ${createdAt})
			`;
			if (withBucket) await tx.unsafe(OCCURRENCE_BUCKET_UPSERT, [issueId, floorToHour(createdAt)]);
		});
	}

	async function exactCount(issueId: string): Promise<number> {
		const [row] = await sql<{ n: number }[]>`
			select count(*)::int as n from error_occurrences where issue_id = ${issueId}
		`;
		return row.n;
	}

	it('a system-error issue: the bucket sum equals COUNT(*), before and after a retention-style delete at one cutoff', async () => {
		const issueId = await seedIssue('system_error');
		const now = Date.now();
		// Aged and fresh occurrences, several sharing an hour (the upsert's DO UPDATE arm), one on an
		// hour boundary exactly.
		const times = [
			now - 45 * DAY,
			now - 45 * DAY + 60_000,
			now - 31 * DAY,
			floorToHour(new Date(now - 10 * DAY)).getTime(),
			now - 10 * DAY + 1_000,
			now - 10 * DAY + 2_000,
			now - 2 * HOUR,
			now - 60_000,
		];
		for (const t of times) await storeOccurrence(issueId, new Date(t));

		expect(await exactCount(issueId)).toBe(times.length);
		expect(await issueQueries.countOccurrencesByIssueId(issueId, 'system_error')).toBe(times.length);

		// Retention's two statements for this issue, at one cutoff (an hour boundary, so no bucket
		// straddles it): occurrences `created_at < cutoff`, buckets `bucket_start < cutoff`.
		const cutoff = floorToHour(new Date(now - RETENTION_DAYS * DAY));
		await sql`delete from error_occurrences where issue_id = ${issueId} and created_at < ${cutoff}`;
		await sql`delete from issue_occurrence_buckets where issue_id = ${issueId} and bucket_start < ${cutoff}`;

		const retained = times.filter((t) => t >= cutoff.getTime()).length;
		expect(retained).toBe(5);
		expect(await exactCount(issueId)).toBe(retained);
		expect(await issueQueries.countOccurrencesByIssueId(issueId, 'system_error')).toBe(retained);
	});

	it('a system-error issue: the bucket sum equals COUNT(*) after the real cleanupRetainedData sweep', async () => {
		const { cleanupRetainedData } = await import('$lib/server/retention');
		const issueId = await seedIssue('system_error');
		const now = Date.now();
		// Clear of the one hour the sweep's cutoff falls in (removed up to an hour early, by design).
		const times = [now - 40 * DAY, now - 35 * DAY, now - 20 * DAY, now - 20 * DAY + 5_000, now - HOUR];
		for (const t of times) await storeOccurrence(issueId, new Date(t));
		expect(await issueQueries.countOccurrencesByIssueId(issueId, 'system_error')).toBe(5);

		const result = await cleanupRetainedData(RETENTION_DAYS, 365, 30);
		expect(result.occurrenceStopped).toBe('drained');

		expect(await exactCount(issueId)).toBe(3);
		expect(await issueQueries.countOccurrencesByIssueId(issueId, 'system_error')).toBe(3);
	}, 120_000);

	it('a user_report issue is counted exactly, from error_occurrences, whatever its buckets say', async () => {
		const issueId = await seedIssue('user_report');
		expect(await issueQueries.countOccurrencesByIssueId(issueId, 'user_report')).toBe(0);

		const now = Date.now();
		await storeOccurrence(issueId, new Date(now - 100 * DAY), false);
		await storeOccurrence(issueId, new Date(now - HOUR), false);

		expect(await exactCount(issueId)).toBe(2);
		expect(await issueQueries.countOccurrencesByIssueId(issueId, 'user_report')).toBe(2);
		// The bucket sum would have said 0: the exact path is what makes this 2.
		expect(await issueQueries.countOccurrencesByIssueId(issueId, 'system_error')).toBe(0);
	});
});
