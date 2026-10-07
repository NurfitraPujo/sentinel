import { z } from 'zod';
import { AGENT_EVENT_TYPES } from '$lib/server/agent-events';

/**
 * N6 (this phase): zod schemas describing every `/api/agent/*` route's request and response
 * shapes, READ off the actual handlers + their helpers (agent-ops.ts, agent-events.ts, events.ts,
 * agent-reads.ts, agent-work.ts, upload-core.ts, comments.ts, issues.ts, reports.ts). These
 * schemas DESCRIBE existing behavior -- nothing under src/routes/api/agent adopts them for
 * runtime validation in this phase (EXISTING BEHAVIOR IS FROZEN). `docs/agents/openapi.agent.yaml`
 * is GENERATED from these via `scripts/generate-agent-openapi.ts` (`pnpm openapi:agent`) --
 * hand-editing that file is pointless, it will be overwritten.
 *
 * Response schemas use `.strict()` wherever the handler's output shape is fully known (a straight
 * `db.select()`/`.returning()` or a hand-built object literal), so an added/removed/renamed field
 * breaks `contract.test.ts` and `openapi-drift.test.ts` -- that is the whole point of this phase.
 */

// ---------------------------------------------------------------------------
// Shared primitives
// ---------------------------------------------------------------------------

/** 401 shape thrown by `authenticateAgentRequest` (SvelteKit `error(401, message)` -> `{message}`). */
export const UnauthorizedErrorSchema = z.object({ message: z.string() }).strict();

/** Generic SvelteKit `error(status, message)` shape used by most non-2xx responses in this API. */
export const MessageErrorSchema = z.object({ message: z.string() }).strict();

/** Manual-validation routes (issues list, events, questions via body-shape checks) return `{error}`. */
export const ErrorFieldErrorSchema = z.object({ error: z.string() }).strict();

const nullableIso = () => z.string().nullable();

// ---------------------------------------------------------------------------
// issues.ts / reports.ts `issues` row (full `typeof issues.$inferSelect`)
// ---------------------------------------------------------------------------

export const IssueRowSchema = z
	.object({
		id: z.string(),
		projectId: z.string(),
		// sentinel-project-environments decision 54: `issues.environment` (§4.9) is part of the
		// issue's IDENTITY and reaches this response whether or not it is declared -- getAgentIssueDetail
		// is a full-row `db.select()`. NOT NULL in the schema, so a plain string. Declaring it only
		// widens the declared type (the same reasoning claimedAt's note records).
		environment: z.string(),
		fingerprint: z.string(),
		message: z.string(),
		errorClass: z.string(),
		status: z.enum(['unresolved', 'resolved', 'ignored']),
		regressionStatus: z.string(),
		issueType: z.string(),
		sourceChannel: z.string(),
		assigneeType: z.string().nullable(),
		assignedTo: z.string().nullable(),
		resolvedInVersion: z.string().nullable(),
		resolvedAt: z.coerce.date().nullable(),
		resolvedByType: z.string().nullable(),
		resolvedBy: z.string().nullable(),
		regressionCount: z.number(),
		lastRegressedAt: z.coerce.date().nullable(),
		firstSeen: z.coerce.date().nullable(),
		lastSeen: z.coerce.date().nullable(),
		count: z.number(),
		waitingOn: z.string().nullable(),
		// N7e (A07): when the CURRENT claim (assigneeType/assignedTo) was made -- null if unclaimed
		// or claimed before the 1723500000_add_claimed_at.sql backfill (N7c).
		claimedAt: z.coerce.date().nullable(),
		// sentinel-affected-users decision 14: how many DISTINCT users / tenants this issue's
		// occurrences have touched within the retention window (contract §4.4, §4.6). Integers,
		// >= 0, never null -- the column is NOT NULL DEFAULT 0, so an issue that predates the count
		// or whose events carry no id reads 0, not null. The count can go DOWN: the hourly retention
		// sweep recomputes it from what remains (decision 12).
		affectedUserCount: z.number().int().nonnegative(),
		affectedTenantCount: z.number().int().nonnegative(),
		// sentinel-releases-snooze decision 9: the four §4.5 columns reach this response by the same
		// full-row select, so they are declared -- not excused via KNOWN_UNDECLARED. All four are
		// nullable with no default (§4.5): NULL for every pre-feature row, for a plain indefinite
		// ignore, and for an issue whose first occurrence carried no version. A snooze is
		// `status: 'ignored'` with `snoozedUntil` and/or `snoozeOccurrenceTarget` set (no fourth
		// status -- decision 2); `snoozeCountAt` is the `count` the target was set at and travels
		// with it (the migrated check_snooze_pair). The three snooze keys are non-null ONLY while
		// `status` is `ignored` (check_snooze_only_when_ignored). Dates render as ISO strings on
		// the wire like every other timestamp here.
		snoozedUntil: z.coerce.date().nullable(),
		snoozeOccurrenceTarget: z.number().int().positive().nullable(),
		snoozeCountAt: z.number().int().nullable(),
		firstReleaseVersion: z.string().nullable(),
	})
	.strict();

