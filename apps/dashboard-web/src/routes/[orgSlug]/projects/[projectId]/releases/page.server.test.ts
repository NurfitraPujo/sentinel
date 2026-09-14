import { describe, it, expect, vi, beforeEach } from 'vitest';
import { getTableName, sql } from 'drizzle-orm';
import { PgDialect } from 'drizzle-orm/pg-core';

/**
 * sentinel-releases-snooze §4.4 / §2 — `GET /{orgSlug}/projects/{projectId}/releases`, the new page's
 * loader. Flows R1–R5 on the loader side.
 *
 * The guard matrix is the point of this file, and the case that matters most is the one a reviewer
 * reads past: **`404 Project not found` is one string with three causes** — an unknown project, a
 * project of another organization under this slug, and a member whose environment scope is non-null
 * (decision 20 (b)). Each is asserted as THAT literal, never the settings loader's `Project not found
 * in this organization`, and the scoped case is asserted as never-`403`.
 *
 * Below the guards: `releases: []` is shown, never `null`; `truncated` flips at 201; and
 * `issuesIntroduced` is ONE grouped statement for the page, counted across every environment. The
 * db is a recording double — every statement the loader and `releases.ts` build is captured with its
 * `from`/`where`/`orderBy`/`limit`/`groupBy` arguments and rendered through drizzle's own
 * `PgDialect`, so the assertions are about the SQL that would reach Postgres (which table, which
 * predicate, grouped or not, how many statements) rather than about a mocked return value. What
 * that SQL does against a real database — the index range scan on `idx_releases_project_first_seen`
 * and the grouped count on `idx_issues_project_first_release` — is item 7's M2, not this file's.
 *
 * sentinel-sourcemaps §4.6 (flows R1, R4, R5 on the loader side): `sourcemapCount` on every row from
 * ONE more grouped statement, over `sourcemaps` this time, `0` — never absent — for a version with no
 * rows; `org.id` beside `slug` and `name` (the page needs it for §4.2's URL); and the scoped member's
 * `404` unchanged, with the count withheld along with the page.
 */

interface RecordedStatement {
	fields: unknown;
	from?: unknown;
	where?: unknown;
	orderBy?: unknown;
	limit?: unknown;
	groupBy?: unknown;
}

const statements: RecordedStatement[] = [];
// Result rows per table, consumed in statement order: `releases` is asked twice (the list, then the
// COUNT(*)), `projects`, `issues` and `sourcemaps` once each.
const queues: Record<string, unknown[][]> = {};

const dbMock = {
	select: vi.fn((fields: unknown) => {
		const stmt: RecordedStatement = { fields };
		statements.push(stmt);
		const chain: any = {};
		for (const name of ['from', 'where', 'orderBy', 'limit', 'groupBy'] as const) {
			chain[name] = vi.fn((arg: unknown) => {
				stmt[name] = arg;
				return chain;
			});
		}
		chain.then = (resolve: (rows: unknown[]) => unknown, reject: (e: unknown) => unknown) => {
			const table = getTableName(stmt.from as any);
			const queue = queues[table] ?? [];
			const rows = queue.length > 0 ? queue.shift()! : [];
			return Promise.resolve(rows).then(resolve, reject);
		};
		return chain;
	}),
};

vi.mock('$lib/server/db', () => ({ db: dbMock }));

const requireProjectAccessMock = vi.fn();
vi.mock('$lib/server/issue-access', () => ({
	requireProjectAccess: requireProjectAccessMock,
}));

const resolveEnvironmentScopeMock = vi.fn();
vi.mock('$lib/server/environment-access', () => ({
	resolveEnvironmentScope: resolveEnvironmentScopeMock,
}));

const { load } = await import('./+page.server');

const ORG = { id: 'org-1', name: 'Acme', slug: 'acme' };
const PROJECT = { id: '0b1f0000-0000-4000-8000-000000000001', name: 'checkout' };
const USER = { id: 'user-1', email: 'engineer@example.com' };

