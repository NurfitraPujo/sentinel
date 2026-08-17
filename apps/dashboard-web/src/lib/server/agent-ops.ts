import { error, json } from '@sveltejs/kit';
import type { RequestEvent } from '@sveltejs/kit';
import { authenticateAgentRequest, type AgentAuthContext } from '$lib/server/agent-auth';
import { resolveAgentIssueScope } from '$lib/server/agent-issue-scope';
import { writeAgentAuditLog } from '$lib/server/agent-audit';
import { updateIssueStatus } from '$lib/db/queries/issues';
import { validateResolvedInVersion } from '$lib/server/issue-access';
import { sendIssueNotificationEmails } from '$lib/server/notify';
import { claimIssue, releaseClaim, updateManualIssueReport, ClaimConflictError } from '$lib/db/queries/reports';
import type { ReportSeverity } from '$lib/db/queries/reports';
import {
	createComment,
	editComment,
	deleteComment,
	getCommentById,
	CommentValidationError,
	CommentNotFoundError,
} from '$lib/db/queries/comments';
import { recordAgentProgress } from '$lib/db/queries/agent-work';
import { createIssueRelation, deleteIssueRelation, RelationCycleError } from '$lib/db/queries/issues';
import type { AgentAuditDescriptor } from '$lib/server/agent-route';
import { log } from '$lib/server/observability/log';
import type { AgentIssueScope } from '$lib/server/agent-issue-scope';

/**
 * N2 (AI-agent-native plan): the op registry that backs both `POST /api/agent/batch` and the
 * individual `/api/agent/issues/[issueId]/{status,claim,comments,progress,relations}` routes.
 *
 * Each op handler is a straight EXTRACTION of what its single route already did -- same
 * validation, same `resolveAgentIssueScope(issueId, ctx.organizationId)` scope resolution (B7:
 * tenant scope from the credential, never the request), same domain-error -> HTTP-status mapping,
 * same audit descriptor. The single routes become thin wrappers (`agentOpRoute` below) so their
 * behavior -- and their existing tests -- must not change.
 *
 * Batch calls these handlers SEQUENTIALLY, one at a time, each against its own underlying
 * transaction (every mutation query below already wraps itself in `db.transaction`, e.g.
 * `claimIssue`/`releaseClaim` in reports.ts). There is deliberately no outer transaction spanning
 * multiple ops: batch's contract is at-least-once / partial-completion, matching what calling the
 * single routes N times over HTTP would already give you. A `stopOnError` batch that fails on op 3
 * of 5 leaves ops 1-2 committed -- callers must design for that.
 */

export interface AgentOpResult {
	status: number;
	body: unknown;
	audit?: AgentAuditDescriptor;
}

export type AgentOpHandler = (
	ctx: AgentAuthContext,
	issueId: string,
	params: unknown,
	originUrl: string
) => Promise<AgentOpResult>;

function asRecord(params: unknown): Record<string, unknown> {
	return params && typeof params === 'object' ? (params as Record<string, unknown>) : {};
}

/**
 * A11 (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md N7f, "keep advisory, document loudly +
 * enrich errors"): a plain `error(409, message)` gives a protocol-following agent a conflict
 * signal but no way to see WHO holds the claim without a second read. Re-resolves the issue's
 * current scope (cheap, and correctness-critical here -- the whole reason this is being called is
 * that the claim state just changed under us) and throws via SvelteKit's `error()` helper with
 * `claimedBy`/`claimedAt` on the body (`App.Error`, app.d.ts) -- deliberately NOT a raw `Response`:
 * both the single-route path (SvelteKit's own error serialization) and `POST /api/agent/batch`
 * (which checks `isHttpError(err)` and reads `err.body`) need to recognize this as a normal
 * `HttpError` to extract the status/body correctly; a raw thrown `Response` falls through both as
 * an unmapped 500.
 */
async function throwClaimConflict(issueId: string, organizationId: string, message: string): Promise<never> {
	let claimedBy: string | null = null;
	let claimedAt: string | null = null;
	try {
		const fresh = await resolveAgentIssueScope(issueId, organizationId);
		claimedBy = fresh.assignedTo;
		claimedAt = fresh.claimedAt ? fresh.claimedAt.toISOString() : null;
	} catch {
		// Best-effort enrichment only -- if the re-read itself fails (issue deleted mid-race, etc.)
		// the caller still gets the 409 with null context rather than a swallowed/opaque 500.
	}
	throw error(409, { message, claimedBy, claimedAt });
}