// ---------------------------------------------------------------------------
// GET /api/agent/issues -- agent-work.ts's `listAgentIssues`. NOTE: this is a DIFFERENT, lighter
// shape than the full issue row (getAgentIssueDetail's) -- it joins project name/isInbox and the
// manual-report's severity/reporterId, and omits fingerprint/regressionStatus/sourceChannel/
// resolvedInVersion/resolvedAt/resolvedByType/resolvedBy/regressionCount/lastRegressedAt. The
// hand-written N5 spec conflated this with the full `Issue` schema -- see the reconciliation notes.
// ---------------------------------------------------------------------------

export const AgentIssueListItemSchema = z
	.object({
		id: z.string(),
		projectId: z.string(),
		projectName: z.string(),
		isInbox: z.boolean(),
		issueType: z.string(),
		message: z.string(),
		errorClass: z.string(),
		status: z.string(),
		assigneeType: z.string().nullable(),
		assignedTo: z.string().nullable(),
		waitingOn: z.string().nullable(),
		firstSeen: z.coerce.date().nullable(),
		lastSeen: z.coerce.date().nullable(),
		count: z.number(),
		severity: z.string().nullable(),
		reporterId: z.string().nullable(),
		isWaiting: z.boolean(),
		// sentinel-project-environments decision 54: decision 7 puts `environment` into issue identity,
		// so one error across N environments is now N independently claimable rows -- without this
		// field a list caller cannot tell two siblings apart.
		environment: z.string(),
		// N9 (AGENT_WORKER_PLAN C12): when the current blocking question was asked -- null unless the
		// issue is currently waiting (waitingOn set). Lets an agent nag stale questions by age.
		waitingSince: z.coerce.date().nullable(),
		// N7e (A07): see IssueRowSchema's claimedAt note.
		claimedAt: z.coerce.date().nullable(),
		// sentinel-affected-users decision 14: see IssueRowSchema's note. Named in
		// listAgentIssues's projection (agent-work.ts), so the list carries the same two integers
		// the detail does.
		affectedUserCount: z.number().int().nonnegative(),
		affectedTenantCount: z.number().int().nonnegative(),
	})
	.strict();

// N7b (A02): `nextCursor` is present ONLY when the request supplied `limit` and more rows exist
// beyond this page -- absent (not null) otherwise, matching the route's byte-identical-when-
// absent contract for pre-N7b callers.
export const ListIssuesResponseSchema = z
	.object({ issues: z.array(AgentIssueListItemSchema), nextCursor: z.string().optional() })
	.strict();

export const ListIssuesQuerySchema = z
	.object({
		type: z.enum(['any', 'user_report', 'system_error']).optional(),
		// N9 (C12): `me` restricts to the calling agent's own claims (resolved from the credential).
		claimed: z.enum(['true', 'false', 'me']).optional(),
		project: z.string().optional(),
		/** sentinel-project-environments §4.6: exact, case-sensitive match on issues.environment.
		 * Absent = every environment (unchanged behaviour). */
		environment: z.string().optional(),
		waiting: z.enum(['true', 'false']).optional(),
		/** ISO timestamp; only issues with firstSeen >= since. */
		since: z.string().optional(),
		/** Default 'lastSeen' (pre-N7b ordering) when omitted. */
		sort: z.enum(['firstSeen', 'lastSeen']).optional(),
		/** No `.limit()` applied when omitted (legacy unbounded). 1..200 when supplied. */
		limit: z.coerce.number().int().min(1).max(200).optional(),
		/** Opaque keyset cursor from a prior response's `nextCursor`. */
		cursor: z.string().optional(),
	})
	.strict();

// ---------------------------------------------------------------------------
// GET /api/agent/issues/{issueId} -- agent-reads.ts's `getAgentIssueDetail` / `getAgentReportDetail`
// / `getLatestAgentOccurrence`, plus `getIssueRelations` (queries/issues.ts).
// ---------------------------------------------------------------------------

export const AgentReportDetailSchema = z
	.object({
		bodyMd: z.string(),
		severity: z.string(),
		reporterId: z.string().nullable(),
	})
	.strict();

// sentinel-event-schema-v2 §8.1: the inner objects describe the STORED wire JSON (snake_case, as
// `stacktrace`/`metadata` already are) and are deliberately NOT strict -- a stored key this document does
// not list must not fail the contract test. Every field is null on an occurrence written before the
// feature (migration 1725300000). The outer object stays `.strict()`, the existing freeze.
const OccurrenceSdkSchema = z.object({ name: z.string().optional(), version: z.string().optional() });

