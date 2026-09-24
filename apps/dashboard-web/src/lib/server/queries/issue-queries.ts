import { db } from "$lib/server/db";
import { issues, errorOccurrences, projects, issueOccurrenceBuckets, users, organizationMembers, agents } from "$lib/db/schema";
import { eq, gte, lte, desc, asc, sql, and, inArray, isNull, isNotNull, type SQL } from "drizzle-orm";
import { environmentScopeCondition } from "$lib/server/environment-access";
import { coerceIssueSort } from "$lib/server/issue-sort";
import { DAY_MS, HOUR_MS, TREND_HOURLY_SLOTS, utcDayStart } from "$lib/server/trend-window";

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
    /**
     * sentinel-affected-users §4.1, decision 10 — the ORDER, and only the order. `last_seen` (the
     * default, and today's order) or `affected_users`; anything else is coerced to `last_seen` by
     * `coerceIssueSort`, so a `string` is accepted here rather than `IssueSort` — the loader passes
     * what the URL said and the coercion happens in exactly one place.
     *
     * Read by `listIssues` ONLY. `buildIssueConditions` never sees it — a sort is not a read path
     * around a guard (§2) — and `countIssues` ignores it: `pagination.total` does not change with
     * the order the same rows are listed in.
     */
    sort?: string;
    /**
     * sentinel-issue-trend §4.1, decision 6 — the `new=24h` FILTER, as the instant it stands for:
     * `first_seen >= firstSeenAfter`. Unlike `sort` this IS a predicate and goes through
     * `buildIssueConditions`, so `pagination.total` narrows with it. The loader coerces `new` and
     * takes `now` ONCE (`firstSeenLowerBound`), then passes the same `Date` to `listIssues` and
     * `countIssues`; `undefined` (the `all` case) adds nothing.
     */
    firstSeenAfter?: Date;
    /**
     * Claimant filter: 'all' | 'unclaimed' | 'claimed-me' | 'claimed-agent' | specific claimant string (user ID or email).
     */
    claim?: string;
    /**
     * Current authenticated user ID (and optionally email) used to resolve 'claimed-me'.
     */
    currentUserId?: string;
    /**
     * Search query across error_class, fingerprint (ILIKE) and message (FTS vector).
     */
    q?: string;
}

export interface ClaimantFacetCounts {
    total: number;
    unclaimed: number;
    claimedMe: number;
    claimedAgent: number;
    byClaimant: Record<string, number>;
    claimantTypes: Record<string, string | null>;
}

/**
 * sentinel-issue-trend §4.2 — one point of a dense series: the bucket's (hourly) or the UTC day's
 * (daily) start as RFC 3339 UTC with a `Z`, and a count that is `≥ 0` and never `null`.
 */
export interface TrendPoint {
    start: string;
    count: number;
}

/** §4.2's `trend` page-data key: `hourly` is 24 long, `daily` is 30 long, both dense, oldest first. */
export interface IssueTrend {
    hourly: TrendPoint[];
    daily: TrendPoint[];
}

/** The slot's start as the contract writes it: `2026-09-13T14:00:00Z` — second precision, `Z`. */
function slotStartIso(ms: number): string {
    return new Date(ms).toISOString().slice(0, 19) + "Z";
}

/** The number of hourly slots in `[windowStart … windowEnd]`, both ends inclusive. */
function hourlySlotCount(windowStart: Date, windowEnd: Date): number {
    return Math.floor((windowEnd.getTime() - windowStart.getTime()) / HOUR_MS) + 1;
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

    // sentinel-issue-trend §4.1 (decision 6): `new=24h`, ANDed inside the guards above — one more
    // condition on the same query, never a read path around them. Here rather than in `listIssues`
    // so the count narrows with the list by construction.
    if (filters.firstSeenAfter) {
        conditions.push(gte(issues.firstSeen, filters.firstSeenAfter));
    }

    if (filters.claim && filters.claim !== "all") {
        if (filters.claim === "unclaimed") {
            conditions.push(isNull(issues.assignedTo));
        } else if (filters.claim === "claimed-me") {
            const meCondition = filters.currentUserId
                ? eq(issues.assignedTo, filters.currentUserId)
                : sql`false`;
            conditions.push(and(isNotNull(issues.assignedTo), eq(issues.assigneeType, "user"), meCondition)!);
        } else if (filters.claim === "claimed-agent") {
            conditions.push(and(isNotNull(issues.assignedTo), eq(issues.assigneeType, "agent"))!);
        } else {
            // Specific claimant identifier (user ID, agent ID, or email)
            conditions.push(eq(issues.assignedTo, filters.claim));
        }
    }

    if (filters.q && filters.q.trim()) {
        const rawQ = filters.q.trim();
        const term = `%${rawQ}%`;
        conditions.push(
            sql`(${issues.errorClass} ILIKE ${term} OR ${issues.fingerprint} ILIKE ${term} OR to_tsvector('english', ${issues.message}) @@ plainto_tsquery('english', ${rawQ}))`
        );
    }

    return conditions;
}

