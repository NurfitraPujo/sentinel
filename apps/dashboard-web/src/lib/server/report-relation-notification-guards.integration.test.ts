import { describe, it, expect, beforeAll, afterAll, afterEach } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

/**
 * sentinel-project-environments — contract §11's three remaining §2.2 read boundaries, the ones the
 * issue-read guard does not reach, against a REAL migrated Postgres:
 *
 *   - `lib/db/queries/reports.ts: listReports` (decision 36) — **a separate boundary from
 *     `requireIssueAccessAnyType`'s, and that is the whole point.** The route guards at the
 *     ORGANIZATION level and hands this function `accessibleProjectIds = null` for any role above
 *     `viewer`, so nothing in the existing report tests can observe an environment predicate.
 *   - `lib/db/queries/issues.ts: getIssueRelations` (decision 37) — the one recorded exception to
 *     decision 20: an out-of-scope relation is a REDACTED PLACEHOLDER, not a `404` and not an
 *     omission. Both branches, because `outgoing` and `incoming` join on different columns.
 *   - `lib/db/queries/notifications.ts: listNotifications` (decision 25) — and that
 *     `getNotificationCount` / `getUnreadNotificationCount` carry the same predicate, because a
 *     badge that counts what the list refuses to show is the §4.4 count leak in the one place a
 *     user looks every day.
 *
 * **Why a real database.** Every claim here is a claim about a query's SQL or about rows that came
 * back from one. A chained Drizzle double cannot tell "the predicate was composed into the WHERE
 * clause" from "the predicate object was constructed and dropped" (B10 addendum).
 *
 * **Every guard is asserted against an UNRESTRICTED member on the same fixture**, in the same test
 * or the one beside it. Three of these criteria are absences, and an absence over an empty
 * population passes vacuously: "no production rows" is true when there are no rows at all.
 *
 * Point DATABASE_URL at a DISPOSABLE database — never the shared dev one (CLAUDE.md):
 *
 *   docker run -d --name sentinel-task04-pg -p 15454:5432 \
 *     -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme -e POSTGRES_DB=sentinel postgres:15-alpine
 *   cd packages/db-migrations && GOWORK=off \
 *     DB_URL_DASHBOARD=postgres://sentinel:changeme@localhost:15454/sentinel?sslmode=disable \
 *     go run ./cmd/migrate up -target dashboard
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:changeme@localhost:15454/sentinel \
 *     REPORT_GUARDS_INTEGRATION_REQUIRED=1 \
 *     pnpm vitest run src/lib/server/report-relation-notification-guards.integration.test.ts
 *
 * Skipped when nothing answers, so a machine with no Postgres is not broken by it. Set
 * REPORT_GUARDS_INTEGRATION_REQUIRED=1 to make "no reachable database" a hard failure, matching
 * READ_GUARDS_INTEGRATION_REQUIRED and its neighbours.
 */
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.REPORT_GUARDS_INTEGRATION_REQUIRED === '1';

let dbReachable = false;
const probeSql = postgres(connectionString, { connect_timeout: 3, max: 1 });
try {
	await probeSql`select 1 from project_environment_grants limit 1`;
	dbReachable = true;
} catch {
	dbReachable = false;
} finally {
	await probeSql.end({ timeout: 1 }).catch(() => {});
}

if (required && !dbReachable) {
	throw new Error(
		`REPORT_GUARDS_INTEGRATION_REQUIRED=1 but no migrated database answered at ${connectionString}.`
	);
}

const suffix = randomUUID().slice(0, 8);

const orgId = randomUUID();
/** Narrowed for `narrowedUser`, unrestricted for everyone else. */
const projectNarrowed = randomUUID();
/** Unrestricted for everybody — the per-project half of the assertion lives on this one. */
const projectOpen = randomUUID();

/** An org `engineer` with NO `project_members` row (the ordinary case, §2.2), `{staging}` in A. */
const narrowedUser = randomUUID();
/** The same shape with no grant row anywhere. */
const openUser = randomUUID();

const reportNarrowedProd = randomUUID();
const reportNarrowedStaging = randomUUID();
const reportOpenProd = randomUUID();

