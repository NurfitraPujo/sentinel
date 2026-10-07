import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { and, eq } from 'drizzle-orm';
import { db } from '$lib/server/db';
import { projectEnvironmentGrants } from '$lib/db/schema';
import { upsertEnvironmentGrant } from '$lib/db/queries/environments';
import {
	MANAGE_MEMBERS_FORBIDDEN,
	refuseOwner,
	requireProjectRoute,
	resolveGrantTarget,
	validateGrantEnvironments,
} from '../../../_shared';

// PUT / DELETE /api/organizations/[orgId]/projects/[projectId]/environment-grants/[userId]
// sentinel-project-environments §4.10 — what makes decision 12's boundary configurable by a person
// rather than by a psql session. A boundary nobody can set is not a boundary.

/**
 * §4.10's `PUT` — narrow, or re-narrow. Idempotent.
 *
 * **Self-narrowing is allowed and is not guarded against.** An admin may narrow themselves and can
 * widen themselves back, because `manage_members` is a project/org role and this endpoint does not
 * consult the caller's own grant. The org members route guards self-role-changes because a role
 * change can be irreversible; an environment grant is not, so the same guard would be ceremony.
 */
export const PUT: RequestHandler = async ({ params, request, locals }) => {
	const { userId: callerId, orgId, projectId } = await requireProjectRoute(
		locals,
		params,
		'manage_members',
		MANAGE_MEMBERS_FORBIDDEN
	);
	const targetUserId = params.userId!;

	const body = await request.json().catch(() => ({}) as Record<string, unknown>);

	const target = await resolveGrantTarget(orgId, projectId, targetUserId);
	if (!target.hasProjectAccess) {
		throw error(404, 'User has no access to this project');
	}
	// §2.2: the person who can undo a narrowing must never be the person narrowed out.
	refuseOwner(target.orgRole ?? undefined);

	// Every name must exist in this project's registry — every row of it, `hidden` ones included
	// (decision 49). An unknown name is a `400`, unlike §4.7's, because a grant naming an
	// environment that does not exist is a grant that does nothing and looks like it does something.
	const environments = await validateGrantEnvironments(projectId, body?.environments);

	const grant = await upsertEnvironmentGrant(db, {
		userId: targetUserId,
		projectId,
		environments,
		createdBy: callerId,
	});

	return json(grant);
};

/**
 * §4.10's `DELETE` — widen back to everything. Deleting the row restores unrestricted access, which
 * is what absence means (§2.2).
 *
 * `204` on a user who has no grant too: deleting nothing is the state the caller asked for. The
 * `404` above still applies — a caller must not learn from this endpoint whether a user they cannot
 * see exists.
 */
export const DELETE: RequestHandler = async ({ params, locals }) => {
	const { orgId, projectId } = await requireProjectRoute(
		locals,
		params,
		'manage_members',
		MANAGE_MEMBERS_FORBIDDEN
	);
	const targetUserId = params.userId!;

	const target = await resolveGrantTarget(orgId, projectId, targetUserId);
	if (!target.hasProjectAccess) {
		throw error(404, 'User has no access to this project');
	}

	await db
		.delete(projectEnvironmentGrants)
		.where(
			and(
				eq(projectEnvironmentGrants.userId, targetUserId),
				eq(projectEnvironmentGrants.projectId, projectId)
			)
		);

	return new Response(null, { status: 204 });
};