/**
 * §4.1's `ORDER BY` per `sort` (decisions 10 and 22).
 *
 * `affected_users` is `affected_user_count DESC, last_seen DESC, id DESC`: ties on the first key
 * are common (every pre-existing issue is `0`, decision 4) and ties on the first TWO are likely
 * wherever `last_seen` is set in bulk — a seed from `generate_series`, a burst ingested in one
 * second — so `id DESC` is the third, UNIQUE key. Without a unique final key an offset page
 * boundary can repeat or skip a row. It is also the key order of BOTH indexes that serve it, in
 * full: `idx_issues_project_env_affected_users` (§4.6) under a project AND environment equality,
 * and `idx_issues_project_affected_users` (1724600000, BUG-01) under `project_id` alone — the
 * `environment=all` form, which a btree keyed on `environment` second cannot order without a Sort.
 *
 * `last_seen` is `last_seen DESC` and nothing else — unchanged, deliberately: the default must be
 * exactly today's order, ties included.
 *
 * sentinel-issue-trend §4.1 (decision 5) adds two, both ending in the unique key:
 *
 *  - `count` is `count DESC, last_seen DESC, id DESC`: ties on `count` are common at the low end
 *    (every fresh issue is `1`), `last_seen` breaks most of them, `id` is the unique final key. The
 *    key order of `idx_issues_project_count` (1724700000).
 *  - `first_seen` is `first_seen DESC NULLS LAST, id DESC`, and the `NULLS LAST` is spelled out on
 *    purpose: the column is nullable in the DDL (`1716508800_init.sql`: `first_seen TIMESTAMP WITH
 *    TIME ZONE DEFAULT NOW()`, no `NOT NULL`) and PostgreSQL's default for `DESC` is `NULLS FIRST`,
 *    so Drizzle's `desc()` — a bare `DESC` — would put a null-`first_seen` row at the TOP of
 *    "newest first". The key order of `idx_issues_project_first_seen`.
 */
function issueOrder(sort: string | undefined): SQL[] {
    switch (coerceIssueSort(sort)) {
        case "affected_users":
            return [desc(issues.affectedUserCount), desc(issues.lastSeen), desc(issues.id)];
        case "count":
            return [desc(issues.count), desc(issues.lastSeen), desc(issues.id)];
        case "first_seen":
            return [sql`${issues.firstSeen} DESC NULLS LAST`, desc(issues.id)];
        case "last_seen":
            return [desc(issues.lastSeen)];
    }
}

/** The pre-v2 `error_occurrences` columns the issue page receives (sentinel-event-schema-v2 §8.3). */
export const issuePageOccurrenceColumns = {
    id: errorOccurrences.id,
    issueId: errorOccurrences.issueId,
    environment: errorOccurrences.environment,
    platform: errorOccurrences.platform,
    releaseVersion: errorOccurrences.releaseVersion,
    stacktrace: errorOccurrences.stacktrace,
    metadata: errorOccurrences.metadata,
    traceId: errorOccurrences.traceId,
    spanId: errorOccurrences.spanId,
    eventId: errorOccurrences.eventId,
    projectId: errorOccurrences.projectId,
    createdAt: errorOccurrences.createdAt,
};

