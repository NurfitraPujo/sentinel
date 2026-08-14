import { error, json } from '@sveltejs/kit';
import type { RequestEvent } from '@sveltejs/kit';
import { authenticateAgentRequest, type AgentAuthContext } from '$lib/server/agent-auth';
import { resolveAgentIssueScope } from '$lib/server/agent-issue-scope';
import { writeAgentAuditLog } from '$lib/server/agent-audit';
import { updateIssueStatus } from '$lib/db/queries/issues';
import { validateResolvedInVersion } from '$lib/server/issue-access';
import { sendIssueNotificationEmails } from '$lib/server/notify';
import { claimIssue, releaseClaim, ClaimConflictError } from '$lib/db/queries/reports';
import { createComment, CommentValidationError, CommentNotFoundError } from '$lib/db/queries/comments';
import { recordAgentProgress } from '$lib/db/queries/agent-work';
import { createIssueRelation, deleteIssueRelation } from '$lib/db/queries/issues';
import type { AgentAuditDescriptor } from '$lib/server/agent-route';

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

	const notified = await updateIssueStatus(
		issue.issueId,
		status,
		validatedResolvedInVersion ?? undefined,
		'agent',
		ctx.agentId
	);
	await sendIssueNotificationEmails(notified, { issueId: issue.issueId, origin: originUrl });

	return {
		status: 200,
		body: { success: true, status },
		audit: {
			action: 'agent.issue.status_changed',
			resourceType: 'issue',
			resourceId: issue.issueId,
			metadata: { status, resolvedInVersion: validatedResolvedInVersion ?? undefined },
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
			throw error(409, err.message);
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
			throw error(409, err.message);
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

const issuesProgress: AgentOpHandler = async (ctx, issueId, params) => {
	const issue = await resolveAgentIssueScope(issueId, ctx.organizationId);
	const body = asRecord(params);
	if (typeof body.message_md !== 'string' || body.message_md.trim().length === 0) {
		throw error(400, 'message_md is required');
	}

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
