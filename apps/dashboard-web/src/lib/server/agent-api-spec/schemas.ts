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
		// N7e (A07): see IssueRowSchema's claimedAt note.
		claimedAt: z.coerce.date().nullable(),
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
		claimed: z.enum(['true', 'false']).optional(),
		project: z.string().optional(),
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
// GET /api/agent/issues/{issueId}/occurrences
// ---------------------------------------------------------------------------

export const OccurrencesQuerySchema = z
	.object({
		limit: z.coerce.number().int().min(1).max(50).optional(),
		before: z.string().datetime({ offset: true }).or(z.string()).optional(),
	})
	.strict();

export const OccurrencesResponseSchema = z.object({ occurrences: z.array(AgentOccurrenceSchema) }).strict();

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

export const StatusBodySchema = z
	.object({
		status: z.enum(['unresolved', 'resolved', 'ignored']),
		resolved_in_version: z.string().optional(),
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
	})
	.strict();

export const PostCommentResponseSchema = z.object({ comment: CommentRowSchema }).strict();

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
	})
	.strict();

export const QuestionResponseSchema = z.object({ comment: CommentRowSchema }).strict();

// ---------------------------------------------------------------------------
// POST /api/agent/issues/{issueId}/progress
// ---------------------------------------------------------------------------

export const ProgressBodySchema = z.object({ message_md: z.string() }).strict();
export const ProgressResponseSchema = z.object({ success: z.literal(true) }).strict();

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

export const AgentProjectSchema = z.object({ id: z.string(), name: z.string(), isInbox: z.boolean() }).strict();
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
