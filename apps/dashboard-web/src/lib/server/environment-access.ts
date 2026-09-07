import { and, eq } from 'drizzle-orm';
import { db } from './db';
import { projectEnvironmentGrants } from '$lib/db/schema';

/**
 * sentinel-project-environments §2.2 — the environment access model, and the ONLY place the rule
 * "which environments may this user see in this project" is derived.
 *
 * **Absence means all.** No `project_environment_grants` row for a (user, project) pair means that
 * user sees every environment of that project, present and future. That is what makes the migration
 * narrow nobody: it creates no rows, so nobody's access changes when this feature lands. Narrowing
 * is an explicit act performed through §4.10 and never a side effect of deploying.
 *
 * Two exported forms, and deliberately exactly two — §2.2's *One resolver* rule. Six call sites
 * (the notification badge, `/` with `project=all`, `searchErrors`'s no-project branch, `notify.ts`'s
 * subscriber loop, `listReports` and `getIssueRelations`) are not about one project, and §2.2 claims
 * its guard table is complete: a definition repeated in six places will not stay complete.
 *
 * The composed predicate at every call site is the same one line:
 *
 *     scope === null  ?  <no predicate>  :  issues.environment = ANY(scope)
 *
 * A single-column predicate on `issues`, which is only true because decision 7 put `environment`
 * into issue identity. Under the old identity it would have been a semi-join over
 * `error_occurrences`, and a guard that is a semi-join is a guard that gets dropped from one query
 * out of six.
 */

/** What every executor these resolvers accept must be able to do. `db` or a transaction. */
type Executor = Pick<typeof db, 'select'>;

/**
 * §2.2 — the single-project resolver.
 *
 * Returns `null` for **unrestricted** (no grant row) and the granted environment names otherwise.
 *
 * **It must return `null` for a user with no `project_members` row at all**, and that is the case
 * that matters rather than an edge one. `issue-access.ts: requireIssueAccess` resolves access as
 * `project_members` **OR** an `organization_members` role in `ISSUE_WRITE_ROLES` (D17), and nothing
 * in this application writes `project_members` — `tests/e2e/alerting_test.go` and
 * `tests/e2e/dashboard_test.go` are its only two writers in the whole repository. So the ordinary
 * caller here is an org-role staff member with no membership row, and this function never reads one:
 * it is a primary-key lookup on the grant table and nothing else. A resolver that joined
 * `project_members` to "confirm" access would deny or crash for exactly the people §2.2 calls
 * "the people with the most reach".
 *
 * This is NOT an access check. Whether the caller may reach the project at all is the project
 * dimension and is `requireIssueAccess`/`requireProjectAccess`'s job; this answers only the
 * environment dimension, for a caller who has already passed that one.
 */
export async function resolveEnvironmentScope(
	userId: string,
	projectId: string,
	executor: Executor = db
): Promise<string[] | null> {
	const [row] = await executor
		.select({ environments: projectEnvironmentGrants.environments })
		.from(projectEnvironmentGrants)
		.where(
			and(
				eq(projectEnvironmentGrants.userId, userId),
				eq(projectEnvironmentGrants.projectId, projectId)
			)
		);

	return row ? row.environments : null;
}

/**
 * §2.2, decision 31 — the multi-project form, and a separate boundary from the one above because it
 * has a separate failure.
 *
 * **A project ABSENT from the map is unrestricted, not denied.** That is the inverse of the natural
 * `Map` reading (`map.get(id) ?? []` — "no entry, so nothing"), and it is the bug that would
 * silently empty every list on `project=all`, every notification badge and every search. The map
 * holds an entry ONLY for a (user, project) pair that HAS a grant row; every other project the
 * caller can reach is unrestricted, exactly as `resolveEnvironmentScope` returns `null` for it.
 *
 * Use {@link scopeForProject} to read the map rather than indexing it directly — it is the same
 * rule written once, and it returns `null` for an absent project so the composed predicate is
 * identical to the single-project form's.
 *
 * One round trip for the whole map. It is deliberately not filtered by an accessible-project set:
 * a grant only exists for a project somebody narrowed this user in, so the row count is bounded by
 * the narrowings, not by the projects.
 */
export async function resolveEnvironmentScopes(
	userId: string,
	executor: Executor = db
): Promise<Map<string, string[]>> {
	const rows = await executor
		.select({
			projectId: projectEnvironmentGrants.projectId,
			environments: projectEnvironmentGrants.environments,
		})
		.from(projectEnvironmentGrants)
		.where(eq(projectEnvironmentGrants.userId, userId));

	return new Map(rows.map((r) => [r.projectId, r.environments]));
}

/**
 * Reads a {@link resolveEnvironmentScopes} map the one way it may be read: an absent project is
 * unrestricted (`null`), not denied (`[]`).
 *
 * It exists so no call site writes `map.get(projectId) ?? []`, which is the natural `Map` reading
 * and is wrong in the direction that empties the page. It is not a third resolver — it performs no
 * I/O and derives nothing; it is the map's accessor.
 */
export function scopeForProject(
	scopes: Map<string, string[]>,
	projectId: string
): string[] | null {
	return scopes.get(projectId) ?? null;
}
