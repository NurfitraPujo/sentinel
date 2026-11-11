import { error } from '@sveltejs/kit';
import { and, eq, inArray } from 'drizzle-orm';
import { db } from './db';
import { issues, projects, organizationMembers, projectMembers } from '$lib/db/schema';
import { resolveEnvironmentScope } from './environment-access';

/**
 * Org roles allowed to perform WRITE operations on issues (resolve/ignore/unresolve/assign).
 * Mirrors the allowlist enforced by the bulk path
 * (apps/dashboard-web/src/routes/api/projects/[projectId=uuid]/issues/batch/+server.ts:33) so a
 * single-issue action can never be more permissive than the bulk one. D10/D23.
 */
export const ISSUE_WRITE_ROLES = ['owner', 'admin', 'engineer', 'support'] as const;
type IssueWriteRole = (typeof ISSUE_WRITE_ROLES)[number];

function isIssueWriteRole(role: string): role is IssueWriteRole {
	return (ISSUE_WRITE_ROLES as readonly string[]).includes(role);
}

/**
 * sentinel-user-report-issue §2/§4.4a: who may WRITE an on-call schedule — its rotation, its
 * timezone or its overrides.
 *
 * **Deliberately narrower than `ISSUE_WRITE_ROLES` above**, and it lives beside it so the gap is
 * visible rather than discovered. The contract states the reason: a rota that any `engineer` can
 * rewrite is a rota that decides who gets woken up, edited by the person who does not want to be.
 *
 * READ is the other half of the split and is NOT this list — §4.4a puts it at any recognised org
 * member, i.e. `requireReportAccess(userId, orgId, 'read')`, viewers included.
 */
export const ONCALL_WRITE_ROLES = ['owner', 'admin'] as const;
type OncallWriteRole = (typeof ONCALL_WRITE_ROLES)[number];

export function isOncallWriteRole(role: string): role is OncallWriteRole {
	return (ONCALL_WRITE_ROLES as readonly string[]).includes(role);
}

/**
 * Throws unless `userId` holds an `ONCALL_WRITE_ROLES` role in `organizationId`.
 *
 * A non-member and a member below the bar are told apart deliberately: the first is "you have no
 * access to this organization" (the message every other org route uses), the second names the
 * roles, because an `engineer` who is refused here has to be able to tell "I am in the wrong org"
 * from "this is above my role" without guessing.
 */
export async function requireOncallWriteAccess(
	userId: string,
	organizationId: string
): Promise<{ organizationId: string; role: string }> {
	const memberRows = await db
		.select({ role: organizationMembers.role })
		.from(organizationMembers)
		.where(
			and(
				eq(organizationMembers.userId, userId),
				eq(organizationMembers.organizationId, organizationId)
			)
		);

	if (memberRows.length === 0) {
		throw error(403, 'Forbidden: You do not have access to this organization');
	}

	const role = memberRows[0].role;
	if (!isOncallWriteRole(role)) {
		throw error(403, 'Forbidden: Only an org owner or admin may change the on-call schedule');
	}

	return { organizationId, role };
}

export type IssuePermission = 'read' | 'write';

export interface IssueAccessContext {
	issueId: string;
	projectId: string;
	organizationId: string;
}

/**
 * Single place that decides whether `userId` may act on `issueId` at `permission` level.
 *
 * Two membership models coexist in this codebase (see docs/plans/UI_PARITY_REMEDIATION_PLAN.md
 * P1-6): page loads check `project_members` via `checkProjectAccess`
 * (lib/server/projects.ts), while these API routes historically checked only
 * `organization_members`. That divergence let an org `viewer` — who cannot bulk-resolve via
 * `projects/[projectId]/issues/batch` — resolve issues one at a time (D10), and it meant an org
 * member with no project membership at all could still act on any project's issues.
 *
 * The two are resolved as **OR, not AND** (see DECISIONS.md D17). An org role on
 * `ISSUE_WRITE_ROLES` is itself an org-wide grant covering every project in the org — no
 * `project_members` row is required or consulted. Project membership is the ALTERNATIVE path for a
 * caller whose org role does not grant access (`viewer`), and it conveys READ only.
 *
 * An earlier revision of this helper required BOTH, and this comment described that. It was wrong:
 * tests/e2e U13 failed because an org admin could not link two issues in their own organization,
 * since `project_members` is populated only for per-project grants and never for org-level staff.
 * D10 stays closed either way — an org `viewer` with no project membership is still refused.
 *
 * Throws a SvelteKit `error()` (404/400/403) — call sites should let it propagate.
 */
