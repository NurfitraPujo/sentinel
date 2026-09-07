import { db } from "$lib/server/db";
import { issues, errorOccurrences, projects } from "$lib/db/schema";
import { eq, desc, sql, and, inArray, type SQL } from "drizzle-orm";
import { environmentScopeCondition } from "$lib/server/environment-access";

export interface IssueFilters {
    status?: string;
    projectId?: string;
    /**
     * sentinel-project-environments §4.4, decisions 22 and 35 — **the tenancy predicate, and it is
     * applied UNCONDITIONALLY.**
     *
     * Every project this caller may reach. Before decision 35 this was consumed as
     * `if (projectIds.length > 0) eq(issues.projectId, projectIds[0])`, which was wrong twice: on a
     * non-empty array it narrowed the whole list to the caller's FIRST project (decision 22), and
     * on an EMPTY array it added no project predicate at all — and the array was empty for every
     * user in any real deployment, because the derivation read `project_members` and nothing in
     * this application writes that table. `/` therefore returned every issue in the database,
     * across every organization, to any authenticated user.
     *
     * So: an omitted `projectIds` is treated as `[]` and `[]` DENIES. Do not reintroduce a
     * `.length` guard here in any form — that is the defect, not a tidy-up of it.
     */
    projectIds?: string[];
    /**
     * §4.4's `environment` FILTER — `all` (or omitted) means every environment. Matched exactly and
     * case-sensitively against `issues.environment`; under decision 7 an issue belongs to exactly
     * one environment, so this is a plain column predicate and never a semi-join over
     * `error_occurrences`.
     *
     * **This is not the guard.** A predicate the caller controls cannot be one; see
     * `environmentScopes` below, which applies whether or not the caller asked.
     */
    environment?: string;
    /**
     * §2.2's environment GUARD, as `resolveEnvironmentScopes` returns it. A project absent from the
     * map is unrestricted; an empty map is no predicate at all.
     */
    environmentScopes?: Map<string, string[]>;
}

/**
 * §4.4's tenancy predicate, over a possibly-EMPTY authorised set, and the empty case DENIES.
 *
 * **The contract's literal code shape does not survive this repository's Drizzle version, and this
 * is the deviation.** §4.4 specifies `inArray(issues.projectId, filters.projectIds)` applied
 * unconditionally and says the empty array "composes to `project_id IN ()` — matched by nothing".
 * In `drizzle-orm@0.30.10` it does not compose to anything: `inArray` **throws**
 * `Error: inArray requires at least one value` (`src/sql/expressions/conditions.ts:292`), so the
 * literal shape turns "the caller sees nothing" into a `500` on `/` for exactly the callers the
 * predicate is there to deny.
 *
 * What is kept is the REQUIREMENT, which is decision 35's actual content: the predicate is applied
 * unconditionally and an empty authorised set matches no row. `sql`false`` is `IN ()`'s meaning
 * written in the one way this version can express it. What is NOT kept — and must not be
 * reintroduced under any spelling — is a `.length` guard that omits the predicate, which is the
 * defect itself.
 */
function inAuthorisedProjects(column: typeof issues.projectId | typeof projects.id, ids: string[]) {
    return ids.length > 0 ? inArray(column, ids) : sql`false`;
}

/**
 * §4.4 — the one condition builder `listIssues` and `countIssues` share.
 *
 * They are deliberately not two copies: a total that counts rows the list is not allowed to show is
 * the same leak in a smaller font, and the two drifting apart is exactly how that happens. One
 * builder makes the count agree with the list by construction.
 */
function buildIssueConditions(filters: IssueFilters): SQL[] {
    const conditions: SQL[] = [];

    if (filters.status && filters.status !== "all") {
        conditions.push(eq(issues.status, filters.status));
    }

    if (filters.projectId && filters.projectId !== "all") {
        // ANDed with the tenancy predicate below, never instead of it: `eq(projectId, selected)`
        // alone does not check that the caller may reach the selected project, so `?project=<a
        // foreign uuid>` would read another organization's issues.
        conditions.push(eq(issues.projectId, filters.projectId));
    }

    conditions.push(inAuthorisedProjects(issues.projectId, filters.projectIds ?? []));

    if (filters.environment && filters.environment !== "all") {
        conditions.push(eq(issues.environment, filters.environment));
    }

    const guard = environmentScopeCondition(filters.environmentScopes ?? new Map());
    if (guard) {
        conditions.push(guard);
    }

    return conditions;
}

export const issueQueries = {
    async getIssueById(id: string) {
        const [issue] = await db
            .select()
            .from(issues)
            .where(eq(issues.id, id))
            .limit(1);
        return issue || null;
    },

    async getOccurrencesByIssueId(issueId: string, limit = 100) {
        return await db
            .select()
            .from(errorOccurrences)
            .where(eq(errorOccurrences.issueId, issueId))
            .orderBy(desc(errorOccurrences.createdAt))
            .limit(limit);
    },

    /**
     * §4.5a, flow `D2` — the real `COUNT(*)` behind the `Occurrence History` heading.
     *
     * `getOccurrencesByIssueId` caps at 100 and the heading rendered `occurrences.length` as though
     * it were a total, which is untrue for any issue with more than a page of occurrences. The
     * heading shows this number and the list below says it is showing the most recent *n*.
     */
    async countOccurrencesByIssueId(issueId: string) {
        const [row] = await db
            .select({ count: sql<number>`cast(count(*) as integer)` })
            .from(errorOccurrences)
            .where(eq(errorOccurrences.issueId, issueId));
        return Number(row?.count ?? 0);
    },

    async listIssues(filters: IssueFilters, page = 1, limit = 20) {
        const offset = (page - 1) * limit;
        const conditions = buildIssueConditions(filters);

        const results = await db
            .select({
                id: issues.id,
                projectId: issues.projectId,
                fingerprint: issues.fingerprint,
                message: issues.message,
                errorClass: issues.errorClass,
                environment: issues.environment,
                status: issues.status,
                firstSeen: issues.firstSeen,
                lastSeen: issues.lastSeen,
                count: issues.count,
            })
            .from(issues)
            .where(and(...conditions))
            .orderBy(desc(issues.lastSeen))
            .limit(limit)
            .offset(offset);

        return results;
    },

    async countIssues(filters: IssueFilters) {
        const conditions = buildIssueConditions(filters);

        const totalResult = await db
            .select({ count: sql<number>`cast(count(*) as integer)` })
            .from(issues)
            .where(and(...conditions));

        return Number(totalResult[0]?.count ?? 0);
    },

    /**
     * §4.4, decision 35 — the `?project=` picker's feed, scoped to the SAME set the issue list is.
     *
     * It was `SELECT id, name FROM projects LIMIT 100` with no `WHERE` clause at all: every project
     * in the deployment, in every organization. A guarded issue list beside an unguarded project
     * list is the same leak in a smaller font.
     *
     * `projectIds` is the caller's authorised set and, like the issue list's tenancy predicate, an
     * empty set returns nothing rather than everything. Decision 47: the set spans organizations
     * because `/` has no organization in scope and never has.
     */
    async listProjects(projectIds: string[]) {
        return await db
            .select({
                id: projects.id,
                name: projects.name,
            })
            .from(projects)
            .where(inAuthorisedProjects(projects.id, projectIds))
            .limit(100);
    },

    async getProjectById(id: string) {
        const [project] = await db
            .select()
            .from(projects)
            .where(eq(projects.id, id))
            .limit(1);
        return project || null;
    },
};
