import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomBytes, randomUUID } from 'node:crypto';

/**
 * sentinel-cross-repo-trace-view D7 — the trace view's guards, against a REAL migrated Postgres,
 * because every claim here is a claim about SQL: that the project set is ONE organization's, that
 * the environment grant composes into the same WHERE as the trace id, and that a keyset cursor
 * neither skips nor repeats a row. A mocked Drizzle chain cannot tell "the predicate was composed"
 * from "the predicate object was built and dropped".
 *
 * One trace id is seeded in two projects × two environments of org A AND in org B (trace ids are
 * client-asserted, so a foreign org can send the same one). Asserts, through the three real
 * consumers (the page loader, the issue page's related count, the agent read):
 *   - an org A member sees both projects, oldest first, and never org B's row;
 *   - a user with an environment grant does not see the other environment's occurrences, and the
 *     related-errors count obeys the same rule;
 *   - an unknown, a foreign and a hidden trace render the SAME empty page;
 *   - the agent read is org-scoped (NOT environment-scoped) and its cursor pages exactly, including
 *     across rows that tie on `created_at`.
 *
 * Point DATABASE_URL at a DISPOSABLE database — never the shared dev one (CLAUDE.md):
 *
 *   podman run -d --name tv-dash-pg -p 127.0.0.1:55493:5432 -e POSTGRES_USER=sentinel \
 *     -e POSTGRES_PASSWORD=<pw> -e POSTGRES_DB=sentinel postgres:15-alpine
 *   cd packages/db-migrations && GOWORK=off DB_URL_DASHBOARD='postgres://sentinel:<pw>@127.0.0.1:55493/sentinel?sslmode=disable' \
 *     go run ./cmd/migrate up -target dashboard
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:<pw>@127.0.0.1:55493/sentinel \
 *     TRACE_VIEW_INTEGRATION_REQUIRED=1 pnpm vitest run src/lib/server/trace-view.integration.test.ts
 *
 * Skipped when nothing answers; TRACE_VIEW_INTEGRATION_REQUIRED=1 makes that a hard failure, like
 * READ_GUARDS_INTEGRATION_REQUIRED.
 */
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.TRACE_VIEW_INTEGRATION_REQUIRED === '1';

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
		`TRACE_VIEW_INTEGRATION_REQUIRED=1 but no database answered at ${connectionString}.`
	);
}

const suffix = randomUUID().slice(0, 8);
const hex32 = () => randomBytes(16).toString('hex');

const TRACE = hex32(); // seeded in org A (4 rows) and org B (1 row)
const TRACE_B_ONLY = hex32(); // exists in org B only
const TRACE_HIDDEN = hex32(); // exists in org A, only in webA/staging
const TRACE_TIE = hex32(); // ordering + paging fixture, coreA only
const TRACE_UNKNOWN = hex32(); // exists nowhere

const orgA = randomUUID();
const orgB = randomUUID();
const webA = randomUUID();
const coreA = randomUUID();
const projB = randomUUID();

const memberUser = randomUUID(); // org A engineer, no grant
const grantUser = randomUUID(); // org A engineer, narrowed to {production} in webA only
const outsider = randomUUID();

// Relative to now: retention.ts sweeps old occurrences across the whole database, and the
// retention suite shares it.
const T0 = Math.floor(Date.now() / 1000) * 1000 - 3 * 60 * 60 * 1000;
const at = (ms: number) => new Date(T0 + ms).toISOString().replace('T', ' ').replace('Z', '');

const issue = {
	webProd: randomUUID(),
	webStaging: randomUUID(),
	coreProd: randomUUID(),
	coreStaging: randomUUID(),
	foreign: randomUUID(),
	tie: randomUUID(),
};
const occ = {
	webProd: randomUUID(),
	coreProd: randomUUID(),
	webStaging: randomUUID(),
	coreStaging: randomUUID(),
	foreign: randomUUID(),
};
// Same created_at to the microsecond; the HIGHER id was received FIRST, so D3's order (received_at
// before id) and a cursor that omitted received_at would disagree.
const tieEarlyReceived = 'ffffffff-0000-4000-8000-000000000001';
const tieLateReceived = '00000000-0000-4000-8000-000000000001';
const tieLast = '00000000-0000-4000-8000-000000000002';