/**
 * A11: fires a structured, non-blocking warning when a NON-claimant agent successfully mutates an
 * issue someone/something else currently holds. Advisory-only by design (see the plan's A11
 * decision note -- enforcing this as a hard error would break e2e's human-collaborator parity);
 * this is purely observability, so it never affects the response.
 */
function warnIfMutatingSomeoneElsesClaim(ctx: AgentAuthContext, issue: AgentIssueScope, action: string): void {
	if (issue.assignedTo && issue.assignedTo !== ctx.agentId) {
		log.warn('agent.mutated_claimed_issue', {
			action,
			issueId: issue.issueId,
			agentId: ctx.agentId,
			claimedBy: issue.assignedTo,
			claimedByType: issue.assigneeType,
			claimedAt: issue.claimedAt ? issue.claimedAt.toISOString() : null,
		});
	}
}

const VALID_STATUSES = ['unresolved', 'resolved', 'ignored'] as const;
type IssueStatus = (typeof VALID_STATUSES)[number];

function isValidStatus(value: unknown): value is IssueStatus {
	return typeof value === 'string' && (VALID_STATUSES as readonly string[]).includes(value);
}

const issuesStatus: AgentOpHandler = async (ctx, issueId, params, originUrl) => {
	const issue = await resolveAgentIssueScope(issueId, ctx.organizationId);
	const body = asRecord(params);
	const { status } = body;
	if (!isValidStatus(status)) {
		throw error(400, `status must be one of: ${VALID_STATUSES.join(', ')}`);
	}

	const validatedResolvedInVersion = validateResolvedInVersion(body.resolved_in_version);
	warnIfMutatingSomeoneElsesClaim(ctx, issue, 'issues.status');

	// A05-status: `changed:false` means updateIssueStatus recognized this as an exact retry of the
	// already-applied status (same status AND same resolved_in_version) -- no activity row, no
	// notification. Notification emails are only ever sent on the `changed:true` path.
	const { changed, notified } = await updateIssueStatus(
		issue.issueId,
		status,
		validatedResolvedInVersion ?? undefined,
		'agent',
		ctx.agentId
	);
	if (changed) {
		await sendIssueNotificationEmails(notified, { issueId: issue.issueId, origin: originUrl });
	}

	return {
		status: 200,
		body: { success: true, status, changed },
		audit: {
			action: 'agent.issue.status_changed',
			resourceType: 'issue',
			resourceId: issue.issueId,
			metadata: { status, resolvedInVersion: validatedResolvedInVersion ?? undefined, changed },
		},
	};
};

const issuesClaim: AgentOpHandler = async (ctx, issueId, _params, originUrl) => {
	const issue = await resolveAgentIssueScope(issueId, ctx.organizationId);
	try {
		const { issue: updated, notified } = await claimIssue(issue.issueId, 'agent', ctx.agentId);
		await sendIssueNotificationEmails(notified, { issueId: issue.issueId, origin: originUrl });
		return {
			status: 200,
			body: { success: true, issue: updated },
			audit: { action: 'agent.issue.claimed', resourceType: 'issue', resourceId: issue.issueId },
		};
	} catch (err) {
		if (err instanceof ClaimConflictError) {
			await throwClaimConflict(issue.issueId, ctx.organizationId, err.message);
		}
		throw err;
	}
};

const issuesClaimRelease: AgentOpHandler = async (ctx, issueId, _params, originUrl) => {
	const issue = await resolveAgentIssueScope(issueId, ctx.organizationId);
	try {
		// Non-force: releaseClaim's own conditional UPDATE (`WHERE assigned_to = $agentId`) is what
		// actually enforces "only the current claimant may release" -- an agent cannot pass someone
		// else's id here since `ctx.agentId` comes from the credential (B7), not the request.
		const { issue: updated, notified } = await releaseClaim(issue.issueId, ctx.agentId, { actorType: 'agent' });
		await sendIssueNotificationEmails(notified, { issueId: issue.issueId, origin: originUrl });
		return {
			status: 200,
			body: { success: true, issue: updated },
			audit: { action: 'agent.issue.claim_released', resourceType: 'issue', resourceId: issue.issueId },
		};
	} catch (err) {
		if (err instanceof ClaimConflictError) {
			await throwClaimConflict(issue.issueId, ctx.organizationId, err.message);
		}
		throw err;
	}
};

