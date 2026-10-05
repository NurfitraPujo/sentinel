import { db } from "$lib/server/db";
import { issues, errorOccurrences, projects, issueOccurrenceBuckets, users, organizationMembers, agents } from "$lib/db/schema";
import { eq, ne, gte, lte, desc, asc, sql, and, inArray, isNull, isNotNull, type SQL } from "drizzle-orm";
import { environmentScopeCondition } from "$lib/server/environment-access";
import { coerceIssueSort, firstSeenLowerBound, HIGH_VOLUME_THRESHOLD } from "$lib/server/issue-sort";
import { DAY_MS, HOUR_MS, TREND_HOURLY_SLOTS, utcDayStart } from "$lib/server/trend-window";
import type { TagFacetRow } from "$lib/tag-facets";

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
     * fix-sentinel-issues-dashboard D3 — `volume=high`: `count >= HIGH_VOLUME_THRESHOLD`. Coerced by
     * the loader (`coerceIssueVolume`); a real predicate in the shared builder, so the list, the
     * total and every facet group narrow with it.
     */
    volume?: string;
    /** D3 — `last_seen >= seenAfter` (`seen=` preset or `seen_from`). Same shared builder. */
    seenAfter?: Date;
    /** D3 — `last_seen <= seenBefore` (`seen_to`). */
    seenBefore?: Date;
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

/**
 * The FILTERS a facet group may drop from its own count (§4.1: "every active filter except X").
 * Deliberately a closed list of filter names: the two GUARDS (`projectIds`, `environmentScopes`) are
 * not on it, so no caller can express "drop the guard".
 */
export type IssueFilterName = "status" | "new" | "environment" | "project" | "claim";

/**
 * fix-sentinel-issues-dashboard §4.1 — every sidebar count, over ALL issues the caller may see
 * under the other active filters, never the page.
 */
export interface IssueFacets {
    status: { all: number; unresolved: number; resolved: number; ignored: number };
    age: { all: number; new24h: number };
    /** `byEnvironment` is empty unless a project is selected; keys are `issues.environment`. */
    environment: { all: number; byEnvironment: Record<string, number> };
    /** Keyed by project id. */
    project: { all: number; byProject: Record<string, number> };
    claim: ClaimantFacetCounts;
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
function buildIssueConditions(filters: IssueFilters, omit: ReadonlySet<IssueFilterName> = new Set()): SQL[] {
    const conditions: SQL[] = [];

    if (!omit.has("status") && filters.status && filters.status !== "all") {
        conditions.push(eq(issues.status, filters.status));
    }

    if (!omit.has("project") && filters.projectId && filters.projectId !== "all") {
        // ANDed with the tenancy predicate below, never instead of it: `eq(projectId, selected)`
        // alone does not check that the caller may reach the selected project, so `?project=<a
        // foreign uuid>` would read another organization's issues.
        conditions.push(eq(issues.projectId, filters.projectId));
    }

    conditions.push(inAuthorisedProjects(issues.projectId, filters.projectIds ?? []));

    if (!omit.has("environment") && filters.environment && filters.environment !== "all") {
        conditions.push(eq(issues.environment, filters.environment));
    }

    const guard = environmentScopeCondition(filters.environmentScopes ?? new Map());
    if (guard) {
        conditions.push(guard);
    }

    // sentinel-issue-trend §4.1 (decision 6): `new=24h`, ANDed inside the guards above — one more
    // condition on the same query, never a read path around them. Here rather than in `listIssues`
    // so the count narrows with the list by construction.
    if (!omit.has("new") && filters.firstSeenAfter) {
        conditions.push(gte(issues.firstSeen, filters.firstSeenAfter));
    }

    // D3: volume and the `last_seen` window. Not omittable by any facet group: no group counts
    // "except volume" or "except seen", so the counts always agree with the list on them.
    if (filters.volume === "high") {
        conditions.push(gte(issues.count, HIGH_VOLUME_THRESHOLD));
    }
    if (filters.seenAfter) {
        conditions.push(gte(issues.lastSeen, filters.seenAfter));
    }
    if (filters.seenBefore) {
        conditions.push(lte(issues.lastSeen, filters.seenBefore));
    }

    if (!omit.has("claim") && filters.claim && filters.claim !== "all") {
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
            // Specific claimant: the EXACT `assigned_to` value (user id, agent id, or a legacy email).
            conditions.push(eq(issues.assignedTo, filters.claim));
        }
    }

