import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

/**
 * sentinel-user-report-issue §4.2 / §11 — `GET /api/organizations/{orgId}/reports/{issueId}`,
 * driven through the route handler against a REAL migrated Postgres.
 *
 * §11 names this boundary and says exactly what it is for: "The `triage` object's nullability, its
 * empty-vs-absent distinction, its bound, and — the point of §4.2's correction — that an
 * externally-sourced report is returned **at all**."
 *
 * The correction is a QUERY change, not a field addition. `getReportDetail` reached a report
 * through `.innerJoin(manual_issue_reports, …)` and `.leftJoin("user", …)`; an externally
 * submitted report has neither row (Decisions taken, OQ-6), so the query returned `null`, the
 * route's `loadReportOrNotFound` guard threw, and every report this feature creates 404'd. A mock
 * cannot show that: the assertion is that a real row comes back from a real join, so this file
 * exists (B10 addendum).
 *
 * Disposable Postgres only, never the shared dev database (CLAUDE.md). See
 * `src/lib/server/triage.integration.test.ts`'s header for the three commands.
 * `REPORT_DETAIL_INTEGRATION_REQUIRED=1` turns the skip into a hard failure.
 */
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.REPORT_DETAIL_INTEGRATION_REQUIRED === '1';

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
		`REPORT_DETAIL_INTEGRATION_REQUIRED=1 but no migrated database was EXPLICITLY targeted at ${connectionString} (set DATABASE_URL). ` +
			'This test is the committed proof that §4.2 returns an externally-sourced report at all, ' +
			'and that its three triage states are distinguishable; skipping it silently would leave ' +
			'the whole point of §4.2 unverified.'
	);
}

const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const otherOrgId = randomUUID();
const projectId = randomUUID();
const otherProjectId = randomUUID();
const externalReporterId = randomUUID();
const memberId = `usr_member_${suffix}`;

// RELATIVE to now, never a fixed date: `cleanupRetainedData` (default 30 days) deletes every occurrence
// older than its cutoff -- and then the issues left without one -- across ALL projects, and the retention
// integration files run it concurrently with this one. A hard-coded date silently aged past that cutoff
// and the seeded candidates started disappearing mid-test. A day back keeps it well inside the window.
const OCCURRED_AT = new Date(Math.floor((Date.now() - 24 * 60 * 60_000) / 1000) * 1000);
const minutes = (n: number) => new Date(OCCURRED_AT.getTime() + n * 60_000);

let sql: ReturnType<typeof postgres>;
let GET: (event: unknown) => Promise<Response>;
let listReports: typeof import('$lib/db/queries/reports').listReports;
let getReporterId: typeof import('$lib/server/report-access').getReporterId;
let getAgentReportDetail: typeof import('$lib/db/queries/agent-reads').getAgentReportDetail;
let listAgentIssues: typeof import('$lib/db/queries/agent-work').listAgentIssues;
let getIssueReporterId: typeof import('$lib/server/agent-issue-scope').getIssueReporterId;
let cleanupRetainedData: typeof import('$lib/server/retention').cleanupRetainedData;

/** Every captured §4.2 response, for `status/be.md`'s "three captured responses". */
const captured: Array<{ label: string; status: number; body: unknown }> = [];

async function callGet(issueId: string, label?: string, asOrg = orgId) {
	const res = await GET({
		params: { orgId: asOrg, issueId },
		locals: { auth: async () => ({ user: { id: memberId } }) },
	});
	const body = await res.clone().json();
	if (label) captured.push({ label, status: res.status, body });
	return { status: res.status, body: body as Record<string, unknown> };
}

async function seedExternalReport(context: Record<string, string>, firstSeen?: Date): Promise<string> {
	const issueId = randomUUID();
	await sql`
		insert into issues (id, project_id, environment, fingerprint, message, error_class, issue_type, source_channel, first_seen)
		values (${issueId}, ${projectId}, ${'production'}, ${'fp-' + issueId}, ${'Visits — the save button spins forever'},
		        ${'UserReport'}, ${'user_report'}, ${'user_report_api'},
		        ${firstSeen ?? new Date()})
	`;
	await sql`
		insert into external_issue_reports (issue_id, external_reporter_id, body_md, severity, context)
		values (${issueId}, ${externalReporterId}, ${'the save button spins forever'}, ${'high'},
		        ${sql.json(context)})
	`;
	return issueId;
}

async function seedMemberReport(): Promise<string> {
	const issueId = randomUUID();
	await sql`
		insert into issues (id, project_id, environment, fingerprint, message, error_class, issue_type, source_channel)
		values (${issueId}, ${projectId}, ${'production'}, ${'fp-' + issueId}, ${'A member-filed report'},
		        ${'ManualReport'}, ${'user_report'}, ${'manual_support'})
	`;
	await sql`
		insert into manual_issue_reports (issue_id, reporter_id, body_md, severity)
		values (${issueId}, ${memberId}, ${'filed from the dashboard'}, ${'medium'})
	`;
	return issueId;
}

