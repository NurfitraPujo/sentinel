import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import { resolveAgentIssueScope } from '$lib/server/agent-issue-scope';
import { createIssueRelation, deleteIssueRelation } from '$lib/db/queries/issues';
import { writeAgentAuditLog } from '$lib/server/agent-audit';
import { sendIssueNotificationEmails } from '$lib/server/notify';

// Manual Issues M5 stage 2 (design §7 step 3: "POST …/relations (link to service issues, mark
// duplicate_of)"). Mirrors the session-authenticated relations route's validation exactly
// (self-relation guard, same-org guard, duplicate_of cycle guard, same Postgres error-code
// mapping), scoped by the agent's own org (B7) rather than a session role.

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

export const POST: RequestHandler = async ({ request, params, url }) => {
	const ctx = await authenticateAgentRequest(request);
	const sourceIssueId = params.issueId;
	if (!sourceIssueId) {
		throw error(400, 'Missing issueId');
	}

	const body = await request.json().catch(() => ({}));
	const { targetIssueId, relationType } = body;

	if (!targetIssueId || typeof targetIssueId !== 'string') {
		throw error(400, 'targetIssueId is required');
	}
	if (!isValidRelationType(relationType)) {
		throw error(400, `relationType must be one of: ${VALID_RELATION_TYPES.join(', ')}`);
	}
	if (sourceIssueId === targetIssueId) {
		throw error(400, 'An issue cannot be related to itself');
	}

	// Both issues must resolve within the agent's OWN org (B7) -- resolveAgentIssueScope 404s an
	// id belonging to a different tenant the same way as an unknown id.
	await resolveAgentIssueScope(sourceIssueId, ctx.organizationId);
	await resolveAgentIssueScope(targetIssueId, ctx.organizationId);

	try {
		const { relation, notified } = await createIssueRelation(
			sourceIssueId,
			targetIssueId,
			relationType,
			'agent',
			ctx.agentId
		);
		await sendIssueNotificationEmails(notified, { issueId: sourceIssueId, origin: url.origin });
		await writeAgentAuditLog(ctx, 'agent.issue.linked', 'issue', sourceIssueId, {
			targetIssueId,
			relationType,
		});

		return json(relation, { status: 201 });
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

export const DELETE: RequestHandler = async ({ request, params }) => {
	const ctx = await authenticateAgentRequest(request);
	const sourceIssueId = params.issueId;
	if (!sourceIssueId) {
		throw error(400, 'Missing issueId');
	}

	const body = await request.json().catch(() => ({}));
	const { targetIssueId, relationType } = body;

	if (!targetIssueId || typeof targetIssueId !== 'string') {
		throw error(400, 'targetIssueId is required');
	}
	if (!isValidRelationType(relationType)) {
		throw error(400, `relationType must be one of: ${VALID_RELATION_TYPES.join(', ')}`);
	}

	await resolveAgentIssueScope(sourceIssueId, ctx.organizationId);
	await resolveAgentIssueScope(targetIssueId, ctx.organizationId);

	const deleted = await deleteIssueRelation(sourceIssueId, targetIssueId, relationType, 'agent', ctx.agentId);
	if (!deleted) {
		throw error(404, 'Relation not found');
	}

	await writeAgentAuditLog(ctx, 'agent.issue.unlinked', 'issue', sourceIssueId, {
		targetIssueId,
		relationType,
	});

	return json({ success: true }, { status: 200 });
};
