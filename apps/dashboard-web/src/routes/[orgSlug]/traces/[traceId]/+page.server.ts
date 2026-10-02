import { error } from '@sveltejs/kit';
import type { PageServerLoad } from './$types';
import { getAccessibleProjects } from '$lib/server/issue-access';
import { resolveEnvironmentScopes } from '$lib/server/environment-access';
import { getRetentionDays } from '$lib/server/retention-window';
import {
	TRACE_PAGE_SIZE,
	countTraceOccurrences,
	countTraceProjects,
	decodeTraceCursor,
	listTraceOccurrences,
	listTraceProjects,
	parseTraceId,
	traceRowColumns,
	type TraceScope,
} from '$lib/server/queries/trace-queries';

// sentinel-cross-repo-trace-view TASK-01 (contract D1–D4). Lives under `[orgSlug]` so `orgHandle`'s
// membership check applies and `reservedRoutes` is untouched.
//
// D2: a trace with no VISIBLE occurrence answers the same empty page whether the id never existed,
// belongs to another organization, or is hidden by an environment grant — so the empty case below
// is one return, with nothing in it that varies by cause. The project set is the CURRENT org's
// only: trace ids are client-asserted, so a foreign org can send the same one (this is deliberately
// not `/search`'s cross-organization union).
export const load: PageServerLoad = async ({ params, locals, url }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const currentOrg = locals.currentOrg;
	if (!currentOrg || currentOrg.slug !== params.orgSlug) {
		throw error(403, 'Forbidden: Unauthorized access to organization');
	}

	// D1: a malformed id is a 404 before any query runs.
	const traceId = parseTraceId(params.traceId);
	if (!traceId) {
		throw error(404, 'Trace not found');
	}

	let cursor;
	const rawCursor = url.searchParams.get('cursor');
	if (rawCursor) {
		try {
			cursor = decodeTraceCursor(rawCursor);
		} catch {
			throw error(404, 'Trace not found');
		}
	}

	const accessible = await getAccessibleProjects(session.user.id, currentOrg.id);
	const scope: TraceScope = {
		projectIds: accessible.map((p) => p.id),
		environmentScopes: await resolveEnvironmentScopes(session.user.id),
	};

	let page;
	try {
		page = await listTraceOccurrences({
			traceId,
			scope,
			limit: TRACE_PAGE_SIZE,
			cursor,
			columns: traceRowColumns,
		});
	} catch (err) {
		// A cursor Postgres refused (the decoder's backstop) is a bad cursor, like a malformed one.
		if (err instanceof Error && err.message === 'invalid cursor') throw error(404, 'Trace not found');
		throw err;
	}
	const [total, spanned, projectCount] = await Promise.all([
		countTraceOccurrences(traceId, scope),
		listTraceProjects(traceId, scope),
		countTraceProjects(traceId, scope),
	]);
	const { rows, nextCursor } = page;

	return {
		orgSlug: currentOrg.slug,
		traceId,
		occurrences: rows,
		nextCursor: nextCursor ?? null,
		total: total.total,
		totalCapped: total.capped,
		projects: spanned,
		// The true distinct count: `projects` is capped at 50 names.
		projectCount,
		retentionDays: getRetentionDays(),
	};
};