const dialect = new PgDialect();
/** A recorded fragment — a predicate, an ordering, a column — as the SQL text Postgres would get. */
function render(fragment: unknown): { sql: string; params: unknown[] } {
	const q = dialect.sqlToQuery(sql`${fragment as any}`);
	return { sql: q.sql, params: q.params };
}

function statementsFrom(table: string) {
	return statements.filter((s) => getTableName(s.from as any) === table);
}

function makeLocals(opts: { session?: typeof USER | null; currentOrg?: typeof ORG | undefined } = {}) {
	const session = opts.session === undefined ? USER : opts.session;
	return {
		auth: async () => (session ? { user: { id: session.id, email: session.email } } : null),
		currentOrg: 'currentOrg' in opts ? opts.currentOrg : ORG,
	};
}

async function loadWith(
	opts: { orgSlug?: string; projectId?: string; session?: typeof USER | null; currentOrg?: typeof ORG | undefined } = {}
) {
	return await load({
		params: { orgSlug: opts.orgSlug ?? ORG.slug, projectId: opts.projectId ?? PROJECT.id },
		locals: makeLocals(opts),
	} as any);
}

async function catchLoad(opts: Parameters<typeof loadWith>[0] = {}) {
	try {
		await loadWith(opts);
	} catch (e) {
		return e as any;
	}
	return undefined;
}

const HOUR = 60 * 60 * 1000;

/** `n` releases, newest first, as the index order would hand them back. */
function releaseRows(n: number) {
	return Array.from({ length: n }, (_, i) => ({
		version: `v1.${n - i}.0`,
		firstSeen: new Date(Date.UTC(2026, 8, 13, 12) - i * HOUR),
		lastSeen: new Date(Date.UTC(2026, 8, 13, 12) - i * HOUR + 30 * 60 * 1000),
		occurrenceCount: 10 + i,
	}));
}

beforeEach(() => {
	vi.clearAllMocks();
	statements.length = 0;
	for (const key of Object.keys(queues)) delete queues[key];
	queues.projects = [[PROJECT]];
	queues.releases = [[], [{ total: 0 }]];
	queues.issues = [[]];
	queues.sourcemaps = [[]];
	requireProjectAccessMock.mockResolvedValue({ projectId: PROJECT.id, organizationId: ORG.id });
	resolveEnvironmentScopeMock.mockResolvedValue(null);
});