const OccurrenceUserSchema = z.object({
	id: z.string().optional(),
	username: z.string().optional(),
	email: z.string().optional(),
	ip_address: z.string().optional(),
	geo: z
		.object({
			city: z.string().optional(),
			region: z.string().optional(),
			country_code: z.string().optional(),
			subdivision: z.string().optional(),
		})
		.optional(),
});

const OccurrenceRequestSchema = z.object({
	method: z.string().optional(),
	url: z.string().optional(),
	query_string: z.string().optional(),
	headers: z.record(z.string()).optional(),
	data: z.unknown().optional(),
	env: z.record(z.string()).optional(),
});

const OccurrenceBreadcrumbSchema = z.object({
	timestamp: z.string().optional(),
	type: z.string().optional(),
	category: z.string().optional(),
	level: z.string().optional(),
	message: z.string().optional(),
	data: z.record(z.unknown()).optional(),
});

const OccurrenceExceptionSchema = z.object({
	values: z.array(
		z.object({
			type: z.string().optional(),
			value: z.string().optional(),
			mechanism: z.object({ type: z.string().optional(), handled: z.boolean().optional() }).optional(),
			stacktrace: z.unknown().optional(),
		})
	),
});

export const AgentOccurrenceSchema = z
	.object({
		id: z.string(),
		environment: z.string(),
		platform: z.string(),
		releaseVersion: z.string().nullable(),
		stacktrace: z.unknown(),
		metadata: z.unknown(),
		traceId: z.string().nullable(),
		createdAt: z.coerce.date().nullable(),
		level: z.enum(['fatal', 'error', 'warning', 'info', 'debug']).nullable(),
		message: z.string().nullable(),
		errorClass: z.string().nullable(),
		logger: z.string().nullable(),
		serverName: z.string().nullable(),
		transaction: z.string().nullable(),
		dist: z.string().nullable(),
		source: z.string().nullable(),
		// bigint('trace_flags', { mode: 'number' }) in the Drizzle mirror -- a string here would fail.
		traceFlags: z.number().int().nullable(),
		receivedAt: z.coerce.date().nullable(),
		sdk: OccurrenceSdkSchema.nullable(),
		// Stored in the `event_user` column (`user` is reserved in PostgreSQL); the API field is `user`.
		user: OccurrenceUserSchema.nullable(),
		tags: z.record(z.string()).nullable(),
		// A value is usually an object, but a context under a sensitive key is the string '***REDACTED***'.
		contexts: z.record(z.unknown()).nullable(),
		request: OccurrenceRequestSchema.nullable(),
		breadcrumbs: z.array(OccurrenceBreadcrumbSchema).nullable(),
		exception: OccurrenceExceptionSchema.nullable(),
	})
	.strict();

/** `getIssueRelations` row shape -- not `.strict()`: it returns two branches (outgoing/incoming)
 * whose `relatedIssue` sub-object shape is read off a join, not enumerated here field-by-field
 * (out of scope for this phase's freeze; see reconciliation notes). */
export const IssueRelationSchema = z.object({
	id: z.string(),
	sourceIssueId: z.string(),
	targetIssueId: z.string(),
	relationType: z.enum(['linked_to', 'caused_by', 'duplicate_of']),
	createdByType: z.string(),
	createdBy: z.string(),
	createdAt: z.coerce.date().nullable(),
	direction: z.enum(['outgoing', 'incoming']),
	relatedIssue: z.record(z.unknown()),
});

export const IssueDetailResponseSchema = z
	.object({
		issue: IssueRowSchema,
		report: AgentReportDetailSchema.nullable(),
		latestOccurrence: AgentOccurrenceSchema.nullable(),
		relations: z.array(IssueRelationSchema),
	})
	.strict();

// ---------------------------------------------------------------------------
// POST /api/agent/issues/claim-next -- agent-work.ts's `claimNextAgentIssue`
// ---------------------------------------------------------------------------

/**
 * sentinel-agent-claim-next-issue: the list route's own filter vocabulary, as a JSON body. Every
 * key is optional; an empty (or absent) body claims the next unresolved, unclaimed issue in the
 * org by `lastSeen` descending -- the first row `GET /api/agent/issues?claimed=false` would show.
 * The 200 body is `IssueDetailResponseSchema` with `issue` already carrying the claim.
 */
export const ClaimNextBodySchema = z
	.object({
		type: z.enum(['any', 'user_report', 'system_error']).optional(),
		project: z.string().optional(),
		/** Exact, case-sensitive match on issues.environment, as on the list route. */
		environment: z.string().optional(),
		source: z.enum(['ingestion_sdk', 'manual_support', 'api', 'user_report_api']).optional(),
		/** Which end of the queue is "next": descending on this column, default 'lastSeen'. */
		sort: z.enum(['firstSeen', 'lastSeen']).optional(),
	})
	.strict();

// ---------------------------------------------------------------------------
// GET /api/agent/issues/{issueId}/occurrences
// ---------------------------------------------------------------------------