const issuesComment: AgentOpHandler = async (ctx, issueId, params, originUrl) => {
	const issue = await resolveAgentIssueScope(issueId, ctx.organizationId);
	const body = asRecord(params);
	if (typeof body.body_md !== 'string' || body.body_md.trim().length === 0) {
		throw error(400, 'body_md is required');
	}
	warnIfMutatingSomeoneElsesClaim(ctx, issue, 'issues.comment');

	// R18 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): snake_case (`attachment_ids`), matching `body_md`.
	let attachmentIds: string[] | undefined;
	if (body.attachment_ids !== undefined) {
		if (!Array.isArray(body.attachment_ids) || !body.attachment_ids.every((id: unknown) => typeof id === 'string')) {
			throw error(400, 'attachment_ids must be an array of strings');
		}
		attachmentIds = body.attachment_ids as string[];
	}

	try {
		const { comment, notified } = await createComment({
			issueId: issue.issueId,
			authorType: 'agent',
			authorId: ctx.agentId,
			bodyMd: body.body_md as string,
			attachmentIds,
		});
		await sendIssueNotificationEmails(notified, { issueId: issue.issueId, origin: originUrl });

		return {
			status: 201,
			body: { comment },
			audit: {
				action: 'agent.issue.commented',
				resourceType: 'issue',
				resourceId: issue.issueId,
				metadata: { commentId: comment.id },
			},
		};
	} catch (err) {
		if (err instanceof CommentValidationError) {
			throw error(400, err.message);
		}
		if (err instanceof CommentNotFoundError) {
			throw error(404, err.message);
		}
		throw err;
	}
};

/**
 * A08 (N7e, docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md): `comment_id` travels in `params`
 * (snake_case, matching every other op body field) rather than as a second route param on
 * `AgentOpHandler` -- batch's contract is `{op, issueId, params}`, so an op that needs a second id
 * has nowhere else to put it. The single `/comments/[commentId]` route (agent-comment-route.ts)
 * injects `commentId` from its own URL param into `params.comment_id` before calling this, so both
 * callers see the same shape.
 *
 * Ownership gate mirrors the human route (api/issues/[issueId]/comments/[commentId]/+server.ts:39,
 * 81) with one deliberate narrowing: only the AUTHORING AGENT may edit/delete its own comment --
 * no moderator carve-out (that route's `isCommentModeratorRole`) exists for agents, since agent
 * credentials have no org role to check (B7: an agent's authority is scoped to its own identity,
 * never a claimed role).
 */
async function loadOwnedAgentComment(issueId: string, commentId: unknown) {
	if (typeof commentId !== 'string' || commentId.length === 0) {
		throw error(400, 'comment_id is required');
	}
	const comment = await getCommentById(commentId);
	if (!comment || comment.issueId !== issueId) {
		throw error(404, 'Comment not found');
	}
	return comment;
}

const issuesCommentsEdit: AgentOpHandler = async (ctx, issueId, params) => {
	const issue = await resolveAgentIssueScope(issueId, ctx.organizationId);
	const body = asRecord(params);
	const comment = await loadOwnedAgentComment(issue.issueId, body.comment_id);

	if (comment.authorType !== 'agent' || comment.authorId !== ctx.agentId) {
		throw error(403, 'Forbidden: only the comment author may edit it');
	}
	if (typeof body.body_md !== 'string' || body.body_md.trim().length === 0) {
		throw error(400, 'body_md is required');
	}

	try {
		const updated = await editComment(comment.id, body.body_md as string);
		return {
			status: 200,
			body: { comment: updated },
			audit: {
				action: 'agent.issue.comment_edited',
				resourceType: 'issue',
				resourceId: issue.issueId,
				metadata: { commentId: comment.id },
			},
		};
	} catch (err) {
		if (err instanceof CommentValidationError) {
			throw error(400, err.message);
		}
		if (err instanceof CommentNotFoundError) {
			throw error(404, err.message);
		}
		throw err;
	}
};

const issuesCommentsDelete: AgentOpHandler = async (ctx, issueId, params) => {
	const issue = await resolveAgentIssueScope(issueId, ctx.organizationId);
	const body = asRecord(params);
	const comment = await loadOwnedAgentComment(issue.issueId, body.comment_id);

	if (comment.authorType !== 'agent' || comment.authorId !== ctx.agentId) {
		throw error(403, 'Forbidden: only the comment author may delete it');
	}

	try {
		const result = await deleteComment(comment.id);
		return {
			status: 200,
			body: { success: true, issueId: result.issueId },
			audit: {
				action: 'agent.issue.comment_deleted',
				resourceType: 'issue',
				resourceId: issue.issueId,
				metadata: { commentId: comment.id },
			},
		};
	} catch (err) {
		if (err instanceof CommentNotFoundError) {
			throw error(404, err.message);
		}
		throw err;
	}
};