describe('/[orgSlug]/projects/[projectId]/releases load — the guard matrix (§2)', () => {
	it('401s an unauthenticated request before touching the database', async () => {
		const caught = await catchLoad({ session: null });

		expect(caught.status).toBe(401);
		expect(statements).toHaveLength(0);
		expect(requireProjectAccessMock).not.toHaveBeenCalled();
	});

	it('403s "Forbidden: Unauthorized access to organization" when the slug is not the current org', async () => {
		const caught = await catchLoad({ orgSlug: 'other-org' });

		expect(caught.status).toBe(403);
		expect(caught.body.message).toBe('Forbidden: Unauthorized access to organization');
		expect(statements).toHaveLength(0);
	});

	it('403s with the same string when there is no current org at all', async () => {
		const caught = await catchLoad({ currentOrg: undefined });

		expect(caught.status).toBe(403);
		expect(caught.body.message).toBe('Forbidden: Unauthorized access to organization');
	});

	it('propagates requireProjectAccess’s 403 for a non-member — read is the level asked for', async () => {
		requireProjectAccessMock.mockRejectedValue({
			status: 403,
			body: { message: 'Forbidden: You do not have access to this organization' },
		});

		const caught = await catchLoad();

		expect(caught.status).toBe(403);
		expect(caught.body.message).toBe('Forbidden: You do not have access to this organization');
		expect(requireProjectAccessMock).toHaveBeenCalledWith(USER.id, PROJECT.id, 'read', {
			organizationId: ORG.id,
		});
		// Refused at the project dimension: nothing below it runs.
		expect(resolveEnvironmentScopeMock).not.toHaveBeenCalled();
		expect(statementsFrom('releases')).toHaveLength(0);
	});

	// Cause 1 of 3 for the one string.
	it('404s "Project not found" — that string — for an unknown project, before any access helper', async () => {
		queues.projects = [[]];

		const caught = await catchLoad({ projectId: '0b1f0000-0000-4000-8000-00000000dead' });

		expect(caught.status).toBe(404);
		expect(caught.body.message).toBe('Project not found');
		expect(caught.body.message).not.toBe('Project not found in this organization');
		expect(requireProjectAccessMock).not.toHaveBeenCalled();
		expect(resolveEnvironmentScopeMock).not.toHaveBeenCalled();
		expect(statementsFrom('releases')).toHaveLength(0);
	});

	// Cause 2 of 3. `requireProjectAccess` does NOT make this check (it verifies membership in the
	// PROJECT's organization, not that the project sits under `orgSlug`'s), so the loader's own
	// lookup carries `projects.organization_id = currentOrg.id` — and an id that exists in another
	// organization the caller also belongs to is an empty result here, i.e. the same `404`.
	it('404s "Project not found" — the same string — for another organization’s project under this slug', async () => {
		// The row exists in org-2; the predicate below is what keeps it from being found under acme.
		queues.projects = [[]];

		const caught = await catchLoad();

		expect(caught.status).toBe(404);
		expect(caught.body.message).toBe('Project not found');

		const [lookup] = statementsFrom('projects');
		const { sql, params } = render(lookup.where);
		expect(sql).toContain('"projects"."id" = $1');
		expect(sql).toContain('"projects"."organization_id" = $2');
		expect(params).toEqual([PROJECT.id, ORG.id]);
		expect(requireProjectAccessMock).not.toHaveBeenCalled();
	});

	// Cause 3 of 3 — decision 20 (b). The member passed BOTH the project checks; what withholds the
	// page is the environment dimension, and it withholds it as "no such project", never `403`.
	it('404s "Project not found" — the same string, never 403 — for a member with a non-null environment scope', async () => {
		resolveEnvironmentScopeMock.mockResolvedValue(['staging']);
		queues.releases = [releaseRows(3), [{ total: 3 }]];

		const caught = await catchLoad();

		expect(caught.status).toBe(404);
		expect(caught.status).not.toBe(403);
		expect(caught.body.message).toBe('Project not found');
		expect(requireProjectAccessMock).toHaveBeenCalledWith(USER.id, PROJECT.id, 'read', {
			organizationId: ORG.id,
		});
		expect(resolveEnvironmentScopeMock).toHaveBeenCalledWith(USER.id, PROJECT.id);
		// No scoped caller reaches the query — which is why it carries no scope clause (decision 36).
		expect(statementsFrom('releases')).toHaveLength(0);
		expect(statementsFrom('issues')).toHaveLength(0);
		// sentinel-sourcemaps §2.2: the map counts are withheld with the page, unchanged (flow R4).
		expect(statementsFrom('sourcemaps')).toHaveLength(0);
	});

	it('resolves the scope only after requireProjectAccess has passed — the settings loader’s order', async () => {
		const order: string[] = [];
		requireProjectAccessMock.mockImplementation(async () => {
			order.push('project-access');
			return { projectId: PROJECT.id, organizationId: ORG.id };
		});
		resolveEnvironmentScopeMock.mockImplementation(async () => {
			order.push('scope');
			return null;
		});

		await loadWith();

		expect(order).toEqual(['project-access', 'scope']);
		expect(getTableName(statements[0].from as any)).toBe('projects');
	});
});