// sentinel-occurrence-spans TASK-01: whether a trace's spans can be shown. `available`: at least one span the key's
// organization holds; `pending`: a pull of the trace is queued or running; `none`: nothing, and `reason` says why.
export const SpansStatusSchema = z.enum(['available', 'pending', 'none']);
export const SpansReasonSchema = z.enum(['not_pulled', 'not_found', 'too_large', 'failed', 'not_allowed', 'not_visible']);
export const SpansStateSchema = z.object({ status: SpansStatusSchema, reason: SpansReasonSchema.nullable() }).strict();

const spansFields = {
	// null: the occurrence has no canonical (32-hex) trace id, so there is no trace to have spans.
	spansStatus: SpansStatusSchema.nullable(),
	hasSpans: z.boolean(),
};

export const AgentOccurrenceWithSpansSchema = AgentOccurrenceSchema.extend(spansFields).strict();

export const OccurrencesQuerySchema = z
	.object({
		limit: z.coerce.number().int().min(1).max(50).optional(),
		before: z.string().datetime({ offset: true }).or(z.string()).optional(),
		// `true`: only occurrences with spans, found by walking the issue's newest 2,000 occurrences (`before` continues
		// the walk); the response then carries `spansScan` and, when older occurrences may remain, `nextBefore`.
		hasSpans: z.enum(['true', 'false']).optional(),
	})
	.strict();

export const OccurrencesResponseSchema = z
	.object({
		occurrences: z.array(AgentOccurrenceWithSpansSchema),
		// hasSpans=true only: what the bounded walk looked at, and where to continue (`before=<nextBefore>`).
		spansScan: z.object({ scanned: z.number().int(), exhausted: z.boolean() }).strict().optional(),
		nextBefore: z.string().optional(),
	})
	.strict();

// ---------------------------------------------------------------------------
// GET /api/agent/traces/{traceId} -- agent-reads.ts's `listAgentTraceOccurrences`
// ---------------------------------------------------------------------------

/**
 * sentinel-cross-repo-trace-view D6: one occurrence of a trace, i.e. the occurrence shape plus the
 * four fields that identify WHERE in the org it happened (`projectId`, `projectName`, `issueId`)
 * and which span (`spanId`). Strict, like every other schema here, so an added or dropped field
 * fails the contract test.
 */
export const AgentTraceOccurrenceSchema = AgentOccurrenceSchema.extend({
	projectId: z.string(),
	projectName: z.string(),
	issueId: z.string(),
	spanId: z.string().nullable(),
	...spansFields,
}).strict();

export const TraceQuerySchema = z
	.object({
		limit: z.coerce.number().int().min(1).max(200).optional(),
		/** Opaque: the `nextCursor` of a previous page. */
		cursor: z.string().optional(),
		/** A filter, not a guard: agent keys are org-scoped, not environment-scoped. */
		environment: z.string().optional(),
	})
	.strict();

export const TraceResponseSchema = z
	.object({
		occurrences: z.array(AgentTraceOccurrenceSchema),
		nextCursor: z.string().optional(),
		// sentinel-occurrence-spans: the trace's spans state (also the status poll of POST .../pull).
		spans: SpansStateSchema,
	})
	.strict();

// ---------------------------------------------------------------------------
// GET /api/agent/occurrences/{occurrenceId} -- one occurrence of the key's organization (resolves an occurrence id to its trace)
// ---------------------------------------------------------------------------

export const OccurrenceResponseSchema = z.object({ occurrence: AgentTraceOccurrenceSchema }).strict();

// ---------------------------------------------------------------------------
// POST /api/agent/traces/{traceId}/pull -- sentinel-occurrence-spans TASK-02
// ---------------------------------------------------------------------------

/**
 * `action` is what THIS call did to the pull queue: `none` (spans already available), `queued` (new), `pulled_forward`
 * (an automatic pull moved up), `rearmed` (a finished pull with no visible spans, run once more), `already_pending`
 * (nothing to change). The call is idempotent: repeating it changes nothing while the pull is pending. `requestId` is the
 * audit record of an accepted request (null when nothing was written).
 */
export const PullSpansResponseSchema = z
	.object({
		status: SpansStatusSchema,
		reason: SpansReasonSchema.nullable(),
		action: z.enum(['none', 'queued', 'pulled_forward', 'rearmed', 'already_pending']),
		requestId: z.string().nullable(),
	})
	.strict();

// ---------------------------------------------------------------------------
// POST/DELETE /api/agent/issues/{issueId}/claim -- agent-ops.ts's `issuesClaim`/`issuesClaimRelease`
// ---------------------------------------------------------------------------

// N9 (sentinel-worker plan, C1): `alreadyClaimed` is present ONLY on an idempotent self-reclaim
// (200) -- the caller already held the claim, so no new activity/notification was written. Absent
// on a fresh claim (additive; the schema is `.strict()`).
export const ClaimResponseSchema = z
	.object({ success: z.literal(true), issue: IssueRowSchema, alreadyClaimed: z.literal(true).optional() })
	.strict();
