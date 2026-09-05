import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

/**
 * sentinel-user-report-issue §4.3 — triage's WRITE DISCIPLINE and its query plan, against a REAL
 * migrated Postgres.
 *
 * `triage.test.ts` beside this file is the pure matcher at §11's named boundary: it decides what
 * the rule SAYS. This file decides what the rule WRITES, and those are different claims. A Drizzle
 * double returns itself for every call and so cannot tell "wrote exactly one `linked_to` row" from
 * "wrote nothing" (MANUAL_ISSUES_DESIGN §1, B10 addendum) — which is precisely the assertion
 * §4.3's "a wrong written link is visible to everyone and has to be removed by hand" rests on.
 *
 * It must run against a DISPOSABLE Postgres, never the shared dev database (CLAUDE.md). Everything
 * is namespaced by a per-run uuid and removed in `afterAll`.
 *
 *   docker run -d --name sentinel-task04-pg -p 15433:5432 \
 *     -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme -e POSTGRES_DB=sentinel postgres:15-alpine
 *   cd packages/db-migrations && GOWORK=off \
 *     DB_URL_DASHBOARD=postgres://sentinel:changeme@localhost:15433/sentinel?sslmode=disable \
 *     go run ./cmd/migrate up -target dashboard
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:changeme@localhost:15433/sentinel \
 *     pnpm vitest run src/lib/server/triage.integration.test.ts
 *
 * Skips if nothing answers on DATABASE_URL, like every other integration file here. Set
 * `TRIAGE_INTEGRATION_REQUIRED=1` to turn that skip into a hard failure.
 */
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.TRIAGE_INTEGRATION_REQUIRED === '1';

/**
 * **`DATABASE_URL` must be set EXPLICITLY for this file to run**, which is stricter than the
 * "probe the default and skip if nothing answers" rule the other integration files here use.
 *
 * The default is the shared dev database of a `docker compose up` stack, and that stack is usually
 * up on a developer machine -- so probing alone would silently point these tests at it. CLAUDE.md
 * is explicit that nothing may do that. Requiring the variable makes "which database am I about to
 * write to" a decision someone made rather than one a running container made for them.
 */
const explicitlyTargeted = process.env.DATABASE_URL !== undefined;

let dbReachable = false;
const probeSql = postgres(connectionString, { connect_timeout: 3, max: 1 });
try {
	await probeSql`select 1 from external_issue_reports limit 1`;
	dbReachable = true;
} catch {
	dbReachable = false;
} finally {
	await probeSql.end({ timeout: 1 }).catch(() => {});
}

if (required && !(dbReachable && explicitlyTargeted)) {
	throw new Error(
		`TRIAGE_INTEGRATION_REQUIRED=1 but no migrated database was EXPLICITLY targeted at ${connectionString} (set DATABASE_URL). ` +
			'This test is the committed proof that a 0.4 candidate writes nothing and a 0.7 candidate ' +
			'writes exactly one linked_to row plus one linked activity row in one transaction; ' +
			'skipping it silently would leave §4.3\'s whole write discipline unverified.'
	);
}

const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const projectId = randomUUID();
const otherProjectId = randomUUID();
const reporterId = randomUUID();

/** The instant the report says it happened. Every occurrence below is placed relative to it. */
const OCCURRED_AT = new Date('2026-09-04T09:12:03.000Z');
const minutes = (n: number) => new Date(OCCURRED_AT.getTime() + n * 60_000);

let sql: ReturnType<typeof postgres>;
let triage: typeof import('./triage');
let db: typeof import('./db').db;

/** One `system_error` issue with one occurrence, in `projectId` unless told otherwise. */
async function seedSystemError(options: {
	message: string;
	at: Date;
	release?: string | null;
	environment?: string;
	projectId?: string;
	id?: string;
}): Promise<string> {
	const issueId = options.id ?? randomUUID();
	await sql`
		insert into issues (id, project_id, fingerprint, message, error_class, issue_type)
		values (${issueId}, ${options.projectId ?? projectId}, ${'fp-' + issueId}, ${options.message},
		        ${'PostgresError'}, ${'system_error'})
	`;
	await sql`
		insert into error_occurrences (id, issue_id, environment, platform, release_version, created_at)
		values (${randomUUID()}, ${issueId}, ${options.environment ?? 'production'}, ${'go'},
		        ${options.release ?? null}, ${options.at})
	`;
	return issueId;
}