export const issueQueries = {
    async getIssueById(id: string) {
        const [issue] = await db
            .select()
            .from(issues)
            .where(eq(issues.id, id))
            .limit(1);
        return issue || null;
    },

    // sentinel-event-schema-v2 §8.3: an explicit projection of the pre-v2 columns. A bare select()
    // would start shipping every v2 JSONB body (breadcrumbs, contexts, request, ...) of up to 100
    // occurrences to the issue page the moment the Drizzle table declared them -- a UI-visible payload
    // change in a feature that promises none. The agent API is where the v2 fields are read.
    async getOccurrencesByIssueId(issueId: string, limit = 100) {
        return await db
            .select(issuePageOccurrenceColumns)
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
                // sentinel-affected-users §4.1 page data: `0` is `0`, never omitted — NOT NULL DEFAULT 0 on the column.
                affectedUserCount: issues.affectedUserCount,
                affectedTenantCount: issues.affectedTenantCount,
                // sentinel-issue-trend §4.1, decision 22 — two EXISTING columns added to the
                // projection for the card's `REGRESSED` badge; nothing on the schema changes.
                // `regression_status` is `NOT NULL DEFAULT 'none'` (`none | regressed` by CHECK);
                // `last_regressed_at` is null until the processor's regression UPDATE has run.
                regressionStatus: issues.regressionStatus,
                lastRegressedAt: issues.lastRegressedAt,
                assignedTo: issues.assignedTo,
                assigneeType: issues.assigneeType,
            })
            .from(issues)
            .where(and(...conditions))
            .orderBy(...issueOrder(filters.sort))
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
     * Aggregates claimant counts across all matching issues in the database under active project/environment guards.
     * Evaluated WITHOUT applying `filters.claim`, so the sidebar shows true facet totals across all issues.
     */
    async getClaimantFacetCounts(filters: Omit<IssueFilters, 'claim'>, currentUserId?: string): Promise<ClaimantFacetCounts> {
        const conditions = buildIssueConditions(filters);

        // Overall summary counts
        const [summaryRow] = await db
            .select({
                total: sql<number>`cast(count(*) as integer)`,
                unclaimed: sql<number>`cast(count(*) filter (where ${issues.assignedTo} is null) as integer)`,
                claimedAgent: sql<number>`cast(count(*) filter (where ${issues.assignedTo} is not null and ${issues.assigneeType} = 'agent') as integer)`,
                claimedMe: sql<number>`cast(count(*) filter (where ${issues.assignedTo} is not null and ${issues.assigneeType} = 'user' and ${issues.assignedTo} = ${currentUserId ?? ''}) as integer)`,
            })
            .from(issues)
            .where(and(...conditions));

        // Group by assigned_to and assignee_type for distinct claimants
        const claimantRows = await db
            .select({
                assignedTo: issues.assignedTo,
                assigneeType: issues.assigneeType,
                count: sql<number>`cast(count(*) as integer)`,
            })
            .from(issues)
            .where(and(...conditions, isNotNull(issues.assignedTo)))
            .groupBy(issues.assignedTo, issues.assigneeType);

        const byClaimant: Record<string, number> = {};
        const claimantTypes: Record<string, string | null> = {};

        for (const row of claimantRows) {
            if (row.assignedTo) {
                byClaimant[row.assignedTo] = Number(row.count ?? 0);
                claimantTypes[row.assignedTo] = row.assigneeType ?? null;
            }
        }

        return {
            total: Number(summaryRow?.total ?? 0),
            unclaimed: Number(summaryRow?.unclaimed ?? 0),
            claimedMe: currentUserId ? Number(summaryRow?.claimedMe ?? 0) : 0,
            claimedAgent: Number(summaryRow?.claimedAgent ?? 0),
            byClaimant,
            claimantTypes,
        };
    },

    /**
     * sentinel-issue-trend §4.1 — the per-card series for one page of the list, in ONE query.
     *
     * Keyed on the ids `listIssues` already returned under the project and environment guards — it
     * takes ids, never a project or a user, so it cannot widen the set (§2): an id outside `ids`
     * contributes nothing even when it has rows. Bounded at BOTH ends (§3, decision 18): a row past
     * `windowEnd` — a future-dated event timestamp — neither extends a series nor lands in its last
     * slot.
     *
     * Dense by construction: every id in `ids` maps to `hourlySlotCount` zeros before any row is
     * placed, so a quiet issue is `[0 × 24]`, never absent and never shorter. Rows are placed by
     * slot index; the query's bounds guarantee every row has a slot, so nothing is dropped.
     *
     * **`ids = []` returns an empty `Map` and issues NO query.** `drizzle-orm@0.30.10`'s `inArray`
     * throws on an empty array (`inArray requires at least one value` — the hazard this file's
     * header documents for the project guard), and an empty page (`new=24h` with nothing new, a
     * page past the end) has nothing to attach a series to and must not 500.
     */
    async trendForIssues(ids: string[], windowStart: Date, windowEnd: Date): Promise<Map<string, number[]>> {
        const slots = hourlySlotCount(windowStart, windowEnd);
        const series = new Map<string, number[]>();
        for (const id of ids) {
            series.set(id, new Array<number>(slots).fill(0));
        }
        if (ids.length === 0) {
            return series;
        }

        const rows = await db
            .select({
                issueId: issueOccurrenceBuckets.issueId,
                bucketStart: issueOccurrenceBuckets.bucketStart,
                count: issueOccurrenceBuckets.count,
            })
            .from(issueOccurrenceBuckets)
            .where(
                and(
                    inArray(issueOccurrenceBuckets.issueId, ids),
                    gte(issueOccurrenceBuckets.bucketStart, windowStart),
                    lte(issueOccurrenceBuckets.bucketStart, windowEnd)
                )
            );

        for (const row of rows) {
            const points = series.get(row.issueId);
            const slot = Math.floor((row.bucketStart.getTime() - windowStart.getTime()) / HOUR_MS);
            // Neither branch is reachable — the query is keyed on `ids` and bounded on both ends —
            // but a densifier that could write past its array would silently grow a series.
            if (!points || slot < 0 || slot >= slots) continue;
            points[slot] += row.count;
        }
        return series;
    },

    /**
     * sentinel-issue-trend §4.2, decision 11 — both detail series from ONE query over the 30-day
     * CALENDAR window: `windowStart` = `00:00:00Z` of (UTC today − 29 days), `windowEnd` =
     * `trunc(now)`, ≤ 720 rows. Densified here, not in the template (decision 7):
     *
     *  - `hourly`: the 24 slots ending at `windowEnd` — `[0].start = windowEnd − 23h`,
     *    `[23].start = windowEnd`, the current partial hour.
     *  - `daily`: one entry per UTC calendar day from `windowStart` to `windowEnd`'s day, so
     *    `daily[i].start = windowStart + i days` and the last entry is today (UTC), partial. Each
     *    day's count is the sum of the hourly rows whose `bucket_start` falls in that UTC day.
     *
     * Bounded at both ends for the same reason as `trendForIssues`; the densifier never grows an
     * array, and the bounds mean it is never handed a row it would have to drop.
     */
    async trendForIssue(issueId: string, windowStart: Date, windowEnd: Date): Promise<IssueTrend> {
        const rows = await db
            .select({
                bucketStart: issueOccurrenceBuckets.bucketStart,
                count: issueOccurrenceBuckets.count,
            })
            .from(issueOccurrenceBuckets)
            .where(
                and(
                    eq(issueOccurrenceBuckets.issueId, issueId),
                    gte(issueOccurrenceBuckets.bucketStart, windowStart),
                    lte(issueOccurrenceBuckets.bucketStart, windowEnd)
                )
            )
            .orderBy(asc(issueOccurrenceBuckets.bucketStart));

        const hourlyStartMs = windowEnd.getTime() - (TREND_HOURLY_SLOTS - 1) * HOUR_MS;
        const hourly: TrendPoint[] = Array.from({ length: TREND_HOURLY_SLOTS }, (_, i) => ({
            start: slotStartIso(hourlyStartMs + i * HOUR_MS),
            count: 0,
        }));

        const dailyStartMs = windowStart.getTime();
        const dayCount = Math.floor((utcDayStart(windowEnd).getTime() - dailyStartMs) / DAY_MS) + 1;
        const daily: TrendPoint[] = Array.from({ length: dayCount }, (_, i) => ({
            start: slotStartIso(dailyStartMs + i * DAY_MS),
            count: 0,
        }));

        for (const row of rows) {
            const at = row.bucketStart.getTime();
            const hour = Math.floor((at - hourlyStartMs) / HOUR_MS);
            if (hour >= 0 && hour < TREND_HOURLY_SLOTS) {
                hourly[hour].count += row.count;
            }
            const day = Math.floor((at - dailyStartMs) / DAY_MS);
            if (day >= 0 && day < dayCount) {
                daily[day].count += row.count;
            }
        }
        return { hourly, daily };
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

    /**
     * Lists users belonging to the given organizations, used to populate claimant facets and resolve IDs.
     */
    async listOrgUsers(orgIds: string[]) {
        if (orgIds.length === 0) return [];
        return await db
            .select({
                id: users.id,
                name: users.name,
                email: users.email,
            })
            .from(users)
            .innerJoin(organizationMembers, eq(organizationMembers.userId, users.id))
            .where(inArray(organizationMembers.organizationId, orgIds));
    },

    /**
     * Lists agents belonging to the given organizations, used to resolve agent IDs to names.
     */
    async listOrgAgents(orgIds: string[]) {
        if (orgIds.length === 0) return [];
        return await db
            .select({
                id: agents.id,
                name: agents.name,
            })
            .from(agents)
            .where(inArray(agents.orgId, orgIds));
    },
};
