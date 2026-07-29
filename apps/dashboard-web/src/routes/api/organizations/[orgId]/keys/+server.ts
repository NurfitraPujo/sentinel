import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { getOrganizationApiKeys, createApiKey } from '$lib/db/queries/apikeys';
import { hasPermission } from '$lib/rbac';
import { requireOrgMembership, resolveProjectInOrg } from './_shared';

const VALID_SCOPES = ['ingest', 'read', 'admin'] as const;

export const GET: RequestHandler = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { orgId } = params;
	const membership = await requireOrgMembership(session.user.id, orgId!);
	if (!membership) {
		throw error(403, 'Forbidden: not a member of this organization');
	}
	if (!hasPermission(membership.role, 'read')) {
		throw error(403, 'Forbidden: insufficient permissions');
	}

	const keys = await getOrganizationApiKeys(orgId!);
	return json({ keys });
};

export const POST: RequestHandler = async ({ params, request, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { orgId } = params;
	const membership = await requireOrgMembership(session.user.id, orgId!);
	if (!membership) {
		throw error(403, 'Forbidden: not a member of this organization');
	}
	// FR-007: only owner, admin, engineer may manage API keys.
	if (!hasPermission(membership.role, 'manage_keys')) {
		throw error(403, 'Forbidden: only owners, admins, and engineers can create API keys');
	}

	const body = await request.json().catch(() => ({}) as any);
	if (!body?.name || typeof body.name !== 'string') {
		throw error(400, 'name is required');
	}

	const scope = VALID_SCOPES.includes(body.scope) ? body.scope : 'ingest';

	// targetProject/projectId identifies a project WITHIN this organization; absent (or the
	// "All Projects [Org-Wide]" sentinel the UI uses) means an organization-wide key. Whatever the
	// caller names is resolved scoped to THIS org (never globally) — naming another organization's
	// project id/name must behave exactly like naming nothing that exists.
	const requestedProject: string | undefined =
		body.projectId ?? (body.targetProject && body.targetProject !== 'All Projects [Org-Wide]' ? body.targetProject : undefined);

	let projectId: string | null = null;
	if (requestedProject) {
		const resolved = await resolveProjectInOrg(orgId!, requestedProject);
		if (!resolved) {
			throw error(400, 'targetProject not found in this organization');
		}
		projectId = resolved;
	}

	const { apiKey, secretToken } = await createApiKey(session.user.id, {
		organizationId: orgId!,
		projectId,
		name: body.name,
		scope,
		rateLimitRpm: typeof body.rateLimitRpm === 'number' ? body.rateLimitRpm : undefined,
	});

	// The raw secret token is returned exactly once, here, and is never stored (only its SHA256
	// hash is persisted) — FR-002.
	return json({ key: apiKey, token: secretToken }, { status: 201 });
};
