import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

/**
 * sentinel-project-environments §2.2 — contract §11's two NEW resolver boundaries:
 *
 *   - `lib/server/environment-access.ts: resolveEnvironmentScope` — `null` for no row, the array
 *     for a row, and the case that matters: `null` for a user reaching the project by the
 *     `organization_members` path with **no `project_members` row at all**. That is the ORDINARY
 *     case, not an edge one — `tests/e2e/alerting_test.go:165` and `tests/e2e/dashboard_test.go:137`
 *     are the only two writers of `project_members` in the whole repository.
 *   - `resolveEnvironmentScopes` — a **separate** boundary because it has a separate failure: a
 *     project ABSENT from the map is unrestricted, not denied. That is the inverse of the natural
 *     `Map` reading (`map.get(id) ?? []`) and it is the bug that would silently empty every list on
 *     `project=all`.
 *
 * Against a REAL migrated Postgres, because both claims are claims about rows: a mocked query layer
 * cannot tell "returned null because there is no grant row" from "returned null because the query
 * was never issued". Point DATABASE_URL at a DISPOSABLE database — never the shared dev one
 * (CLAUDE.md) — e.g.:
 *
 *   docker run -d --name sentinel-envaccess-pg -p 15442:5432 \
 *     -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme -e POSTGRES_DB=sentinel postgres:15-alpine
 *   cd packages/db-migrations && DB_URL_DASHBOARD=postgres://sentinel:changeme@localhost:15442/sentinel?sslmode=disable \
 *     go run ./cmd/migrate up -target=dashboard
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:changeme@localhost:15442/sentinel \
 *     pnpm vitest run src/lib/server/environment-access.integration.test.ts
 *
 * Skipped when nothing answers, so a machine with no Postgres is not broken by it. Set
 * ENVIRONMENT_ACCESS_INTEGRATION_REQUIRED=1 to make "no reachable database" a hard failure, matching
 * SEARCH_INTEGRATION_REQUIRED / SCHEMA_DRIFT_REQUIRED / M1_FLOW_INTEGRATION_REQUIRED.
 */
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.ENVIRONMENT_ACCESS_INTEGRATION_REQUIRED === '1';

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
		`ENVIRONMENT_ACCESS_INTEGRATION_REQUIRED=1 but no database answered at ${connectionString}.`
	);
}

const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const projectA = randomUUID();
const projectB = randomUUID();
const projectC = randomUUID();
// The ordinary caller: an org-role staff member with NO project_members row anywhere.
const orgOnlyUser = randomUUID();
// The narrowed one: same, plus a grant in project A only.
const narrowedUser = randomUUID();

