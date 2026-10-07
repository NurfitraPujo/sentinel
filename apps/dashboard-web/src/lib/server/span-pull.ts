import { env } from '$env/dynamic/private';
import { sql } from 'drizzle-orm';
import { db } from '$lib/server/db';
import { log } from '$lib/server/observability/log';
import { earliestTraceOccurrenceAt } from '$lib/server/queries/trace-queries';
import { getSpansState, type SpanScope } from '$lib/server/queries/span-queries';
import type { SpansReason, SpansStatus } from '$lib/spans/status';
import { isLowerUuid } from '$lib/uuid';

/**
 * sentinel-occurrence-spans TASK-02 — pull ONE trace's spans from the trace store now, on a person's or an agent's
 * request, instead of waiting for the per-issue-throttled automatic pull (which gave ~3-4% of traced occurrences spans).
 *
 * Everything lives in the database the processor already reads, so there is no new service and no RPC: this module
 * arms a `span_pull_queue` row and the processor's worker (apps/processor-go/spanpull) fetches it like any other row —
 * allow-list re-check, size bound, retries. It does NOT touch `span_pull_issue_window`: that throttle is the
 * processor's, per issue, for the AUTOMATIC path; an explicit request is a different budget (the per-organization hourly
 * limit below).
 *
 * ## Which stage a new row starts at: 1 (one fetch)
 *
 * The automatic path queues stage 0 and fetches twice (+2 min, +10 min after the first error) because it fires the moment
 * an error is stored, when the trace is still being written. An on-demand pull comes from someone LOOKING at an occurrence,
 * which is almost always minutes to days old, so the late-span fetch buys nothing but a second wait: the UI would sit
 * `pending` until +10 min. The row therefore starts at stage 1, and the worker finishes it (stage 2) after one successful
 * fetch. The one case a single fetch can be early is an occurrence younger than the store's flush time, so the first fetch
 * is never scheduled before (earliest occurrence of the trace + FIRST_FETCH_DELAY) — the same 2 minutes the automatic path
 * waits. A trace that is still growing after that is covered by asking again (it re-arms; see below).
 *
 * ## What a request does to the existing row (idempotent: asking twice is asking once)
 *
 *  - spans already visible       -> nothing written (`available`).
 *  - no row                      -> insert at stage 1, due at max(now, earliest + 2 min)            (`queued`).
 *  - row stage < 2, not claimed  -> pull next_fetch_at forward to that time; the stage is KEPT, so an automatic stage-0 row
 *      still gets its +10 min late-span fetch (`pulled_forward`).
 *      "Not claimed" is `attempts = 0`: a claimed row is in flight (its lease pushed next_fetch_at ahead), and pulling it
 *      forward would only let a second worker fetch the same trace.
 *  - row stage < 2, already due or in flight                                                         (`already_pending`, nothing written).
 *  - row stage 2 and no visible spans -> re-arm: stage 1, attempts 0, outcome '', created_at reset (so retention, which reaps
 *      the queue by created_at, cannot delete it before it is fetched), due as above                   (`rearmed`).
 *  - row finished `too_large`    -> nothing written, nothing counted (the trace only grows)             (`none`, reason too_large).
 *
 * Only the requests that wrote something count against the hourly limit, so a double click or a second tab is free.
 */

/** The first fetch is never earlier than this after the trace's first error: the automatic path's own FirstFetchDelay. */
export const FIRST_FETCH_DELAY_MS = 2 * 60 * 1000;
export const DEFAULT_ON_DEMAND_LIMIT_PER_HOUR = 30;
const HOUR_MS = 60 * 60 * 1000;

export interface SpanPullConfig {
	/** `SPAN_PULL_SCOPES`: organization id -> its core (scope) project id, both lower-case. The allow-list. */
	scopes: Map<string, string>;
	/** `SPAN_PULL_ON_DEMAND_LIMIT`: accepted on-demand pulls per organization per rolling hour. */
	limitPerHour: number;
}

/**
 * Read per call (`$env/dynamic/private` is dynamic). The value is the SAME `SPAN_PULL_SCOPES` the processor reads — the
 * chart renders it into both — so the two cannot disagree about who is allow-listed. A malformed entry is skipped here
 * (the processor refuses to start on one, so it cannot reach production; a page must not 500 over it).
 */
