import { json, error } from '@sveltejs/kit';
import { withAgentIssue } from '$lib/server/agent-route';
import { resolveAgentIssueScope } from '$lib/server/agent-issue-scope';
import { createIssueRelation, deleteIssueRelation } from '$lib/db/queries/issues';
import { sendIssueNotificationEmails } from '$lib/server/notify';

// Manual Issues M5 stage 2 (design §7 step 3: "POST …/relations (link to service issues, mark
// duplicate_of)"). Mirrors the session-authenticated relations route's validation exactly
// (self-relation guard, same-org guard, duplicate_of cycle guard, same Postgres error-code
// mapping), scoped by the agent's own org (B7) rather than a session role.
//
// R16 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): migrated onto `withAgentIssue` for the
// SOURCE issue's auth/guard/scope-resolve boilerplate; the target issue's scope is a SECOND
// resolve the handler still does itself (this route is the one place that needs two), and the
// Postgres unique/check-violation -> 409/400 mapping stays local since `withAgentIssue`'s shared
// error mapping only covers ClaimConflictError/comment errors, not this route's own error codes.

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

export const POST = withAgentIssue(async (ctx, sourceIssue, event) => {
	// R18 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): snake_case body fields, matching every
	// other agent API route.
	const body = await event.request.json().catch(() => ({}));
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

	// The target issue must also resolve within the agent's OWN org (B7) -- resolveAgentIssueScope
	// 404s an id belonging to a different tenant the same way as an unknown id.
	await resolveAgentIssueScope(targetIssueId, ctx.organizationId);

	try {
		const { relation, notified } = await createIssueRelation(
			sourceIssue.issueId,
			targetIssueId,
			relationType,
			'agent',
			ctx.agentId
		);
		await sendIssueNotificationEmails(notified, { issueId: sourceIssue.issueId, origin: event.url.origin });

		return {
			response: json(relation, { status: 201 }),
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
});

export const DELETE = withAgentIssue(async (ctx, sourceIssue, event) => {
	const body = await event.request.json().catch(() => ({}));
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
		response: json({ success: true }, { status: 200 }),
		audit: {
			action: 'agent.issue.unlinked',
			resourceType: 'issue',
			resourceId: sourceIssue.issueId,
			metadata: { targetIssueId, relationType },
		},
	};
});