/** The relations fixture: a staging issue and its production twin, in the NARROWED project. */
const issueStaging = randomUUID();
const issueProdTwin = randomUUID();
/** A third issue in the UNRESTRICTED project, to prove the relations filter is per project too. */
const issueOpenProd = randomUUID();

const REPORTS = [
	[reportNarrowedProd, projectNarrowed, 'production'],
	[reportNarrowedStaging, projectNarrowed, 'staging'],
	[reportOpenProd, projectOpen, 'production'],
] as const;

describe.skipIf(!dbReachable)(
	'the §2.2 report, relation and notification guards (integration, real Postgres)',
	() => {
		let sql: ReturnType<typeof postgres>;

		beforeAll(async () => {
			sql = postgres(connectionString, { connect_timeout: 5, max: 1 });

			await sql`
				insert into organizations (id, name, slug)
				values (${orgId}, ${`rnq-${suffix}`}, ${`rnq-${suffix}`})
			`;
			for (const [id, label] of [
				[projectNarrowed, 'narrowed'],
				[projectOpen, 'open'],
			] as const) {
				await sql`
					insert into projects (id, name, organization_id, api_key, api_key_hash)
					values (${id}, ${`rnq-${label}-${suffix}`}, ${orgId},
					        ${`k_${label}_${suffix}`}, ${`h_${label}_${suffix}`})
				`;
			}
			for (const [id, label] of [
				[narrowedUser, 'narrowed'],
				[openUser, 'open'],
			] as const) {
				await sql`
					insert into "user" (id, name, email)
					values (${id}, ${label}, ${`rnq-${label}-${suffix}@example.test`})
				`;
				await sql`
					insert into organization_members (organization_id, user_id, role)
					values (${orgId}, ${id}, ${'engineer'})
				`;
			}

			// The ONE narrowing in this fixture. Project B is left alone deliberately: a
			// per-organization implementation of any guard below passes the narrow case and fails
			// this one.
			await sql`
				insert into project_environment_grants (user_id, project_id, environments, created_by)
				values (${narrowedUser}, ${projectNarrowed}, ${sql.array(['staging'])}, ${openUser})
			`;

			// Three user reports, each with the `manual_issue_reports` companion `listReports`
			// requires -- an issue with neither companion is invisible to it for reasons that have
			// nothing to do with this guard.
			for (const [id, projectId, environment] of REPORTS) {
				await sql`
					insert into issues (id, project_id, environment, fingerprint, message, error_class,
					                    issue_type, source_channel)
					values (${id}, ${projectId}, ${environment}, ${`fp-${id}`}, ${`report ${environment} ${suffix}`},
					        ${'UserReport'}, ${'user_report'}, ${'manual_support'})
				`;
				await sql`
					insert into manual_issue_reports (issue_id, reporter_id, body_md, severity)
					values (${id}, ${openUser}, ${'it broke'}, ${'high'})
				`;
			}

			// The relations fixture.
			for (const [id, projectId, environment] of [
				[issueStaging, projectNarrowed, 'staging'],
				[issueProdTwin, projectNarrowed, 'production'],
				[issueOpenProd, projectOpen, 'production'],
			] as const) {
				await sql`
					insert into issues (id, project_id, environment, fingerprint, message, error_class)
					values (${id}, ${projectId}, ${environment}, ${`fp-${id}`}, ${`relation seed ${suffix}`},
					        ${'RelationSeedError'})
				`;
			}
		});

		afterAll(async () => {
			if (!sql) return;
			try {
				await sql`delete from issues where project_id in (${projectNarrowed}, ${projectOpen})`;
				await sql`delete from project_environment_grants where user_id in (${narrowedUser}, ${openUser})`;
				await sql`delete from organization_members where organization_id = ${orgId}`;
				await sql`delete from projects where organization_id = ${orgId}`;
				await sql`delete from "user" where id in (${narrowedUser}, ${openUser})`;
				await sql`delete from organizations where id = ${orgId}`;
			} finally {
				await sql.end({ timeout: 5 }).catch(() => {});
			}
		});

		// -----------------------------------------------------------------------------------------
		// listReports — decision 36
		// -----------------------------------------------------------------------------------------

		describe('listReports (decision 36) — the ORG-level route never reaches requireIssueAccessAnyType', () => {
			async function reportsFor(userId: string) {
				const { listReports } = await import('$lib/db/queries/reports');
				// EXACTLY what the route passes for any role above `viewer`: the whole organization.
				// The guard has to be the function's own, because there is no project narrowing here.
				const rows = await listReports({
					organizationId: orgId,
					tab: 'all',
					userId,
					accessibleProjectIds: null,
				});
				return rows.map((r) => r.issue.id).sort();
			}

			it('an unrestricted member in the same organization gets them ALL', async () => {
				// Asserted FIRST: the guard below is an absence, and an absence over an empty list is
				// true for the wrong reason.
				expect(await reportsFor(openUser)).toEqual(
					[reportNarrowedProd, reportNarrowedStaging, reportOpenProd].sort()
				);
			});

			it('a member granted only {staging} gets NO production report from that project', async () => {
				const rows = await reportsFor(narrowedUser);

				expect(rows).not.toContain(reportNarrowedProd);
				expect(rows).toContain(reportNarrowedStaging);
			});

			it('and the scope is PER PROJECT — project B’s production reports still arrive', async () => {
				// A single-scope implementation passes the test above and fails this one.
				const rows = await reportsFor(narrowedUser);

				expect(rows).toEqual([reportNarrowedStaging, reportOpenProd].sort());
			});
		});

		// -----------------------------------------------------------------------------------------
		// getIssueRelations — decision 37
		// -----------------------------------------------------------------------------------------

		describe('getIssueRelations (decision 37) — the redacted placeholder, and BOTH branches', () => {
			async function relate(sourceIssueId: string, targetIssueId: string) {
				const id = randomUUID();
				await sql`
					insert into issue_relations (id, source_issue_id, target_issue_id, relation_type,
					                             created_by_type, created_by)
					values (${id}, ${sourceIssueId}, ${targetIssueId}, ${'duplicate_of'}, ${'user'}, ${openUser})
				`;
				return id;
			}

			async function relationsFor(userId: string | null, issueId: string) {
				const { getIssueRelations } = await import('$lib/db/queries/issues');
				const { resolveEnvironmentScopes } = await import('./environment-access');
				// `null` is the FOURTH caller's shape -- `routes/api/agent/issues/[issueId=uuid]/+server.ts`
				// is key-authenticated with no user and no grant to resolve (*Known limits* 2), so it
				// passes no scopes at all.
				return userId === null
					? await getIssueRelations(issueId)
					: await getIssueRelations(issueId, await resolveEnvironmentScopes(userId));
			}

			// Relations are seeded per test, so each one starts from an empty graph.
			afterEach(async () => {
				await sql`delete from issue_relations where source_issue_id in (${issueStaging}, ${issueProdTwin}, ${issueOpenProd})`;
			});

			it('OUTGOING: an out-of-scope related issue is a placeholder carrying no identifying field', async () => {
				await relate(issueStaging, issueProdTwin);

				const [row] = await relationsFor(narrowedUser, issueStaging);

				expect(row.direction).toBe('outgoing');
				// Present, not dropped -- the whole point of decision 37.
				expect(row.relatedIssue).toEqual({ redacted: true });
				expect(row.relatedIssue).not.toHaveProperty('id');
				expect(row.relatedIssue).not.toHaveProperty('message');
				expect(row.relatedIssue).not.toHaveProperty('errorClass');
				expect(row.relatedIssue).not.toHaveProperty('status');
				expect(row.relatedIssue).not.toHaveProperty('fingerprint');
				// The related issue's id is ALSO the relation's `target_issue_id` on this branch, so
				// redacting `relatedIssue.id` alone would hand it straight back under another key.
				expect(row.targetIssueId).toBeNull();
				expect(row.sourceIssueId).toBe(issueStaging);
			});

			it('INCOMING: the same, on the branch that joins the other column', async () => {
				// `outgoing` joins on `targetIssueId` and `incoming` on `sourceIssueId`; filtering one
				// and not the other is the natural half-fix and is invisible from the page unless the
				// relation happens to run the other way.
				await relate(issueProdTwin, issueStaging);

				const [row] = await relationsFor(narrowedUser, issueStaging);

				expect(row.direction).toBe('incoming');
				expect(row.relatedIssue).toEqual({ redacted: true });
				expect(row.sourceIssueId).toBeNull();
				expect(row.targetIssueId).toBe(issueStaging);
			});

			it('an IN-SCOPE relation beside it is returned whole', async () => {
				// Both directions on one page: one redacted, one entire.
				await relate(issueStaging, issueProdTwin);
				await relate(issueStaging, issueOpenProd);

				const rows = await relationsFor(narrowedUser, issueStaging);
				const visible = rows.filter((r) => r.relatedIssue.redacted === false);

				expect(rows).toHaveLength(2);
				expect(visible).toHaveLength(1);
				// PER PROJECT: `issueOpenProd` is `production` too, and it arrives whole because the
				// narrowing is on project A only.
				expect(visible[0].relatedIssue).toMatchObject({
					redacted: false,
					id: issueOpenProd,
					errorClass: 'RelationSeedError',
					status: 'unresolved',
				});
			});

			it('an UNRESTRICTED viewer sees every relation whole, exactly as today', async () => {
				// The regression that matters most: this is every viewer on the day this ships.
				await relate(issueStaging, issueProdTwin);
				await relate(issueProdTwin, issueStaging);

				const rows = await relationsFor(openUser, issueStaging);

				expect(rows).toHaveLength(2);
				for (const row of rows) {
					expect(row.relatedIssue).toMatchObject({ redacted: false, id: issueProdTwin });
					expect(row.sourceIssueId).not.toBeNull();
					expect(row.targetIssueId).not.toBeNull();
				}
			});

			it('the agent caller, which passes no scopes at all, is unrestricted', async () => {
				// Its route file is unchanged: key auth, no user, no grant to resolve.
				await relate(issueStaging, issueProdTwin);

				const [row] = await relationsFor(null, issueStaging);

				expect(row.relatedIssue).toMatchObject({ redacted: false, id: issueProdTwin });
			});
		});

		// -----------------------------------------------------------------------------------------
		// listNotifications and BOTH count helpers — decision 25
		// -----------------------------------------------------------------------------------------

		describe('listNotifications and its two count helpers (decision 25)', () => {
			beforeAll(async () => {
				// One notification per (user, report) for both members, over all three reports: the
				// list and the badge must narrow, and they must narrow the SAME way.
				for (const userId of [narrowedUser, openUser]) {
					for (const [issueId] of REPORTS) {
						await sql`
							insert into notifications (id, user_id, issue_id, kind, actor_type, actor_id)
							values (${randomUUID()}, ${userId}, ${issueId}, ${'commented'}, ${'system'}, ${'system'})
						`;
					}
				}
			});

			it('an unrestricted member sees all three, and both counts agree', async () => {
				const { listNotifications, getNotificationCount, getUnreadNotificationCount } =
					await import('$lib/db/queries/notifications');

				const rows = await listNotifications({ userId: openUser, limit: 100 });

				expect(rows.map((r) => r.issueId).sort()).toEqual(REPORTS.map(([id]) => id).sort());
				expect(await getNotificationCount(openUser)).toBe(3);
				expect(await getUnreadNotificationCount(openUser)).toBe(3);
			});

			it('a member granted only {staging} sees no production notification, per project', async () => {
				const { listNotifications } = await import('$lib/db/queries/notifications');

				const rows = await listNotifications({ userId: narrowedUser, limit: 100 });

				expect(rows.map((r) => r.issueId).sort()).toEqual(
					[reportNarrowedStaging, reportOpenProd].sort()
				);
			});

			it('and the BADGE agrees with the list rather than counting what it refuses to show', async () => {
				const { listNotifications, getNotificationCount, getUnreadNotificationCount } =
					await import('$lib/db/queries/notifications');

				const rows = await listNotifications({ userId: narrowedUser, limit: 100 });

				// Both helpers were `count(*) FROM notifications WHERE user_id = $1` with no join at
				// all, so both would still say 3 here.
				expect(await getNotificationCount(narrowedUser)).toBe(rows.length);
				expect(await getUnreadNotificationCount(narrowedUser)).toBe(rows.length);
				expect(await getNotificationCount(narrowedUser)).toBe(2);
			});
		});
	}
);