export async function requireIssueAccess(
	userId: string,
	issueId: string,
	permission: IssuePermission
): Promise<IssueAccessContext> {
	const issueRows = await db
		.select({
			issueId: issues.id,
			projectId: issues.projectId,
			environment: issues.environment,
			organizationId: projects.organizationId,
		})
		.from(issues)
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.where(eq(issues.id, issueId));

	if (issueRows.length === 0) {
		throw error(404, 'Issue not found');
	}

	const { projectId, environment, organizationId } = issueRows[0];

	if (!organizationId) {
		throw error(400, 'Issue project does not belong to an organization');
	}

	const { organizationId: orgId } = await requireProjectAccess(userId, projectId, permission, {
		organizationId,
	});

	// sentinel-project-environments §2.2 — the ENVIRONMENT dimension, applied after the project one.
	//
	// Denial here is `404`, never `403` (decision 20). An issue in an environment the caller may not
	// see is indistinguishable from an issue that does not exist: a `403` on `/issues/<uuid>` tells
	// a contractor that a production issue with that id exists, which is the fact the boundary was
	// drawn to withhold. The project dimension keeps its `403`s above, and that asymmetry is
	// deliberate.
	const scope = await resolveEnvironmentScope(userId, projectId);
	if (scope !== null && !scope.includes(environment)) {
		throw error(404, 'Issue not found');
	}

	return { issueId: issueRows[0].issueId, projectId, organizationId: orgId };
}

/**
 * The project-scoped half of `requireIssueAccess`, callable on its own when the caller has a
 * projectId but no single issue id — the bulk endpoint
 * (`api/projects/[projectId]/issues/batch`) being the case that matters.
 *
 * Extracted so the bulk and single-issue write paths cannot drift apart again. They previously
 * disagreed in BOTH directions: the single-issue path had no role gate at all (D10), and once
 * that was fixed the bulk path was left as the more permissive of the two (D23) because it checked
 * only `organization_members` and never project membership — so a user in the org but not on the
 * project could bulk-resolve that project's issues while being refused a single one.
 *
 * `knownOrganizationId` lets `requireIssueAccess` pass the org it already resolved via the issue
 * join, avoiding a second lookup; omit it and the project's organization is fetched here.
 */
export async function requireProjectAccess(
	userId: string,
	projectId: string,
	permission: IssuePermission,
	opts?: { organizationId?: string }
): Promise<{ projectId: string; organizationId: string }> {
	let organizationId = opts?.organizationId;

	if (!organizationId) {
		const projectRows = await db
			.select({ organizationId: projects.organizationId })
			.from(projects)
			.where(eq(projects.id, projectId));

		if (projectRows.length === 0) {
			throw error(404, 'Project not found');
		}
		if (!projectRows[0].organizationId) {
			throw error(400, 'Project does not belong to an organization');
		}
		organizationId = projectRows[0].organizationId;
	}

	const orgMemberRows = await db
		.select({ role: organizationMembers.role })
		.from(organizationMembers)
		.where(
			and(
				eq(organizationMembers.userId, userId),
				eq(organizationMembers.organizationId, organizationId)
			)
		);

	if (orgMemberRows.length === 0) {
		throw error(403, 'Forbidden: You do not have access to this organization');
	}

	const orgRole = orgMemberRows[0].role;
	const orgRoleGrantsAccess = isIssueWriteRole(orgRole);

	// An organization-level role IS an org-wide grant: owner/admin/engineer/support administer every
	// project in their org and are not expected to also hold a `project_members` row for each one.
	// Requiring both broke exactly that (tests/e2e U13: an org admin could not link two issues),
	// because in practice `project_members` is populated only for per-project grants.
	//
	// Project membership is therefore the ALTERNATIVE path, not an additional hurdle — it is how a
	// user whose org role does not itself grant issue access (`viewer`) gets access to the specific
	// projects they were added to. This still closes D10: an org `viewer` with no project membership
	// is refused, and cannot see or mutate a project they are not on.
	if (!orgRoleGrantsAccess) {
		const projectMemberRows = await db
			.select({ role: projectMembers.role })
			.from(projectMembers)
			.where(and(eq(projectMembers.userId, userId), eq(projectMembers.projectId, projectId)));

		if (projectMemberRows.length === 0) {
			throw error(403, 'Forbidden: You do not have access to this project');
		}

		// A project grant conveys read only. Writing still requires an org role on the write
		// allowlist, so the single-issue path can never be more permissive than the bulk one.
		if (permission === 'write') {
			throw error(403, 'Forbidden: Insufficient permissions to modify these issues');
		}
	}

	return { projectId, organizationId };
}

/**
 * Project ids within `organizationId` that `userId` has a `project_members` row for.
 *
 * `searchIssuesInOrg` (lib/db/queries/issues.ts) is org-wide with no project filter, so callers
 * use this to scope results down to projects the caller actually belongs to — otherwise an org
 * member who is not on a given project could see that project's issues via search.
 */