export function getSpanPullConfig(): SpanPullConfig {
	const scopes = new Map<string, string>();
	for (const pair of (env.SPAN_PULL_SCOPES ?? '').split(',')) {
		const [org, project, extra] = pair.trim().toLowerCase().split(':');
		if (extra === undefined && org && project && isLowerUuid(org) && isLowerUuid(project)) scopes.set(org, project);
	}
	const raw = (env.SPAN_PULL_ON_DEMAND_LIMIT ?? '').trim();
	const parsed = /^\d+$/.test(raw) ? Number(raw) : NaN;
	return { scopes, limitPerHour: Number.isSafeInteger(parsed) ? parsed : DEFAULT_ON_DEMAND_LIMIT_PER_HOUR };
}

/** Whether on-demand pull is offered for the organization at all (drives the buttons; the worker re-checks anyway). */
export function isSpanPullEnabled(orgId: string, cfg: SpanPullConfig = getSpanPullConfig()): boolean {
	return cfg.scopes.has(orgId.toLowerCase());
}

/** Whether the viewer can reach the core (scope) project that governs pulled spans; without it a pull's result is invisible to them. */
export function canReachSpanPullCore(scope: SpanScope, cfg: SpanPullConfig = getSpanPullConfig()): boolean {
	const core = cfg.scopes.get(scope.orgId.toLowerCase());
	return !!core && scope.projects.some((p) => p.id.toLowerCase() === core);
}

export type SpanPullAction = 'none' | 'queued' | 'pulled_forward' | 'rearmed' | 'already_pending';

export interface SpanPullResult {
	status: SpansStatus;
	reason: SpansReason | null;
	/** What this request did to the queue. `none`: nothing to do (spans are available). */
	action: SpanPullAction;
	/** The ledger row of an accepted request (the audit record), when there is one. */
	requestId: string | null;
}

export type SpanPullFailure = 'not_allowed' | 'not_found' | 'rate_limited';

export class SpanPullError extends Error {
	constructor(
		public readonly code: SpanPullFailure,
		message: string,
		/** For `rate_limited`: seconds until the oldest counted request leaves the hour. */
		public readonly retryAfterSeconds?: number
	) {
		super(message);
		this.name = 'SpanPullError';
	}
}

export interface SpanPullRequester {
	kind: 'user' | 'agent';
	id: string;
}

export interface SpanPullInput {
	traceId: string;
	/** The viewer's guarded scope (org, accessible projects, environment grants). Agent keys pass the whole org. */
	scope: SpanScope;
	requester: SpanPullRequester;
	/** Test seam. */
	config?: SpanPullConfig;
}

/**
 * Asks for the spans of `traceId` in `scope.orgId`. Throws `SpanPullError`:
 *  - `not_allowed`  the organization is not allow-listed (nothing is ever queued for it; the worker's re-check is the backstop);
 *  - `not_found`    the viewer can see no occurrence of the trace, or cannot reach the core project that governs pulled spans
 *                   (the pulled spans would be invisible to them) — one outcome, the app's 404-not-403 convention;
 *  - `rate_limited` the organization has used its hourly budget.
 */
