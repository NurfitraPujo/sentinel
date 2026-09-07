import { describe, it, expect, beforeAll, afterAll, beforeEach } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

/**
 * sentinel-project-environments — contract §11's boundary "**The §4.1–§4.3, §4.3a and §4.10
 * routes**", which that table calls *"the highest boundary that reaches every one of them"*: auth,
 * org/project scoping, the `404`-not-`403` rule, the `409`, the `422` cap, the `422` owner guard and
 * the empty-`[]` case are all observable in the response.
 *
 * Driven against a **real migrated Postgres**, with only `locals.auth()` faked, because most of what
 * is asserted here is a claim about rows that a mocked query layer cannot make:
 * `PATCH {isDefault:true}` clearing the previous default *in one write* is only observable by
 * setting it twice and reading the table back, and §4.1's `{"environments": []}` is only meaningful
 * against a project whose registry is genuinely empty — which, under decision 8, is the state of
 * every project on the day this ships, because no registry row is ever created by traffic.
 *
 * **The registry starts empty and is seeded through `POST`**, never by fixtures: that is the
 * precondition the work item states, and seeding rows directly would test a state the API cannot
 * produce.
 *
 * Point DATABASE_URL at a DISPOSABLE database — never the shared dev one (CLAUDE.md):
 *
 *   docker run -d --name sentinel-envroutes-pg -p 15442:5432 \
 *     -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme -e POSTGRES_DB=sentinel postgres:15-alpine
 *   cd packages/db-migrations && DB_URL_DASHBOARD=postgres://sentinel:changeme@localhost:15442/sentinel?sslmode=disable \
 *     go run ./cmd/migrate up -target=dashboard
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:changeme@localhost:15442/sentinel \
 *     pnpm vitest run src/routes/api/organizations/environments.integration.test.ts
 *
 * Skipped when nothing answers. `ENVIRONMENT_ROUTES_INTEGRATION_REQUIRED=1` makes that a failure.
 */
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.ENVIRONMENT_ROUTES_INTEGRATION_REQUIRED === '1';

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
		`ENVIRONMENT_ROUTES_INTEGRATION_REQUIRED=1 but no database answered at ${connectionString}.`
	);
}

const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const otherOrgId = randomUUID();
const projectId = randomUUID();
const secondProjectId = randomUUID();
const foreignProjectId = randomUUID();

const ownerId = randomUUID(); // org owner: write + manage_members, and cannot be narrowed
const adminId = randomUUID(); // org admin: write + manage_members
const engineerId = randomUUID(); // org engineer: write, NO manage_members
const supportId = randomUUID(); // org support: read only, NO write
const outsiderId = randomUUID(); // no membership anywhere

/** The only thing faked: the Auth.js session. Everything below it is the real stack. */
function locals(userId: string | null) {
	return { auth: async () => (userId ? { user: { id: userId } } : null) } as never;
}

function jsonRequest(body: unknown) {
	return new Request('http://x', { method: 'POST', body: JSON.stringify(body) }) as never;
}

const params = { orgId, projectId };