export async function getAccessibleProjectIds(
	userId: string,
	organizationId: string
): Promise<Set<string>> {
	const rows = await db
		.select({ projectId: projectMembers.projectId })
		.from(projectMembers)
		.innerJoin(projects, eq(projects.id, projectMembers.projectId))
		.where(and(eq(projectMembers.userId, userId), eq(projects.organizationId, organizationId)));

	return new Set(rows.map((r) => r.projectId));
}

/** One project as the routes that list a caller's projects return it. */
export interface AccessibleProject {
	id: string;
	name: string;
	organizationId: string;
}

/**
 * sentinel-project-environments §4.3a / §4.10, decision 31 — "which projects may this caller see",
 * as the WIDE population:
 *
 *     project_members  ∪  (organization_members whose role is in ISSUE_WRITE_ROLES, over that
 *                          organization's projects)
 *
 * — the same `OR` `requireIssueAccess` above already resolves access by, and the reason §2.2 uses
 * its own grant table rather than a column on `project_members`.
 *
 * **It is deliberately NOT `getAccessibleProjectIds` widened in place.** That function keeps its
 * `project_members`-only derivation because decision 45 leaves one of its two callers
 * (`routes/api/organizations/[orgId=uuid]/reports/+server.ts`) exactly as it is: an org `viewer` sees
 * zero reports today and widening it would make viewers start seeing their organization's reports,
 * which is an access decision and not a mechanical one (contract §12 *Known limits* 8). Both
 * derivations live here, beside each other, so the disagreement is written down rather than latent.
 *
 * Why the wide population and not `project_members` alone: **nothing in this application writes
 * `project_members`** — its only two writers in the whole repository are `tests/e2e/alerting_test.go`
 * and `tests/e2e/dashboard_test.go`. Built on that table alone, every list this feeds would be empty
 * for every user in any real deployment, and §4.7's decision-26 guard would refuse every environment
 * to every caller.
 *
 * Sorted by name — §4.10's `GET /api/organizations/{orgId}/projects` returns it straight to a picker.
 * Pass `organizationId` to narrow to one organization; omit it for §4.3a's cross-organization union
 * (decision 47: `/search` is not organization-scoped either).
 */
export async function getAccessibleProjects(
	userId: string,
	organizationId?: string
): Promise<AccessibleProject[]> {
	const columns = {
		id: projects.id,
		name: projects.name,
		organizationId: projects.organizationId,
	};

	const byOrgRole = await db
		.select(columns)
		.from(projects)
		.innerJoin(
			organizationMembers,
			eq(organizationMembers.organizationId, projects.organizationId)
		)
		.where(
			and(
				eq(organizationMembers.userId, userId),
				inArray(organizationMembers.role, ISSUE_WRITE_ROLES as unknown as string[]),
				organizationId ? eq(projects.organizationId, organizationId) : undefined
			)
		);

	const byProjectRole = await db
		.select(columns)
		.from(projects)
		.innerJoin(projectMembers, eq(projectMembers.projectId, projects.id))
		.where(
			and(
				eq(projectMembers.userId, userId),
				organizationId ? eq(projects.organizationId, organizationId) : undefined
			)
		);

	const merged = new Map<string, AccessibleProject>();
	for (const row of [...byOrgRole, ...byProjectRole]) {
		// projects.organization_id is nullable in the schema; a project belonging to no organization
		// is reachable by neither half of the union, so it cannot appear here.
		if (row.organizationId === null) continue;
		merged.set(row.id, { id: row.id, name: row.name, organizationId: row.organizationId });
	}

	return [...merged.values()].sort((a, b) => a.name.localeCompare(b.name));
}

// Matches issues.resolved_in_version VARCHAR(100)
// (packages/db-migrations/migrations/1721900000_add_issue_lifecycle_and_relations.sql:38).
const RESOLVED_IN_VERSION_MAX_LENGTH = 100;

/**
 * Validates a client-supplied `resolvedInVersion` instead of passing it through unchecked from
 * the request body — an oversized value previously reached the DB write and 500'd on the
 * `varchar(100)` constraint instead of 400ing (D10 item 3).
 *
 * Returns the trimmed value, or null if omitted/blank. Throws SvelteKit `error(400, ...)` for a
 * present-but-invalid value (wrong type or too long).
 */
export function validateResolvedInVersion(value: unknown): string | null {
	if (value === undefined || value === null) {
		return null;
	}
	if (typeof value !== 'string') {
		throw error(400, 'resolvedInVersion must be a string');
	}
	const trimmed = value.trim();
	if (trimmed.length === 0) {
		return null;
	}
	if (trimmed.length > RESOLVED_IN_VERSION_MAX_LENGTH) {
		throw error(
			400,
			`resolvedInVersion must be at most ${RESOLVED_IN_VERSION_MAX_LENGTH} characters`
		);
	}
	return trimmed;
}