export const ReleaseResponseSchema = z.object({ success: z.literal(true), issue: IssueRowSchema }).strict();

/**
 * A11 (N7f): claim/release's 409 -- enriched with the current claim state (`throwClaimConflict`
 * in agent-ops.ts) so a caller can see WHO holds the claim without a second read.
 */
export const ClaimConflictErrorSchema = z
	.object({ message: z.string(), claimedBy: z.string().nullable(), claimedAt: z.string().nullable() })
	.strict();

// ---------------------------------------------------------------------------
// PATCH /api/agent/issues/{issueId}/status -- agent-ops.ts's `issuesStatus`
// ---------------------------------------------------------------------------

// sentinel-releases-snooze §4.2 / decision 38: this schema is DECLARATION-ONLY. It is referenced by
// registry.ts (the OpenAPI) and by nothing that runs -- `agent-ops.ts: issuesStatus` hand-validates
// through `asRecord` and the `issue-access.ts` validators (`validateSnoozedUntil`,
// `validateSnoozeOccurrences`), and no runtime `.parse()` of it is to be added. So the schema
// documents and the validator enforces: whatever `snoozed_until` declares refused here (a date-only
// `2026-09-20`, a zone-less `2026-09-20T09:00`) is what the validator's ISO 8601 shape check
// refuses, with §4.2's single `snoozedUntil must be a future timestamp` string; the future check is
// the validator's, not the schema's.
export const StatusBodySchema = z
	.object({
		status: z.enum(['unresolved', 'resolved', 'ignored']),
		resolved_in_version: z
			.string()
			.optional()
			.describe(
				'Free text, trimmed, at most 100 characters; meaningful only with status=resolved. ' +
					'The literal `next` is reserved: "resolved in the next release, whichever it turns ' +
					'out to be" -- rewritten to the first release the PROJECT sees after the resolve ' +
					'(project-level, any environment: a version already seen on staging is not the ' +
					'next release). Compared byte-for-byte after trimming (`Next` is a version).'
			),
		// Offsets accepted, as `before` on the occurrences query already does; `2026-09-20T09:00:00Z`
		// and `2026-09-20T16:00:00+07:00` are the same instant. Must be strictly in the future
		// (the validator's check). Only with status=ignored. `''` is treated as omitted (§3).
		snoozed_until: z
			.string()
			.datetime({ offset: true })
			.optional()
			.describe(
				'Only with status=ignored: ignore until this instant (full ISO 8601 date-time with ' +
					'seconds and a zone -- `Z` or `±hh:mm`; offsets accepted). Must be strictly in ' +
					'the future. 400 `snoozedUntil is only valid with status ignored` / `snoozedUntil ' +
					'must be a future timestamp`. An empty string is treated as omitted.'
			),
		// A JSON integer in [1, 1000000]; any string other than '' is refused. Only with status=ignored.
		snooze_occurrences: z
			.number()
			.int()
			.min(1)
			.max(1000000)
			.optional()
			.describe(
				'Only with status=ignored: ignore until this many MORE occurrences have been stored ' +
					'(stored as snoozeOccurrenceTarget, with snoozeCountAt = the count at the time). ' +
					'400 `snoozeOccurrences is only valid with status ignored` / `snoozeOccurrences ' +
					'must be an integer between 1 and 1000000`. An empty string is treated as omitted.'
			),
	})
	.strict();

// N7d (A05-status): `changed` is additive -- `false` means this call was recognized as an exact
// retry of the already-applied status (same status AND same resolved_in_version): no new
// issue_activity row, no notification email. `true` is the normal, real-transition path
// (unchanged from before this field existed).
export const StatusResponseSchema = z
	.object({
		success: z.literal(true),
		status: z.enum(['unresolved', 'resolved', 'ignored']),
		changed: z.boolean(),
	})
	.strict();

// ---------------------------------------------------------------------------
// GET/POST /api/agent/issues/{issueId}/comments
// ---------------------------------------------------------------------------

export const CommentRowSchema = z
	.object({
		id: z.string(),
		issueId: z.string(),
		parentId: z.string().nullable(),
		authorType: z.enum(['user', 'agent']),
		authorId: z.string(),
		blocking: z.boolean(),
		bodyMd: z.string(),
		createdAt: z.coerce.date(),
		editedAt: z.coerce.date().nullable(),
	})
	.strict();

export const CommentsQuerySchema = z.object({ after: z.string().optional() }).strict();
export const ListCommentsResponseSchema = z.object({ comments: z.array(z.record(z.unknown())) }).strict();

export const PostCommentBodySchema = z
	.object({
		body_md: z.string(),
		attachment_ids: z.array(z.string()).optional(),
		// N9 (D21): optional client-generated key (e.g. per-job UUID). A repeat replays the original
		// comment with `deduplicated: true` -- no second activity row, no second email.
		idempotency_key: z.string().max(255).optional(),
	})
	.strict();

