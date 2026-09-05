import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID, createHash } from 'node:crypto';

/**
 * sentinel-user-report-issue §4.1 / §11 — `POST /api/report` driven end to end against a REAL
 * migrated Postgres, through the route handler itself.
 *
 * §11 names this boundary and says why: "Auth, scope, tenancy, validation, idempotency and every
 * §7 code are observable in the response. This is the highest boundary that reaches all of them,
 * and it is the one an SDK actually sees." The mock-based `report.test.ts` beside this file
 * asserts the route's DECISIONS; this file asserts the ROWS — that one `issues` row and one
 * `external_issue_reports` row appear, that `manual_issue_reports` is not written, that a repeat
 * creates nothing, and that two organizations cannot reach each other's data. A Drizzle mock
 * cannot tell "wrote the right rows" from "wrote nothing" (B10 addendum), which is the whole
 * reason this file exists.
 *
 * This must run against a DISPOSABLE Postgres, never the shared dev database (CLAUDE.md, "Never
 * point tests/integration at the shared dev database" — the same rule applies here even though
 * this file lives under src/routes). Everything it touches is namespaced by a per-run uuid and
 * deleted in `afterAll`, but a `down` from another ledger can still corrupt a shared database.
 *
 *   docker run -d --name sentinel-report-pg -p 15432:5432 \
 *     -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme -e POSTGRES_DB=sentinel postgres:15-alpine
 *   cd packages/db-migrations && GOWORK=off \
 *     DB_URL_DASHBOARD=postgres://sentinel:changeme@localhost:15432/sentinel?sslmode=disable \
 *     go run ./cmd/migrate up -target dashboard
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:changeme@localhost:15432/sentinel \
 *     pnpm vitest run src/routes/api/report/report.intake.integration.test.ts
 *
 * If nothing answers on DATABASE_URL the suite SKIPS, mirroring every other integration file here.
 * Set `REPORT_INTAKE_INTEGRATION_REQUIRED=1` to turn that skip into a hard failure.
 */
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.REPORT_INTAKE_INTEGRATION_REQUIRED === '1';

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

if (required && !dbReachable) {
	throw new Error(
		`REPORT_INTAKE_INTEGRATION_REQUIRED=1 but no migrated database answered at ${connectionString}. ` +
			'This test is the committed proof that POST /api/report creates the right rows and refuses ' +
			'a cross-tenant caller; skipping it silently would leave both claims unverified.'
	);
}

const suffix = randomUUID().slice(0, 8);

// Two organizations, because criterion 2 is a tenancy proof and one org cannot demonstrate it.
const orgA = randomUUID();
const orgB = randomUUID();
const projectA = randomUUID();
const projectB = randomUUID();

// Secrets are per-run so a rerun against the same database cannot collide on key_hash's UNIQUE.
const SECRET_A = `sent_report_a_${suffix}`;
const SECRET_B = `sent_report_b_${suffix}`;
const SECRET_A_ORGWIDE = `sent_report_ow_${suffix}`;
const SECRET_A_INGEST = `sent_ingest_a_${suffix}`;
const SECRET_A_THROTTLED = `sent_report_rl_${suffix}`;

const keyIdA = randomUUID();
const keyIdB = randomUUID();
const keyIdAOrgWide = randomUUID();
const keyIdAIngest = randomUUID();
const keyIdAThrottled = randomUUID();

const sha256 = (v: string) => createHash('sha256').update(v).digest('hex');

let sql: ReturnType<typeof postgres>;
// `url` joined `request` when escalation landed: §4.4's `escalated` email builds its issue link
// from the request origin, exactly as every other notification email in this app does.
let POST: (event: { request: Request; url: URL }) => Promise<Response>;
let deriveReportTitle: typeof import('$lib/server/report-title').deriveReportTitle;

/** Every response this suite captures, for `status/be.md`'s "one captured response each". */
const captured: Array<{ label: string; status: number; body: unknown }> = [];