/**
 * sentinel-releases-snooze §4.2 (decisions 33, 38): the ISO 8601 date-time SHAPE `snoozedUntil`
 * must have -- a calendar date, `T`, a time with seconds, optional fractional seconds, and `Z` or
 * `±hh:mm`. Checked BEFORE `Date.parse`, because `Date.parse` alone accepts what the agent
 * OpenAPI (`z.string().datetime({ offset: true })`) declares refused: a date-only `2026-09-20`
 * (as UTC midnight) and a zone-less `2026-09-20T09:00` (as server-local time). That schema is
 * declaration-only (nothing runs a `.parse()` on it), so this regex is what actually enforces the
 * declared shape, on both routes.
 */
const ISO_8601_DATE_TIME = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/;

const SNOOZED_UNTIL_ONLY_WITH_IGNORED = 'snoozedUntil is only valid with status ignored';
const SNOOZED_UNTIL_MUST_BE_FUTURE = 'snoozedUntil must be a future timestamp';
const SNOOZE_OCCURRENCES_ONLY_WITH_IGNORED = 'snoozeOccurrences is only valid with status ignored';
const SNOOZE_OCCURRENCES_RANGE = 'snoozeOccurrences must be an integer between 1 and 1000000';

// sentinel-releases-snooze §4.2: `1 ≤ n ≤ 1000000`.
const SNOOZE_OCCURRENCES_MIN = 1;
const SNOOZE_OCCURRENCES_MAX = 1000000;

/**
 * sentinel-releases-snooze §4.2 -- validates a client-supplied `snoozedUntil` (human route) /
 * `snoozed_until` (agent route and the batch op's `issues.status`). ONE validator for both routes
 * (decision 38), so they refuse the same inputs with the same words.
 *
 * Returns the instant as a `Date`, or `null` when omitted: `undefined`, `null` and a trimmed-empty
 * string are all "no condition of that kind" (§3), exactly as `validateResolvedInVersion` treats a
 * blank string. Throws SvelteKit `error(400, ...)` with one of the two §4.2 strings:
 *
 * - `snoozedUntil is only valid with status ignored` -- a present value with any other status;
 * - `snoozedUntil must be a future timestamp` -- ONE string for a wrong-shaped value (the regex
 *   above; a non-string counts as wrong-shaped), an unparseable one (`'tomorrow'`), and one that
 *   is not STRICTLY after the server's now (decision 33).
 *
 * An offset is accepted and normalised: `2026-09-20T16:00:00+07:00` and `2026-09-20T09:00:00Z`
 * are the same instant and produce the same `Date`.
 */
export function validateSnoozedUntil(value: unknown, status: string): Date | null {
	if (value === undefined || value === null) {
		return null;
	}
	if (typeof value === 'string' && value.trim().length === 0) {
		return null;
	}
	if (status !== 'ignored') {
		throw error(400, SNOOZED_UNTIL_ONLY_WITH_IGNORED);
	}
	if (typeof value !== 'string') {
		throw error(400, SNOOZED_UNTIL_MUST_BE_FUTURE);
	}
	const trimmed = value.trim();
	if (!ISO_8601_DATE_TIME.test(trimmed)) {
		throw error(400, SNOOZED_UNTIL_MUST_BE_FUTURE);
	}
	const ms = Date.parse(trimmed);
	if (Number.isNaN(ms) || ms <= Date.now()) {
		throw error(400, SNOOZED_UNTIL_MUST_BE_FUTURE);
	}
	return new Date(ms);
}

/**
 * sentinel-releases-snooze §4.2 -- validates a client-supplied `snoozeOccurrences` (human route)
 * / `snooze_occurrences` (agent route). The same one-validator-both-routes rule as
 * `validateSnoozedUntil`.
 *
 * Returns the integer, or `null` when omitted (`undefined`, `null`, or a trimmed-empty string --
 * §3). Any OTHER string (`'12'`) is refused: the value must be a JSON integer in
 * `[1, 1000000]`. Throws SvelteKit `error(400, ...)` with one of the two §4.2 strings.
 */
export function validateSnoozeOccurrences(value: unknown, status: string): number | null {
	if (value === undefined || value === null) {
		return null;
	}
	if (typeof value === 'string' && value.trim().length === 0) {
		return null;
	}
	if (status !== 'ignored') {
		throw error(400, SNOOZE_OCCURRENCES_ONLY_WITH_IGNORED);
	}
	if (
		typeof value !== 'number' ||
		!Number.isInteger(value) ||
		value < SNOOZE_OCCURRENCES_MIN ||
		value > SNOOZE_OCCURRENCES_MAX
	) {
		throw error(400, SNOOZE_OCCURRENCES_RANGE);
	}
	return value;
}
