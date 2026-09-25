import { db } from './db';
import { errorSearchIndex, errorOccurrences, issues, projects } from '$lib/db/schema';
import { eq, and, or, inArray, sql, desc, asc, type SQL } from 'drizzle-orm';
import type { AuthUser } from './auth';
import { getAccessibleProjects } from './issue-access';
import { environmentScopeCondition, resolveEnvironmentScopes } from './environment-access';
import type { TagFilter } from '$lib/tag-filter';

export interface SearchFilters {
	userId?: string;
	tenantId?: string;
	traceId?: string;
	spanId?: string;
	requestId?: string;
	projectId?: string;
	environment?: string;
	platform?: string;
	search?: string;
	// sentinel-issue-tag-facets §6.3: parsed `tag` filters (≤ MAX_TAG_FILTERS), each ONE more ANDed
	// element. Absent or empty: the search is exactly today's (§6.6).
	tags?: TagFilter[];
}

/**
 * sentinel-issue-tag-facets §6.4: branch (b)'s index hints for the DERIVED keys — an extra predicate every
 * occurrence carrying (k, v) satisfies, so an index can serve the overflowed issue's probe. The one
 * TypeScript structure keyed by derived-key names; an optimisation, never a definition (the key list is
 * issue_tag_derived's, and `search.tag-hints.test.ts` pins every entry to it). `index` names the index the
 * hint is written for (null: none needed — `environment` is part of an issue's identity so it never
 * overflows; the OS/browser names are bounded vocabularies and scan the issue's occurrences).
 */
export const TAG_SEARCH_HINTS: Record<string, { index: string | null; predicate: (value: string) => SQL }> = {
	release: { index: 'idx_occurrences_issue_release', predicate: (v) => sql`o2.release_version = ${v}` },
	environment: { index: null, predicate: (v) => sql`o2.environment = ${v}` },
	'os.name': { index: null, predicate: () => sql`TRUE` },
	'client_os.name': { index: null, predicate: () => sql`TRUE` },
	'device.model': {
		index: 'idx_error_occurrences_device_model',
		predicate: (v) =>
			sql`jsonb_typeof(o2.contexts->'device'->'model') = 'string' AND (o2.contexts->'device'->>'model') = ${v}`,
	},
	'browser.name': { index: null, predicate: () => sql`TRUE` },
	'user.id': {
		index: 'idx_error_occurrences_user_id',
		predicate: (v) => sql`jsonb_typeof(o2.event_user->'id') = 'string' AND (o2.event_user->>'id') = ${v}`,
	},
};

/** The issue-scoped jsonb_path_ops GIN that serves branch (b) for a custom (non-derived) key. */
export const CUSTOM_TAG_HINT_INDEX = 'idx_error_occurrences_issue_tags';

/**
 * sentinel-issue-tag-facets §6.4: one tag filter as ONE condition on the outer `issues` row. An issue
 * matches iff one of its counted retained occurrences has (k, v) among issue_tag_pairs:
 *   (a) v has its own rollup row — by I-cap it then holds every counted v; or
 *   (b) k overflowed on this issue (its '' row exists) and one of the issue's occurrences carries v.
 * Branch (b) is driven by the overflow row, so it reads occurrences only of issues whose key overflowed,
 * stops at the first match, and is correlated to that issue through the key's hint index. The decision is
 * always issue_tag_pairs'. For a custom key the issue-scoped containment REPLACES `o2.issue_id =
 * ov.issue_id` (with both present the planner BitmapAnds the whole issue's issue_id entries).
 */
export function tagCondition({ key, value }: TagFilter): SQL {
	const hint = Object.hasOwn(TAG_SEARCH_HINTS, key) ? TAG_SEARCH_HINTS[key] : undefined;
	const correlate = hint
		? sql`o2.issue_id = ov.issue_id AND ${hint.predicate(value)}`
		: sql`o2.tags IS NOT NULL AND jsonb_set('{}'::jsonb, ARRAY[o2.issue_id::text], o2.tags) @> jsonb_set('{}'::jsonb, ARRAY[ov.issue_id::text], jsonb_build_object(${key}::text, ${value}::text))`;
	return sql`(EXISTS (SELECT 1 FROM issue_tag_values t
	                 WHERE t.issue_id = ${issues.id} AND t.key = ${key} AND t.value = ${value})
	     OR EXISTS (SELECT 1 FROM issue_tag_values ov
	                 CROSS JOIN LATERAL (
	                       SELECT 1 FROM error_occurrences o2
	                        CROSS JOIN LATERAL issue_tag_pairs(o2.tags, o2.contexts, o2.event_user, o2.release_version, o2.environment) p
	                        WHERE ${correlate} AND p.key = ${key} AND p.value = ${value}
	                        LIMIT 1) x
	                 WHERE ov.issue_id = ${issues.id} AND ov.key = ${key} AND ov.value = ''))`;
}