    if (filters.q && filters.q.trim()) {
        const rawQ = filters.q.trim();
        // D6: `%`, `_` and `\` are escaped so a search for `100%` matches the literal. The FTS arm
        // takes the raw term and is unchanged.
        const term = `%${rawQ.replace(/[\\%_]/g, "\\$&")}%`;
        conditions.push(
            sql`(${issues.errorClass} ILIKE ${term} ESCAPE '\\' OR ${issues.fingerprint} ILIKE ${term} ESCAPE '\\' OR to_tsvector('english', ${issues.message}) @@ plainto_tsquery('english', ${rawQ}))`
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
 * `last_seen` is `last_seen DESC NULLS LAST, id DESC` (fix-sentinel-issues-dashboard D5): it used to
 * have no unique final key. The leading order is today's.
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
            // D5: a unique final key, or an offset page can repeat or skip rows. NULLS LAST spelled
            // out for the same reason as `first_seen` below.
            return [sql`${issues.lastSeen} DESC NULLS LAST`, desc(issues.id)];
    }
}

/**
 * Whether `countOccurrencesByIssueId` must take the exact `COUNT(*)` for an issue of this
 * `issue_type` instead of the bucket sum. True for exactly the issues retention.ts keeps past the
 * bucket cutoff: `'user_report'`, the A04 branch whose deletion is gated on `manualRetentionDays`
 * (`MANUAL_ISSUE_RETENTION_DAYS`, default 365) instead of `retentionDays` (`DATA_RETENTION_DAYS`,
 * which is also where every `issue_occurrence_buckets` row goes). Anything retained under that longer
 * constant would be undercounted by buckets swept at the shorter one, so it is counted exactly.
 *
 * As retention.ts stands, that undercount cannot happen: the occurrence delete applies the short
 * `cutoffDate` to every issue type, and manual issues have no `error_occurrences` rows by
 * construction, so the `COUNT(*)` is an index probe that finds nothing. The rule is kept so that it
 * stays correct if either changes. A retention change that keeps occurrences of some issue past the
 * bucket cutoff, or keeps buckets past the occurrence cutoff, has to update this rule too.
 */
export function occurrenceTotalIsExact(issueType: string): boolean {
    return issueType === 'user_report';
}

/**
 * The `error_occurrences` columns the issue page receives for every listed occurrence: the 12 pre-v2
 * columns (sentinel-event-schema-v2 §8.3) plus the six light v2 header columns
 * (sentinel-occurrence-detail-panels §3.1). The heavy JSONB bodies are not here; they go only to the
 * selected occurrence, through `issuePageOccurrenceDetailColumns`.
 */
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
    level: errorOccurrences.level,
    message: errorOccurrences.message,
    serverName: errorOccurrences.serverName,
    transaction: errorOccurrences.transaction,
    dist: errorOccurrences.dist,
    sdk: errorOccurrences.sdk,
};

/**
 * The whole row the issue page can render, for the ONE selected occurrence
 * (sentinel-occurrence-detail-panels §3.1): the list keys (a selection older than the newest 100 has
 * no listed row to borrow them from) plus the six heavy JSONB bodies. `user` is aliased to
 * `eventUser` as `agentOccurrenceColumns` does. `logger`, `errorClass`, `source`, `traceFlags` and
 * `receivedAt` are not rendered, so not selected.
 */