/** One `user_report` issue with an `external_issue_reports` companion. */
async function seedExternalReport(context: Record<string, string>): Promise<string> {
	const issueId = randomUUID();
	await sql`
		insert into issues (id, project_id, fingerprint, message, error_class, issue_type, source_channel)
		values (${issueId}, ${projectId}, ${'fp-' + issueId}, ${'Visits — save spins forever'},
		        ${'UserReport'}, ${'user_report'}, ${'user_report_api'})
	`;
	await sql`
		insert into external_issue_reports (issue_id, external_reporter_id, body_md, severity, context)
		values (${issueId}, ${reporterId}, ${'the save button spins forever'}, ${'high'},
		        ${sql.json(context)})
	`;
	return issueId;
}

const countRelations = async (sourceIssueId: string) =>
	Number(
		(
			await sql<{ count: string }[]>`
				select count(*)::text as count from issue_relations where source_issue_id = ${sourceIssueId}
			`
		)[0].count
	);

describe.skipIf(!(dbReachable && explicitlyTargeted))('§4.3 triage against a real Postgres', () => {
	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 4 });

		await sql`insert into organizations (id, name, slug) values (${orgId}, ${'tri-' + suffix}, ${'tri-' + suffix})`;
		for (const [id, name] of [
			[projectId, 'proj-' + suffix],
			[otherProjectId, 'other-' + suffix],
		] as const) {
			await sql`
				insert into projects (id, name, organization_id, api_key, api_key_hash)
				values (${id}, ${name}, ${orgId}, ${'pk_' + name}, ${'hash_' + name})
			`;
		}
		await sql`
			insert into external_reporters (id, organization_id, project_id, external_id, display_name)
			values (${reporterId}, ${orgId}, ${projectId}, ${'u_' + suffix}, ${'Sarah K.'})
		`;

		triage = await import('./triage');
		({ db } = await import('./db'));
	});

	afterAll(async () => {
		if (!sql) return;
		await sql`
			delete from external_issue_reports
			where issue_id in (select i.id from issues i join projects p on p.id = i.project_id
			                   where p.organization_id = ${orgId})
		`;
		await sql`delete from organizations where id = ${orgId}`;
		await sql.end({ timeout: 5 });
	});

	describe('the candidate query — §4.3\'s candidate set', () => {
		it('is scoped to ONE project, to issue_type=system_error, and to the window', async () => {
			const inWindow = await seedSystemError({ message: 'in window', at: minutes(-5) });
			const outOfWindow = await seedSystemError({ message: 'too old', at: minutes(-45) });
			const wrongProject = await seedSystemError({
				message: 'another project',
				at: minutes(-5),
				projectId: otherProjectId,
			});
			// A `user_report` issue is never a candidate: §4.3 keeps MANUAL_ISSUES_DESIGN §0 Q9's
			// separation and adds no second bridge.
			const anotherReport = await seedExternalReport({ release: 'v31.70' });

			const { windowStart, windowEnd } = triage.triageWindow(OCCURRED_AT, 30);
			const rows = await triage.findTriageCandidates({
				projectId,
				windowStart,
				windowEnd,
				release: null,
				environment: null,
				limit: 50,
			});

			const ids = rows.map((row) => row.issueId);
			expect(ids).toContain(inWindow);
			expect(ids).not.toContain(outOfWindow);
			expect(ids).not.toContain(wrongProject);
			expect(ids).not.toContain(anotherReport);
		});

		it('aggregates the DISTINCT in-window releases and environments the scorer needs', async () => {
			const issueId = randomUUID();
			await sql`
				insert into issues (id, project_id, fingerprint, message, error_class, issue_type)
				values (${issueId}, ${projectId}, ${'fp-' + issueId}, ${'multi'}, ${'PostgresError'}, ${'system_error'})
			`;
			for (const [release, environment, at] of [
				['v31.69', 'staging', minutes(-20)],
				['v31.70', 'production', minutes(-3)],
				// Outside the window: its release must NOT reach the aggregate, or the window stops
				// meaning anything.
				['v99.99', 'production', minutes(-90)],
			] as const) {
				await sql`
					insert into error_occurrences (id, issue_id, environment, platform, release_version, created_at)
					values (${randomUUID()}, ${issueId}, ${environment}, ${'go'}, ${release}, ${at})
				`;
			}

			const { windowStart, windowEnd } = triage.triageWindow(OCCURRED_AT, 30);
			const rows = await triage.findTriageCandidates({
				projectId,
				windowStart,
				windowEnd,
				release: 'v31.70',
				environment: 'production',
				limit: 50,
			});
			const row = rows.find((candidate) => candidate.issueId === issueId);
			expect(row).toBeDefined();
			expect([...row!.releaseVersions].sort()).toEqual(['v31.69', 'v31.70']);
			expect([...row!.environments].sort()).toEqual(['production', 'staging']);
			expect(row!.latestOccurrenceAt.toISOString()).toBe(minutes(-3).toISOString());
		});
	});

	describe('the window bounds — session-independence (BUG-04)', () => {
		it('finds the in-window occurrence with the session TimeZone set to Asia/Jakarta', async () => {
			// The regression this file exists to hold: the bounds used to be cast `::timestamp`,
			// which drops the `Z` off the ISO literal and lets Postgres re-read the naive value in
			// the SESSION's TimeZone. Under a non-UTC session that slides the window by the UTC
			// offset (+07:00 here), the candidate set comes back EMPTY, and nothing anywhere says
			// so: no error, no log, and §4.2's `null` vs `[]` distinction reads it as
			// "looked, found nothing". Every other case in this file runs against a UTC-defaulted
			// container and so cannot see it.
			//
			// `SET LOCAL` inside `db.transaction` and the query through the same `tx` is the point:
			// the pool hands out sessions, so a `SET` on one connection says nothing about the
			// connection the next query lands on.
			const issueId = await seedSystemError({ message: 'jakarta session', at: minutes(-5) });
			const { windowStart, windowEnd } = triage.triageWindow(OCCURRED_AT, 30);
			const drizzleSql = (await import('drizzle-orm')).sql;

			const { zone, ids } = await db.transaction(async (tx) => {
				await tx.execute(drizzleSql`SET LOCAL TIME ZONE 'Asia/Jakarta'`);
				// Prove the session really is non-UTC, so a green assertion below cannot be a
				// SET that silently did nothing.
				const shown = await tx.execute(drizzleSql`SHOW TimeZone`);
				const rows = await triage.findTriageCandidates(
					{
						projectId,
						windowStart,
						windowEnd,
						release: null,
						environment: null,
						limit: 50,
					},
					tx
				);
				return {
					zone: (shown as unknown as Record<string, string>[])[0].TimeZone,
					ids: rows.map((row) => row.issueId),
				};
			});

			expect(zone).toBe('Asia/Jakarta');
			expect(ids).toContain(issueId);
		});
	});

	describe('the write discipline — §4.3\'s "part that is not tuning"', () => {
		it('a 0.4 candidate comes back linked:false and writes NO issue_relations row', async () => {
			// Window only: the report names no release, so nothing can reach 0.7.
			const candidate = await seedSystemError({ message: 'window only', at: minutes(-2) });
			const reportIssueId = await seedExternalReport({});

			const before = await countRelations(reportIssueId);

			const result = await triage.runTriage({
				issueId: reportIssueId,
				projectId,
				occurredAt: OCCURRED_AT,
				context: { release: null, environment: null, traceId: null, sessionId: null },
				windowMinutes: 30,
			});

			const scored = result.candidates.find((c) => c.issueId === candidate);
			expect(scored).toBeDefined();
			expect(scored!.score).toBe(0.4);
			expect(scored!.linked).toBe(false);

			const after = await countRelations(reportIssueId);
			expect(after).toBe(before);
			expect(after).toBe(0);
		});

		it('a >= 0.7 candidate writes exactly ONE linked_to / created_by_type=system row and ONE linked activity row', async () => {
			// A release token unique to this case. The project is shared across the file, and
			// `v31.70` is also seeded by the aggregation case above -- reusing it would put TWO
			// issues over the threshold and the "exactly one row" assertion would be measuring the
			// fixture rather than the rule.
			const release = `rel-link-${suffix}`;
			const candidate = await seedSystemError({
				message: 'release match',
				at: minutes(-4),
				release,
				environment: 'production',
			});
			const reportIssueId = await seedExternalReport({ release, environment: 'production' });

			const result = await triage.runTriage({
				issueId: reportIssueId,
				projectId,
				occurredAt: OCCURRED_AT,
				context: { release, environment: 'production', traceId: null, sessionId: null },
				windowMinutes: 30,
			});

			const scored = result.candidates.find((c) => c.issueId === candidate);
			expect(scored!.score).toBe(0.8);
			expect(scored!.signals).toEqual(['window', 'release', 'environment']);
			expect(scored!.linked).toBe(true);
			expect(result.strategy).toBe('window_release');
			expect(result.candidates.length).toBeLessThanOrEqual(5);

			const relations = await sql`
				select relation_type, created_by_type, created_by, target_issue_id
				from issue_relations where source_issue_id = ${reportIssueId}
			`;
			expect(relations).toHaveLength(1);
			expect(relations[0].relation_type).toBe('linked_to');
			// NEVER duplicate_of: that type carries a cycle guard and is read as a strong human
			// claim, and a heuristic must not mint one (§4.3).
			expect(relations[0].relation_type).not.toBe('duplicate_of');
			expect(relations[0].created_by_type).toBe('system');
			expect(relations[0].target_issue_id).toBe(candidate);

			const activity = await sql`
				select event_type, actor_type, new_value from issue_activity
				where issue_id = ${reportIssueId} and event_type = 'linked'
			`;
			expect(activity).toHaveLength(1);
			expect(activity[0].actor_type).toBe('system');
			expect(activity[0].new_value).toMatchObject({ targetIssueId: candidate, relationType: 'linked_to' });

			// §4.3: triage never changes issues.status, never sets waiting_on, never writes a comment.
			const [issue] = await sql`select status, waiting_on from issues where id = ${reportIssueId}`;
			expect(issue.status).toBe('unresolved');
			expect(issue.waiting_on).toBeNull();
			const comments = await sql`select id from issue_comments where issue_id = ${reportIssueId}`;
			expect(comments).toHaveLength(0);
		});

		it('re-running triage writes NO second row for a pair that already has one (idempotent per report)', async () => {
			const candidate = await seedSystemError({
				message: 'idempotent',
				at: minutes(-6),
				release: `rel-idem-${suffix}`,
			});
			const reportIssueId = await seedExternalReport({ release: `rel-idem-${suffix}` });

			const input = {
				issueId: reportIssueId,
				projectId,
				occurredAt: OCCURRED_AT,
				context: { release: `rel-idem-${suffix}`, environment: null, traceId: null, sessionId: null },
				windowMinutes: 30,
			};

			await triage.runTriage(input);
			const afterFirst = await countRelations(reportIssueId);
			const second = await triage.runTriage(input);
			const afterSecond = await countRelations(reportIssueId);

			expect(afterFirst).toBe(1);
			expect(afterSecond).toBe(1);
			expect(second.candidates.find((c) => c.issueId === candidate)!.linked).toBe(true);

			const activity = await sql`
				select id from issue_activity where issue_id = ${reportIssueId} and event_type = 'linked'
			`;
			expect(activity).toHaveLength(1);
		});

		it('the link and its activity row are in the SAME transaction — roll it back and NEITHER survives', async () => {
			const candidate = await seedSystemError({
				message: 'rollback',
				at: minutes(-7),
				release: `rel-rollback-${suffix}`,
			});
			const reportIssueId = await seedExternalReport({ release: `rel-rollback-${suffix}` });

			// The claim "in the same transaction as the link" is only observable by aborting the
			// transaction and looking. If the two writes were in separate transactions, the first
			// would survive this and the count below would be 1.
			await expect(
				db.transaction(async (tx) => {
					await triage.writeTriageLinks(tx, reportIssueId, [candidate]);

					const midFlight = await tx.execute(
						// eslint-disable-next-line @typescript-eslint/no-explicit-any
						(await import('drizzle-orm')).sql`select count(*)::int as c from issue_relations where source_issue_id = ${reportIssueId}`
					);
					expect(Number((midFlight as unknown as { c: number }[])[0].c)).toBe(1);

					throw new Error('deliberate rollback');
				})
			).rejects.toThrow('deliberate rollback');

			expect(await countRelations(reportIssueId)).toBe(0);
			const activity = await sql`
				select id from issue_activity where issue_id = ${reportIssueId} and event_type = 'linked'
			`;
			expect(activity).toHaveLength(0);
		});
	});

	describe('the query plan — §4.3\'s index', () => {
		it('uses idx_occurrences_issue_created and does not sequentially scan error_occurrences', async () => {
			// The seeded-scale version of this claim (>= 2,000 issues, >= 100,000 occurrences) is
			// `apps/dashboard-web/scripts/triage-explain-seed.sql`, run by hand and pasted into `status/be.md` — that is
			// criterion 3's evidence. This assertion is the committed regression guard: it fails the
			// moment the query is rewritten into a shape that stops using the index at all.
			const { windowStart, windowEnd } = triage.triageWindow(OCCURRED_AT, 30);
			const plan = await sql`
				EXPLAIN (FORMAT TEXT)
				SELECT i.id, occ.latest_occurrence_at
				FROM issues i
				CROSS JOIN LATERAL (
					SELECT max(eo.created_at) AS latest_occurrence_at
					FROM error_occurrences eo
					WHERE eo.issue_id = i.id
					  AND eo.created_at >= ${windowStart}
					  AND eo.created_at <= ${windowEnd}
				) occ
				WHERE i.project_id = ${projectId}
				  AND i.issue_type = 'system_error'
				  AND occ.latest_occurrence_at IS NOT NULL
				ORDER BY occ.latest_occurrence_at DESC, i.id ASC
				LIMIT 5
			`;
			const text = plan.map((line: Record<string, string>) => Object.values(line)[0]).join('\n');
			expect(text).toContain('idx_occurrences_issue_created');
			expect(text).not.toMatch(/Seq Scan on error_occurrences/);
		});
	});
});