describe('/[orgSlug]/projects/[projectId]/releases load — page data (§4.4)', () => {
	it('returns org, project and releases: [] — shown, never null — with truncated false for an empty project', async () => {
		queues.releases = [[], [{ total: 0 }]];

		const result: any = await loadWith();

		expect(result.org).toEqual({ id: ORG.id, slug: ORG.slug, name: ORG.name });
		expect(result.project).toEqual({ id: PROJECT.id, name: PROJECT.name });
		expect(Array.isArray(result.releases)).toBe(true);
		expect(result.releases).toEqual([]);
		expect(result.releases).not.toBeNull();
		expect(result.truncated).toBe(false);
		// Nothing to count against: neither grouped statement is even built for an empty page.
		expect(statementsFrom('issues')).toHaveLength(0);
		expect(statementsFrom('sourcemaps')).toHaveLength(0);
	});

	it('lists the releases first_seen DESC with a limit of 200 and truncated false at exactly 200', async () => {
		queues.releases = [releaseRows(200), [{ total: 200 }]];
		queues.issues = [[]];

		const result: any = await loadWith();

		expect(result.releases).toHaveLength(200);
		expect(result.truncated).toBe(false);

		const [list, count] = statementsFrom('releases');
		expect(list.limit).toBe(200);
		expect(render(list.orderBy!).sql).toBe('"releases"."first_seen" desc');
		expect(render(list.where!)).toEqual({ sql: '"releases"."project_id" = $1', params: [PROJECT.id] });
		// The COUNT(*) beside the list, over the same project predicate and nothing else.
		expect(render(count.where!)).toEqual({ sql: '"releases"."project_id" = $1', params: [PROJECT.id] });
		expect(count.limit).toBeUndefined();
	});

	it('flips truncated to true at 201 rows and returns the 200 newest, newest first', async () => {
		const rows = releaseRows(200);
		queues.releases = [rows, [{ total: 201 }]];
		queues.issues = [[]];

		const result: any = await loadWith();

		expect(result.truncated).toBe(true);
		expect(result.releases).toHaveLength(200);
		expect(result.releases[0].version).toBe('v1.200.0');
		expect(result.releases[199].version).toBe('v1.1.0');
		for (let i = 1; i < result.releases.length; i++) {
			expect(result.releases[i - 1].firstSeen.getTime()).toBeGreaterThan(
				result.releases[i].firstSeen.getTime()
			);
		}
		// Every §4.4 key on every row, and `issuesIntroduced` is 0 — not absent — with no matching
		// issue; sentinel-sourcemaps §4.6's `sourcemapCount` likewise 0, not absent, with no map.
		expect(result.releases[0]).toEqual({
			version: 'v1.200.0',
			firstSeen: rows[0].firstSeen,
			lastSeen: rows[0].lastSeen,
			occurrenceCount: 10,
			issuesIntroduced: 0,
			sourcemapCount: 0,
		});
	});

	it('counts issuesIntroduced across every environment — the grouped predicate carries no scope clause', async () => {
		queues.releases = [releaseRows(3), [{ total: 3 }]];
		// One production issue and two staging issues on v1.3.0, one on v1.1.0, none on v1.2.0 — the
		// database has already summed across environments; the loader never asked it to split.
		queues.issues = [
			[
				{ version: 'v1.3.0', issuesIntroduced: 3 },
				{ version: 'v1.1.0', issuesIntroduced: 1 },
			],
		];

		const result: any = await loadWith();

		expect(result.releases.map((r: any) => [r.version, r.issuesIntroduced])).toEqual([
			['v1.3.0', 3],
			['v1.2.0', 0],
			['v1.1.0', 1],
		]);

		const [grouped] = statementsFrom('issues');
		const { sql, params } = render(grouped.where!);
		expect(sql).toBe(
			'("issues"."project_id" = $1 and "issues"."first_release_version" is not null and "issues"."first_release_version" in ($2, $3, $4))'
		);
		expect(params).toEqual([PROJECT.id, 'v1.3.0', 'v1.2.0', 'v1.1.0']);
		expect(sql).not.toContain('environment');
		expect(resolveEnvironmentScopeMock).toHaveBeenCalledTimes(1);
	});

	it('produces the counts with ONE grouped statement for 200 rows, not one per row', async () => {
		queues.releases = [releaseRows(200), [{ total: 200 }]];
		queues.issues = [[{ version: 'v1.200.0', issuesIntroduced: 7 }]];

		const result: any = await loadWith();

		expect(result.releases).toHaveLength(200);
		expect(result.releases[0].issuesIntroduced).toBe(7);

		const grouped = statementsFrom('issues');
		expect(grouped).toHaveLength(1);
		expect(render(grouped[0].groupBy!).sql).toBe('"issues"."first_release_version"');
		expect(render(grouped[0].where!).params).toHaveLength(201);
		// Five statements for the whole page — the project lookup, the list, its COUNT(*), the
		// grouped issue count, the grouped map count — whatever the row count.
		expect(statements).toHaveLength(5);
		expect(statements.map((s) => getTableName(s.from as any))).toEqual([
			'projects',
			'releases',
			'releases',
			'issues',
			'sourcemaps',
		]);
	});
});