describe.skipIf(!dbReachable)('environment routes (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });

		for (const [id, label] of [
			[orgId, 'envroutes'],
			[otherOrgId, 'envroutes-other'],
		] as const) {
			await sql`insert into organizations (id, name, slug) values (${id}, ${`${label}-${suffix}`}, ${`${label}-${suffix}`})`;
		}
		for (const [id, org, label] of [
			[projectId, orgId, 'p1'],
			[secondProjectId, orgId, 'p2'],
			[foreignProjectId, otherOrgId, 'pf'],
		] as const) {
			await sql`
				insert into projects (id, name, organization_id, api_key, api_key_hash)
				values (${id}, ${`envroutes-${label}-${suffix}`}, ${org}, ${`k_${label}_${suffix}`}, ${`h_${label}_${suffix}`})
			`;
		}
		for (const [id, role] of [
			[ownerId, 'owner'],
			[adminId, 'admin'],
			[engineerId, 'engineer'],
			[supportId, 'support'],
		] as const) {
			await sql`insert into "user" (id, name, email) values (${id}, ${role}, ${`${role}-${suffix}@example.test`})`;
			await sql`insert into organization_members (organization_id, user_id, role) values (${orgId}, ${id}, ${role})`;
		}
		await sql`insert into "user" (id, name, email) values (${outsiderId}, ${'Outsider'}, ${`outsider-${suffix}@example.test`})`;
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from project_environment_grants where project_id in (${projectId}, ${secondProjectId}, ${foreignProjectId})`;
			await sql`delete from project_environments where project_id in (${projectId}, ${secondProjectId}, ${foreignProjectId})`;
			await sql`delete from organization_members where organization_id in (${orgId}, ${otherOrgId})`;
			await sql`delete from projects where organization_id in (${orgId}, ${otherOrgId})`;
			await sql`delete from "user" where id in (${ownerId}, ${adminId}, ${engineerId}, ${supportId}, ${outsiderId})`;
			await sql`delete from organizations where id in (${orgId}, ${otherOrgId})`;
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	beforeEach(async () => {
		// Every test starts from the state decision 8 says every project is in on day one: an EMPTY
		// registry. Nothing here seeds rows; the tests that need them declare them through POST.
		await sql`delete from project_environment_grants where project_id in (${projectId}, ${secondProjectId}, ${foreignProjectId})`;
		await sql`delete from project_environments where project_id in (${projectId}, ${secondProjectId}, ${foreignProjectId})`;
	});

	async function importRegistry() {
		return await import('./[orgId]/projects/[projectId]/environments/+server');
	}
	async function importEnvPatch() {
		return await import('./[orgId]/projects/[projectId]/environments/[envId]/+server');
	}
	async function importGrants() {
		return await import('./[orgId]/projects/[projectId]/environment-grants/+server');
	}
	async function importGrantUser() {
		return await import('./[orgId]/projects/[projectId]/environment-grants/[userId]/+server');
	}

	async function declare(
		body: Record<string, unknown>,
		as: string = adminId,
		on: string = projectId
	) {
		const { POST } = await importRegistry();
		return await POST({
			params: { orgId, projectId: on },
			request: jsonRequest(body),
			locals: locals(as),
		} as never);
	}

	// ---------------------------------------------------------------- §4.1 / §4.3, criterion 1

	it('§4.1: returns {"environments": []} — not null, not 404 — for a project with nothing registered', async () => {
		const { GET } = await importRegistry();
		const res = await GET({
			params,
			locals: locals(supportId),
			url: new URL('http://x/api/organizations/o/projects/p/environments'),
		} as never);

		expect(res.status).toBe(200);
		const body = await res.json();
		expect(body).toEqual({ environments: [] });
		expect(body.environments).not.toBeNull();
	});

	it("§4.3: POST 'pre-prod' succeeds — decision 10's widened accepted set", async () => {
		const res = await declare({ name: 'pre-prod' });
		expect(res.status).toBe(201);
		const body = await res.json();
		expect(body.name).toBe('pre-prod');
		// displayName defaults to name; both timestamps are null until the processor first touches
		// the row (§4.8), and createdAt is always non-null because a person created it (decision 8).
		expect(body.displayName).toBe('pre-prod');
		expect(body.firstSeenAt).toBeNull();
		expect(body.lastSeenAt).toBeNull();
		expect(body.createdAt).toBeTruthy();
	});

	it('§4.3: POST rejects a name outside the accepted set with the exact §7 message', async () => {
		await expect(declare({ name: 'Production' })).rejects.toMatchObject({
			status: 400,
			body: { message: 'name must match ^[a-z0-9][a-z0-9._-]{0,49}$' },
		});
		await expect(declare({ name: '-prod' })).rejects.toMatchObject({ status: 400 });
		await expect(declare({ name: 'a'.repeat(51) })).rejects.toMatchObject({ status: 400 });
	});

	it('§4.3: POST of a duplicate name is 409, not a second row', async () => {
		expect((await declare({ name: 'production' })).status).toBe(201);
		await expect(declare({ name: 'production' })).rejects.toMatchObject({
			status: 409,
			body: { message: 'Environment already exists' },
		});

		const rows = await sql`select id from project_environments where project_id = ${projectId}`;
		expect(rows.length).toBe(1);
	});

	it('§4.3: POST at MAX_ENVIRONMENTS_PER_PROJECT is 422', async () => {
		const { MAX_ENVIRONMENTS_PER_PROJECT } = await import('$lib/db/queries/environments');
		// Fill through the API itself, so the cap is asserted against the state the API can produce.
		for (let i = 0; i < MAX_ENVIRONMENTS_PER_PROJECT; i++) {
			expect((await declare({ name: `env-${i}` })).status).toBe(201);
		}
		await expect(declare({ name: 'one-too-many' })).rejects.toMatchObject({
			status: 422,
			body: { message: 'Environment limit reached for this project' },
		});
	});

	it('§4.2: PATCH {isDefault:true} clears the previous default in one write — set twice, exactly one default remains', async () => {
		const first = await (await declare({ name: 'staging', isDefault: true })).json();
		const second = await (await declare({ name: 'production' })).json();

		const { PATCH } = await importEnvPatch();
		const res = await PATCH({
			params: { orgId, projectId, envId: second.id },
			request: jsonRequest({ isDefault: true }),
			locals: locals(adminId),
		} as never);
		expect(res.status).toBe(200);
		expect((await res.json()).isDefault).toBe(true);

		const defaults = await sql<{ id: string }[]>`
			select id from project_environments where project_id = ${projectId} and is_default
		`;
		expect(defaults.length).toBe(1);
		expect(defaults[0].id).toBe(second.id);

		// And the first one really was cleared rather than merely out-ranked.
		const [firstRow] = await sql<{ is_default: boolean }[]>`
			select is_default from project_environments where id = ${first.id}
		`;
		expect(firstRow.is_default).toBe(false);
	});

	it('§4.2: a body with no recognised key is 400 No changes supplied, and an unknown envId is 404', async () => {
		const env = await (await declare({ name: 'staging' })).json();
		const { PATCH } = await importEnvPatch();

		await expect(
			PATCH({
				params: { orgId, projectId, envId: env.id },
				request: jsonRequest({ nothingWeKnow: true }),
				locals: locals(adminId),
			} as never)
		).rejects.toMatchObject({ status: 400, body: { message: 'No changes supplied' } });

		await expect(
			PATCH({
				params: { orgId, projectId, envId: randomUUID() },
				request: jsonRequest({ hidden: true }),
				locals: locals(adminId),
			} as never)
		).rejects.toMatchObject({
			status: 404,
			body: { message: 'Environment not found in this project' },
		});
	});

	// ------------------------------------------------------------------------ §4.1's scope rule

	it('§4.1: the default view is environment-scope-filtered and excludes hidden; includeHidden=true is neither', async () => {
		await declare({ name: 'production' });
		await declare({ name: 'staging' });
		const canary = await (await declare({ name: 'canary' })).json();

		const { PATCH } = await importEnvPatch();
		await PATCH({
			params: { orgId, projectId, envId: canary.id },
			request: jsonRequest({ hidden: true }),
			locals: locals(adminId),
		} as never);

		const { PUT } = await importGrantUser();
		await PUT({
			params: { orgId, projectId, userId: engineerId },
			request: jsonRequest({ environments: ['staging', 'canary'] }),
			locals: locals(adminId),
		} as never);

		const { GET } = await importRegistry();
		const scoped = await (
			await GET({
				params,
				locals: locals(engineerId),
				url: new URL('http://x/e'),
			} as never)
		).json();
		// staging: granted and visible. production: not granted. canary: granted but hidden, and
		// hidden removes a name from SELECTORS (decision 43) — which is what this view is.
		expect(scoped.environments.map((e: { name: string }) => e.name)).toEqual(['staging']);

		const management = await (
			await GET({
				params,
				locals: locals(engineerId),
				url: new URL('http://x/e?includeHidden=true'),
			} as never)
		).json();
		// Not scope-filtered, hidden included: an environment's NAME is not protected, its DATA is.
		expect(management.environments.map((e: { name: string }) => e.name).sort()).toEqual([
			'canary',
			'production',
			'staging',
		]);
	});

	it('§4.1: only the literal string true counts as includeHidden — a mistyped value is the ordinary scoped list, never an error', async () => {
		const { GET } = await importRegistry();
		for (const raw of ['1', 'yes', 'TRUE', '']) {
			const res = await GET({
				params,
				// `support` holds read and NOT write, so if any of these were parsed as true the
				// request would 403 instead of returning the scoped list.
				locals: locals(supportId),
				url: new URL(`http://x/e?includeHidden=${raw}`),
			} as never);
			expect(res.status).toBe(200);
		}
	});

	it('§4.1: literal includeHidden=true without write is 403 with §7s write message', async () => {
		const { GET } = await importRegistry();
		await expect(
			GET({
				params,
				locals: locals(supportId),
				url: new URL('http://x/e?includeHidden=true'),
			} as never)
		).rejects.toMatchObject({
			status: 403,
			body: { message: 'Forbidden: write permission required to manage environments' },
		});
	});

	// -------------------------------------------------------------- §4.2 / §4.3 auth, criterion 2

	it('§4.2/§4.3: a member without write gets 403', async () => {
		await expect(declare({ name: 'staging' }, supportId)).rejects.toMatchObject({
			status: 403,
			body: { message: 'Forbidden: write permission required to manage environments' },
		});

		const env = await (await declare({ name: 'staging' })).json();
		const { PATCH } = await importEnvPatch();
		await expect(
			PATCH({
				params: { orgId, projectId, envId: env.id },
				request: jsonRequest({ hidden: true }),
				locals: locals(supportId),

			} as never)
		).rejects.toMatchObject({ status: 403 });
	});

	it('§2.1: no session is 401, a non-member is 403, and a project in another organization is 404', async () => {
		const { GET } = await importRegistry();
		const url = new URL('http://x/e');

		await expect(GET({ params, locals: locals(null), url } as never)).rejects.toMatchObject({
			status: 401,
		});
		await expect(GET({ params, locals: locals(outsiderId), url } as never)).rejects.toMatchObject({
			status: 403,
			body: { message: 'Forbidden: not a member of this organization' },
		});
		// The project exists — in a different organization. Indistinguishable from absent.
		await expect(
			GET({ params: { orgId, projectId: foreignProjectId }, locals: locals(adminId), url } as never)
		).rejects.toMatchObject({
			status: 404,
			body: { message: 'Project not found in this organization' },
		});
	});

	// --------------------------------------------------------------------- §4.10, criterion 2

	it('§4.10 PUT: refuses [] with 400 and an absent environments key with the same 400', async () => {
		await declare({ name: 'staging' });
		const { PUT } = await importGrantUser();

		for (const body of [{ environments: [] }, {}, { environments: 'staging' }]) {
			await expect(
				PUT({
					params: { orgId, projectId, userId: engineerId },
					request: jsonRequest(body),
					locals: locals(adminId),
				} as never)
			).rejects.toMatchObject({
				status: 400,
				body: { message: 'environments must contain at least one environment' },
			});
		}
	});

	it('§4.10 PUT: refuses an org owner with 422', async () => {
		await declare({ name: 'staging' });
		const { PUT } = await importGrantUser();
		await expect(
			PUT({
				params: { orgId, projectId, userId: ownerId },
				request: jsonRequest({ environments: ['staging'] }),
				locals: locals(adminId),
			} as never)
		).rejects.toMatchObject({
			status: 422,
			body: { message: 'An organization owner cannot be restricted to specific environments' },
		});
		const rows = await sql`select 1 from project_environment_grants where user_id = ${ownerId}`;
		expect(rows.length).toBe(0);
	});

	it('§4.10 PUT: a name absent from the registry is 400 Unknown environment, and a hidden row IS grantable (decision 49)', async () => {
		const hidden = await (await declare({ name: 'canary' })).json();
		const { PATCH } = await importEnvPatch();
		await PATCH({
			params: { orgId, projectId, envId: hidden.id },
			request: jsonRequest({ hidden: true }),
			locals: locals(adminId),
		} as never);

		const { PUT } = await importGrantUser();
		await expect(
			PUT({
				params: { orgId, projectId, userId: engineerId },
				request: jsonRequest({ environments: ['prodution'] }),
				locals: locals(adminId),
			} as never)
		).rejects.toMatchObject({ status: 400, body: { message: 'Unknown environment: prodution' } });

		// A grant is the boundary, not the menu: validation reads the registry the way §4.1 does
		// with includeHidden=true, so a hidden row is a valid grant target.
		const ok = await PUT({
			params: { orgId, projectId, userId: engineerId },
			request: jsonRequest({ environments: ['canary'] }),
			locals: locals(adminId),
		} as never);
		expect(ok.status).toBe(200);
		expect((await ok.json()).environments).toEqual(['canary']);
	});

	it('§4.10: a member without manage_members gets 403 on GET, PUT and DELETE', async () => {
		const message = 'Forbidden: manage_members permission required to manage environment access';
		const { GET } = await importGrants();
		const { PUT, DELETE } = await importGrantUser();

		// `engineer` holds write (so §4.2/§4.3 admit it) and NOT manage_members — the split this
		// endpoint's gate exists for.
		await expect(GET({ params, locals: locals(engineerId) } as never)).rejects.toMatchObject({
			status: 403,
			body: { message },
		});
		await expect(
			PUT({
				params: { orgId, projectId, userId: supportId },
				request: jsonRequest({ environments: ['staging'] }),
				locals: locals(engineerId),
			} as never)
		).rejects.toMatchObject({ status: 403, body: { message } });
		await expect(
			DELETE({
				params: { orgId, projectId, userId: supportId },
				locals: locals(engineerId),
			} as never)
		).rejects.toMatchObject({ status: 403, body: { message } });
	});

	it('§4.10 PUT: a user with no access to this project by any path is 404', async () => {
		await declare({ name: 'staging' });
		const { PUT } = await importGrantUser();
		await expect(
			PUT({
				params: { orgId, projectId, userId: outsiderId },
				request: jsonRequest({ environments: ['staging'] }),
				locals: locals(adminId),
			} as never)
		).rejects.toMatchObject({
			status: 404,
			body: { message: 'User has no access to this project' },
		});
	});

	it('§4.10 DELETE: 204 and the row is gone; 204 again on a user with no grant', async () => {
		await declare({ name: 'staging' });
		const { PUT, DELETE } = await importGrantUser();
		await PUT({
			params: { orgId, projectId, userId: engineerId },
			request: jsonRequest({ environments: ['staging'] }),
			locals: locals(adminId),
		} as never);

		const first = await DELETE({
			params: { orgId, projectId, userId: engineerId },
			locals: locals(adminId),
		} as never);
		expect(first.status).toBe(204);
		const rows = await sql`select 1 from project_environment_grants where user_id = ${engineerId} and project_id = ${projectId}`;
		expect(rows.length).toBe(0);

		// Deleting nothing is the state the caller asked for.
		const second = await DELETE({
			params: { orgId, projectId, userId: engineerId },
			locals: locals(adminId),
		} as never);
		expect(second.status).toBe(204);
	});

	it('§4.10 GET: returns the population beside the grants, with environments: null for the unrestricted', async () => {
		await declare({ name: 'staging' });
		const { PUT } = await importGrantUser();
		await PUT({
			params: { orgId, projectId, userId: engineerId },
			request: jsonRequest({ environments: ['staging'] }),
			locals: locals(adminId),
		} as never);

		const { GET } = await importGrants();
		const body = await (await GET({ params, locals: locals(adminId) } as never)).json();

		expect(body.grants).toHaveLength(1);
		expect(body.grants[0]).toMatchObject({
			userId: engineerId,
			environments: ['staging'],
			createdBy: adminId,
		});

		type Member = { userId: string; environments: string[] | null; orgRole: string | null };
		const byId = new Map<string, Member>(body.members.map((m: Member) => [m.userId, m]));
		// owner/admin/engineer/support are all in ISSUE_WRITE_ROLES, so all four are the population.
		expect(byId.get(engineerId)?.environments).toEqual(['staging']);
		// null, deliberately, and not [] — [] is a state the PUT refuses to store, and using it here
		// would make "sees nothing" and "sees everything" the same value on the wire.
		expect(byId.get(adminId)?.environments).toBeNull();
		expect(byId.get(ownerId)?.environments).toBeNull();
		expect(byId.get(adminId)?.orgRole).toBe('admin');
		// Nobody has a project_members row anywhere in this application.
		expect(body.members.every((m: { projectRole: string | null }) => m.projectRole === null)).toBe(true);
	});

	// --------------------------------------------------- §4.3a and the org project list (dec. 31)

	it('GET /api/organizations/{orgId}/projects: the caller\'s union, {id,name} only, sorted by name', async () => {
		const { GET } = await import('./[orgId]/projects/+server');
		const body = await (await GET({ params: { orgId }, locals: locals(adminId) } as never)).json();

		const ids = body.projects.map((p: { id: string }) => p.id);
		expect(ids).toContain(projectId);
		expect(ids).toContain(secondProjectId);
		expect(ids).not.toContain(foreignProjectId);
		expect(Object.keys(body.projects[0]).sort()).toEqual(['id', 'name']);
		const names = body.projects.map((p: { name: string }) => p.name);
		expect(names).toEqual([...names].sort((a, b) => a.localeCompare(b)));

		await expect(
			GET({ params: { orgId }, locals: locals(outsiderId) } as never)
		).rejects.toMatchObject({ status: 403 });
	});

	it('§4.3a: the union is per project, after scope resolution, with hidden excluded before de-duplication', async () => {
		await declare({ name: 'production' });
		await declare({ name: 'staging' });
		// The same name in a second project, hidden THERE and not here — decision 43's case: an
		// exclusion applied to the merged result would let this project's un-hidden `production`
		// carry the other one in, and vice versa.
		await declare({ name: 'production' }, adminId, secondProjectId);
		const hiddenThere = await (
			await declare({ name: 'canary' }, adminId, secondProjectId)
		).json();
		const { PATCH } = await importEnvPatch();
		await PATCH({
			params: { orgId, projectId: secondProjectId, envId: hiddenThere.id },
			request: jsonRequest({ hidden: true }),
			locals: locals(adminId),
		} as never);

		const { GET } = await import('../environments/+server');
		const all = await (
			await GET({ locals: locals(adminId), url: new URL('http://x/api/environments') } as never)
		).json();
		// De-duplicated by name, sorted, raw names only — no id, no displayName, no timestamps.
		expect(all.environments).toEqual([{ name: 'production' }, { name: 'staging' }]);

		// Now narrow the engineer in ONE project. The union must lose `staging` (not granted there)
		// but keep `production`, which the second project supplies unrestricted.
		const { PUT } = await importGrantUser();
		await PUT({
			params: { orgId, projectId, userId: engineerId },
			request: jsonRequest({ environments: ['production'] }),
			locals: locals(adminId),
		} as never);

		const scoped = await (
			await GET({ locals: locals(engineerId), url: new URL('http://x/api/environments') } as never)
		).json();
		expect(scoped.environments).toEqual([{ name: 'production' }]);

		// organizationId narrows the population; an org the caller is not in is 403.
		await expect(
			GET({
				locals: locals(adminId),
				url: new URL(`http://x/api/environments?organizationId=${otherOrgId}`),
			} as never)
		).rejects.toMatchObject({ status: 403 });
	});
});