describe.skipIf(!dbReachable)('the cross-repo trace view (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });

		for (const [id, label] of [[orgA, 'a'], [orgB, 'b']] as const) {
			await sql`insert into organizations (id, name, slug) values (${id}, ${`tv-${label}-${suffix}`}, ${`tv-${label}-${suffix}`})`;
		}
		for (const [id, org, label] of [
			[webA, orgA, 'web'],
			[coreA, orgA, 'core'],
			[projB, orgB, 'other'],
		] as const) {
			await sql`
				insert into projects (id, name, organization_id, api_key, api_key_hash)
				values (${id}, ${`tv-${label}-${suffix}`}, ${org}, ${`k_${label}_${suffix}`}, ${`h_${label}_${suffix}`})
			`;
		}
		for (const [id, label] of [[memberUser, 'member'], [grantUser, 'grant'], [outsider, 'outsider']] as const) {
			await sql`insert into "user" (id, name, email) values (${id}, ${label}, ${`tv-${label}-${suffix}@example.test`})`;
		}
		for (const id of [memberUser, grantUser]) {
			await sql`insert into organization_members (organization_id, user_id, role) values (${orgA}, ${id}, 'engineer')`;
		}
		await sql`
			insert into project_environment_grants (user_id, project_id, environments, created_by)
			values (${grantUser}, ${webA}, ${sql.array(['production'])}, ${memberUser})
		`;

		for (const [id, project, env] of [
			[issue.webProd, webA, 'production'],
			[issue.webStaging, webA, 'staging'],
			[issue.coreProd, coreA, 'production'],
			[issue.coreStaging, coreA, 'staging'],
			[issue.foreign, projB, 'production'],
			[issue.tie, coreA, 'production'],
		] as const) {
			await sql`
				insert into issues (id, project_id, environment, fingerprint, message, error_class)
				values (${id}, ${project}, ${env}, ${`fp-${id}`}, ${`tv seed ${suffix}`}, 'TraceSeedError')
			`;
		}

		const insertOcc = async (
			id: string, issueId: string, env: string, trace: string, createdAt: string, receivedAt: string | null, message: string
		) => {
			await sql`
				insert into error_occurrences (id, issue_id, environment, platform, trace_id, created_at, received_at, message, level, error_class)
				values (${id}, ${issueId}, ${env}, 'node', ${trace}, ${createdAt}::timestamptz, ${receivedAt}::timestamptz, ${message}, 'error', 'TraceSeedError')
			`;
		};
		// Inserted OUT of time order, so an assertion on order cannot pass by insertion order.
		await insertOcc(occ.coreStaging, issue.coreStaging, 'staging', TRACE, at(3000), at(3000), 'core staging');
		await insertOcc(occ.webProd, issue.webProd, 'production', TRACE, at(0), at(0), 'web prod');
		await insertOcc(occ.webStaging, issue.webStaging, 'staging', TRACE, at(2000), at(2000), 'web staging');
		await insertOcc(occ.coreProd, issue.coreProd, 'production', TRACE, at(1000), at(1000), 'core prod');
		await insertOcc(occ.foreign, issue.foreign, 'production', TRACE, at(500), at(500), 'foreign org');
		await insertOcc(randomUUID(), issue.foreign, 'production', TRACE_B_ONLY, at(0), at(0), 'foreign only');
		await insertOcc(randomUUID(), issue.webStaging, 'staging', TRACE_HIDDEN, at(0), at(0), 'hidden');

		// The tie fixture: microsecond-equal created_at, received_at in the opposite order to id.
		const tieCreated = at(10_000).replace(/\.\d{3}$/, ".123456");
		await insertOcc(tieEarlyReceived, issue.tie, 'production', TRACE_TIE, tieCreated, at(10_100), 'tie early-received');
		await insertOcc(tieLateReceived, issue.tie, 'production', TRACE_TIE, tieCreated, at(10_900), 'tie late-received');
		await insertOcc(tieLast, issue.tie, 'production', TRACE_TIE, at(20_000), null, 'tie last (pre-v2: received_at NULL)');
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			const projectIds = [webA, coreA, projB];
			await sql`delete from error_occurrences where issue_id in (select id from issues where project_id in ${sql(projectIds)})`;
			await sql`delete from issues where project_id in ${sql(projectIds)}`;
			await sql`delete from project_environment_grants where user_id in ${sql([memberUser, grantUser])}`;
			await sql`delete from organization_members where organization_id in ${sql([orgA, orgB])}`;
			await sql`delete from projects where organization_id in ${sql([orgA, orgB])}`;
			await sql`delete from "user" where id in ${sql([memberUser, grantUser, outsider])}`;
			await sql`delete from organizations where id in ${sql([orgA, orgB])}`;
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	// The page loader, called the way SvelteKit calls it: a session from `locals.auth()` and the
	// `currentOrg` that `orgHandle` set after its membership check.
	async function loadPage(userId: string, org: { id: string; slug: string }, traceId: string, search = '', paramSlug = org.slug) {
		const { load } = await import('../../routes/[orgSlug]/traces/[traceId]/+page.server');
		return (await (load as any)({
			params: { orgSlug: paramSlug, traceId },
			locals: { auth: async () => ({ user: { id: userId } }), currentOrg: org },
			url: new URL(`http://localhost/${org.slug}/traces/${traceId}${search}`),
		})) as any;
	}
	const orgAHandle = { id: orgA, slug: `tv-a-${suffix}` };

	it('an org member sees both projects, oldest first, and never the foreign org’s row', async () => {
		const data = await loadPage(memberUser, orgAHandle, TRACE);

		expect(data.occurrences.map((o: any) => o.id)).toEqual([
			occ.webProd,
			occ.coreProd,
			occ.webStaging,
			occ.coreStaging,
		]);
		expect(data.occurrences.map((o: any) => o.projectName)).toEqual([
			`tv-web-${suffix}`,
			`tv-core-${suffix}`,
			`tv-web-${suffix}`,
			`tv-core-${suffix}`,
		]);
		expect(data.occurrences.map((o: any) => o.id)).not.toContain(occ.foreign);
		expect(data.total).toBe(4);
		expect(data.projects.map((p: any) => p.name)).toEqual([`tv-core-${suffix}`, `tv-web-${suffix}`]);
		// Each row links to its issue (the page builds the href from these two).
		expect(data.occurrences[0]).toMatchObject({ issueId: issue.webProd, id: occ.webProd });
	});

	it('an environment grant hides the other environment of ITS project only', async () => {
		const data = await loadPage(grantUser, orgAHandle, TRACE);

		// webA/staging is hidden; coreA (no grant) keeps both environments.
		expect(data.occurrences.map((o: any) => o.id)).toEqual([occ.webProd, occ.coreProd, occ.coreStaging]);
		expect(data.total).toBe(3);
	});

	it('the related-errors count (D5) excludes the own project, the foreign org and hidden rows', async () => {
		const { getAccessibleProjects } = await import('./issue-access');
		const { resolveEnvironmentScopes } = await import('./environment-access');
		const { countTraceOccurrences } = await import('./queries/trace-queries');
		const scopeFor = async (userId: string) => ({
			projectIds: (await getAccessibleProjects(userId, orgA)).map((p) => p.id),
			environmentScopes: await resolveEnvironmentScopes(userId),
		});

		// Viewing a coreA occurrence: the "other projects" are webA's two rows for the member…
		expect((await countTraceOccurrences(TRACE, await scopeFor(memberUser), { excludeProjectId: coreA })).total).toBe(2);
		// …but the grant hides webA/staging, leaving one.
		expect((await countTraceOccurrences(TRACE, await scopeFor(grantUser), { excludeProjectId: coreA })).total).toBe(1);
		// Viewing a webA occurrence: coreA's two rows; org B's row is in neither count.
		expect((await countTraceOccurrences(TRACE, await scopeFor(memberUser), { excludeProjectId: webA })).total).toBe(2);
	});

	it('loadTraceLinks (the issue page’s D5 data): slug, and a count that excludes own project, foreign org and hidden rows', async () => {
		const { loadTraceLinks } = await import('./trace-links');
		const links = (userId: string, projectId: string, selectedTraceId: string | null) =>
			loadTraceLinks({ userId, organizationId: orgA, projectId, selectedTraceId });

		expect(await links(memberUser, coreA, TRACE)).toEqual({
			orgSlug: orgAHandle.slug,
			related: { count: 2, capped: false },
		});
		// The grant hides webA/staging, so the same view from coreA shows one related error, not two.
		expect((await links(grantUser, coreA, TRACE)).related).toEqual({ count: 1, capped: false });
		// No trace on the selected occurrence: the slug still comes back (the cards link), no count.
		expect(await links(memberUser, coreA, null)).toEqual({ orgSlug: orgAHandle.slug, related: null });
		// A stored id that is not 32-hex never reaches the count query.
		expect((await links(memberUser, coreA, 'not-a-trace')).related).toBeNull();
	});

	it('loadTraceLinks offers nothing for a stored id that is not canonical (m3)', async () => {
		const { loadTraceLinks } = await import('./trace-links');
		const r = await loadTraceLinks({
			userId: memberUser, organizationId: orgA, projectId: coreA, selectedTraceId: TRACE.toUpperCase(),
		});
		expect(r.related).toBeNull();
	});

	it('an unknown, a foreign and a hidden trace render the SAME empty page', async () => {
		const strip = (d: any) => ({ ...d, traceId: 'x' });
		const unknown = strip(await loadPage(memberUser, orgAHandle, TRACE_UNKNOWN));
		const foreign = strip(await loadPage(memberUser, orgAHandle, TRACE_B_ONLY));
		const hidden = strip(await loadPage(grantUser, orgAHandle, TRACE_HIDDEN));

		expect(unknown.occurrences).toEqual([]);
		expect(unknown.total).toBe(0);
		expect(foreign).toEqual(unknown);
		expect(hidden).toEqual(unknown);
	});

	it('a user in no org reaches nothing: an empty project set denies', async () => {
		const data = await loadPage(outsider, orgAHandle, TRACE);
		expect(data.occurrences).toEqual([]);
	});

	it('a malformed trace id is a 404 and a foreign currentOrg slug is a 403', async () => {
		await expect(loadPage(memberUser, orgAHandle, 'not-a-trace')).rejects.toMatchObject({ status: 404 });
		await expect(loadPage(memberUser, orgAHandle, 'A'.repeat(31))).rejects.toMatchObject({ status: 404 });
		await expect(
			loadPage(memberUser, orgAHandle, TRACE, '', 'someone-else')
		).rejects.toMatchObject({ status: 403 });
	});

	it('an upper-case trace id is lower-cased before the query', async () => {
		const data = await loadPage(memberUser, orgAHandle, TRACE.toUpperCase());
		expect(data.total).toBe(4);
	});

	it('pages with a keyset cursor, in D3 order, across microsecond and received_at ties', async () => {
		const { listAgentTraceOccurrences } = await import('$lib/db/queries/agent-reads');

		// limit 1 forces a cursor boundary between EVERY pair, including the two ties.
		const seen: string[] = [];
		let cursor: string | undefined;
		for (let i = 0; i < 5; i++) {
			const page = await listAgentTraceOccurrences({ organizationId: orgA, traceId: TRACE_TIE, limit: 1, cursor });
			seen.push(...page.occurrences.map((o) => o.id));
			cursor = page.nextCursor;
			if (!cursor) break;
		}

		expect(seen).toEqual([tieEarlyReceived, tieLateReceived, tieLast]);
		expect(cursor).toBeUndefined();
	});

	it('the agent read is org-scoped, NOT environment-scoped, and carries the cross-project fields', async () => {
		const { listAgentTraceOccurrences } = await import('$lib/db/queries/agent-reads');

		const all = await listAgentTraceOccurrences({ organizationId: orgA, traceId: TRACE });
		expect(all.occurrences.map((o) => o.id)).toEqual([occ.webProd, occ.coreProd, occ.webStaging, occ.coreStaging]);
		expect(all.occurrences[0]).toMatchObject({
			projectId: webA,
			projectName: `tv-web-${suffix}`,
			issueId: issue.webProd,
			spanId: null,
		});
		expect(all.nextCursor).toBeUndefined();

		// org B's key sees only its own row for the same trace id.
		const foreign = await listAgentTraceOccurrences({ organizationId: orgB, traceId: TRACE });
		expect(foreign.occurrences.map((o) => o.id)).toEqual([occ.foreign]);

		const filtered = await listAgentTraceOccurrences({ organizationId: orgA, traceId: TRACE, environment: 'staging' });
		expect(filtered.occurrences.map((o) => o.id)).toEqual([occ.webStaging, occ.coreStaging]);
	});

	it('the agent read rejects a cursor it did not mint', async () => {
		const { listAgentTraceOccurrences } = await import('$lib/db/queries/agent-reads');
		await expect(
			listAgentTraceOccurrences({ organizationId: orgA, traceId: TRACE, cursor: 'garbage' })
		).rejects.toThrow('invalid cursor');
	});

	it('load more: the page loader’s nextCursor yields the rest with no repeat', async () => {
		const { listTraceOccurrences, traceRowColumns, TRACE_PAGE_SIZE } = await import('./queries/trace-queries');
		expect(TRACE_PAGE_SIZE).toBe(200);
		const scope = { projectIds: [webA, coreA] };
		const first = await listTraceOccurrences({ traceId: TRACE, scope, limit: 3, columns: traceRowColumns });
		expect(first.rows).toHaveLength(3);
		expect(first.nextCursor).toBeDefined();

		const { decodeTraceCursor } = await import('./queries/trace-queries');
		const data = await loadPage(memberUser, orgAHandle, TRACE, `?cursor=${encodeURIComponent(first.nextCursor!)}`);
		expect(data.occurrences.map((o: any) => o.id)).toEqual([occ.coreStaging]);
		expect(decodeTraceCursor(first.nextCursor!).id).toBe(occ.webStaging);
	});

	// M1 — a base64url JSON cursor whose fields pass the SHAPE checks but that Postgres refuses.
	describe('a tampered cursor is a bad cursor, never a 500', () => {
		const cursorOf = (c: Record<string, string>) => Buffer.from(JSON.stringify(c), 'utf8').toString('base64url');
		const ID = '00000000-0000-4000-8000-000000000009';
		const probes: Array<[string, Record<string, string>]> = [
			['month 13 / day 45', { created: '2026-13-45 00:00:00', received: '1', id: ID }],
			['Feb 30 and 25:61:61', { created: '2026-02-30 25:61:61', received: '1', id: ID }],
			['numeric overflow (200 000 digits)', { created: '2026-01-01 00:00:00', received: '9'.repeat(200000), id: ID }],
			['offset out of range', { created: '2026-01-01 00:00:00+99', received: '1', id: ID }],
		];

		it.each(probes)('page loader: %s → 404', async (_n, c) => {
			await expect(loadPage(memberUser, orgAHandle, TRACE, `?cursor=${cursorOf(c)}`)).rejects.toMatchObject({ status: 404 });
		});

		it.each(probes)('agent read: %s → invalid cursor (the route maps it to 400)', async (_n, c) => {
			const { listAgentTraceOccurrences } = await import('$lib/db/queries/agent-reads');
			await expect(
				listAgentTraceOccurrences({ organizationId: orgA, traceId: TRACE, cursor: cursorOf(c) })
			).rejects.toThrow('invalid cursor');
		});

		it('the query layer’s own backstop: a value that bypasses the decoder and fails Postgres’ cast is still "invalid cursor"', async () => {
			const { listTraceOccurrences, traceRowColumns } = await import('./queries/trace-queries');
			await expect(
				listTraceOccurrences({
					traceId: TRACE,
					scope: { projectIds: [webA] },
					limit: 5,
					columns: traceRowColumns,
					cursor: { created: '2026-13-45 00:00:00', received: '1', id: ID },
				})
			).rejects.toThrow('invalid cursor');
		});

		it('a legitimate cursor still pages (the decoder did not get stricter than what it mints)', async () => {
			const { listAgentTraceOccurrences } = await import('$lib/db/queries/agent-reads');
			const first = await listAgentTraceOccurrences({ organizationId: orgA, traceId: TRACE, limit: 1 });
			const second = await listAgentTraceOccurrences({ organizationId: orgA, traceId: TRACE, limit: 1, cursor: first.nextCursor });
			expect(second.occurrences[0].id).toBe(occ.coreProd);
		});
	});

	it('counts the TRUE number of projects when the trace spans more than the 50-name cap (m4)', async () => {
		const orgMany = randomUUID();
		const trace = hex32();
		const many = Array.from({ length: 52 }, () => randomUUID());
		await sql`insert into organizations (id, name, slug) values (${orgMany}, ${`tv-many-${suffix}`}, ${`tv-many-${suffix}`})`;
		await sql`insert into organization_members (organization_id, user_id, role) values (${orgMany}, ${memberUser}, 'engineer')`;
		try {
			for (const [i, pid] of many.entries()) {
				await sql`insert into projects (id, name, organization_id, api_key, api_key_hash) values (${pid}, ${`tv-m${String(i).padStart(2, '0')}-${suffix}`}, ${orgMany}, ${`k_m${i}_${suffix}`}, ${`h_m${i}_${suffix}`})`;
				const iss = randomUUID();
				await sql`insert into issues (id, project_id, environment, fingerprint, message, error_class) values (${iss}, ${pid}, 'production', ${`fp-${iss}`}, 'm', 'E')`;
				await sql`insert into error_occurrences (id, issue_id, environment, platform, trace_id, created_at) values (${randomUUID()}, ${iss}, 'production', 'node', ${trace}, ${at(i)}::timestamptz)`;
			}
			const data = await loadPage(memberUser, { id: orgMany, slug: `tv-many-${suffix}` }, trace);
			expect(data.projects).toHaveLength(50);
			expect(data.projectCount).toBe(52);
		} finally {
			await sql`delete from error_occurrences where issue_id in (select id from issues where project_id in ${sql(many)})`;
			await sql`delete from issues where project_id in ${sql(many)}`;
			await sql`delete from projects where organization_id = ${orgMany}`;
			await sql`delete from organization_members where organization_id = ${orgMany}`;
			await sql`delete from organizations where id = ${orgMany}`;
		}
	});
});
