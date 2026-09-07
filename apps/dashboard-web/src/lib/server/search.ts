import { db } from './db';
import { errorSearchIndex, errorOccurrences, issues, projects } from '$lib/db/schema';
import { eq, and, or, inArray, sql } from 'drizzle-orm';
import type { AuthUser } from './auth';
import { getAccessibleProjects } from './issue-access';
import { environmentScopeCondition, resolveEnvironmentScopes } from './environment-access';

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

	const baseQuery = db
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
		.where(allConditions.length > 0 ? and(...allConditions) : undefined);

	const results = await baseQuery
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