export const PostCommentResponseSchema = z
	.object({ comment: CommentRowSchema, deduplicated: z.literal(true).optional() })
	.strict();

// ---------------------------------------------------------------------------
// PATCH/DELETE /api/agent/issues/{issueId}/comments/{commentId} -- A08 (N7e), agent-ops.ts's
// `issuesCommentsEdit`/`issuesCommentsDelete`
// ---------------------------------------------------------------------------

export const EditCommentBodySchema = z.object({ body_md: z.string() }).strict();
export const EditCommentResponseSchema = z.object({ comment: CommentRowSchema }).strict();
export const DeleteCommentResponseSchema = z.object({ success: z.literal(true), issueId: z.string() }).strict();

// ---------------------------------------------------------------------------
// PATCH /api/agent/issues/{issueId}/report/severity -- A09 (N7e), agent-ops.ts's
// `issuesReportSeverity`
// ---------------------------------------------------------------------------

export const SeverityBodySchema = z
	.object({ severity: z.enum(['low', 'medium', 'high', 'critical']) })
	.strict();

export const SeverityResponseSchema = z
	.object({ success: z.literal(true), severity: z.enum(['low', 'medium', 'high', 'critical']).optional() })
	.strict();

// ---------------------------------------------------------------------------
// POST /api/agent/issues/{issueId}/questions
// ---------------------------------------------------------------------------

export const QuestionBodySchema = z
	.object({
		body_md: z.string(),
		audience: z.enum(['reporter', 'team']),
		// N9 (D21): a repeat key replays the ORIGINAL question's comment id and sends NO second email
		// (a blocking question bypasses the throttle, so the dedupe is what prevents a double-notify).
		idempotency_key: z.string().max(255).optional(),
	})
	.strict();

export const QuestionResponseSchema = z
	.object({ comment: CommentRowSchema, deduplicated: z.literal(true).optional() })
	.strict();

// ---------------------------------------------------------------------------
// POST /api/agent/issues/{issueId}/progress
// ---------------------------------------------------------------------------

export const ProgressBodySchema = z
	.object({
		message_md: z.string(),
		// N9 (D21): a repeat key replays a bare deduplicated success -- no second progress row.
		idempotency_key: z.string().max(255).optional(),
	})
	.strict();
export const ProgressResponseSchema = z
	.object({ success: z.literal(true), deduplicated: z.literal(true).optional() })
	.strict();

// ---------------------------------------------------------------------------
// POST/DELETE /api/agent/issues/{issueId}/relations
// ---------------------------------------------------------------------------

export const RelationBodySchema = z
	.object({
		target_issue_id: z.string(),
		relation_type: z.enum(['linked_to', 'caused_by', 'duplicate_of']),
	})
	.strict();

/** `typeof issueRelations.$inferSelect`, returned directly (unwrapped) by `issues.relations.add`. */
export const RelationRowSchema = z
	.object({
		id: z.string(),
		sourceIssueId: z.string(),
		targetIssueId: z.string(),
		relationType: z.enum(['linked_to', 'caused_by', 'duplicate_of']),
		createdByType: z.string(),
		createdBy: z.string(),
		createdAt: z.coerce.date().nullable(),
	})
	.strict();

export const RelationRemoveResponseSchema = z.object({ success: z.literal(true) }).strict();

// ---------------------------------------------------------------------------
// GET /api/agent/projects -- agent-reads.ts's `listAgentProjects`
// ---------------------------------------------------------------------------

/** N10 part 1 (DECISIONS.md D23): repo connection, always returned in full to agents -- including
 * `testCmd`/`agentCmd`, which are server-stored operator-provided commands the worker executes. */
export const AgentProjectRepoSchema = z
	.object({
		provider: z.enum(['github', 'bitbucket']),
		owner: z.string(),
		repo: z.string(),
		defaultBranch: z.string(),
		testCmd: z.string(),
		agentCmd: z.string().nullable(),
		cloneDepth: z.number().int().nullable(),
	})
	.strict();

export const AgentProjectAgentSettingsSchema = z
	.object({
		fixEnabled: z.boolean(),
		maxPrsPerDay: z.number().int().nullable(),
		repo: AgentProjectRepoSchema.nullable(),
	})
	.strict();

export const AgentProjectSchema = z
	.object({
		id: z.string(),
		name: z.string(),
		isInbox: z.boolean(),
		agentSettings: AgentProjectAgentSettingsSchema,
	})
	.strict();
export const ListProjectsResponseSchema = z.object({ projects: z.array(AgentProjectSchema) }).strict();

// ---------------------------------------------------------------------------
// GET /api/agent/events -- events.ts's `listOrgActivity` / agent-events.ts's `AGENT_EVENT_TYPES`
// ---------------------------------------------------------------------------

export const AgentEventTypeSchema = z.enum(AGENT_EVENT_TYPES);

