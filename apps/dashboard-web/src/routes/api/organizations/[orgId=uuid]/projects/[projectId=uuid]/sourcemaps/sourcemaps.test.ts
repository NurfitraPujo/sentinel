import { describe, it, expect, vi, beforeEach } from 'vitest';
import { getTableName, sql } from 'drizzle-orm';
import { PgDialect } from 'drizzle-orm/pg-core';

/**
 * sentinel-sourcemaps §4.2 / §2.2 — `GET /api/organizations/{orgId}/projects/{projectId}/sourcemaps
 * ?release_version=…`, the list the releases page fetches on demand (flows R2, R3).
 *
 * The guard matrix is the releases loader's (`page.server.test.ts` beside it), transcribed to an
 * `[orgId]` route: `401`, `403` for a non-member of `orgId`, and **`404 Project not found` — the
 * loader's one string — for a project of another organization and for a member whose environment
 * scope is non-null**, never `403` and never this tree's `Project not found in this organization`.
 * Then `400 release_version is required` for a missing or empty parameter, `200 { releaseVersion,
 * files: [] }` for a release with none, and the row shape — five keys, no `storageKey`.
 *
 * The db is the loader test's recording double: every statement is captured with its
 * `from`/`where`/`orderBy` arguments and rendered through drizzle's `PgDialect`, so the "no
 * storageKey" claim is asserted on the SELECT list that would reach Postgres and not only on a
 * mocked return value. `requireOrgMembership` is real and reads through that double
 * (`organization_members`); the two access helpers are mocked at their module boundary, as the
 * loader test mocks them.
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
// Result rows per table, consumed in statement order: `organization_members` (the membership row),
// `projects` (the project-under-org lookup), `sourcemaps` (the list).
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

const { GET } = await import('./+server');

const ORG_ID = 'a1b2c3d4-0000-4000-8000-000000000001';
const PROJECT_ID = '0b1f0000-0000-4000-8000-000000000001';
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

function makeLocals(session: typeof USER | null = USER) {
	return {
		auth: async () => (session ? { user: { id: session.id, email: session.email } } : null),
	};
}

async function getWith(
	opts: { orgId?: string; projectId?: string; session?: typeof USER | null; query?: string } = {}
) {
	const query = opts.query === undefined ? '?release_version=v1.3.0' : opts.query;
	return await GET({
		params: { orgId: opts.orgId ?? ORG_ID, projectId: opts.projectId ?? PROJECT_ID },
		locals: makeLocals(opts.session === undefined ? USER : opts.session),
		url: new URL(`http://dashboard.test/api/organizations/${ORG_ID}/projects/${PROJECT_ID}/sourcemaps${query}`),
	} as any);
}

async function catchGet(opts: Parameters<typeof getWith>[0] = {}) {
	try {
		await getWith(opts);
	} catch (e) {
		return e as any;
	}
	return undefined;
}

const UPLOADED = new Date('2026-09-14T08:12:44.000Z');

/** Two rows, as the index order (`minified_path ASC`) would hand them back — with the column the
 * response must NOT carry, so a route that spread the row would fail the shape assertion. */
function mapRows() {
	return [
		{
			id: '0f2c0000-0000-4000-8000-000000000001',
			minifiedPath: '/assets/index-3f2a1c.js',
			sizeBytes: 4193821,
			sha256: '9'.repeat(64),
			uploadedAt: UPLOADED,
			storageKey: 'sourcemaps/p/r/index-3f2a1c.map',
		},
		{
			id: '1a7b0000-0000-4000-8000-000000000002',
			minifiedPath: '/assets/vendor-9a0d4e.js',
			sizeBytes: 11820044,
			sha256: 'e'.repeat(64),
			uploadedAt: UPLOADED,
			storageKey: 'sourcemaps/p/r/vendor-9a0d4e.map',
		},
	];
}

beforeEach(() => {
	vi.clearAllMocks();
	statements.length = 0;
	for (const key of Object.keys(queues)) delete queues[key];
	queues.organization_members = [[{ role: 'engineer' }]];
	queues.projects = [[{ id: PROJECT_ID }]];
	queues.sourcemaps = [[]];
	requireProjectAccessMock.mockResolvedValue({ projectId: PROJECT_ID, organizationId: ORG_ID });
	resolveEnvironmentScopeMock.mockResolvedValue(null);
});

