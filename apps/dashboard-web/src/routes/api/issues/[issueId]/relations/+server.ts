import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { db } from '$lib/server/db';
import { issues, projects, issueRelations } from '$lib/db/schema';
import { createIssueRelation, deleteIssueRelation } from '$lib/db/queries/issues';
import { requireIssueAccess } from '$lib/server/issue-access';
import { eq, and } from 'drizzle-orm';

// Must match the CHECK constraint on issue_relations.relation_type in
// packages/db-migrations/migrations/1721900000_add_issue_lifecycle_and_relations.sql.
const VALID_RELATION_TYPES = ['linked_to', 'caused_by', 'duplicate_of'] as const;
type RelationType = (typeof VALID_RELATION_TYPES)[number];

function isValidRelationType(value: unknown): value is RelationType {
	return typeof value === 'string' && (VALID_RELATION_TYPES as readonly string[]).includes(value);
}

// Postgres error codes surfaced by pg/postgres.js drivers on constraint violations.
// 23505 = unique_violation (issue_relations_unique -- an exact re-link).
// 23514 = check_violation (issue_relations_no_self_relation, added in
// 1722400000_add_issue_relations_no_self_check.sql -- belt-and-suspenders behind the
// application-level self-relation guard above).
function isUniqueViolation(err: unknown): boolean {
	return typeof err === 'object' && err !== null && (err as { code?: string }).code === '23505';
}

function isCheckViolation(err: unknown): boolean {
	return typeof err === 'object' && err !== null && (err as { code?: string }).code === '23514';
}

export const POST: RequestHandler = async ({ request, params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const userId = session.user.id;
	const sourceIssueId = params.issueId;

	const body = await request.json().catch(() => ({}));
	const { targetIssueId, relationType } = body;

	if (!targetIssueId) {
		throw error(400, 'targetIssueId is required');
	}

	if (!isValidRelationType(relationType)) {
		throw error(400, `relationType must be one of: ${VALID_RELATION_TYPES.join(', ')}`);
	}

	// D22: reject self-relations at the endpoint (also enforced by a DB CHECK constraint,
	// issue_relations_no_self_relation, as the last line of defense).
	if (sourceIssueId === targetIssueId) {
		throw error(400, 'An issue cannot be related to itself');
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

	// D10: this used to check ONLY that an `organization_members` row existed — no role check and
	// no project check. Org `viewer` is a valid role, so a viewer who could not resolve issues via
	// the batch endpoint could still create and delete `duplicate_of` relations here (and write
	// `issue_activity` rows) one at a time. Creating a relation mutates the source issue's graph,
	// so it needs `write` on the source; the target must at least be readable by the caller, or
	// linking becomes a way to confirm the existence of issues in projects they cannot see.
	await requireIssueAccess(userId, sourceIssueId, 'write');
	await requireIssueAccess(userId, targetIssueId, 'read');

	// D22: prevent duplicate_of 2-cycles (A duplicate_of B + B duplicate_of A). duplicate_of is read
	// semantically by the UI ("this issue is a duplicate of that one"), so a cycle is a real data
	// integrity problem, not just a redundant edge. Other relation types (linked_to, caused_by) are
	// symmetric-ish or directional-but-harmless enough that we don't block them here.
	if (relationType === 'duplicate_of') {
		const inverseRelation = await db
			.select()
			.from(issueRelations)
			.where(
				and(
					eq(issueRelations.sourceIssueId, targetIssueId),
					eq(issueRelations.targetIssueId, sourceIssueId),
					eq(issueRelations.relationType, 'duplicate_of')
				)
			);

		if (inverseRelation.length > 0) {
			throw error(400, 'This would create a duplicate_of cycle between these issues');
		}
	}

	// Create the relation and its issue_activity 'linked' entry together, transactionally.
	try {
		const relation = await createIssueRelation(sourceIssueId, targetIssueId, relationType, 'user', userId);
		return json(relation, { status: 201 });
	} catch (err: unknown) {
		// 23505 = Postgres unique_violation. Covers both an exact re-link (source, target, type
		// already exists via issue_relations_unique) and, for duplicate_of, the inverse-cycle check
		// above having raced -- surface both as 409 rather than an unhandled 500.
		if (isUniqueViolation(err)) {
			throw error(409, 'This relation already exists');
		}
		if (isCheckViolation(err)) {
			throw error(400, 'An issue cannot be related to itself');
		}
		throw err;
	}
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

	const body = await request.json().catch(() => ({}));
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

	// D10: same hole as POST above — a bare org-membership existence check let an org `viewer`
	// delete relations. Unlinking mutates the source issue's graph, so it needs `write`.
	await requireIssueAccess(userId, sourceIssueId, 'write');

	const deleted = await deleteIssueRelation(sourceIssueId, targetIssueId, relationType, 'user', userId);

	if (!deleted) {
		throw error(404, 'Relation not found');
	}

	return json({ success: true }, { status: 200 });
};