export const EventsQuerySchema = z
	.object({
		after: z.string().regex(/^\d+$/).optional(),
		limit: z.string().regex(/^\d+$/).optional(),
		type: z.string().optional(),
		project: z.string().optional(),
		claimed: z.literal('me').optional(),
	})
	.strict();

export const OrgActivityEventSchema = z
	.object({
		seq: z.number(),
		eventType: AgentEventTypeSchema,
		actorType: z.enum(['user', 'agent', 'system']),
		actorId: z.string(),
		oldValue: z.unknown(),
		newValue: z.unknown(),
		createdAt: z.coerce.date().nullable(),
		issue: z
			.object({
				id: z.string(),
				title: z.string(),
				status: z.string(),
				issueType: z.string(),
				projectId: z.string(),
				// N9 (C2): CURRENT claim/waiting state at read time -- NOT the state at event time.
				// Lets an agent dispatcher evaluate "claimed by me" per-event without a follow-up
				// GET /api/agent/issues/:id.
				assigneeType: z.string().nullable(),
				assignedTo: z.string().nullable(),
				claimedAt: z.coerce.date().nullable(),
				waitingOn: z.string().nullable(),
			})
			.strict(),
	})
	.strict();

export const EventsResponseSchema = z
	.object({
		events: z.array(OrgActivityEventSchema),
		cursor: z.number(),
		hasMore: z.boolean(),
	})
	.strict();

// ---------------------------------------------------------------------------
// GET /api/agent/issues/{issueId}/activity -- issue-activity.ts's `listIssueActivityPage` ($lib/issue-activity types)
// ---------------------------------------------------------------------------

export const IssueActivityQuerySchema = z
	.object({
		// Entries per page (an `occurrence_burst` run counts as one), clamped to [1, 200], default 50.
		limit: z.string().regex(/^\d+$/).optional(),
		// The previous page's `nextCursor` (opaque).
		before: z.string().optional(),
	})
	.strict();

const ActivityActorSchema = z
	.object({
		type: z.enum(['user', 'agent', 'system']),
		id: z.string(),
		// A user's name (else email); an org agent's name; "<owner name> (CLI)" for a personal CLI agent; a label for a
		// system actor. Null only when the user/agent no longer exists.
		name: z.string().nullable(),
		personal: z.boolean(),
	})
	.strict();

export const IssueActivityEntrySchema = z
	.object({
		id: z.string(),
		seq: z.number(),
		eventType: AgentEventTypeSchema,
		actorType: z.enum(['user', 'agent', 'system']),
		actorId: z.string(),
		actor: ActivityActorSchema,
		oldValue: z.unknown(),
		newValue: z.unknown(),
		createdAt: z.string(),
		comment: z.object({ id: z.string(), excerpt: z.string().nullable(), deleted: z.boolean() }).strict().optional(),
		assignee: ActivityActorSchema.optional(),
		// occurrence_burst only: the run of consecutive burst rows this entry collapses.
		burst: z
			.object({
				rows: z.number().int(),
				firstAt: z.string(),
				lastAt: z.string(),
				occurrencesFrom: z.number().nullable(),
				occurrencesTo: z.number().nullable(),
			})
			.strict()
			.optional(),
	})
	.strict();

export const IssueActivityResponseSchema = z
	.object({
		entries: z.array(IssueActivityEntrySchema),
		nextCursor: z.string().nullable(),
		hasMore: z.boolean(),
	})
	.strict();

// ---------------------------------------------------------------------------
// POST /api/agent/uploads -- upload-core.ts's `UploadCoreResult`
// ---------------------------------------------------------------------------

export const UploadResponseSchema = z
	.object({
		id: z.string(),
		url: z.string(),
		filename: z.string(),
		contentType: z.string(),
		sizeBytes: z.number(),
	})
	.strict();

// ---------------------------------------------------------------------------
// POST /api/agent/batch
// ---------------------------------------------------------------------------

export const BatchOperationSchema = z
	.object({
		op: z.enum([
			'issues.status',
			'issues.claim',
			'issues.claim.release',
			'issues.comment',
			'comments.edit',
			'comments.delete',
			'issues.report.severity',
			'issues.progress',
			'issues.relations.add',
			'issues.relations.remove',
		]),
		issueId: z.string(),
		params: z.unknown().optional(),
	})
	.strict();

export const BatchBodySchema = z
	.object({
		operations: z.array(BatchOperationSchema).min(1).max(20),
		stopOnError: z.boolean().optional(),
	})
	.strict();

export const BatchResultSchema = z
	.object({
		ok: z.boolean(),
		status: z.number(),
		result: z.unknown().optional(),
		error: z.string().optional(),
		skipped: z.boolean().optional(),
		// A11 (N7f): present only for a claim/release conflict result.
		claimedBy: z.string().nullable().optional(),
		claimedAt: z.string().nullable().optional(),
	})
	.strict();