describe.skipIf(!dbReachable)('environment-access resolvers (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });

		await sql`insert into organizations (id, name, slug) values (${orgId}, ${'envacc-' + suffix}, ${'envacc-' + suffix})`;
		for (const [id, label] of [
			[projectA, 'a'],
			[projectB, 'b'],
			[projectC, 'c'],
		] as const) {
			await sql`
				insert into projects (id, name, organization_id, api_key, api_key_hash)
				values (${id}, ${`envacc-${label}-${suffix}`}, ${orgId},
				        ${`k_${label}_${suffix}`}, ${`h_${label}_${suffix}`})
			`;
		}
		await sql`insert into "user" (id, name, email) values (${orgOnlyUser}, ${'Org Only'}, ${`org-only-${suffix}@example.test`})`;
		await sql`insert into "user" (id, name, email) values (${narrowedUser}, ${'Narrowed'}, ${`narrowed-${suffix}@example.test`})`;
		// Both reach every project by the org path. Neither has a project_members row: that is what
		// makes this the ordinary case rather than a contrived one.
		await sql`insert into organization_members (organization_id, user_id, role) values (${orgId}, ${orgOnlyUser}, ${'engineer'})`;
		await sql`insert into organization_members (organization_id, user_id, role) values (${orgId}, ${narrowedUser}, ${'engineer'})`;

		await sql`
			insert into project_environment_grants (user_id, project_id, environments, created_by)
			values (${narrowedUser}, ${projectA}, ${sql.array(['staging'])}, ${orgOnlyUser})
		`;
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from project_environment_grants where user_id in (${orgOnlyUser}, ${narrowedUser})`;
			await sql`delete from organization_members where organization_id = ${orgId}`;
			await sql`delete from projects where organization_id = ${orgId}`;
			await sql`delete from "user" where id in (${orgOnlyUser}, ${narrowedUser})`;
			await sql`delete from organizations where id = ${orgId}`;
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	it('resolveEnvironmentScope returns null for a user reaching the project by the organization_members path with no project_members row', async () => {
		const { resolveEnvironmentScope } = await import('./environment-access');

		// The assertion that matters, and the one a resolver written against project_members fails:
		// there is no membership row for this user in ANY project, and every one of them is
		// unrestricted.
		const [{ count }] = await sql<{ count: string }[]>`
			select count(*)::text as count from project_members where user_id = ${orgOnlyUser}
		`;
		expect(count).toBe('0');

		expect(await resolveEnvironmentScope(orgOnlyUser, projectA)).toBeNull();
		expect(await resolveEnvironmentScope(orgOnlyUser, projectB)).toBeNull();
		expect(await resolveEnvironmentScope(orgOnlyUser, projectC)).toBeNull();
	});

	it('resolveEnvironmentScope returns the granted names for a project with a grant row, and null for the same user elsewhere', async () => {
		const { resolveEnvironmentScope } = await import('./environment-access');

		expect(await resolveEnvironmentScope(narrowedUser, projectA)).toEqual(['staging']);
		// Absence means all — the grant narrows ONE project, not the user.
		expect(await resolveEnvironmentScope(narrowedUser, projectB)).toBeNull();
		expect(await resolveEnvironmentScope(narrowedUser, projectC)).toBeNull();
	});

	it('resolveEnvironmentScopes returns a map of size one among three accessible projects, and the two unmapped projects are unrestricted by the composed predicate', async () => {
		const { resolveEnvironmentScopes, scopeForProject } = await import('./environment-access');

		const scopes = await resolveEnvironmentScopes(narrowedUser);
		expect(scopes.size).toBe(1);
		expect(scopes.get(projectA)).toEqual(['staging']);

		// The composed predicate, exactly as every call site writes it:
		//     scope === null ? <no predicate> : issues.environment = ANY(scope)
		const visible = (projectId: string, environment: string) => {
			const scope = scopeForProject(scopes, projectId);
			return scope === null || scope.includes(environment);
		};

		expect(visible(projectA, 'production')).toBe(false);
		expect(visible(projectA, 'staging')).toBe(true);
		// The inverse Map reading (`map.get(id) ?? []`) would make BOTH of these false and silently
		// empty every list on project=all. Absent means unrestricted.
		expect(visible(projectB, 'production')).toBe(true);
		expect(visible(projectC, 'production')).toBe(true);
	});

	it('resolveEnvironmentScopes returns an empty map — not an empty result set — for an unrestricted user', async () => {
		const { resolveEnvironmentScopes, scopeForProject } = await import('./environment-access');

		const scopes = await resolveEnvironmentScopes(orgOnlyUser);
		expect(scopes.size).toBe(0);
		expect(scopeForProject(scopes, projectA)).toBeNull();
	});

	/**
	 * CONTRACT DEVIATION, proved here and deliberately NOT fixed — reported, per the rule that a
	 * worker never edits a settled contract.
	 *
	 * Contract §2.2 states `CHECK (array_length(environments, 1) >= 1)` and says of it "**an empty
	 * array is refused**". It is not. In PostgreSQL `array_length('{}', 1)` is **NULL**, not 0, so
	 * the predicate evaluates to NULL — and a CHECK constraint that evaluates to NULL is
	 * **satisfied**. The migration (1724400000_add_project_environments.sql) transcribes the
	 * contract's expression faithfully; the expression itself is what does not hold. Proved on a
	 * freshly migrated Postgres:
	 *
	 *     select array_length('{}'::text[], 1) is null;  -->  t
	 *
	 * The refusal that DOES hold is §4.10's `400 environments must contain at least one
	 * environment`, at the route, which is where every writer in this application goes through:
	 * `PUT …/environment-grants/{userId}` and the invitation scope validator both call
	 * `validateGrantEnvironments`. So "sees nothing" is unreachable through the product — but it is
	 * reachable through a `psql` session, which is exactly what the database-level guard was for.
	 * `CHECK (cardinality(environments) >= 1)` is the expression that holds; changing it is a
	 * contract edit and a migration, and neither is this item's to make.
	 *
	 * This test asserts what is TRUE today so the gap is visible rather than latent. It is written
	 * to go RED the moment somebody corrects the constraint, which is the signal to delete it.
	 */
	it('DEVIATION: the grant table does NOT refuse an empty environments array — array_length({},1) is NULL and a NULL CHECK passes', async () => {
		await sql`
			insert into project_environment_grants (user_id, project_id, environments, created_by)
			values (${orgOnlyUser}, ${projectB}, ${sql.array([] as string[])}, ${orgOnlyUser})
		`;

		const [row] = await sql<{ environments: string[] }[]>`
			select environments from project_environment_grants
			where user_id = ${orgOnlyUser} and project_id = ${projectB}
		`;
		expect(row.environments).toEqual([]);

		// And the consequence the contract's refusal existed to prevent: a row stored this way makes
		// "sees nothing" and "sees everything" two different values that the resolver tells apart
		// correctly — `[]` is a scope, not an absence — so the boundary itself is not broken by it.
		const { resolveEnvironmentScope } = await import('./environment-access');
		expect(await resolveEnvironmentScope(orgOnlyUser, projectB)).toEqual([]);

		await sql`delete from project_environment_grants where user_id = ${orgOnlyUser} and project_id = ${projectB}`;
	});
});