const VALID_SEVERITIES = ['low', 'medium', 'high', 'critical'] as const;

function isValidSeverity(value: unknown): value is ReportSeverity {
	return typeof value === 'string' && (VALID_SEVERITIES as readonly string[]).includes(value);
}

/**
 * A09 (N7e): reuses `updateManualIssueReport` (reports.ts) with only `severity` set, and
 * `actorType: 'agent'` so the `report_edited` activity row attributes the change correctly
 * (reports.ts's function used to hardcode `actorType: 'user'` -- fixed alongside this op so an
 * agent-driven severity change is never misattributed to a user). 400 when the issue isn't a
 * `user_report` -- `manual_issue_reports` has no row for a `system_error` issue, so this is
 * checked BEFORE calling into the query (which would otherwise throw a generic "Report not
 * found" 500-shaped error).
 */
const issuesReportSeverity: AgentOpHandler = async (ctx, issueId, params) => {
	const issue = await resolveAgentIssueScope(issueId, ctx.organizationId);
	if (issue.issueType !== 'user_report') {
		throw error(400, 'severity can only be set on user_report issues');
	}

	const body = asRecord(params);
	if (!isValidSeverity(body.severity)) {
		throw error(400, `severity must be one of: ${VALID_SEVERITIES.join(', ')}`);
	}

	const { report } = await updateManualIssueReport({
		issueId: issue.issueId,
		actorId: ctx.agentId,
		actorType: 'agent',
		severity: body.severity,
	});

	return {
		status: 200,
		body: { success: true, severity: report?.severity },
		audit: {
			action: 'agent.issue.report_severity_changed',
			resourceType: 'issue',
			resourceId: issue.issueId,
			metadata: { severity: body.severity },
		},
	};
};

const issuesProgress: AgentOpHandler = async (ctx, issueId, params) => {
	const issue = await resolveAgentIssueScope(issueId, ctx.organizationId);
	const body = asRecord(params);
	if (typeof body.message_md !== 'string' || body.message_md.trim().length === 0) {
		throw error(400, 'message_md is required');
	}
	warnIfMutatingSomeoneElsesClaim(ctx, issue, 'issues.progress');

	await recordAgentProgress(issue.issueId, ctx.agentId, body.message_md.trim());

	return {
		status: 201,
		body: { success: true },
		audit: { action: 'agent.issue.progress_update', resourceType: 'issue', resourceId: issue.issueId },
	};
};

const VALID_RELATION_TYPES = ['linked_to', 'caused_by', 'duplicate_of'] as const;
type RelationType = (typeof VALID_RELATION_TYPES)[number];

function isValidRelationType(value: unknown): value is RelationType {
	return typeof value === 'string' && (VALID_RELATION_TYPES as readonly string[]).includes(value);
}

function isUniqueViolation(err: unknown): boolean {
	return typeof err === 'object' && err !== null && (err as { code?: string }).code === '23505';
}

function isCheckViolation(err: unknown): boolean {
	return typeof err === 'object' && err !== null && (err as { code?: string }).code === '23514';
}

const issuesRelationsAdd: AgentOpHandler = async (ctx, issueId, params, originUrl) => {
	const sourceIssue = await resolveAgentIssueScope(issueId, ctx.organizationId);
	const body = asRecord(params);
	const targetIssueId = body.target_issue_id;
	const relationType = body.relation_type;

	if (!targetIssueId || typeof targetIssueId !== 'string') {
		throw error(400, 'target_issue_id is required');
	}
	if (!isValidRelationType(relationType)) {
		throw error(400, `relation_type must be one of: ${VALID_RELATION_TYPES.join(', ')}`);
	}
	if (sourceIssue.issueId === targetIssueId) {
		throw error(400, 'An issue cannot be related to itself');
	}

	// The target issue must also resolve within the agent's OWN org (B7).
	await resolveAgentIssueScope(targetIssueId, ctx.organizationId);

	try {
		const { relation, notified } = await createIssueRelation(
			sourceIssue.issueId,
			targetIssueId,
			relationType,
			'agent',
			ctx.agentId
		);
		await sendIssueNotificationEmails(notified, { issueId: sourceIssue.issueId, origin: originUrl });

		return {
			status: 201,
			body: relation,
			audit: {
				action: 'agent.issue.linked',
				resourceType: 'issue',
				resourceId: sourceIssue.issueId,
				metadata: { targetIssueId, relationType },
			},
		};
	} catch (err) {
		if (err instanceof RelationCycleError) {
			throw error(409, err.message);
		}
		if (isUniqueViolation(err)) {
			throw error(409, 'This relation already exists');
		}
		if (isCheckViolation(err)) {
			throw error(400, 'An issue cannot be related to itself');
		}
		throw err;
	}
};

