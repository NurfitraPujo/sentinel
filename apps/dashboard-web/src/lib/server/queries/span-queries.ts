import { db } from '$lib/server/db';
import { projects, spans, spanPullQueue } from '$lib/db/schema';
import { and, eq, inArray, or, sql, type SQL } from 'drizzle-orm';
import { isCanonicalTraceId } from '$lib/trace-id';
import type { SpansReason, SpansState, SpansStatus } from '$lib/spans/status';
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

/**
 * The tenancy and environment half of the guard, WITHOUT the trace id: org, then (mobile | pulled) under the viewer's
 * projects and grants. `spanConditions` adds the trace id for the waterfall; the status helpers below add a correlated
 * one. There is one spelling of the guard, so "has spans" can never advertise a span the waterfall would not show.
 */
export function spanVisibility(scope: SpanScope): SQL {
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

	return and(eq(spans.orgId, scope.orgId), or(and(...mobile), and(...pulled)))!;
}

function spanConditions(traceId: string, scope: SpanScope): SQL {
	return and(eq(spans.traceId, traceId), spanVisibility(scope))!;
}

/** The most trace ids one status call takes: a page of occurrences is 100-200. Beyond it the caller batches. */
export const SPANS_STATUS_MAX_TRACES = 500;

/**
 * sentinel-occurrence-spans TASK-01 — `available | pending | none` for a SET of traces of ONE organization, in ONE
 * statement however many traces there are.
 *
 *   SELECT t.trace_id,
 *          EXISTS (SELECT 1 FROM spans WHERE org_id = $org AND trace_id = t.trace_id AND <the waterfall's guard>),
 *          EXISTS (SELECT 1 FROM span_pull_queue WHERE org_id = $org AND trace_id = t.trace_id AND stage < 2)
 *     FROM unnest(ARRAY[...]) AS t(trace_id)
 *
 * The EXISTS is a correlated probe per trace, so Postgres stops at the first visible span instead of reading all of a
 * 7,000-span trace, and each probe is an index lookup on (org_id, trace_id, ...) — the primary key's leading columns — on
 * EVERY partition of `spans` (the PK holds the partition key start_ts last, and nothing here bounds start_ts, so no
 * partition is pruned: ~15 partitions x N traces probes). span-status.integration.test.ts EXPLAINs it. A trace id that is
 * not canonical 32-hex is `none` without a probe (the trace page cannot show it either).
 *
 * `pending` only matters when `available` is false, and `available` wins.
 */
export async function getSpansStatuses(traceIds: Array<string | null | undefined>, scope: SpanScope): Promise<Map<string, SpansStatus>> {
	const ids = [...new Set(traceIds.filter((id): id is string => isCanonicalTraceId(id)))];
	const out = new Map<string, SpansStatus>(ids.map((id) => [id, 'none']));
	if (ids.length === 0) return out;
	if (ids.length > SPANS_STATUS_MAX_TRACES) {
		for (let i = 0; i < ids.length; i += SPANS_STATUS_MAX_TRACES) {
			for (const [id, status] of await getSpansStatuses(ids.slice(i, i + SPANS_STATUS_MAX_TRACES), scope)) out.set(id, status);
		}
		return out;
	}
	const rows = await db.execute<{ trace_id: string; available: boolean; pending: boolean }>(statusQuery(ids, scope));
	for (const r of rows) out.set(r.trace_id, r.available ? 'available' : r.pending ? 'pending' : 'none');
	return out;
}

/** Exported so the integration test can EXPLAIN exactly the statement the helper runs. */
export function statusQuery(ids: string[], scope: SpanScope): SQL {
	const list = sql.join(ids.map((id) => sql`${id}`), sql`, `);
	return sql`SELECT t.trace_id AS trace_id,
		EXISTS (SELECT 1 FROM ${spans} WHERE ${spans.traceId} = t.trace_id AND ${spanVisibility(scope)}) AS available,
		EXISTS (SELECT 1 FROM ${spanPullQueue} WHERE ${spanPullQueue.orgId} = ${scope.orgId} AND ${spanPullQueue.traceId} = t.trace_id AND ${spanPullQueue.stage} < 2) AS pending
		FROM unnest(ARRAY[${list}]::text[]) AS t(trace_id)`;
}

/** What the queue row of a finished pull says about why there are no (visible) spans. */
export function reasonFromQueueRow(row: { stage: number; outcome: string } | null): SpansReason {
	if (!row) return 'not_pulled';
	switch (row.outcome) {
		case 'stored':
			return 'not_visible';
		case 'too_large':
			return 'too_large';
		case 'not_allowed':
			return 'not_allowed';
		case 'gave_up':
			return 'failed';
		// 'not_found', and a row finished before the outcome column existed (outcome ''): it ended without spans.
		default:
			return 'not_found';
	}
}

/** One trace's state with the reason a `none` is a `none` (the trace page and the pull endpoints). */
export async function getSpansState(traceId: string, scope: SpanScope): Promise<SpansState> {
	const status = (await getSpansStatuses([traceId], scope)).get(traceId) ?? 'none';
	if (status !== 'none') return { status, reason: null };
	const [row] = await db
		.select({ stage: spanPullQueue.stage, outcome: spanPullQueue.outcome })
		.from(spanPullQueue)
		.where(and(eq(spanPullQueue.orgId, scope.orgId), eq(spanPullQueue.traceId, traceId)));
	return { status, reason: reasonFromQueueRow(row ?? null) };
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
