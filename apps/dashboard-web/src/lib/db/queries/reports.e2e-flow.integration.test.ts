import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

// M1 end-to-end proof (docs/plans/MANUAL_ISSUES_DESIGN.md phase M1;
// CLAUDE.md's "Final M1 stage — GATES + REAL END-TO-END PROOF"): create -> list -> claim ->
// second-claim-conflicts -> move -> activity, driven through the SAME query-layer functions the
// session-authenticated API routes call (src/routes/[orgSlug]/reports/*, api/issues/[issueId]/{claim,move}),
// executed against a REAL, freshly migrated Postgres. Auth.js sessions make raw HTTP hard to drive from a
// standalone script (see CLAUDE.md's routing instructions for this stage), so this is the
// integration-style-vitest-against-the-query-layer route explicitly allowed as the fallback: the query
// layer IS what those routes call after `requireReportAccess`/`requireIssueAccess` pass, so exercising it
// directly under this file's real Postgres connection proves the same sequence a real request would drive.
//
// This must run against a DISPOSABLE Postgres, never the shared dev database (CLAUDE.md, "Never point
// tests/integration at the shared dev database" -- the same rule applies here even though this file lives
// under src/lib, because it is exactly that kind of test). Point DATABASE_URL at one before running, e.g.:
//
//   docker run -d --name sentinel-m1-disposable-pg -p 15432:5432 \
//     -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme -e POSTGRES_DB=sentinel postgres:15-alpine
//   cd packages/db-migrations && DB_URL_DASHBOARD=postgres://sentinel:changeme@localhost:15432/sentinel?sslmode=disable \
//     go run ./cmd/migrate up -target=dashboard
//   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:changeme@localhost:15432/sentinel \
//     pnpm vitest run src/lib/db/queries/reports.e2e-flow.integration.test.ts
//
// If nothing answers on DATABASE_URL the suite is skipped (decided via top-level await, mirroring
// issues.integration.test.ts's D02 guard) rather than failed, so this file does not break a machine with no
// Postgres reachable. Set M1_FLOW_INTEGRATION_REQUIRED=1 to turn "no reachable database" into a hard
// failure, matching SEARCH_INTEGRATION_REQUIRED / SCHEMA_DRIFT_REQUIRED.
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.M1_FLOW_INTEGRATION_REQUIRED === '1';

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
		`M1_FLOW_INTEGRATION_REQUIRED=1 but no database answered at ${connectionString}. ` +
			'This test is the committed, repeatable proof that the M1 manual-issues flow runs end-to-end ' +
			'against a real migrated Postgres; skipping it silently would leave that claim unverified.'
	);
}

// Unique per run so two concurrent runs against one shared disposable database cannot collide.
const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const projectId = randomUUID();
const reporterId = randomUUID();
const claimantAId = randomUUID();
const claimantBId = randomUUID();