export async function requestSpanPull(input: SpanPullInput): Promise<SpanPullResult> {
	const { traceId, scope, requester } = input;
	const cfg = input.config ?? getSpanPullConfig();
	const orgId = scope.orgId.toLowerCase();

	const coreProject = cfg.scopes.get(orgId);
	if (!coreProject) throw new SpanPullError('not_allowed', 'Span pull is not enabled for this organization');
	const projectIds = scope.projects.map((p) => p.id);
	if (!projectIds.includes(coreProject)) throw new SpanPullError('not_found', 'Trace not found');

	const earliest = await earliestTraceOccurrenceAt(traceId, { projectIds, environmentScopes: scope.environmentScopes });
	if (!earliest) throw new SpanPullError('not_found', 'Trace not found');

	const current = await getSpansState(traceId, scope);
	if (current.status === 'available') return { ...current, action: 'none', requestId: null };
	// A trace over the size bound only grows: asking again would fetch and drop it again. Report it, write nothing, spend nothing.
	if (current.reason === 'too_large') return { ...current, action: 'none', requestId: null };

	const dueAt = new Date(Math.max(Date.now(), earliest.getTime() + FIRST_FETCH_DELAY_MS));
	const lockKey = `span_pull:${orgId}`;

	// Throw to roll back: a `return` from inside the callback would COMMIT the upsert (D18).
	const written = await db.transaction(async (tx) => {
		// Serialises this organization's requests across every dashboard replica, so the count below is the count.
		await tx.execute(sql`SELECT pg_advisory_xact_lock(hashtextextended(${lockKey}, 0))`);

		const prev = await tx.execute<{ stage: number }>(
			sql`SELECT stage FROM span_pull_queue WHERE org_id = ${orgId}::uuid AND trace_id = ${traceId} FOR UPDATE`
		);
		const upserted = await tx.execute<{ one: number }>(sql`
			INSERT INTO span_pull_queue (org_id, trace_id, stage, next_fetch_at, on_demand, requested_by, requested_by_kind, requested_at, outcome)
			VALUES (${orgId}::uuid, ${traceId}, 1, ${dueAt.toISOString()}::timestamptz, true, ${requester.id}, ${requester.kind}, now(), '')
			ON CONFLICT (org_id, trace_id) DO UPDATE SET
				stage = CASE WHEN span_pull_queue.stage = 2 THEN 1 ELSE span_pull_queue.stage END,
				attempts = 0, last_error = '', outcome = '',
				created_at = CASE WHEN span_pull_queue.stage = 2 THEN now() ELSE span_pull_queue.created_at END,
				next_fetch_at = EXCLUDED.next_fetch_at, on_demand = true,
				requested_by = EXCLUDED.requested_by, requested_by_kind = EXCLUDED.requested_by_kind, requested_at = EXCLUDED.requested_at
			WHERE (span_pull_queue.stage = 2 AND span_pull_queue.outcome <> 'too_large')
			   OR (span_pull_queue.attempts = 0 AND span_pull_queue.next_fetch_at > EXCLUDED.next_fetch_at)
			RETURNING 1 AS one`);
		if (upserted.length === 0) return null; // already pending and not worth moving: nothing written, nothing counted

		const action: SpanPullAction = prev.length === 0 ? 'queued' : Number(prev[0].stage) === 2 ? 'rearmed' : 'pulled_forward';
		const [ledger] = await tx.execute<{ id: string }>(sql`
			INSERT INTO span_pull_requests (org_id, trace_id, action, requested_by, requested_by_kind)
			VALUES (${orgId}::uuid, ${traceId}, ${action}, ${requester.id}, ${requester.kind}) RETURNING id::text AS id`);

		const [usage] = await tx.execute<{ used: number; oldest: Date | string | null }>(sql`
			SELECT count(*)::int AS used, min(requested_at) AS oldest FROM span_pull_requests
			 WHERE org_id = ${orgId}::uuid AND requested_at > now() - interval '1 hour'`);
		if (Number(usage.used) > cfg.limitPerHour) {
			const oldest = usage.oldest ? new Date(usage.oldest).getTime() : Date.now();
			const wait = Math.max(1, Math.ceil((oldest + HOUR_MS - Date.now()) / 1000));
			throw new SpanPullError(
				'rate_limited',
				`This organization has used its ${cfg.limitPerHour} on-demand span pulls for the hour; try again in about ${Math.ceil(wait / 60)} minute(s)`,
				wait
			);
		}
		return { action, requestId: ledger.id };
	});

	if (!written) {
		// Pending already (queued, due or in flight). Re-read: it may have just been fetched by the worker.
		const now = await getSpansState(traceId, scope);
		return { ...now, action: now.status === 'available' ? 'none' : 'already_pending', requestId: null };
	}
	log.info('span_pull.requested', { orgId, traceId, action: written.action, requesterKind: requester.kind, requestId: written.requestId });
	return { status: 'pending', reason: null, action: written.action, requestId: written.requestId };
}
