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
import { getSpansState, listTraceSpans } from '$lib/server/queries/span-queries';
import { canReachSpanPullCore } from '$lib/server/span-pull';
import { SPAN_RETENTION_DAYS } from '$lib/spans/waterfall';

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
	const environmentScopes = await resolveEnvironmentScopes(session.user.id);
	const scope: TraceScope = {
		projectIds: accessible.map((p) => p.id),
		environmentScopes,
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
	// sentinel-span-ingest D6/D8: the waterfall's spans under the same org/project/environment guards. A trace
	// with no spans (never pulled, older than 14 days, or none visible) leaves the list below unchanged.
	const [total, spanned, projectCount, traceSpans] = await Promise.all([
		countTraceOccurrences(traceId, scope),
		listTraceProjects(traceId, scope),
		countTraceProjects(traceId, scope),
		listTraceSpans(traceId, { orgId: currentOrg.id, projects: accessible, environmentScopes }),
	]);
	const { rows, nextCursor } = page;
	// sentinel-occurrence-spans TASK-01/02: a trace with no visible spans says WHY (never pulled / pending / not found in the
	// trace store ...) instead of an empty waterfall, and offers the pull. Only for a trace with a visible error (D2: the empty
	// page must not vary by cause, and the state of a trace the viewer cannot see is not theirs to learn).
	const spansState =
		traceSpans.spans.length > 0
			? { status: 'available' as const, reason: null }
			: rows.length > 0
				? await getSpansState(traceId, { orgId: currentOrg.id, projects: accessible, environmentScopes })
				: { status: 'none' as const, reason: null };

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
		organizationId: currentOrg.id,
		spansState,
		spanPullEnabled: canReachSpanPullCore({ orgId: currentOrg.id, projects: accessible, environmentScopes }),
		spans: traceSpans.spans,
		spansTruncated: traceSpans.truncated,
		spanRetentionDays: SPAN_RETENTION_DAYS,
	};
};