describe('/[orgSlug]/projects/[projectId]/releases load — sourcemapCount and org.id (sentinel-sourcemaps §4.6)', () => {
	it('returns org.id beside slug and name — the page needs it for §4.2’s URL', async () => {
		const result: any = await loadWith();

		expect(result.org).toEqual({ id: ORG.id, slug: ORG.slug, name: ORG.name });
		expect(result.org.id).toBe('org-1');
	});

	it('puts sourcemapCount on every row, 0 — never absent — for a version with no sourcemaps row (flow R1)', async () => {
		queues.releases = [releaseRows(3), [{ total: 3 }]];
		queues.issues = [[]];
		// Two maps on v1.3.0, one on v1.1.0, none on v1.2.0 — the database has already grouped.
		queues.sourcemaps = [
			[
				{ version: 'v1.3.0', sourcemapCount: 2 },
				{ version: 'v1.1.0', sourcemapCount: 1 },
			],
		];

		const result: any = await loadWith();

		expect(result.releases.map((r: any) => [r.version, r.sourcemapCount])).toEqual([
			['v1.3.0', 2],
			['v1.2.0', 0],
			['v1.1.0', 1],
		]);
		for (const row of result.releases) {
			expect(row).toHaveProperty('sourcemapCount');
			expect(typeof row.sourcemapCount).toBe('number');
		}
	});

	it('counts sourcemapCount with ONE grouped statement over the page’s versions, never one per row', async () => {
		queues.releases = [releaseRows(200), [{ total: 200 }]];
		queues.issues = [[]];
		queues.sourcemaps = [[{ version: 'v1.200.0', sourcemapCount: 7 }]];

		const result: any = await loadWith();

		expect(result.releases).toHaveLength(200);
		expect(result.releases[0].sourcemapCount).toBe(7);
		expect(result.releases[1].sourcemapCount).toBe(0);

		const grouped = statementsFrom('sourcemaps');
		expect(grouped).toHaveLength(1);
		const { sql, params } = render(grouped[0].where!);
		// `(project_id, release_version)` — `idx_sourcemaps_project_release`'s two columns, and the
		// page's own versions as the IN list (decision 16: a map-only version is never looked up).
		expect(sql).toMatch(
			/^\("sourcemaps"\."project_id" = \$1 and "sourcemaps"\."release_version" in \(\$2(, \$\d+){199}\)\)$/
		);
		expect(params).toHaveLength(201);
		expect(params[0]).toBe(PROJECT.id);
		expect(params.slice(1)).toEqual(result.releases.map((r: any) => r.version));
		expect(render(grouped[0].groupBy!).sql).toBe('"sourcemaps"."release_version"');
		expect(sql).not.toContain('environment');
	});

	it('does not list a version that has maps but no releases row (decision 16, flow R5)', async () => {
		queues.releases = [releaseRows(1), [{ total: 1 }]];
		queues.issues = [[]];
		// A map-only version could only come back if it had been asked for; the IN list is the page's.
		queues.sourcemaps = [[{ version: 'v1.1.0', sourcemapCount: 3 }]];

		const result: any = await loadWith();

		expect(result.releases.map((r: any) => r.version)).toEqual(['v1.1.0']);
		const [grouped] = statementsFrom('sourcemaps');
		expect(render(grouped.where!).params).toEqual([PROJECT.id, 'v1.1.0']);
	});
});