async function call(
	secret: string | null,
	payload: unknown,
	extraHeaders: Record<string, string> = {},
	label?: string
) {
	const headers: Record<string, string> = { 'Content-Type': 'application/json', ...extraHeaders };
	if (secret) headers['X-API-Key'] = secret;
	const request = new Request('http://localhost/api/report', {
		method: 'POST',
		headers,
		body: typeof payload === 'string' ? payload : JSON.stringify(payload),
	});
	const res = await POST({ request, url: new URL('http://localhost/api/report') });
	const body = await res.clone().json();
	if (label) captured.push({ label, status: res.status, body });
	return { status: res.status, headers: res.headers, body: body as any };
}

function reportBody(overrides: Record<string, unknown> = {}) {
	return {
		text: 'The save button on a visit detail spins forever and the visit never saves.',
		reporter: { external_id: `u_${suffix}`, display_name: 'Sarah K.' },
		...overrides,
	};
}

/** Inserts a DRAFT attachment exactly as §4.5's upload route leaves one behind. */
async function seedDraftAttachment(orgId: string, uploaderId: string): Promise<string> {
	const id = randomUUID();
	await sql`
		insert into attachments (id, org_id, uploader_type, uploader_id, filename, content_type, size_bytes, storage_key, status)
		values (${id}, ${orgId}, ${'external_reporter'}, ${uploaderId}, ${'screen.png'},
		        ${'image/png'}, ${184213}, ${'att/' + id}, ${'ready'})
	`;
	return id;
}

async function upsertReporter(orgId: string, projectId: string, externalId: string): Promise<string> {
	const { upsertExternalReporter } = await import('$lib/db/queries/external-reporters');
	const row = await upsertExternalReporter({ organizationId: orgId, projectId, externalId });
	return row.id;
}