/**
 * sentinel-issue-tag-facets §6.4 "The bound": rows a plan read on the occurrence side — (Actual Rows + Rows
 * Removed by Filter + Rows Removed by Index Recheck) × Actual Loops, summed over every scan node whose
 * relation, or whose index's table (`indexTables`, from pg_index), is one of `tables`. The removed terms
 * are what a scan read and its Filter discarded; without them a scan that reads an issue whole and keeps
 * one row looks like a one-row read. Used by the (5c) proof and by P3 on staging.
 */
export function rowsReadFromPlan(
	plan: { Plan?: unknown } | Record<string, unknown>,
	indexTables: Map<string, string>,
	tables: readonly string[] = ['error_occurrences', 'error_search_index']
): number {
	let total = 0;
	const visit = (node: Record<string, unknown>) => {
		const relation = node['Relation Name'] as string | undefined;
		const index = node['Index Name'] as string | undefined;
		const table = relation ?? (index ? indexTables.get(index) : undefined);
		if (table && tables.includes(table)) {
			const n = (k: string) => Number(node[k] ?? 0);
			total += (n('Actual Rows') + n('Rows Removed by Filter') + n('Rows Removed by Index Recheck')) * n('Actual Loops');
		}
		for (const child of (node.Plans as Record<string, unknown>[] | undefined) ?? []) visit(child);
	};
	visit(((plan as { Plan?: unknown }).Plan ?? plan) as Record<string, unknown>);
	return total;
}

export interface SearchResult {
	issueId: string;
	projectId: string;
	fingerprint: string;
	message: string;
	errorClass: string;
	status: string;
	count: number;
	firstSeen: Date | null;
	lastSeen: Date | null;
	projectName: string;
	matchingOccurrence: {
		id: string;
		environment: string;
		platform: string;
		traceId: string | null;
		spanId: string | null;
		requestId: string | null;
		userId: string | null;
		tenantId: string | null;
		createdAt: Date | null;
	};
}

