import { db } from './db';
import { errorSearchIndex, errorOccurrences, issues, projects, projectMembers } from '$lib/db/schema';
import { eq, and, or, inArray, sql } from 'drizzle-orm';
import type { AuthUser } from './auth';

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

	const accessibleProjects = await db
		.select({ projectId: projectMembers.projectId })
		.from(projectMembers)
		.where(eq(projectMembers.userId, user.id));

	const projectIds = accessibleProjects.map(p => p.projectId);

	if (projectIds.length === 0) {
		return { results: [], total: 0 };
	}

	const conditions: any[] = [];

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

	let baseQuery = db
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
		.innerJoin(errorSearchIndex, eq(errorSearchIndex.occurrenceId, errorOccurrences.id));

	if (ftsQuery) {
		baseQuery = baseQuery.where(
			sql`to_tsvector('english', ${issues.message}) @@ to_tsquery('english', ${ftsQuery})`
		) as typeof baseQuery;
	}

	if (conditions.length > 0) {
		baseQuery = baseQuery.where(and(...conditions)) as typeof baseQuery;
	}

	if (searchConditions.length > 0) {
		baseQuery = baseQuery.where(or(...searchConditions)) as typeof baseQuery;
	}

	const results = await baseQuery
		.limit(limit)
		.offset(offset);

	let countQuery = db
		.select({ count: sql<number>`cast(count(DISTINCT ${issues.id}) as integer)` })
		.from(issues)
		.innerJoin(errorOccurrences, eq(errorOccurrences.issueId, issues.id))
		.innerJoin(errorSearchIndex, eq(errorSearchIndex.occurrenceId, errorOccurrences.id));

	if (ftsQuery) {
		countQuery = countQuery.where(
			sql`to_tsvector('english', ${issues.message}) @@ to_tsquery('english', ${ftsQuery})`
		) as typeof countQuery;
	}

	let countConditions = [...conditions];
	if (searchConditions.length > 0) {
		countConditions.push(or(...searchConditions));
	}

	const countResult = countConditions.length > 0
		? await countQuery.where(and(...countConditions))
		: await countQuery;
	
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
