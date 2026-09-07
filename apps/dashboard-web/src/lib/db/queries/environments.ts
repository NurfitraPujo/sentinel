import { and, asc, eq, ne } from 'drizzle-orm';
import { db } from '$lib/server/db';
import { projectEnvironments, projectEnvironmentGrants } from '$lib/db/schema';

/**
 * sentinel-project-environments §4.1–§4.3 and §4.10 — the registry and the grant rows, read and
 * written in one place so §4.1's shape, §4.3's cap and §4.10's validation cannot drift between the
 * five route surfaces that need them.
 */

type Executor = Pick<typeof db, 'select' | 'insert' | 'update' | 'delete'>;

/**
 * §4.9. Deliberately application-level and not a `CHECK`: a constraint cannot count sibling rows.
 * 50 is the same order as the `VARCHAR(50)` name bound and large enough that no honest setup
 * reaches it. Under decision 8 it is enforced at §4.3 only — the processor's registry `UPDATE`
 * (§4.8) creates no rows, so nothing else can cross it.
 */
export const MAX_ENVIRONMENTS_PER_PROJECT = 50;

/** §4.2: `displayName` is 1–64 characters after trim. `VARCHAR(64)` is the column. */
export const DISPLAY_NAME_MAX_LENGTH = 64;

/** §4.1's `Environment` object, exactly. `camelCase`, per §3. */
export interface EnvironmentApiShape {
	id: string;
	name: string;
	displayName: string;
	isDefault: boolean;
	hidden: boolean;
	firstSeenAt: Date | null;
	lastSeenAt: Date | null;
	createdAt: Date;
}

type EnvironmentRow = typeof projectEnvironments.$inferSelect;

/**
 * §4.1's response shape. Timestamps stay `Date` objects — SvelteKit's `json()` serialises them to
 * ISO 8601 with milliseconds and a `Z`, which is what every existing route on this dashboard
 * already emits (`api/alerts/+server.ts: toApiShape`). Do not "tidy" them into RFC 3339 without
 * milliseconds; §3 says so.
 */
export function toEnvironmentApiShape(row: EnvironmentRow): EnvironmentApiShape {
	return {
		id: row.id,
		name: row.name,
		displayName: row.displayName,
		isDefault: row.isDefault,
		hidden: row.hidden,
		firstSeenAt: row.firstSeenAt,
		lastSeenAt: row.lastSeenAt,
		createdAt: row.createdAt,
	};
}

/**
 * EVERY registry row of a project, `hidden` ones included, sorted by name.
 *
 * This is §4.1's `includeHidden=true` read, and it is also the read §4.10 and §4.10a validate a
 * grant against — decision 49: a grant names a registry **row**, hidden or not. Validating through
 * §4.1's default filtered view instead would make a hidden environment ungrantable by accident
 * rather than by decision. Hiding removes a name from *selectors*, not from *grants*.
 */
export async function listProjectEnvironments(
	projectId: string,
	executor: Executor = db
): Promise<EnvironmentRow[]> {
	return await executor
		.select()
		.from(projectEnvironments)
		.where(eq(projectEnvironments.projectId, projectId))
		.orderBy(asc(projectEnvironments.name));
}

/** One registry row by id, scoped to its project. §4.2's `404` is "not in THIS project". */
export async function getProjectEnvironment(
	projectId: string,
	envId: string,
	executor: Executor = db
): Promise<EnvironmentRow | undefined> {
	const [row] = await executor
		.select()
		.from(projectEnvironments)
		.where(and(eq(projectEnvironments.projectId, projectId), eq(projectEnvironments.id, envId)));
	return row;
}

/**
 * §4.2/§4.3: clearing every OTHER default of this project. Called inside the same transaction as
 * the write that sets one, which is what "in one write" means on the wire — the partial unique
 * index `(project_id) WHERE is_default` is what makes at-most-one a database fact, and it fires if
 * the two halves are ever separated.
 */
export async function clearOtherDefaults(
	executor: Executor,
	projectId: string,
	exceptEnvId?: string
): Promise<void> {
	await executor
		.update(projectEnvironments)
		.set({ isDefault: false })
		.where(
			and(
				eq(projectEnvironments.projectId, projectId),
				eq(projectEnvironments.isDefault, true),
				exceptEnvId ? ne(projectEnvironments.id, exceptEnvId) : undefined
			)
		);
}

/** §4.10's grant row, as the API returns it. */
export interface GrantApiShape {
	userId: string;
	environments: string[];
	createdAt: Date;
	createdBy: string;
}

/** Every grant row for one project. §4.10's `GET` returns these beside the member list. */
export async function listProjectEnvironmentGrants(
	projectId: string,
	executor: Executor = db
): Promise<GrantApiShape[]> {
	const rows = await executor
		.select({
			userId: projectEnvironmentGrants.userId,
			environments: projectEnvironmentGrants.environments,
			createdAt: projectEnvironmentGrants.createdAt,
			createdBy: projectEnvironmentGrants.createdBy,
		})
		.from(projectEnvironmentGrants)
		.where(eq(projectEnvironmentGrants.projectId, projectId));
	return rows;
}

/**
 * §4.10's `PUT`, and §4.10a's acceptance path, write through here.
 *
 * **An UPSERT, never a plain `INSERT`** — `PRIMARY KEY (user_id, project_id)`, and on the
 * acceptance path a plain insert raises `23505` **inside the membership transaction** and the
 * invitee cannot join at all. The invitation's scope wins: the inviter made an explicit choice on
 * this invitation and a silently ignored choice is the worse failure (§4.10a).
 */
export async function upsertEnvironmentGrant(
	executor: Executor,
	input: { userId: string; projectId: string; environments: string[]; createdBy: string }
): Promise<GrantApiShape> {
	const [row] = await executor
		.insert(projectEnvironmentGrants)
		.values(input)
		.onConflictDoUpdate({
			target: [projectEnvironmentGrants.userId, projectEnvironmentGrants.projectId],
			set: { environments: input.environments, createdBy: input.createdBy },
		})
		.returning({
			userId: projectEnvironmentGrants.userId,
			environments: projectEnvironmentGrants.environments,
			createdAt: projectEnvironmentGrants.createdAt,
			createdBy: projectEnvironmentGrants.createdBy,
		});
	return row;
}
