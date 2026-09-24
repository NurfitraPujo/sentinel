import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import { db } from '$lib/server/db';
import { organizationMembers, issues, projects, issueSubscriptions, projectEnvironmentGrants } from '$lib/db/schema';
import { eq, and, inArray } from 'drizzle-orm';
import { writeAgentAuditLog } from '$lib/server/agent-audit';

async function findOrgMember(
	executor: any,
	orgId: string,
	memberId: string,
	{ lock = false }: { lock?: boolean } = {}
) {
	const byId = executor
		.select()
		.from(organizationMembers)
		.where(and(eq(organizationMembers.organizationId, orgId), eq(organizationMembers.id, memberId)));
	const [byIdRow] = lock ? await byId.for('update') : await byId;
	if (byIdRow) return byIdRow;

	const byUserId = executor
		.select()
		.from(organizationMembers)
		.where(and(eq(organizationMembers.organizationId, orgId), eq(organizationMembers.userId, memberId)))
		.limit(1);
	const [byUserIdRow] = lock ? await byUserId.for('update') : await byUserId;
	return byUserIdRow;
}

async function countLockedOwners(tx: any, orgId: string): Promise<number> {
	const ownerRows = await tx
		.select({ id: organizationMembers.id })
		.from(organizationMembers)
		.where(and(eq(organizationMembers.organizationId, orgId), eq(organizationMembers.role, 'owner')))
		.for('update');
	return ownerRows.length;
}

// DELETE /api/agent/members/[id] -- revokes access for an organization member.
// Scoped by the agent key's own organization (B7).
export const DELETE: RequestHandler = async ({ params, request }) => {
	const ctx = await authenticateAgentRequest(request);

	const { id } = params;
	if (!id) {
		throw error(400, 'Missing memberId');
	}

	const targetMember = await db.transaction(async (tx) => {
		const target = await findOrgMember(tx, ctx.organizationId, id, { lock: true });

		if (!target) {
			throw error(404, 'Member not found in this organization');
		}

		// Guard against revoking the sole owner
		if (target.role === 'owner') {
			const ownerCount = await countLockedOwners(tx, ctx.organizationId);
			if (ownerCount <= 1) {
				throw error(400, 'Cannot revoke the sole owner of an organization');
			}
		}

		await tx
			.delete(organizationMembers)
			.where(
				and(
					eq(organizationMembers.organizationId, ctx.organizationId),
					eq(organizationMembers.userId, target.userId)
				)
			);

		// Clean up issue subscriptions
		const orgIssueRows = await tx
			.select({ id: issues.id })
			.from(issues)
			.innerJoin(projects, eq(projects.id, issues.projectId))
			.where(eq(projects.organizationId, ctx.organizationId));
		const orgIssueIds = orgIssueRows.map((r: { id: string }) => r.id);

		if (orgIssueIds.length > 0) {
			await tx
				.delete(issueSubscriptions)
				.where(
					and(
						eq(issueSubscriptions.subscriberType, 'user'),
						eq(issueSubscriptions.subscriberId, target.userId),
						inArray(issueSubscriptions.issueId, orgIssueIds)
					)
				);
		}

		// Clean up environment grants
		await tx.delete(projectEnvironmentGrants).where(
			and(
				eq(projectEnvironmentGrants.userId, target.userId),
				inArray(
					projectEnvironmentGrants.projectId,
					tx.select({ id: projects.id }).from(projects).where(eq(projects.organizationId, ctx.organizationId))
				)
			)
		);

		return target;
	});

	await writeAgentAuditLog(ctx, 'agent.member_removed', 'member', targetMember.id, {
		userId: targetMember.userId,
		role: targetMember.role,
	});

	return json({ success: true, memberId: targetMember.id, userId: targetMember.userId });
};
