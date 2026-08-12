import { error } from '@sveltejs/kit';
import type { RequestEvent } from '@sveltejs/kit';
import { authenticateAgentRequest, type AgentAuthContext } from '$lib/server/agent-auth';
import { resolveAgentIssueScope, type AgentIssueScope } from '$lib/server/agent-issue-scope';
import { writeAgentAuditLog } from '$lib/server/agent-audit';
import { ClaimConflictError } from '$lib/db/queries/reports';
import { CommentValidationError, CommentNotFoundError } from '$lib/db/queries/comments';

/**
 * R16 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): every `/api/agent/issues/[issueId]/*` route
 * repeated the SAME five-line preamble (authenticate -> guard `params.issueId` -> resolve scope)
 * and the same error-mapping tail (`ClaimConflictError` -> 409, comment errors -> 400/404) around
 * whatever it actually did. `withAgentIssue` factors that out; each route becomes just its own
 * handler body plus a descriptor of what to audit.
 *
 * The handler receives the resolved `(ctx, issue, event)` and returns `{ response, audit? }` --
 * `audit` is a DESCRIPTOR, not a side effect the handler performs itself, so a route can never
 * forget to audit a mutation it actually made (or, just as easily, audit one that failed) the way
 * six independent hand-written `writeAgentAuditLog` call sites eventually would. Auditing runs
 * only after `handler` resolves successfully, mirroring exactly where each original route's audit
 * call sat (after the mutation, before the response).
 */

export interface AgentAuditDescriptor {
	action: string;
	resourceType: string;
	resourceId: string;
	metadata?: Record<string, unknown>;
}

export interface AgentIssueHandlerResult {
	response: Response;
	/** Omit for a read-only handler (e.g. GET comments) that has nothing to audit. */
	audit?: AgentAuditDescriptor;
}

export type AgentIssueHandler = (
	ctx: AgentAuthContext,
	issue: AgentIssueScope,
	event: RequestEvent
) => Promise<AgentIssueHandlerResult>;

/**
 * Maps the handful of error types every agent-issue route already handled locally to the SAME
 * HTTP status each route already used -- this is a straight extraction, not a behavior change.
 */
function mapAgentIssueError(err: unknown): never {
	if (err instanceof ClaimConflictError) {
		throw error(409, err.message);
	}
	if (err instanceof CommentValidationError) {
		throw error(400, err.message);
	}
	if (err instanceof CommentNotFoundError) {
		throw error(404, err.message);
	}
	throw err;
}

export function withAgentIssue(handler: AgentIssueHandler) {
	return async (event: RequestEvent): Promise<Response> => {
		const ctx = await authenticateAgentRequest(event.request);

		const { issueId } = event.params as { issueId?: string };
		if (!issueId) {
			throw error(400, 'Missing issueId');
		}

		const issue = await resolveAgentIssueScope(issueId, ctx.organizationId);

		let result: AgentIssueHandlerResult;
		try {
			result = await handler(ctx, issue, event);
		} catch (err) {
			mapAgentIssueError(err);
		}

		if (result.audit) {
			await writeAgentAuditLog(
				ctx,
				result.audit.action,
				result.audit.resourceType,
				result.audit.resourceId,
				result.audit.metadata
			);
		}

		return result.response;
	};
}