export const issuePageOccurrenceDetailColumns = {
    ...issuePageOccurrenceColumns,
    user: errorOccurrences.eventUser,
    tags: errorOccurrences.tags,
    contexts: errorOccurrences.contexts,
    request: errorOccurrences.request,
    breadcrumbs: errorOccurrences.breadcrumbs,
    exception: errorOccurrences.exception,
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

    // sentinel-occurrence-detail-panels §3.1: an explicit projection. The list carries the light header
    // fields of every listed occurrence; the heavy v2 JSONB bodies (breadcrumbs, contexts, request, ...)
    // go only to the selected one, through getOccurrenceDetail. A bare select() would ship every body
    // of up to 100 occurrences to the page (§3.3 measures it at about 7x today's payload).
    async getOccurrencesByIssueId(issueId: string, limit = 100) {
        return await db
            .select(issuePageOccurrenceColumns)
            .from(errorOccurrences)
            .where(eq(errorOccurrences.issueId, issueId))
            .orderBy(desc(errorOccurrences.createdAt))
            .limit(limit);
    },

    /**
     * sentinel-occurrence-detail-panels §3.1: the selected occurrence's whole renderable row, or null.
     * BOTH predicates are required: the issueId one is what stops an occurrence id from being read
     * through another issue's page.
     */
    async getOccurrenceDetail(issueId: string, occurrenceId: string) {
        const [row] = await db
            .select(issuePageOccurrenceDetailColumns)
            .from(errorOccurrences)
            .where(and(eq(errorOccurrences.id, occurrenceId), eq(errorOccurrences.issueId, issueId)))
            .limit(1);
        return row ?? null;
    },

    /**
     * sentinel-occurrence-detail-panels §3.2 S3b: whether the id belongs to some OTHER issue. Selects the
     * id only, so no column of another issue's occurrence is read into the load. A primary-key lookup.
     */
    async occurrenceExistsOutsideIssue(issueId: string, occurrenceId: string): Promise<boolean> {
        const rows = await db
            .select({ id: errorOccurrences.id })
            .from(errorOccurrences)
            .where(and(eq(errorOccurrences.id, occurrenceId), ne(errorOccurrences.issueId, issueId)))
            .limit(1);
        return rows.length > 0;
    },

    /**
     * §4.5a, flow `D2` — the RETAINED occurrence total behind the `Occurrence History` heading: how
     * many `error_occurrences` rows this issue still has, not `issues.count` (lifetime, already in
     * the header).
     *
     * `getOccurrencesByIssueId` caps at 100 and the heading rendered `occurrences.length` as though
     * it were a total, which is untrue for any issue with more than a page of occurrences. The
     * heading shows this number and the list below says it is showing the most recent *n*.
     *
     * Read as `sum(count)` over the issue's `issue_occurrence_buckets` rows, NOT `COUNT(*)` over
     * `error_occurrences`. `COUNT(*)` walks every occurrence of the issue on every page view: in
     * production (2026-10-04, 5.1M occurrences) it took 66.9 s for a 1,788,214-occurrence issue,
     * 39 s for 347k and 8.4 s for 286k (8.3 s mean in pg_stat_statements), while every other read
     * on the page took 4–45 ms. The bucket sum returned the same number on those issues in 4–60 ms.
     * It is the same number because the processor upserts the occurrence's hourly bucket in the
     * same transaction as the occurrence (`occurrenceBucketUpsertQuery`, processor-go store.go),
     * and the retention sweep deletes buckets at the same `cutoffDate` as occurrences
     * (retention.ts). The one hour the cutoff falls inside is dropped up to an hour early, so at
     * most that hour's occurrences can be missing from the sum.
     *
     * `issueType` picks the source — see `occurrenceTotalIsExact`.
     */
    async countOccurrencesByIssueId(issueId: string, issueType: string) {
        if (occurrenceTotalIsExact(issueType)) {
            const [row] = await db
                .select({ count: sql<number>`cast(count(*) as integer)` })
                .from(errorOccurrences)
                .where(eq(errorOccurrences.issueId, issueId));
            return Number(row?.count ?? 0);
        }
        const [row] = await db
            .select({ count: sql<number>`cast(coalesce(sum(${issueOccurrenceBuckets.count}), 0) as integer)` })
            .from(issueOccurrenceBuckets)
            .where(eq(issueOccurrenceBuckets.issueId, issueId));
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
    async getClaimantFacetCounts(filters: IssueFilters, currentUserId?: string): Promise<ClaimantFacetCounts> {
        const conditions = buildIssueConditions(filters, new Set<IssueFilterName>(["claim"]));

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
     * fix-sentinel-issues-dashboard §4.1 (D1) — every sidebar count, from the database.
     *
     * Each group is the list's own condition set minus the ONE filter the group switches
     * (`buildIssueConditions`' `omit`), so a count never reflects the page, the page size or the
     * group's own selection — and never loses a guard: `omit` can name filters only, and the project
     * and environment guards are applied by the builder on every call. A fixed handful of statements
     * run in parallel, independent of page size and of the number of issues.
     *
     * `now` is the loader's ONE instant, so the `24h` age count agrees with `new=24h` on the list.
     */
    async getIssueFacets(filters: IssueFilters, now: Date, currentUserId?: string): Promise<IssueFacets> {
        const where = (...omit: IssueFilterName[]) => and(...buildIssueConditions(filters, new Set(omit)));
        const n = (value: unknown) => Number(value ?? 0);
        const count = sql<number>`cast(count(*) as integer)`;
        const newBound = firstSeenLowerBound("24h", now)!;
        const environmentSelected = Boolean(filters.projectId && filters.projectId !== "all");

        const [statusRows, [ageRow], environmentRows, projectRows, claim] = await Promise.all([
            db.select({ status: issues.status, count }).from(issues).where(where("status")).groupBy(issues.status),
            db
                .select({
                    all: count,
                    new24h: sql<number>`cast(count(*) filter (where ${gte(issues.firstSeen, newBound)}) as integer)`,
                })
                .from(issues)
                .where(where("new")),
            environmentSelected
                ? db
                      .select({ environment: issues.environment, count })
                      .from(issues)
                      .where(where("environment"))
                      .groupBy(issues.environment)
                : Promise.resolve([] as { environment: string; count: number }[]),
            // A project change drops `environment` (it is per project), so this group does too.
            db
                .select({ projectId: issues.projectId, count })
                .from(issues)
                .where(where("project", "environment"))
                .groupBy(issues.projectId),
            issueQueries.getClaimantFacetCounts(filters, currentUserId),
        ]);

        const status = { all: 0, unresolved: 0, resolved: 0, ignored: 0 };
        for (const row of statusRows) {
            const c = n(row.count);
            status.all += c;
            if (row.status === "unresolved" || row.status === "resolved" || row.status === "ignored") {
                status[row.status] = c;
            }
        }

        const byEnvironment: Record<string, number> = {};
        let environmentAll = 0;
        for (const row of environmentRows) {
            byEnvironment[row.environment] = n(row.count);
            environmentAll += n(row.count);
        }

        const byProject: Record<string, number> = {};
        let projectAll = 0;
        for (const row of projectRows) {
            byProject[row.projectId] = n(row.count);
            projectAll += n(row.count);
        }

        return {
            status,
            age: { all: n(ageRow?.all), new24h: n(ageRow?.new24h) },
            environment: { all: environmentAll, byEnvironment },
            project: { all: projectAll, byProject },
            claim,
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
            .where(inAuthorisedProjects(projects.id, projectIds));
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
     * sentinel-issue-tag-facets spec §7.2 (TASK-02): the issue page's tag distribution, ONE statement
     * per page load over the `issue_tag_values` rollup (never one per key): each key's top 5 named
     * values (count descending, then value in byte order), its overflow row (`value = ''`) when it has
     * one, and its total over every row. The derived-key display order comes from
     * `issue_tag_derived(NULL, NULL, NULL, NULL)` (the one list of derived keys, in the migration), never
     * from a TypeScript list. Callers have already passed both access checks for this issue; every row
     * belongs to it. The PK `(issue_id, key, value)` serves the range.
     */
    async getTagFacets(issueId: string): Promise<TagFacetRow[]> {
        const rows = await db.execute<{
            key: string;
            value: string;
            count: string | number;
            key_total: string | number;
            derived_ord: number | null;
        }>(sql`
            SELECT s.key, s.value, s.count, s.key_total, d.ord AS derived_ord
              FROM (SELECT t.key, t.value, t.count,
                           sum(t.count) OVER (PARTITION BY t.key) AS key_total,
                           row_number() OVER (PARTITION BY t.key ORDER BY (t.value = ''), t.count DESC, t.value COLLATE "C") AS rn
                      FROM issue_tag_values t
                     WHERE t.issue_id = ${issueId}) s
              LEFT JOIN issue_tag_derived(NULL, NULL, NULL, NULL) d ON d.key = s.key
             WHERE s.rn <= 5 OR s.value = ''
             ORDER BY d.ord NULLS LAST, s.key COLLATE "C", s.rn`);
        return rows.map((r) => ({
            key: r.key,
            value: r.value,
            count: Number(r.count),
            keyTotal: Number(r.key_total),
            derivedOrd: r.derived_ord === null ? null : Number(r.derived_ord),
        }));
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