export async function searchErrors(
	user: AuthUser,
	filters: SearchFilters,
	options: { limit?: number; offset?: number } = {}
): Promise<{ results: SearchResult[]; total: number }> {
	const { limit = 50, offset = 0 } = options;

	// sentinel-project-environments §4.5 change 3, decision 31 — an ACCESS change, written as one.
	// This derivation read `project_members` and nothing in this application writes that table, so
	// `searchErrors` returned `{ results: [], total: 0 }` to EVERYBODY, unconditionally. It widens
	// to the population §4.3a, §4.4 and §4.10 use: `project_members` ∪ `organization_members` in
	// `ISSUE_WRITE_ROLES`. That widens what a caller can read, from "nothing" to "the projects their
	// org role already entitles them to", and it un-breaks the search page for every user — nobody
	// asked this feature for that and it is reported rather than discovered.
	//
	// It is NOT a vulnerability fix: this function is fail-CLOSED today. The early return below is
	// transcribed and stays, so the empty set still denies. (`listIssues` fails the other way from
	// the same cause; that is §4.4's fix, not this one.)
	const accessibleProjects = await getAccessibleProjects(user.id);

	const projectIds = accessibleProjects.map(p => p.id);

	if (projectIds.length === 0) {
		return { results: [], total: 0 };
	}

	// §4.5 change 2, and it goes on AFTER the widening, over the widened set. Resolved per project:
	// a member granted `staging` in one project must not get `production` occurrences back from
	// another project they are unrestricted in, and must not get them from this one.
	const environmentScopes = await resolveEnvironmentScopes(user.id);

	const conditions: any[] = [];

	const environmentGuard = environmentScopeCondition(environmentScopes);
	if (environmentGuard) {
		conditions.push(environmentGuard);
	}

	if (filters.projectId) {
		if (!projectIds.includes(filters.projectId)) {
			return { results: [], total: 0 };
		}
		conditions.push(eq(issues.projectId, filters.projectId));
	} else {
		conditions.push(inArray(issues.projectId, projectIds));
	}

	// sentinel-issue-tag-facets §6.4: with a tag filter and no id filter the search takes the per-issue form
	// (form L), which reads the issue-level conditions on the outer row; `conditions` before the two
	// occurrence-level filters is exactly that set, so it is kept apart here.
	const issueLevelConditions: SQL[] = [...conditions];

	if (filters.environment) {
		conditions.push(eq(errorOccurrences.environment, filters.environment));
	}

	if (filters.platform) {
		conditions.push(eq(errorOccurrences.platform, filters.platform));
	}

	const searchConditions: any[] = [];
	if (filters.userId) {
		searchConditions.push(eq(errorSearchIndex.userId, filters.userId));
	}
	if (filters.tenantId) {
		searchConditions.push(eq(errorSearchIndex.tenantId, filters.tenantId));
	}
	if (filters.traceId) {
		searchConditions.push(eq(errorSearchIndex.traceId, filters.traceId));
	}
	if (filters.spanId) {
		searchConditions.push(eq(errorSearchIndex.spanId, filters.spanId));
	}
	if (filters.requestId) {
		searchConditions.push(eq(errorSearchIndex.requestId, filters.requestId));
	}

	let ftsQuery = '';
	if (filters.search) {
		const sanitized = filters.search
			.replace(/[<>()]/g, ' ')
			.trim();
		if (sanitized) {
			const terms = sanitized.split(/\s+/).filter(t => t.length > 0);
			if (terms.length > 0) {
				const tsqueryTerms = terms.map(t => {
					if (t.startsWith('*') && t.endsWith('*')) {
						return t.slice(1, -1);
					}
					if (t.startsWith('*') || t.endsWith('*')) {
						return t.replace(/\*/g, '') + ':*';
					}
					return t + ':*';
				});
				ftsQuery = tsqueryTerms.join(' & ');
			}
		}
	}

	// §4.5: ONE condition set, composed once and applied once to each query.
	//
	// Drizzle's `.where()` ASSIGNS — `this.config.where = where` — so calling it three times on the
	// same builder keeps only the third. This function did exactly that (fts, then `conditions`,
	// then `searchConditions`), which meant the project scope was dropped from the result query
	// whenever any `errorSearchIndex` filter was supplied, and the full-text predicate was dropped
	// from both queries whenever any condition was. That was invisible while `projectIds` was empty
	// for everybody and the function early-returned; with the derivation widened above it is a live
	// leak of exactly the kind §4.5's guard exists to close, so the composition is fixed here rather
	// than the guard being layered on top of it.
	const allConditions: any[] = [...conditions];
	if (ftsQuery) {
		allConditions.push(
			sql`to_tsvector('english', ${issues.message}) @@ to_tsquery('english', ${ftsQuery})`
		);
	}
	if (searchConditions.length > 0) {
		allConditions.push(or(...searchConditions));
	}

	// sentinel-issue-tag-facets §6.3-§6.4: each tag filter is ONE more ANDed element — with each other, with
	// project / environment / platform / FTS, and with the id OR-group as a whole. With no tag filter
	// nothing below runs and the two queries after it are today's, statement for statement (§6.6).
	const tags = filters.tags ?? [];
	if (tags.length > 0) {
		const tagConditions = tags.map(tagCondition);
		if (searchConditions.length > 0) {
			// Form J: an id filter drives today's join through error_search_index's own index; the tag
			// conditions only filter the issues it reaches.
			allConditions.push(...tagConditions);
		} else {
			return await searchPerIssue(
				[
					...issueLevelConditions,
					...(ftsQuery ? [sql`to_tsvector('english', ${issues.message}) @@ to_tsquery('english', ${ftsQuery})`] : []),
					// I-env: every occurrence's environment equals its issue's, so the issue-level test selects
					// the same issues and the per-issue read needs no environment test at all.
					...(filters.environment ? [eq(issues.environment, filters.environment)] : []),
					...tagConditions,
				],
				filters.platform,
				limit,
				offset
			);
		}
	}

	// sentinel-project-environments/BUG-14 — one row per ISSUE, not one row per OCCURRENCE.
	//
	// `issues → error_occurrences → error_search_index` is one-to-many, so the result query fanned
	// out: an issue with 50 indexed occurrences produced 50 rows and filled an entire 50-row page by
	// itself, while the count query below has always counted `DISTINCT issues.id`. `total` therefore
	// described a set that pagination could not produce. The fan-out was invisible until decision 31
	// widened the access derivation — before that `searchErrors` returned `{ results: [], total: 0 }`
	// to everybody and nothing ever fanned out.
	//
	// `DISTINCT ON (issues.id)` over the SAME `allConditions` picks exactly one occurrence per issue:
	// the most recent MATCHING one, tie-broken on the occurrence id so the choice is stable rather
	// than whatever the plan happens to emit. It selects the occurrence id alone — one column, so the
	// subquery has no ambiguous `"id"` to resolve — and the result query then joins to it, which
	// leaves every column of the projection reading from its own table exactly as before.
	const chosenOccurrence = db
		.selectDistinctOn([issues.id], { occurrenceId: errorOccurrences.id })
		.from(issues)
		.innerJoin(errorOccurrences, eq(errorOccurrences.issueId, issues.id))
		.innerJoin(errorSearchIndex, eq(errorSearchIndex.occurrenceId, errorOccurrences.id))
		.where(allConditions.length > 0 ? and(...allConditions) : undefined)
		// Postgres requires the DISTINCT ON expressions to lead the ORDER BY. This ordering decides
		// WHICH occurrence survives; the ordering the CALLER sees is the one applied below.
		.orderBy(issues.id, desc(errorOccurrences.createdAt), desc(errorOccurrences.id))
		.as('chosen_occurrence');

	// The guard stays on the result query itself (§4.5) — the join above narrows the cardinality, it
	// is not the access predicate. And LIMIT/OFFSET over an unordered set is non-deterministic
	// paging: the same offset can return one row twice across two requests and skip another
	// entirely. `lastSeen` is the order the page presents; `issues.id` is the tie-break that makes
	// it total.
	const results = await db
		.select({
			issueId: issues.id,
			projectId: issues.projectId,
			fingerprint: issues.fingerprint,
			message: issues.message,
			errorClass: issues.errorClass,
			status: issues.status,
			count: issues.count,
			firstSeen: issues.firstSeen,
			lastSeen: issues.lastSeen,
			projectName: projects.name,
			occurrenceId: errorOccurrences.id,
			occurrenceEnvironment: errorOccurrences.environment,
			occurrencePlatform: errorOccurrences.platform,
			occurrenceTraceId: errorOccurrences.traceId,
			occurrenceSpanId: errorOccurrences.spanId,
			occurrenceCreatedAt: errorOccurrences.createdAt,
			searchUserId: errorSearchIndex.userId,
			searchTenantId: errorSearchIndex.tenantId,
			searchTraceId: errorSearchIndex.traceId,
			searchSpanId: errorSearchIndex.spanId,
			searchRequestId: errorSearchIndex.requestId,
		})
		.from(issues)
		.innerJoin(projects, eq(issues.projectId, projects.id))
		.innerJoin(errorOccurrences, eq(errorOccurrences.issueId, issues.id))
		.innerJoin(errorSearchIndex, eq(errorSearchIndex.occurrenceId, errorOccurrences.id))
		.innerJoin(chosenOccurrence, eq(chosenOccurrence.occurrenceId, errorOccurrences.id))
		.where(allConditions.length > 0 ? and(...allConditions) : undefined)
		.orderBy(desc(issues.lastSeen), asc(issues.id))
		.limit(limit)
		.offset(offset);

	// The SAME `allConditions`, not a re-derivation of it. §4.5: a guard on the results and not on
	// the count is a search that reports how many production hits exist and shows none of them.
	const countQuery = db
		.select({ count: sql<number>`cast(count(DISTINCT ${issues.id}) as integer)` })
		.from(issues)
		.innerJoin(errorOccurrences, eq(errorOccurrences.issueId, issues.id))
		.innerJoin(errorSearchIndex, eq(errorSearchIndex.occurrenceId, errorOccurrences.id))
		.where(allConditions.length > 0 ? and(...allConditions) : undefined);

	const countResult = await countQuery;

	const total = countResult[0]?.count ?? 0;

	const searchResults: SearchResult[] = results.map(row => ({
		issueId: row.issueId,
		projectId: row.projectId,
		fingerprint: row.fingerprint,
		message: row.message,
		errorClass: row.errorClass,
		status: row.status,
		count: row.count,
		firstSeen: row.firstSeen,
		lastSeen: row.lastSeen,
		projectName: row.projectName,
		matchingOccurrence: {
			id: row.occurrenceId,
			environment: row.occurrenceEnvironment,
			platform: row.occurrencePlatform,
			traceId: row.occurrenceTraceId,
			spanId: row.occurrenceSpanId,
			requestId: row.searchRequestId,
			userId: row.searchUserId,
			tenantId: row.searchTenantId,
			createdAt: row.occurrenceCreatedAt,
		},
	}));

	return { results: searchResults, total: Number(total) };
}

