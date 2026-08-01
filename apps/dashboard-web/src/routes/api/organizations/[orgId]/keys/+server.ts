import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { getOrganizationApiKeys, createApiKey, toPublicKey } from '$lib/db/queries/apikeys';
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
	return json({ keys: keys.map(toPublicKey) });
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

	// D28: an unrecognized scope used to be silently downgraded to 'ingest' and reported as a 201
	// success — the caller believed they created (e.g.) an 'admin' key and got an 'ingest' one with
	// no indication anything was wrong. Reject it instead. `scope` is optional (defaults to
	// 'ingest' when omitted entirely), but an explicit, unrecognized value is a client error.
	if (body.scope !== undefined && !VALID_SCOPES.includes(body.scope)) {
		throw error(400, `Invalid scope: must be one of ${VALID_SCOPES.join(', ')}`);
	}
	const scope = VALID_SCOPES.includes(body.scope) ? body.scope : 'ingest';

	// D28: rateLimitRpm accepted ANY number — negative, zero, or absurdly large (1e12) — and passed
	// it straight through to the ingestor's rate limiter. Require a positive, finite integer within
	// a sane operational bound.
	const MAX_RATE_LIMIT_RPM = 1_000_000;
	if (body.rateLimitRpm !== undefined) {
		const rpm = body.rateLimitRpm;
		if (
			typeof rpm !== 'number' ||
			!Number.isFinite(rpm) ||
			!Number.isInteger(rpm) ||
			rpm <= 0 ||
			rpm > MAX_RATE_LIMIT_RPM
		) {
			throw error(400, `rateLimitRpm must be a positive integer no greater than ${MAX_RATE_LIMIT_RPM}`);
		}
	}

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
	// hash is persisted) — FR-002. `key` is passed through toPublicKey() so keyHash (the value
	// the ingestor's Redis cache is keyed on, apps/ingestor-go/auth/apikey.go:53) never reaches
	// the browser even if the query above regresses to a bare .returning().
	return json({ key: toPublicKey(apiKey), token: secretToken }, { status: 201 });
};