describe.skipIf(!dbReachable)('POST /api/report (integration, real Postgres)', () => {
	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 2 });

		for (const [orgId, projectId, label] of [
			[orgA, projectA, 'a'],
			[orgB, projectB, 'b'],
		] as const) {
			await sql`insert into organizations (id, name, slug)
			          values (${orgId}, ${`rpt-${label}-${suffix}`}, ${`rpt-${label}-${suffix}`})`;
			await sql`insert into projects (id, name, organization_id, api_key, api_key_hash)
			          values (${projectId}, ${`proj-${label}-${suffix}`}, ${orgId},
			                  ${`pk_${label}_${suffix}`}, ${sha256(`pk_${label}_${suffix}`)})`;
		}

		const keys: Array<[string, string, string, string | null, string, number]> = [
			[keyIdA, orgA, SECRET_A, projectA, 'report', 5000],
			[keyIdB, orgB, SECRET_B, projectB, 'report', 5000],
			[keyIdAOrgWide, orgA, SECRET_A_ORGWIDE, null, 'report', 5000],
			[keyIdAIngest, orgA, SECRET_A_INGEST, projectA, 'ingest', 5000],
			[keyIdAThrottled, orgA, SECRET_A_THROTTLED, projectA, 'report', 1],
		];
		for (const [id, orgId, secret, projectId, scope, rpm] of keys) {
			await sql`
				insert into project_api_keys (id, organization_id, project_id, name, key_prefix, key_hash, scope, rate_limit_rpm, created_by)
				values (${id}, ${orgId}, ${projectId}, ${'k-' + suffix}, ${'sent_'},
				        ${sha256(secret)}, ${scope}, ${rpm}, ${'test'})
			`;
		}

		// The handler's own `RequestEvent` carries a dozen fields this suite never touches; the cast
		// narrows to the two it does drive. Widening the local type instead would mean building a
		// whole fake RequestEvent per call to satisfy fields the route never reads.
		({ POST } = (await import('./+server')) as unknown as { POST: typeof POST });
		({ deriveReportTitle } = await import('$lib/server/report-title'));
	});

	afterAll(async () => {
		if (!sql) return;
		// `external_issue_reports.external_reporter_id` is `REFERENCES external_reporters(id)` with
		// NO `ON DELETE` action -- exactly as §4.6 specifies it. Deleting an organization therefore
		// cascades to BOTH tables, and Postgres can reach `external_reporters` first, which the FK
		// then refuses. The companions are removed explicitly here so the teardown does not depend
		// on cascade ordering. (Recorded in `status/be.md`: there is no production org-delete path
		// in this repo today, so this is a teardown observation, not a live defect.)
		await sql`
			delete from external_issue_reports
			where issue_id in (
				select i.id from issues i
				join projects p on p.id = i.project_id
				where p.organization_id in (${orgA}, ${orgB})
			)
		`;
		await sql`delete from organizations where id in (${orgA}, ${orgB})`;
		await sql.end({ timeout: 5 });

		// eslint-disable-next-line no-console
		console.log('\n=== captured §4.1 responses ===\n' + JSON.stringify(captured, null, 2));
	});

	describe('criterion 1 — the §4.1-b shape and the rows behind it', () => {
		it('201 creates ONE issues row with issue_type/source_channel and ONE companion; manual_issue_reports untouched', async () => {
			const res = await call(SECRET_A, reportBody({ severity: 'high', context: { page: 'Visits' } }), {}, '201 created');
			expect(res.status).toBe(201);
			expect(res.body).toEqual({
				report_id: expect.stringMatching(/^[0-9a-f-]{36}$/),
				issue_id: res.body.report_id,
				status: 'created',
				deduplicated: false,
				// §4.1-b's inline §4.2 object. This project has no `system_error` issue with an
				// in-window occurrence, so it is "looked, found nothing" -- `{candidates: []}`, which
				// §4.2 insists is a DIFFERENT state from `null` ("not looked at").
				triage: {
					ranAt: expect.stringMatching(/^\d{4}-\d{2}-\d{2}T/),
					strategy: 'window_release',
					candidates: [],
				},
			});

			const [issue] = await sql`
				select id, project_id, message, error_class, issue_type, source_channel, status
				from issues where id = ${res.body.report_id}
			`;
			expect(issue).toMatchObject({
				project_id: projectA,
				issue_type: 'user_report',
				source_channel: 'user_report_api',
				error_class: 'user_report',
			});

			const companions = await sql`
				select issue_id, body_md, severity, context, idempotency_key
				from external_issue_reports where issue_id = ${res.body.report_id}
			`;
			expect(companions).toHaveLength(1);
			expect(companions[0]).toMatchObject({
				severity: 'high',
				body_md: 'The save button on a visit detail spins forever and the visit never saves.',
				idempotency_key: null,
			});
			expect(companions[0].context).toEqual({ page: 'Visits' });

			// OQ-6 / §1: the org-member path is not touched. This is the assertion that would go red
			// if someone "reused" createManualIssue for external reports.
			const manual = await sql`
				select count(*)::int as n from manual_issue_reports where issue_id = ${res.body.report_id}
			`;
			expect(manual[0].n).toBe(0);

			// The reporter row exists, scoped to (organization_id, project_id), and is referenced.
			const reporters = await sql`
				select r.id, r.organization_id, r.project_id, r.external_id, r.display_name
				from external_reporters r
				join external_issue_reports e on e.external_reporter_id = r.id
				where e.issue_id = ${res.body.report_id}
			`;
			expect(reporters).toHaveLength(1);
			expect(reporters[0]).toMatchObject({
				organization_id: orgA,
				project_id: projectA,
				external_id: `u_${suffix}`,
				display_name: 'Sarah K.',
			});
		});

		it('a repeat with the same idempotency_key and the same body replays report_id, and count(*) stays 1', async () => {
			const key = `idem-${suffix}-1`;
			const payload = reportBody({ idempotency_key: key, text: 'Idempotent report body.' });

			const first = await call(SECRET_A, payload);
			expect(first.status).toBe(201);
			expect(first.body.deduplicated).toBe(false);

			const second = await call(SECRET_A, payload, {}, '201 deduplicated');
			expect(second.status).toBe(201);
			expect(second.body.report_id).toBe(first.body.report_id);
			expect(second.body.deduplicated).toBe(true);

			const rows = await sql`
				select count(*)::int as n from external_issue_reports where idempotency_key = ${key}
			`;
			expect(rows[0].n).toBe(1);

			const stored = await sql`
				select count(*)::int as n from report_idempotency_keys
				where api_key_id = ${keyIdA} and idempotency_key = ${key}
			`;
			expect(stored[0].n).toBe(1);
		});

		it('409 idempotency_conflict when the same key arrives with a different body, creating nothing', async () => {
			const key = `idem-${suffix}-2`;
			await call(SECRET_A, reportBody({ idempotency_key: key, text: 'First body.' }));
			const res = await call(
				SECRET_A,
				reportBody({ idempotency_key: key, text: 'A different body entirely.' }),
				{},
				'409 idempotency_conflict'
			);
			expect(res.status).toBe(409);
			expect(res.body.error).toMatchObject({ code: 'idempotency_conflict', details: {} });

			const rows = await sql`
				select count(*)::int as n from external_issue_reports where idempotency_key = ${key}
			`;
			expect(rows[0].n).toBe(1);
		});

		it('a repeat with the same key and a REORDERED body still deduplicates', async () => {
			const key = `idem-${suffix}-3`;
			const first = await call(SECRET_A, {
				text: 'Order-independent.',
				reporter: { external_id: `u_${suffix}`, display_name: 'Sarah K.' },
				idempotency_key: key,
			});
			const second = await call(SECRET_A, {
				idempotency_key: key,
				reporter: { display_name: 'Sarah K.', external_id: `u_${suffix}` },
				text: 'Order-independent.',
			});
			expect(second.status).toBe(201);
			expect(second.body).toMatchObject({ report_id: first.body.report_id, deduplicated: true });
		});

		it('400 invalid_request with details.field for an empty text, creating nothing', async () => {
			const before = await sql`select count(*)::int as n from issues where project_id = ${projectA}`;
			const res = await call(SECRET_A, reportBody({ text: '   ' }), {}, '400 invalid_request');
			expect(res.status).toBe(400);
			expect(res.body.error).toMatchObject({ code: 'invalid_request', details: { field: 'text' } });
			const after = await sql`select count(*)::int as n from issues where project_id = ${projectA}`;
			expect(after[0].n).toBe(before[0].n);
		});

		it('claims this reporter\'s own draft attachment onto the issue, in the creation transaction', async () => {
			const reporterId = await upsertReporter(orgA, projectA, `u_${suffix}`);
			const attachmentId = await seedDraftAttachment(orgA, reporterId);

			const res = await call(SECRET_A, reportBody({ attachments: [attachmentId] }));
			expect(res.status).toBe(201);

			const [row] = await sql`select issue_id, comment_id from attachments where id = ${attachmentId}`;
			expect(row.issue_id).toBe(res.body.report_id);
			expect(row.comment_id).toBeNull();
		});

		it('400 attachment_not_found for ANOTHER organization\'s draft, and nothing is created', async () => {
			const foreignReporter = await upsertReporter(orgB, projectB, `u_foreign_${suffix}`);
			const foreignAttachment = await seedDraftAttachment(orgB, foreignReporter);

			const before = await sql`select count(*)::int as n from issues where project_id = ${projectA}`;
			const res = await call(
				SECRET_A,
				reportBody({ attachments: [foreignAttachment] }),
				{},
				'400 attachment_not_found'
			);
			expect(res.status).toBe(400);
			expect(res.body.error).toMatchObject({
				code: 'attachment_not_found',
				details: { field: 'attachments' },
			});

			const after = await sql`select count(*)::int as n from issues where project_id = ${projectA}`;
			expect(after[0].n).toBe(before[0].n);

			// Still a draft in org B: the failed claim must not have moved another tenant's row.
			const [row] = await sql`select issue_id, org_id from attachments where id = ${foreignAttachment}`;
			expect(row.issue_id).toBeNull();
			expect(row.org_id).toBe(orgB);
		});

		it('400 attachment_not_found is ALL-or-nothing: a good id alongside a bad one creates nothing', async () => {
			const reporterId = await upsertReporter(orgA, projectA, `u_${suffix}`);
			const good = await seedDraftAttachment(orgA, reporterId);
			const unknown = randomUUID();

			const res = await call(SECRET_A, reportBody({ attachments: [good, unknown] }));
			expect(res.status).toBe(400);
			expect(res.body.error.code).toBe('attachment_not_found');

			const [row] = await sql`select issue_id from attachments where id = ${good}`;
			expect(row.issue_id).toBeNull();
		});

		it('401 unauthenticated for a missing and for an unknown key', async () => {
			const missing = await call(null, reportBody(), {}, '401 unauthenticated');
			expect(missing.status).toBe(401);
			expect(missing.body.error).toMatchObject({ code: 'unauthenticated', details: {} });

			const unknown = await call('sent_nope_' + suffix, reportBody());
			expect(unknown.status).toBe(401);
			expect(unknown.body.error.code).toBe('unauthenticated');
		});

		it('403 forbidden_scope for an ingest-scoped key, with the scope in details', async () => {
			const res = await call(SECRET_A_INGEST, reportBody(), {}, '403 forbidden_scope');
			expect(res.status).toBe(403);
			expect(res.body.error).toMatchObject({
				code: 'forbidden_scope',
				details: { scope: 'ingest' },
			});
		});

		it('413 payload_too_large above the 1 MiB cap', async () => {
			const oversized = JSON.stringify(reportBody({ text: 'x'.repeat(1 << 20) }));
			const res = await call(SECRET_A, oversized, {}, '413 payload_too_large');
			expect(res.status).toBe(413);
			expect(res.body.error).toMatchObject({ code: 'payload_too_large', details: {} });
		});

		it('429 rate_limited with Retry-After once the key\'s rate_limit_rpm is spent', async () => {
			const first = await call(SECRET_A_THROTTLED, reportBody());
			expect(first.status).toBe(201);
			const second = await call(SECRET_A_THROTTLED, reportBody(), {}, '429 rate_limited');
			expect(second.status).toBe(429);
			expect(second.body.error).toMatchObject({ code: 'rate_limited', details: {} });
			expect(Number(second.headers.get('Retry-After'))).toBeGreaterThan(0);
		});
	});

	describe('criterion 2 — tenancy under attack, with two organizations', () => {
		it('an ORG-WIDE key naming another org\'s project is 403 project_not_found, byte-identical to a project that does not exist', async () => {
			const foreign = await call(
				SECRET_A_ORGWIDE,
				reportBody({ project_key: `proj-b-${suffix}` }),
				{},
				'403 project_not_found (another org\'s project)'
			);
			const absent = await call(SECRET_A_ORGWIDE, reportBody({ project_key: `no-such-project-${suffix}` }));

			expect(foreign.status).toBe(403);
			expect(foreign.body.error.code).toBe('project_not_found');
			// Byte-identical: same status, same serialised body. This is the enumeration guard.
			expect(absent.status).toBe(foreign.status);
			expect(JSON.stringify(absent.body)).toBe(JSON.stringify(foreign.body));
		});

		it('a PROJECT-SCOPED key naming another org\'s project — in the body or in X-Project-Key — is the same 403, and creates nothing', async () => {
			const before = await sql`select count(*)::int as n from issues where project_id in (${projectA}, ${projectB})`;

			const viaBody = await call(SECRET_A, reportBody({ project_key: `proj-b-${suffix}` }));
			const viaHeader = await call(SECRET_A, reportBody(), { 'X-Project-Key': `proj-b-${suffix}` });
			const absent = await call(SECRET_A, reportBody({ project_key: `no-such-project-${suffix}` }));

			for (const res of [viaBody, viaHeader, absent]) {
				expect(res.status).toBe(403);
				expect(res.body.error.code).toBe('project_not_found');
			}
			expect(JSON.stringify(viaBody.body)).toBe(JSON.stringify(absent.body));
			expect(JSON.stringify(viaHeader.body)).toBe(JSON.stringify(absent.body));

			const after = await sql`select count(*)::int as n from issues where project_id in (${projectA}, ${projectB})`;
			expect(after[0].n).toBe(before[0].n);
		});

		it('the same external_id reported by two organizations is TWO external_reporters rows, neither reachable from the other', async () => {
			const shared = `u_shared_${suffix}`;

			const a = await call(SECRET_A, reportBody({ reporter: { external_id: shared, display_name: 'A side' } }));
			const b = await call(SECRET_B, reportBody({ reporter: { external_id: shared, display_name: 'B side' } }));
			expect(a.status).toBe(201);
			expect(b.status).toBe(201);

			const rows = await sql`
				select id, organization_id, project_id, display_name
				from external_reporters where external_id = ${shared} order by display_name
			`;
			expect(rows).toHaveLength(2);
			expect(rows.map((r) => r.organization_id).sort()).toEqual([orgA, orgB].sort());
			expect(rows[0].id).not.toBe(rows[1].id);

			// Each report points at its OWN org's reporter, and at no row of the other org.
			const [aJoin] = await sql`
				select r.organization_id, r.project_id from external_issue_reports e
				join external_reporters r on r.id = e.external_reporter_id
				where e.issue_id = ${a.body.report_id}
			`;
			const [bJoin] = await sql`
				select r.organization_id, r.project_id from external_issue_reports e
				join external_reporters r on r.id = e.external_reporter_id
				where e.issue_id = ${b.body.report_id}
			`;
			expect(aJoin).toMatchObject({ organization_id: orgA, project_id: projectA });
			expect(bJoin).toMatchObject({ organization_id: orgB, project_id: projectB });

			// The tenancy statement §4.2 will rely on: org B's report detail is scoped by
			// organization, so org A's report is not reachable through it and vice versa.
			const crossA = await sql`
				select count(*)::int as n from issues i
				join projects p on p.id = i.project_id
				where i.id = ${a.body.report_id} and p.organization_id = ${orgB}
			`;
			const crossB = await sql`
				select count(*)::int as n from issues i
				join projects p on p.id = i.project_id
				where i.id = ${b.body.report_id} and p.organization_id = ${orgA}
			`;
			expect(crossA[0].n).toBe(0);
			expect(crossB[0].n).toBe(0);
		});

		it('an idempotency_key is scoped to the API KEY row: two organizations may reuse the same string', async () => {
			const key = `shared-idem-${suffix}`;
			const a = await call(SECRET_A, reportBody({ idempotency_key: key }));
			const b = await call(SECRET_B, reportBody({ idempotency_key: key }));
			expect(a.status).toBe(201);
			expect(b.status).toBe(201);
			expect(b.body.deduplicated).toBe(false);
			expect(b.body.report_id).not.toBe(a.body.report_id);
		});
	});

	describe('criterion 3 — the derived title reaches issues.message unchanged', () => {
		it.each([
			['short text, no page', 'The save button spins forever.', undefined],
			['long text, word-boundary truncation', Array(40).fill('word').join(' '), undefined],
			['page prefix', 'The save button spins forever.', 'Visits'],
			['long text WITH a page prefix', Array(40).fill('word').join(' '), 'Visits'],
			['grapheme clusters', '👩‍👩‍👧‍👦'.repeat(20), 'Visits'],
		])('%s', async (_label, text, page) => {
			const res = await call(SECRET_A, reportBody({ text, context: page ? { page } : {} }));
			expect(res.status).toBe(201);
			const [issue] = await sql`select message from issues where id = ${res.body.report_id}`;
			expect(issue.message).toBe(deriveReportTitle({ text, page }));
		});

		it('an empty-after-trim text never reaches the derivation — it is a 400 first', async () => {
			const res = await call(SECRET_A, reportBody({ text: ' ' }));
			expect(res.status).toBe(400);
		});
	});
});