const issuesRelationsRemove: AgentOpHandler = async (ctx, issueId, params) => {
	const sourceIssue = await resolveAgentIssueScope(issueId, ctx.organizationId);
	const body = asRecord(params);
	const targetIssueId = body.target_issue_id;
	const relationType = body.relation_type;

	if (!targetIssueId || typeof targetIssueId !== 'string') {
		throw error(400, 'target_issue_id is required');
	}
	if (!isValidRelationType(relationType)) {
		throw error(400, `relation_type must be one of: ${VALID_RELATION_TYPES.join(', ')}`);
	}

	await resolveAgentIssueScope(targetIssueId, ctx.organizationId);

	const deleted = await deleteIssueRelation(sourceIssue.issueId, targetIssueId, relationType, 'agent', ctx.agentId);
	if (!deleted) {
		throw error(404, 'Relation not found');
	}

	return {
		status: 200,
		body: { success: true },
		audit: {
			action: 'agent.issue.unlinked',
			resourceType: 'issue',
			resourceId: sourceIssue.issueId,
			metadata: { targetIssueId, relationType },
		},
	};
};

/**
 * Op names are the batch API's public vocabulary AND the single-route wiring below. Mutations
 * only (D01 excludes uploads/multipart and questions/blocking semantics from batch v1; GET ops
 * are excluded too -- batch is mutations only).
 */
export const agentOps: Record<string, AgentOpHandler> = {
	'issues.status': issuesStatus,
	'issues.claim': issuesClaim,
	'issues.claim.release': issuesClaimRelease,
	'issues.comment': issuesComment,
	'comments.edit': issuesCommentsEdit,
	'comments.delete': issuesCommentsDelete,
	'issues.report.severity': issuesReportSeverity,
	'issues.progress': issuesProgress,
	'issues.relations.add': issuesRelationsAdd,
	'issues.relations.remove': issuesRelationsRemove,
};

export class UnknownAgentOpError extends Error {
	constructor(op: string) {
		super(`Unknown op: ${op}`);
		this.name = 'UnknownAgentOpError';
	}
}

/** Looks up and runs one op by name. Throws `UnknownAgentOpError` for anything not in the registry. */
export async function runAgentOp(
	op: string,
	ctx: AgentAuthContext,
	issueId: string,
	params: unknown,
	originUrl: string
): Promise<AgentOpResult> {
	const handler = agentOps[op];
	if (!handler) {
		throw new UnknownAgentOpError(op);
	}
	return handler(ctx, issueId, params, originUrl);
}

/**
 * Builds a thin `+server.ts` handler for one op: authenticate -> guard `params.issueId` -> run the
 * op -> audit -> respond. This is the SAME preamble/tail `withAgentIssue` (agent-route.ts) factors
 * out for other agent routes; this one is separate because op handlers take a raw `issueId` and
 * return a `{status, body}` pair rather than a `Response`, so both the single route AND
 * `POST /api/agent/batch` can drive the exact same op handler.
 */
export function agentOpRoute(op: string) {
	return async (event: RequestEvent): Promise<Response> => {
		const ctx = await authenticateAgentRequest(event.request);

		const { issueId } = event.params as { issueId?: string };
		if (!issueId) {
			throw error(400, 'Missing issueId');
		}

		const params = await event.request.json().catch(() => ({}));
		const result = await runAgentOp(op, ctx, issueId, params, event.url.origin);

		if (result.audit) {
			await writeAgentAuditLog(
				ctx,
				result.audit.action,
				result.audit.resourceType,
				result.audit.resourceId,
				result.audit.metadata
			);
		}

		return json(result.body, { status: result.status });
	};
}