describe.skipIf(!dbReachable)('Manual issues M1 flow (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });

		await sql`
			insert into organizations (id, name, slug)
			values (${orgId}, ${'m1-flow-' + suffix}, ${'m1-flow-' + suffix})
		`;
		await sql`
			insert into projects (id, name, organization_id, api_key, api_key_hash)
			values (${projectId}, ${'m1-flow-' + suffix}, ${orgId},
			        ${'k_' + suffix}, ${'h_' + suffix})
		`;
		await sql`
			insert into "user" (id, name, email)
			values (${reporterId}, ${'Reporter ' + suffix}, ${'reporter-' + suffix + '@example.test'})
		`;
		await sql`
			insert into "user" (id, name, email)
			values (${claimantAId}, ${'Claimant A ' + suffix}, ${'claimant-a-' + suffix + '@example.test'})
		`;
		await sql`
			insert into "user" (id, name, email)
			values (${claimantBId}, ${'Claimant B ' + suffix}, ${'claimant-b-' + suffix + '@example.test'})
		`;
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from issue_activity where issue_id in (select id from issues where project_id = ${projectId})`;
			await sql`delete from manual_issue_reports where issue_id in (select id from issues where project_id = ${projectId})`;
			await sql`delete from issues where project_id = ${projectId}`;
			await sql`delete from projects where organization_id = ${orgId}`;
			await sql`delete from "user" where id in (${reporterId}, ${claimantAId}, ${claimantBId})`;
			await sql`delete from organizations where id = ${orgId}`;
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	it('runs create -> list -> claim -> conflict -> move -> activity end-to-end', async () => {
		const {
			createManualIssue,
			listReports,
			claimIssue,
			ClaimConflictError,
			moveIssueToProject,
			getIssueActivity,
		} = await import('./reports');

		// 1. CREATE (triage default: no projectId, per Q12 §2 -- lands in the auto-provisioned
		// per-org Triage inbox project).
		const created = await createManualIssue({
			organizationId: orgId,
			projectId: null,
			reporterId,
			title: `M1 flow report ${suffix}`,
			bodyMd: 'Something is broken, see attached repro steps.',
			severity: 'high',
		});
		expect(created.issue.issueType).toBe('user_report');
		expect(created.report.severity).toBe('high');
		const issueId = created.issue.id;

		const [triageProjectRow] = await sql`
			select id, is_inbox from projects where organization_id = ${orgId} and is_inbox = true
		`;
		expect(triageProjectRow).toBeTruthy();
		expect(created.issue.projectId).toBe(triageProjectRow.id);

		// 2. LIST shows it (both the 'all' and 'triage' tabs).
		const allReports = await listReports({ organizationId: orgId, tab: 'all', userId: reporterId });
		expect(allReports.some((r) => r.issue.id === issueId)).toBe(true);

		const triageReports = await listReports({ organizationId: orgId, tab: 'triage', userId: reporterId });
		expect(triageReports.some((r) => r.issue.id === issueId)).toBe(true);

		const unclaimedBefore = await listReports({
			organizationId: orgId,
			tab: 'unclaimed',
			userId: reporterId,
		});
		expect(unclaimedBefore.some((r) => r.issue.id === issueId)).toBe(true);

		// 3. CLAIM by claimant A succeeds.
		const { issue: claimed } = await claimIssue(issueId, 'user', claimantAId);
		expect(claimed.assignedTo).toBe(claimantAId);
		expect(claimed.assigneeType).toBe('user');

		const claimedByA = await listReports({
			organizationId: orgId,
			tab: 'claimed-by-me',
			userId: claimantAId,
		});
		expect(claimedByA.some((r) => r.issue.id === issueId)).toBe(true);

		// 4. SECOND CLAIM conflicts (409-shaped: ClaimConflictError, atomic `WHERE assigned_to IS
		// NULL` guard per §7/Q6 -- claimant B loses the race).
		await expect(claimIssue(issueId, 'user', claimantBId)).rejects.toBeInstanceOf(
			ClaimConflictError
		);

		// 5. MOVE to a real (non-inbox) project.
		const targetProjectId = randomUUID();
		await sql`
			insert into projects (id, name, organization_id, api_key, api_key_hash)
			values (${targetProjectId}, ${'m1-flow-target-' + suffix}, ${orgId},
			        ${'k2_' + suffix}, ${'h2_' + suffix})
		`;
		const moved = await moveIssueToProject(issueId, targetProjectId, 'user', claimantAId);
		expect(moved.toProjectId).toBe(targetProjectId);
		expect(moved.fromProjectId).toBe(triageProjectRow.id);

		const stillListedAfterMove = await listReports({
			organizationId: orgId,
			tab: 'all',
			userId: reporterId,
		});
		const movedRow = stillListedAfterMove.find((r) => r.issue.id === issueId);
		expect(movedRow).toBeTruthy();
		expect(movedRow?.issue.projectId).toBe(targetProjectId);
		expect(movedRow?.projectIsInbox).toBe(false);

		// 6. ACTIVITY shows created ("report_created", R12), claimed, and moved, in order.
		const activity = await getIssueActivity(issueId);
		const eventTypesInOrder = activity
			.slice()
			.sort((a, b) => new Date(a.createdAt as unknown as string).getTime() - new Date(b.createdAt as unknown as string).getTime())
			.map((a) => a.eventType);

		expect(eventTypesInOrder).toContain('report_created');
		expect(eventTypesInOrder).toContain('claimed');
		expect(eventTypesInOrder).toContain('moved');
		expect(eventTypesInOrder.indexOf('report_created')).toBeLessThan(
			eventTypesInOrder.indexOf('claimed')
		);
		expect(eventTypesInOrder.indexOf('claimed')).toBeLessThan(eventTypesInOrder.indexOf('moved'));

		await sql`delete from projects where id = ${targetProjectId}`;
	});

	/**
	 * sentinel-project-environments §4.8a criterion 3, against the real column.
	 *
	 * This is the half a mocked query layer cannot prove: `issues.environment` is NOT NULL with no
	 * DEFAULT as of §4.9, so a writer that forgot it raises 23502 rather than writing a wrong value,
	 * and only a real migrated Postgres shows the difference.
	 */
	it('files a manual report under `production` by default and under an asserted name verbatim (§4.8a)', async () => {
		const { createManualIssue } = await import('./reports');

		const noAssertion = await createManualIssue({
			organizationId: orgId,
			projectId,
			reporterId,
			title: 'env default ' + suffix,
			bodyMd: 'no environment asserted',
			severity: 'low',
		});

		const [defaultRow] = await sql<{ environment: string }[]>`
			select environment from issues where id = ${noAssertion.issue.id}
		`;
		expect(defaultRow.environment).toBe('production');

		// The registry is EMPTY for this project -- no project_environments row was ever created --
		// and an asserted, unregistered name must still be stored verbatim (decisions 8 and 9). A
		// rule that required a registry row would fail the first report into every new organisation,
		// because findOrCreateTriageProject provisions inbox projects with zero declared
		// environments.
		const registryRows = await sql<{ count: string }[]>`
			select count(*)::text as count from project_environments where project_id = ${projectId}
		`;
		expect(Number(registryRows[0].count)).toBe(0);

		const asserted = await createManualIssue({
			organizationId: orgId,
			projectId,
			reporterId,
			title: 'env asserted ' + suffix,
			bodyMd: 'eu-west-1 asserted',
			severity: 'low',
			environment: 'eu-west-1',
		});

		const [assertedRow] = await sql<{ environment: string }[]>`
			select environment from issues where id = ${asserted.issue.id}
		`;
		expect(assertedRow.environment).toBe('eu-west-1');

		// Still nothing in the registry: only §4.3 creates rows (decision 8). An environment present
		// in the data and absent from the registry is unfilterable until somebody declares it, and
		// that is the accepted cost of an intentional registry, not an accident.
		const registryAfter = await sql<{ count: string }[]>`
			select count(*)::text as count from project_environments where project_id = ${projectId}
		`;
		expect(Number(registryAfter[0].count)).toBe(0);
	});

	it('files an external report under the resolved `context.environment` (§4.8a, writer 3)', async () => {
		const { createExternalIssueReport } = await import('./reports');
		const externalReporterId = randomUUID();
		await sql`
			insert into external_reporters (id, organization_id, project_id, external_id, display_name)
			values (${externalReporterId}, ${orgId}, ${projectId}, ${'ext-' + suffix}, ${'Sarah K.'})
		`;

		const cases: [Record<string, unknown>, string][] = [
			[{}, 'production'],
			[{ environment: 'staging' }, 'staging'],
			// Stored verbatim on the companion row, resolved on the issue. This route gains no new
			// error case at all -- a shipped route that starts 400-ing a body it accepted yesterday
			// is a breaking change (§6 records it as additive).
			[{ environment: 'Production' }, 'production'],
			[{ environment: '' }, 'production'],
		];

		for (const [context, expected] of cases) {
			const created = await createExternalIssueReport({
				organizationId: orgId,
				projectId,
				externalReporterId,
				title: `ext env ${expected} ${randomUUID().slice(0, 6)}`,
				bodyMd: 'the save button spins forever',
				severity: 'high',
				context,
				idempotencyKey: null,
				attachmentIds: [],
			});

			const [row] = await sql<{ environment: string }[]>`
				select environment from issues where id = ${created.issue.id}
			`;
			expect(row.environment, `context ${JSON.stringify(context)}`).toBe(expected);

			// The client's object is kept EXACTLY as sent, including a non-matching "Production".
			const [companion] = await sql<{ context: Record<string, unknown> }[]>`
				select context from external_issue_reports where issue_id = ${created.issue.id}
			`;
			expect(companion.context).toEqual(context);
		}

		await sql`
			delete from external_issue_reports
			where issue_id in (select id from issues where project_id = ${projectId})
		`;
		await sql`delete from external_reporters where id = ${externalReporterId}`;
	});
});