async function seedSystemError(release: string | null, at: Date): Promise<string> {
	const issueId = randomUUID();
	await sql`
		insert into issues (id, project_id, environment, fingerprint, message, error_class, issue_type)
		values (${issueId}, ${projectId}, ${'production'}, ${'fp-' + issueId}, ${'pq: deadlock detected'},
		        ${'PostgresError'}, ${'system_error'})
	`;
	await sql`
		insert into error_occurrences (id, issue_id, environment, platform, release_version, created_at)
		values (${randomUUID()}, ${issueId}, ${'production'}, ${'go'}, ${release}, ${at})
	`;
	return issueId;
}

describe.skipIf(!(dbReachable && explicitlyTargeted))('GET /api/organizations/[orgId]/reports/[issueId] (§4.2, real Postgres)', () => {
	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 4 });

		for (const [id, label] of [
			[orgId, 'det'],
			[otherOrgId, 'oth'],
		] as const) {
			await sql`insert into organizations (id, name, slug)
			          values (${id}, ${`${label}-${suffix}`}, ${`${label}-${suffix}`})`;
		}
		for (const [id, org, label] of [
			[projectId, orgId, 'p'],
			[otherProjectId, otherOrgId, 'q'],
		] as const) {
			await sql`
				insert into projects (id, name, organization_id, api_key, api_key_hash)
				values (${id}, ${`${label}-${suffix}`}, ${org}, ${`pk_${label}_${suffix}`}, ${`h_${label}_${suffix}`})
			`;
		}
		await sql`insert into "user" (id, name, email) values (${memberId}, ${'Member'}, ${`m-${suffix}@example.test`})`;
		await sql`insert into organization_members (organization_id, user_id, role) values (${orgId}, ${memberId}, ${'engineer'})`;
		await sql`
			insert into external_reporters (id, organization_id, project_id, external_id, display_name, email, first_seen_at)
			values (${externalReporterId}, ${orgId}, ${projectId}, ${`u_${suffix}`}, ${'Sarah K.'}, ${null},
			        ${new Date('2026-08-19T04:40:11.000Z')})
		`;

		({ GET } = (await import('./+server')) as unknown as { GET: (event: unknown) => Promise<Response> });
		({ listReports } = await import('$lib/db/queries/reports'));
		({ getReporterId } = await import('$lib/server/report-access'));
		({ getAgentReportDetail } = await import('$lib/db/queries/agent-reads'));
		({ listAgentIssues } = await import('$lib/db/queries/agent-work'));
		({ getIssueReporterId } = await import('$lib/server/agent-issue-scope'));
		({ cleanupRetainedData } = await import('$lib/server/retention'));
	});

	afterAll(async () => {
		if (!sql) return;
		await sql`
			delete from external_issue_reports
			where issue_id in (select i.id from issues i join projects p on p.id = i.project_id
			                   where p.organization_id in (${orgId}, ${otherOrgId}))
		`;
		await sql`delete from organizations where id in (${orgId}, ${otherOrgId})`;
		await sql`delete from "user" where id = ${memberId}`;
		await sql.end({ timeout: 5 });

		// eslint-disable-next-line no-console
		console.log('\n=== captured §4.2 responses ===\n' + JSON.stringify(captured, null, 2));
	});

	describe('criterion 1 — an externally-sourced report is returned AT ALL', () => {
		it('returns sourceChannel, reporter.kind=external, context and attachments', async () => {
			const issueId = await seedExternalReport({
				page: 'Visits',
				release: 'v31.70',
				brand: 'wayist',
				environment: 'production',
				occurred_at: OCCURRED_AT.toISOString(),
				// An unknown key: §4.1 keeps them verbatim, so §4.2 must surface them verbatim too.
				captured_by: 'sdk-0.4.1',
			});
			const attachmentId = randomUUID();
			await sql`
				insert into attachments (id, org_id, issue_id, uploader_type, uploader_id, filename, content_type, size_bytes, storage_key, status)
				values (${attachmentId}, ${orgId}, ${issueId}, ${'external_reporter'}, ${externalReporterId},
				        ${'screen.png'}, ${'image/png'}, ${184213}, ${'att/' + attachmentId}, ${'ready'})
			`;

			const res = await callGet(issueId, '§4.2 an externally-sourced report is returned at all');

			// The whole point: 200, not the 404 the innerJoin produced.
			expect(res.status).toBe(200);
			expect(res.body.sourceChannel).toBe('user_report_api');
			expect(res.body.reporter).toEqual({
				kind: 'external',
				displayName: 'Sarah K.',
				email: null,
				externalId: `u_${suffix}`,
				firstSeenAt: '2026-08-19T04:40:11.000Z',
			});
			expect(res.body.context).toEqual({
				page: 'Visits',
				release: 'v31.70',
				brand: 'wayist',
				environment: 'production',
				occurred_at: OCCURRED_AT.toISOString(),
				captured_by: 'sdk-0.4.1',
			});
			expect(res.body.attachments).toEqual([
				{ id: attachmentId, filename: 'screen.png', contentType: 'image/png', sizeBytes: 184213 },
			]);
		});

		it('the three triage states are distinguishable: null, {candidates: []}, and a bounded populated array', async () => {
			// (a) null — an org-member report has no context, so there is nothing to match on and
			// nothing was looked at. Same shape a report created before this feature shipped gets.
			const memberIssueId = await seedMemberReport();
			const nullCase = await callGet(memberIssueId, '§4.2 triage: null (org-member report)');
			expect(nullCase.status).toBe(200);
			expect(nullCase.body.triage).toBeNull();

			// (b) {candidates: []} — looked, found nothing. A window with no system_error occurrence
			// in it. NOT the same as (a), and a client must render them differently (§4.2).
			const emptyIssueId = await seedExternalReport({
				occurred_at: new Date('2019-01-01T00:00:00.000Z').toISOString(),
			});
			const emptyCase = await callGet(emptyIssueId, '§4.2 triage: {candidates: []}');
			expect(emptyCase.body.triage).not.toBeNull();
			expect(emptyCase.body.triage).toMatchObject({ strategy: 'window_release', candidates: [] });
			expect((emptyCase.body.triage as { ranAt: string }).ranAt).toMatch(/^\d{4}-\d{2}-\d{2}T/);

			// (c) a populated, BOUNDED array. Seven candidates in the window; §4.3 caps the array at 5.
			for (let index = 0; index < 7; index += 1) {
				await seedSystemError(null, minutes(-index - 1));
			}
			const populatedIssueId = await seedExternalReport({ occurred_at: OCCURRED_AT.toISOString() });
			const populated = await callGet(populatedIssueId, '§4.2 triage: populated (bounded at 5)');
			const triage = populated.body.triage as { candidates: unknown[]; strategy: string };
			expect(triage.candidates.length).toBe(5);
			expect(triage.strategy).toBe('window_release');
			for (const candidate of triage.candidates as Record<string, unknown>[]) {
				expect(Object.keys(candidate).sort()).toEqual(
					['errorClass', 'issueId', 'linked', 'message', 'score', 'signals'].sort()
				);
				// Window only (the report names no release): a suggestion, never a written link.
				expect(candidate.score).toBe(0.4);
				expect(candidate.linked).toBe(false);
			}
		});

		it('404s a report that belongs to another organization — the loadReportOrNotFound guard, unchanged', async () => {
			// A report in the OTHER org's project, asked for under an org this caller IS a member of.
			// The org mismatch must be a 404, never a 403 or a body: the endpoint must not become an
			// oracle for whether another tenant's issue id exists.
			const foreignIssueId = randomUUID();
			await sql`
				insert into issues (id, project_id, environment, fingerprint, message, error_class, issue_type)
				values (${foreignIssueId}, ${otherProjectId}, ${'production'}, ${'fp-' + foreignIssueId}, ${'not yours'},
				        ${'ManualReport'}, ${'user_report'})
			`;
			await sql`
				insert into manual_issue_reports (issue_id, reporter_id, body_md)
				values (${foreignIssueId}, ${memberId}, ${'filed elsewhere'})
			`;

			await expect(callGet(foreignIssueId)).rejects.toMatchObject({ status: 404 });
		});
	});

	describe('criterion 1 — the readers §6 enumerates, and SR-6\'s agent-side four', () => {
		it('listReports returns the external report and filters on ?source=', async () => {
			const externalIssueId = await seedExternalReport({ page: 'Visits' });
			const memberIssueId = await seedMemberReport();

			const all = await listReports({ organizationId: orgId, tab: 'all', userId: memberId });
			const allIds = all.map((row) => row.issue.id);
			expect(allIds).toContain(externalIssueId);
			expect(allIds).toContain(memberIssueId);

			const externalRow = all.find((row) => row.issue.id === externalIssueId)!;
			expect(externalRow.severity).toBe('high');
			expect(externalRow.reporterKind).toBe('external');
			expect(externalRow.reporterDisplayName).toBe('Sarah K.');

			// OQ-8's filter.
			const filtered = await listReports({
				organizationId: orgId,
				tab: 'all',
				userId: memberId,
				source: 'user_report_api',
			});
			const filteredIds = filtered.map((row) => row.issue.id);
			expect(filteredIds).toContain(externalIssueId);
			expect(filteredIds).not.toContain(memberIssueId);
		});

		it('an org-member report\'s existing fields come back identical to b768e5b', async () => {
			const memberIssueId = await seedMemberReport();
			const { getReportDetail } = await import('$lib/db/queries/reports');
			const detail = await getReportDetail(memberIssueId);

			// The nine keys `getReportDetail` returned at b768e5b, with the values it returned then:
			// the companion row present, the "user" join resolved, the org and project resolved.
			// §6: "additive in shape — new nullable objects, no field removed, no type or nullability
			// changed".
			expect(detail).not.toBeNull();
			expect(detail!.issue.id).toBe(memberIssueId);
			expect(detail!.report).toMatchObject({
				issueId: memberIssueId,
				reporterId: memberId,
				bodyMd: 'filed from the dashboard',
				severity: 'medium',
			});
			expect(detail!.projectId).toBe(projectId);
			expect(detail!.projectName).toBe(`p-${suffix}`);
			expect(detail!.projectIsInbox).toBe(false);
			expect(detail!.organizationId).toBe(orgId);
			expect(detail!.reporterName).toBe('Member');
			expect(detail!.reporterEmail).toBe(`m-${suffix}@example.test`);
			expect(detail!.commentCount).toBe(0);
			// And the added fields say "org member" rather than inventing an external reporter.
			expect(detail!.reporterKind).toBe('org_member');
			expect(detail!.context).toBeNull();
		});

		it('SR-6: getAgentReportDetail, listAgentIssues, getIssueReporterId and getReporterId all see the external report', async () => {
			const issueId = await seedExternalReport({ page: 'Visits' });

			const agentDetail = await getAgentReportDetail(issueId);
			expect(agentDetail).toMatchObject({
				bodyMd: 'the save button spins forever',
				severity: 'high',
				reporterKind: 'external',
				reporterDisplayName: 'Sarah K.',
				reporterExternalId: `u_${suffix}`,
			});

			const list = await listAgentIssues({ organizationId: orgId, type: 'user_report' });
			const listed = list.issues.find((row) => row.id === issueId)!;
			expect(listed).toBeDefined();
			// Both were silently null before: indistinguishable from "this is not a report".
			expect(listed.severity).toBe('high');
			expect(listed.reporterKind).toBe('external');
			expect(listed.sourceChannel).toBe('user_report_api');

			const filtered = await listAgentIssues({ organizationId: orgId, source: 'user_report_api' });
			expect(filtered.issues.map((row) => row.id)).toContain(issueId);
			const otherSource = await listAgentIssues({ organizationId: orgId, source: 'manual_support' });
			expect(otherSource.issues.map((row) => row.id)).not.toContain(issueId);

			for (const read of [getReporterId, getIssueReporterId]) {
				const reporter = await read(issueId);
				expect(reporter).toMatchObject({
					kind: 'external',
					externalReporterId,
					externalId: `u_${suffix}`,
					displayName: 'Sarah K.',
				});
			}
		});

		it('the retention sweep ages an externally-sourced report out', async () => {
			// Old enough for the LONG manual cutoff, resolved, unclaimed, no occurrences -- the four
			// conditions `manualIssueOrphanCondition` requires. The condition keys on
			// `issues.issue_type`, not on a `manual_issue_reports` join, which is exactly why an
			// external report reaches it.
			const issueId = await seedExternalReport({}, new Date('2020-01-01T00:00:00.000Z'));
			await sql`update issues set status = 'resolved' where id = ${issueId}`;

			const beforeSweep = await sql`select id from issues where id = ${issueId}`;
			expect(beforeSweep).toHaveLength(1);

			// A database-wide sweep, so it runs under the retention.*.integration family's lock like every
			// other sweeping test -- unlocked, it deleted siblings' fixtures mid-file (retention.affected's
			// aged side rows, retention.tombstones' orphan) under --sequence.shuffle (B13). Session-scoped,
			// so on one reserved connection, not the pool.
			const lock = await sql.reserve();
			try {
				await lock`select pg_advisory_lock(hashtext('sentinel.retention.integration'))`;
				await cleanupRetainedData(30, 365, 30);
			} finally {
				await lock`select pg_advisory_unlock(hashtext('sentinel.retention.integration'))`.catch(() => {});
				lock.release();
			}

			const afterSweep = await sql`select id from issues where id = ${issueId}`;
			expect(afterSweep).toHaveLength(0);
			// The companion went with it, through §4.6's ON DELETE CASCADE.
			const companion = await sql`select issue_id from external_issue_reports where issue_id = ${issueId}`;
			expect(companion).toHaveLength(0);
		}, 120_000); // the family lock can wait out a sibling's whole file
	});
});