export const BatchResponseSchema = z
	.object({
		results: z.array(BatchResultSchema),
		completed: z.number(),
	})
	.strict();

export const BatchValidationErrorSchema = z.object({ message: z.string() }).strict();

// ---------------------------------------------------------------------------
// GET /api/agent/self -- R1a (N7f)
// ---------------------------------------------------------------------------

export const SelfResponseSchema = z
	.object({
		agentId: z.string(),
		name: z.string(),
		organizationId: z.string(),
		// Suggested `sentinel` CLI profile name for this deployment (SENTINEL_CLI_PROFILE); present only
		// when the operator configured one. `sentinel login` uses it to name the profile on a fresh machine.
		cliProfile: z.string().optional(),
		// sentinel-cli-browser-login: 'ai' | 'bot' for an org agent, 'personal' for a `sentinel login` key.
		kind: z.string(),
		// The person a personal key acts for, as resolved LIVE for this request; null for an org agent. `canWrite` is
		// rbac.ts's `write` for their current role (false = this key is read-only).
		owner: z
			.object({
				userId: z.string(),
				name: z.string().nullable(),
				email: z.string(),
				role: z.enum(['owner', 'admin', 'engineer', 'support', 'viewer']),
				canWrite: z.boolean(),
			})
			.strict()
			.nullable(),
		key: z
			.object({
				id: z.string(),
				prefix: z.string(),
				// N9 (C6): key `created_at`, ISO string or null, for age-based rotation.
				createdAt: z.string().nullable(),
				expiresAt: z.string().nullable(),
				// project_api_keys tracks no last-used timestamp (N7f R1 note) -- always null today,
				// kept in the shape so a future column addition doesn't break this contract.
				lastUsedAt: z.string().nullable(),
			})
			.strict(),
	})
	.strict();

// ---------------------------------------------------------------------------
// POST /api/agent/key/rotate -- R1b (N7f)
// ---------------------------------------------------------------------------

export const KeyRotateResponseSchema = z
	.object({
		success: z.literal(true),
		oldKey: z.object({ id: z.string(), expiresAt: z.string().nullable() }).strict(),
		newKey: z.object({ id: z.string(), prefix: z.string(), secret: z.string() }).strict(),
	})
	.strict();

// ---------------------------------------------------------------------------
// POST /api/agent/key/revoke -- sentinel-cli-browser-login (`sentinel logout`)
// ---------------------------------------------------------------------------

export const KeyRevokeResponseSchema = z
	.object({
		success: z.literal(true),
		revokedKeyId: z.string(),
	})
	.strict();

// ---------------------------------------------------------------------------
// GET /api/agent/repo-credentials -- N10 part 2 (AGENT_WORKER_PLAN.md §4.5)
// ---------------------------------------------------------------------------

// The decrypted secret: a bare token (github, or a bitbucket access token) or a bitbucket
// username + app-password pair. This shape exists ONLY in this response, only over TLS, only to
// agents holding the admin-set can_access_repo_credentials grant; the worker keeps it in memory
// and never journals it.
export const RepoCredentialSecretSchema = z.union([
	z.object({ token: z.string() }).strict(),
	z.object({ username: z.string(), appPassword: z.string() }).strict(),
]);

export const RepoCredentialsResponseSchema = z
	.object({
		credentials: z.array(
			z
				.object({
					id: z.string(),
					provider: z.enum(['github', 'bitbucket']),
					label: z.string(),
					secret: RepoCredentialSecretSchema,
				})
				.strict()
		),
	})
	.strict();

// ---------------------------------------------------------------------------
// GET /api/agent/members
// ---------------------------------------------------------------------------

export const ListMembersQuerySchema = z
	.object({
		role: z.enum(['owner', 'admin', 'engineer', 'support', 'viewer']).optional(),
	})
	.strict();

export const ListMembersResponseSchema = z
	.object({
		members: z.array(
			z
				.object({
					id: z.string(),
					userId: z.string(),
					role: z.string(),
					user: z
						.object({
							name: z.string(),
							email: z.string(),
						})
						.strict(),
				})
				.strict()
		),
	})
	.strict();

// ---------------------------------------------------------------------------
// POST /api/agent/members
// ---------------------------------------------------------------------------

export const InviteMemberBodySchema = z
	.object({
		email: z.string(),
		role: z.enum(['owner', 'admin', 'engineer', 'support', 'viewer']),
	})
	.strict();

export const InviteMemberResponseSchema = z
	.object({
		id: z.string(),
		email: z.string(),
		role: z.string(),
		status: z.string(),
		expiresAt: z.coerce.date(),
		delivered: z.boolean(),
	})
	.strict();

// ---------------------------------------------------------------------------
// DELETE /api/agent/members/{id}
// ---------------------------------------------------------------------------

export const RemoveMemberResponseSchema = z
	.object({
		success: z.literal(true),
		memberId: z.string(),
		userId: z.string(),
	})
	.strict();