describe('GET /api/organizations/[orgId]/projects/[projectId]/sourcemaps — the guard matrix (§2.2)', () => {
	it('401s an unauthenticated request before touching the database', async () => {
		const caught = await catchGet({ session: null });

		expect(caught.status).toBe(401);
		expect(caught.body.message).toBe('Unauthorized');
		expect(statements).toHaveLength(0);
		expect(requireProjectAccessMock).not.toHaveBeenCalled();
	});

	it('403s a caller with no membership row for orgId, before any project lookup', async () => {
		queues.organization_members = [[]];

		const caught = await catchGet();

		expect(caught.status).toBe(403);
		expect(caught.body.message).toBe('Forbidden: not a member of this organization');
		// The membership row is the caller's own, keyed off the session's user id and the URL's org.
		const [membership] = statementsFrom('organization_members');
		expect(render(membership.where!).params).toEqual([ORG_ID, USER.id]);
		expect(statementsFrom('projects')).toHaveLength(0);
		expect(requireProjectAccessMock).not.toHaveBeenCalled();
	});

	// The loader's cause 2 of 3 for the one string: the row exists in another organization the
	// caller may also belong to; the `organization_id = orgId` predicate is what keeps it from being
	// found here — and it is `404`, never `403`, and never `Project not found in this organization`.
	it('404s "Project not found" — the loader’s string, never 403 — for another organisation’s project under this orgId', async () => {
		queues.projects = [[]];

		const caught = await catchGet();

		expect(caught.status).toBe(404);
		expect(caught.status).not.toBe(403);
		expect(caught.body.message).toBe('Project not found');
		expect(caught.body.message).not.toBe('Project not found in this organization');

		const [lookup] = statementsFrom('projects');
		const { sql, params } = render(lookup.where!);
		expect(sql).toBe('("projects"."id" = $1 and "projects"."organization_id" = $2)');
		expect(params).toEqual([PROJECT_ID, ORG_ID]);
		expect(requireProjectAccessMock).not.toHaveBeenCalled();
		expect(resolveEnvironmentScopeMock).not.toHaveBeenCalled();
		expect(statementsFrom('sourcemaps')).toHaveLength(0);
	});

	it('propagates requireProjectAccess’s 403 for an org viewer with no project row — read is the level asked for', async () => {
		requireProjectAccessMock.mockRejectedValue({
			status: 403,
			body: { message: 'Forbidden: You do not have access to this organization' },
		});

		const caught = await catchGet();

		expect(caught.status).toBe(403);
		expect(requireProjectAccessMock).toHaveBeenCalledWith(USER.id, PROJECT_ID, 'read', {
			organizationId: ORG_ID,
		});
		expect(resolveEnvironmentScopeMock).not.toHaveBeenCalled();
		expect(statementsFrom('sourcemaps')).toHaveLength(0);
	});

	// Cause 3 of 3 — decision 20 (b) of sentinel-releases-snooze, inherited by §4.2 unchanged. The
	// member passed both project checks; the environment dimension withholds the list as "no such
	// project", never `403`.
	it('404s "Project not found" — the same string, never 403 — for a member with a non-null environment scope', async () => {
		resolveEnvironmentScopeMock.mockResolvedValue(['staging']);
		queues.sourcemaps = [mapRows()];

		const caught = await catchGet();

		expect(caught.status).toBe(404);
		expect(caught.status).not.toBe(403);
		expect(caught.body.message).toBe('Project not found');
		expect(resolveEnvironmentScopeMock).toHaveBeenCalledWith(USER.id, PROJECT_ID);
		// No scoped caller reaches the query.
		expect(statementsFrom('sourcemaps')).toHaveLength(0);
	});

	it('runs the guards in the releases loader’s order: membership, project, access helper, scope', async () => {
		const order: string[] = [];
		requireProjectAccessMock.mockImplementation(async () => {
			order.push('project-access');
			return { projectId: PROJECT_ID, organizationId: ORG_ID };
		});
		resolveEnvironmentScopeMock.mockImplementation(async () => {
			order.push('scope');
			return null;
		});

		await getWith();

		expect(order).toEqual(['project-access', 'scope']);
		expect(statements.map((s) => getTableName(s.from as any))).toEqual([
			'organization_members',
			'projects',
			'sourcemaps',
		]);
	});
});

