import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { db } from '$lib/server/db';
import { issues, projects, organizationMembers } from '$lib/db/schema';
import { createIssueRelation, deleteIssueRelation } from '$lib/db/queries/issues';
import { eq, and } from 'drizzle-orm';

// Must match the CHECK constraint on issue_relations.relation_type in
// packages/db-migrations/migrations/1721900000_add_issue_lifecycle_and_relations.sql.
const VALID_RELATION_TYPES = ['linked_to', 'caused_by', 'duplicate_of'] as const;
type RelationType = (typeof VALID_RELATION_TYPES)[number];

function isValidRelationType(value: unknown): value is RelationType {
	return typeof value === 'string' && (VALID_RELATION_TYPES as readonly string[]).includes(value);
}

export const POST: RequestHandler = async ({ request, params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const userId = session.user.id;
	const sourceIssueId = params.issueId;

	const body = await request.json();
	const { targetIssueId, relationType } = body;

	if (!targetIssueId) {
		throw error(400, 'targetIssueId is required');
	}

	if (!isValidRelationType(relationType)) {
		throw error(400, `relationType must be one of: ${VALID_RELATION_TYPES.join(', ')}`);
	}

	// Fetch both issues and their projects to verify organization
	const sourceIssueQuery = await db
		.select({
			issueId: issues.id,
			orgId: projects.organizationId,
		})
		.from(issues)
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.where(eq(issues.id, sourceIssueId));

	const targetIssueQuery = await db
		.select({
			issueId: issues.id,
			orgId: projects.organizationId,
		})
		.from(issues)
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.where(eq(issues.id, targetIssueId));

	if (sourceIssueQuery.length === 0 || targetIssueQuery.length === 0) {
		throw error(404, 'One or both issues not found');
	}

	const sourceOrgId = sourceIssueQuery[0].orgId;
	const targetOrgId = targetIssueQuery[0].orgId;

	if (!sourceOrgId || sourceOrgId !== targetOrgId) {
		throw error(400, 'Both issues must belong to the same organization');
	}

	// Validate user has access to this organization
	const orgMember = await db
		.select()
		.from(organizationMembers)
		.where(
			and(
				eq(organizationMembers.userId, userId),
				eq(organizationMembers.organizationId, sourceOrgId)
			)
		);

	if (orgMember.length === 0) {
		throw error(403, 'Forbidden: You do not have access to this organization');
	}

	// Create the relation and its issue_activity 'linked' entry together, transactionally.
	const relation = await createIssueRelation(sourceIssueId, targetIssueId, relationType, 'user', userId);

	return json(relation, { status: 201 });
};

// Unlink. Matches POST's conventions exactly: same auth check, same RBAC check, same error shapes
// and status codes on failure (401 unauthenticated, 400 bad body, 404 unknown issue(s), 400
// cross-org, 403 no org membership) — only the final action (delete instead of create) differs.
export const DELETE: RequestHandler = async ({ request, params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const userId = session.user.id;
	const sourceIssueId = params.issueId;

	const body = await request.json();
	const { targetIssueId, relationType } = body;

	if (!targetIssueId) {
		throw error(400, 'targetIssueId is required');
	}

	if (!isValidRelationType(relationType)) {
		throw error(400, `relationType must be one of: ${VALID_RELATION_TYPES.join(', ')}`);
	}

	// Fetch both issues and their projects to verify organization
	const sourceIssueQuery = await db
		.select({
			issueId: issues.id,
			orgId: projects.organizationId,
		})
		.from(issues)
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.where(eq(issues.id, sourceIssueId));

	const targetIssueQuery = await db
		.select({
			issueId: issues.id,
			orgId: projects.organizationId,
		})
		.from(issues)
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.where(eq(issues.id, targetIssueId));

	if (sourceIssueQuery.length === 0 || targetIssueQuery.length === 0) {
		throw error(404, 'One or both issues not found');
	}

	const sourceOrgId = sourceIssueQuery[0].orgId;
	const targetOrgId = targetIssueQuery[0].orgId;

	if (!sourceOrgId || sourceOrgId !== targetOrgId) {
		throw error(400, 'Both issues must belong to the same organization');
	}

	// Validate user has access to this organization
	const orgMember = await db
		.select()
		.from(organizationMembers)
		.where(
			and(
				eq(organizationMembers.userId, userId),
				eq(organizationMembers.organizationId, sourceOrgId)
			)
		);

	if (orgMember.length === 0) {
		throw error(403, 'Forbidden: You do not have access to this organization');
	}

	const deleted = await deleteIssueRelation(sourceIssueId, targetIssueId, relationType, 'user', userId);

	if (!deleted) {
		throw error(404, 'Relation not found');
	}

	return json({ success: true }, { status: 200 });
};