/**
 * sentinel-issue-tag-facets §6.4 form L: a tag search with no id filter. A tag matches ISSUES, and today's
 * join would read every occurrence of every matching issue; here each issue that passes the issue-level
 * and tag conditions reads only its most recent qualifying occurrence (with an index row, the inner join
 * kept: §6.5) through a LATERAL — the same `(issue, occurrence)` rows as today's DISTINCT ON shape and the
 * same total. The one occurrence-level filter left, `platform`, goes inside the LATERAL, served by
 * idx_error_occurrences_issue_platform; `environment` is tested on `issues` by the caller (I-env).
 */
async function searchPerIssue(
	issueConditions: SQL[],
	platform: string | undefined,
	limit: number,
	offset: number
): Promise<{ results: SearchResult[]; total: number }> {
	const where = and(...issueConditions);
	const platformCondition = platform ? sql` AND o.platform = ${platform}` : sql``;
	const rows = await db.execute<Record<string, unknown>>(sql`
		SELECT ${issues.id} AS issue_id, ${issues.projectId} AS project_id, ${issues.fingerprint} AS fingerprint,
		       ${issues.message} AS message, ${issues.errorClass} AS error_class, ${issues.status} AS status,
		       ${issues.count} AS count, ${issues.firstSeen} AS first_seen, ${issues.lastSeen} AS last_seen,
		       ${projects.name} AS project_name, m.*
		  FROM ${issues}
		  JOIN ${projects} ON ${issues.projectId} = ${projects.id}
		  CROSS JOIN LATERAL (SELECT o.id AS occurrence_id, o.environment AS occurrence_environment,
		                             o.platform AS occurrence_platform, o.trace_id AS occurrence_trace_id,
		                             o.span_id AS occurrence_span_id, o.created_at AS occurrence_created_at,
		                             s.user_id AS search_user_id, s.tenant_id AS search_tenant_id,
		                             s.trace_id AS search_trace_id, s.span_id AS search_span_id,
		                             s.request_id AS search_request_id
		                        FROM error_occurrences o
		                        JOIN error_search_index s ON s.occurrence_id = o.id
		                       WHERE o.issue_id = ${issues.id}${platformCondition}
		                       ORDER BY o.created_at DESC, o.id DESC
		                       LIMIT 1) m
		 WHERE ${where}
		 ORDER BY ${issues.lastSeen} DESC, ${issues.id}
		 LIMIT ${limit} OFFSET ${offset}`);
	// sentinel-issue-tag-facets §6.4 (5c): unlike the row query above, existence never needs the o.id DESC
	// tiebreak — WHICH occurrence answers "at least one exists" is irrelevant. Ordering by created_at DESC
	// alone is exactly presorted by idx_occurrences_issue_created (and, with a platform filter, by
	// idx_error_occurrences_issue_platform), so the index scan + LIMIT 1 reads exactly one row with no
	// lookahead. Adding the id tiebreak would force an Incremental Sort to read one extra row per group to
	// confirm the group boundary — needless cost for a query that only asks "does one exist".
	const [countRow] = await db.execute<{ count: number | string }>(sql`
		SELECT count(*)::int AS count
		  FROM ${issues}
		  CROSS JOIN LATERAL (SELECT 1 FROM error_occurrences o
		                        JOIN error_search_index s ON s.occurrence_id = o.id
		                       WHERE o.issue_id = ${issues.id}${platformCondition}
		                       ORDER BY o.created_at DESC
		                       LIMIT 1) m
		 WHERE ${where}`);
	// The same driver-value mapping drizzle's builder applies to these columns in today's shape.
	const ts = (column: { mapFromDriverValue: (v: never) => unknown }, v: unknown) =>
		v === null || v === undefined ? null : (column.mapFromDriverValue(v as never) as Date);
	const results: SearchResult[] = rows.map((r) => ({
		issueId: r.issue_id as string,
		projectId: r.project_id as string,
		fingerprint: r.fingerprint as string,
		message: r.message as string,
		errorClass: r.error_class as string,
		status: r.status as string,
		count: Number(r.count),
		firstSeen: ts(issues.firstSeen, r.first_seen),
		lastSeen: ts(issues.lastSeen, r.last_seen),
		projectName: r.project_name as string,
		matchingOccurrence: {
			id: r.occurrence_id as string,
			environment: r.occurrence_environment as string,
			platform: r.occurrence_platform as string,
			traceId: (r.occurrence_trace_id as string | null) ?? null,
			spanId: (r.occurrence_span_id as string | null) ?? null,
			requestId: (r.search_request_id as string | null) ?? null,
			userId: (r.search_user_id as string | null) ?? null,
			tenantId: (r.search_tenant_id as string | null) ?? null,
			createdAt: ts(errorOccurrences.createdAt, r.occurrence_created_at),
		},
	}));
	return { results, total: Number(countRow?.count ?? 0) };
}