describe('GET /api/organizations/[orgId]/projects/[projectId]/sourcemaps — the parameter and the body (§4.2)', () => {
	it('400s { message: "release_version is required" } when the parameter is missing', async () => {
		const caught = await catchGet({ query: '' });

		expect(caught.status).toBe(400);
		expect(caught.body).toEqual({ message: 'release_version is required' });
		expect(statementsFrom('sourcemaps')).toHaveLength(0);
	});

	it('400s the same string when the parameter is present but empty', async () => {
		const caught = await catchGet({ query: '?release_version=' });

		expect(caught.status).toBe(400);
		expect(caught.body).toEqual({ message: 'release_version is required' });
		expect(statementsFrom('sourcemaps')).toHaveLength(0);
	});

	it('answers the parameter only AFTER the guards — an unauthenticated caller with no parameter is a 401, not a 400', async () => {
		const caught = await catchGet({ session: null, query: '' });

		expect(caught.status).toBe(401);
	});

	it('200s { releaseVersion, files: [] } — shown, never null — for a release with no maps (flow R3)', async () => {
		queues.sourcemaps = [[]];

		const res = await getWith({ query: '?release_version=2026.09.13-11aa22' });
		const body = await res.json();

		expect(res.status).toBe(200);
		expect(body).toEqual({ releaseVersion: '2026.09.13-11aa22', files: [] });
		expect(body.files).not.toBeNull();
	});

	it('echoes the version byte-for-byte, decoded from the query and untrimmed', async () => {
		const res = await getWith({ query: '?release_version=' + encodeURIComponent(' v1.3.0+build.7 ') });
		const body = await res.json();

		expect(body.releaseVersion).toBe(' v1.3.0+build.7 ');
		const [list] = statementsFrom('sourcemaps');
		expect(render(list.where!).params).toEqual([PROJECT_ID, ' v1.3.0+build.7 ']);
	});

	it('returns the §4.2 row shape — id, minifiedPath, sizeBytes, sha256, uploadedAt — and no storageKey (flow R2)', async () => {
		queues.sourcemaps = [mapRows()];

		const res = await getWith();
		const body = await res.json();

		expect(res.status).toBe(200);
		expect(body).toEqual({
			releaseVersion: 'v1.3.0',
			files: [
				{
					id: '0f2c0000-0000-4000-8000-000000000001',
					minifiedPath: '/assets/index-3f2a1c.js',
					sizeBytes: 4193821,
					sha256: '9'.repeat(64),
					uploadedAt: '2026-09-14T08:12:44.000Z',
				},
				{
					id: '1a7b0000-0000-4000-8000-000000000002',
					minifiedPath: '/assets/vendor-9a0d4e.js',
					sizeBytes: 11820044,
					sha256: 'e'.repeat(64),
					uploadedAt: '2026-09-14T08:12:44.000Z',
				},
			],
		});
		for (const file of body.files) {
			expect(Object.keys(file).sort()).toEqual(['id', 'minifiedPath', 'sha256', 'sizeBytes', 'uploadedAt']);
			expect(file).not.toHaveProperty('storageKey');
			expect(file).not.toHaveProperty('storage_key');
		}
	});

	it('never SELECTs storage_key, and orders minified_path ASC over (project_id, release_version)', async () => {
		await getWith();

		const [list] = statementsFrom('sourcemaps');
		const selected = Object.values(list.fields as Record<string, { name: string }>).map((c) => c.name);
		expect(selected.sort()).toEqual(['id', 'minified_path', 'sha256', 'size_bytes', 'uploaded_at']);
		expect(selected).not.toContain('storage_key');
		expect(render(list.orderBy!).sql).toBe('"sourcemaps"."minified_path" asc');
		expect(render(list.where!)).toEqual({
			sql: '("sourcemaps"."project_id" = $1 and "sourcemaps"."release_version" = $2)',
			params: [PROJECT_ID, 'v1.3.0'],
		});
		expect(list.limit).toBeUndefined();
	});
});
