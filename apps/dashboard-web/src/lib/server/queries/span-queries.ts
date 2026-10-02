import { db } from '$lib/server/db';
import { projects, spans } from '$lib/db/schema';
import { and, eq, inArray, or, sql, type SQL } from 'drizzle-orm';
import { environmentScopeCondition, scopeForProject } from '$lib/server/environment-access';
import { SPAN_RENDER_CAP, type WaterfallSpan } from '$lib/spans/waterfall';

/**
 * sentinel-span-ingest D6/D8 — the ONE guarded read of "the spans of a trace", for the waterfall on
 * `/{org}/traces/{traceId}`.
 *
 * Trace ids are client-asserted, so every condition below is tenancy, and each is unconditional:
 *
 *  - `org_id` is the CURRENT organization's. Another org's spans of the same trace id never match.
 *  - **mobile** spans (`source = 'mobile'`) belong to a project: only the viewer's accessible projects of this
 *    organization, with the same per-project environment grants the occurrence list applies
 *    (`environmentScopeCondition`, composed over `spans`).
 *  - **pulled** spans (`source = 'pulled'`) are core's data and belong to the organization that pulled them. They are
 *    governed by the organization's configured CORE project (`scope_project_id`, from `SPAN_PULL_SCOPES`): the
 *    viewer must reach that project AND its environment grant must permit the span's `environment` — the occurrence
 *    rules, applied to core's project. They carry db statements and query parameters, so neither "some other project
 *    I reach is unrestricted" nor "a mobile error queued this trace" may open them. A pulled span with no scope
 *    project is never shown.
 *
 * An empty project set DENIES — `inArray([])` throws in Drizzle 0.30 and a dropped predicate would be a leak, so
 * the empty case is spelled out, exactly as in trace-queries.ts.
 */
export interface SpanScope {
	/** The current organization (spans are never read across organizations). */
	orgId: string;
	/** The viewer's accessible projects in that organization. */
	projects: Array<{ id: string }>;
	/** `resolveEnvironmentScopes` map. */
	environmentScopes: Map<string, string[]>;
}

function spanConditions(traceId: string, scope: SpanScope): SQL {
	const projectIds = scope.projects.map((p) => p.id);
	const mobile: SQL[] = [
		eq(spans.source, 'mobile'),
		projectIds.length > 0 ? inArray(spans.projectId, projectIds) : sql`false`,
	];
	const mobileGuard = environmentScopeCondition(scope.environmentScopes, spans);
	if (mobileGuard) mobile.push(mobileGuard);

	// Pulled: the same two conditions over the CORE scope project (an empty project set, or a NULL scope, denies).
	const pulled: SQL[] = [
		eq(spans.source, 'pulled'),
		projectIds.length > 0 ? inArray(spans.scopeProjectId, projectIds) : sql`false`,
	];
	const pulledGuard = environmentScopeCondition(scope.environmentScopes, {
		projectId: spans.scopeProjectId,
		environment: spans.environment,
	});
	if (pulledGuard) pulled.push(pulledGuard);

	return and(eq(spans.traceId, traceId), eq(spans.orgId, scope.orgId), or(and(...mobile), and(...pulled)))!;
}

/**
 * A few scalar attributes per span, trimmed IN POSTGRES: a span's attributes are up to 8 KiB and a trace page
 * reads up to 2,000 spans, so shipping them whole would be 16 MB for a hover card. Eight scalar keys in key
 * order, strings cut to 200 characters; nested values are left for a detail view that does not exist yet.
 */
const attributePreview = sql<Record<string, unknown>>`coalesce((
	SELECT jsonb_object_agg(k, v) FROM (
		SELECT e.key AS k,
		       CASE WHEN jsonb_typeof(e.value) = 'string' THEN to_jsonb(left(e.value #>> '{}', 200)) ELSE e.value END AS v
		  FROM jsonb_each(${spans.attributes}) e
		 WHERE jsonb_typeof(e.value) IN ('string', 'number', 'boolean')
		 ORDER BY e.key LIMIT 8) p), '{}'::jsonb)`;

/** `start_ts` in microseconds since the epoch, computed by Postgres: a JS Date would drop the microseconds. */
const startUs = sql<number>`(extract(epoch from ${spans.startTs}) * 1000000)::bigint::float8`;

export interface TraceSpans {
	spans: WaterfallSpan[];
	/** More spans matched than the render cap; the first `SPAN_RENDER_CAP` by start time are returned. */
	truncated: boolean;
}

/** The spans of a trace the viewer may see, earliest first, at most `SPAN_RENDER_CAP`. */
export async function listTraceSpans(traceId: string, scope: SpanScope, cap: number = SPAN_RENDER_CAP): Promise<TraceSpans> {
	const rows = await db
		.select({
			spanId: spans.spanId,
			parentSpanId: spans.parentSpanId,
			name: spans.name,
			op: spans.op,
			kind: spans.kind,
			source: spans.source,
			serviceName: spans.serviceName,
			projectName: projects.name,
			environment: spans.environment,
			startUs,
			durationUs: spans.durationUs,
			status: spans.status,
			attributes: attributePreview,
		})
		.from(spans)
		.leftJoin(projects, eq(projects.id, spans.projectId))
		.where(spanConditions(traceId, scope))
		.orderBy(spans.startTs, spans.spanId)
		.limit(cap + 1);

	const truncated = rows.length > cap;
	return {
		spans: rows.slice(0, cap).map((r) => ({
			...r,
			startUs: Number(r.startUs),
			durationUs: Number(r.durationUs),
			attributes: r.attributes ?? {},
		})),
		truncated,
	};
}
