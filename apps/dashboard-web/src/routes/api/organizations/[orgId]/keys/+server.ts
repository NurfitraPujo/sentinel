import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { getOrganizationApiKeys, createApiKey, toPublicKey } from '$lib/db/queries/apikeys';
import { getAgentById } from '$lib/db/queries/agents';
import { hasPermission } from '$lib/rbac';
import { requireOrgMembership, resolveProjectInOrg } from './_shared';

const VALID_SCOPES = ['ingest', 'read', 'admin', 'agent'] as const;

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

	// M5 §7/§9: agent-key issuance reuses this same machinery but is gated by 'manage_agents'
	// (owner/admin only), not 'manage_keys' (owner/admin/engineer) -- an engineer can create
	// ordinary ingest/read/admin keys but must not be able to mint an agent identity's
	// credential. Every other scope keeps the FR-007 gate unchanged.
	if (scope === 'agent') {
		if (!hasPermission(membership.role, 'manage_agents')) {
			throw error(403, 'Forbidden: only owners and admins can issue agent keys');
		}
		if (!body.agentId || typeof body.agentId !== 'string') {
			throw error(400, 'agentId is required when scope is "agent"');
		}
		const agentRow = await getAgentById(body.agentId);
		// Same S6-class scoping as everywhere else: an agentId belonging to another organization
		// must be indistinguishable from one that does not exist.
		if (!agentRow || agentRow.orgId !== orgId) {
			throw error(400, 'agentId not found in this organization');
		}
	} else if (!hasPermission(membership.role, 'manage_keys')) {
		// FR-007: only owner, admin, engineer may manage non-agent API keys.
		throw error(403, 'Forbidden: only owners, admins, and engineers can create API keys');
	}

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
	// project id/name must behave exactly like naming nothing that exists. Agent keys (§7) are
	// ALWAYS org-scoped -- an agent works across every project in the org -- so project resolution
	// is skipped entirely for scope='agent' even if the caller (incorrectly) sent one.
	let projectId: string | null = null;
	if (scope !== 'agent') {
		const requestedProject: string | undefined =
			body.projectId ?? (body.targetProject && body.targetProject !== 'All Projects [Org-Wide]' ? body.targetProject : undefined);

		if (requestedProject) {
			const resolved = await resolveProjectInOrg(orgId!, requestedProject);
			if (!resolved) {
				throw error(400, 'targetProject not found in this organization');
			}
			projectId = resolved;
		}
	}

	// N9 (docs/plans/AGENT_WORKER_PLAN.md, C6/C13): optional key lifetime. Applies to every scope
	// (org keys UI and the agent-provisioning path both flow through here). A positive integer
	// number of days; omitted mints a non-expiring key as before. Bounded to keep `now + days` well
	// within a representable Date and to reject obvious typos.
	const MAX_EXPIRES_IN_DAYS = 3650;
	if (body.expiresInDays !== undefined && body.expiresInDays !== null) {
		const days = body.expiresInDays;
		if (
			typeof days !== 'number' ||
			!Number.isFinite(days) ||
			!Number.isInteger(days) ||
			days <= 0 ||
			days > MAX_EXPIRES_IN_DAYS
		) {
			throw error(400, `expiresInDays must be a positive integer no greater than ${MAX_EXPIRES_IN_DAYS}`);
		}
	}

	const { apiKey, secretToken } = await createApiKey(session.user.id, {
		organizationId: orgId!,
		projectId,
		name: body.name,
		scope,
		rateLimitRpm: typeof body.rateLimitRpm === 'number' ? body.rateLimitRpm : undefined,
		agentId: scope === 'agent' ? body.agentId : undefined,
		expiresInDays: typeof body.expiresInDays === 'number' ? body.expiresInDays : undefined,
	});

	// The raw secret token is returned exactly once, here, and is never stored (only its SHA256
	// hash is persisted) — FR-002. `key` is passed through toPublicKey() so keyHash (the value
	// the ingestor's Redis cache is keyed on, apps/ingestor-go/auth/apikey.go:53) never reaches
	// the browser even if the query above regresses to a bare .returning().
	return json({ key: toPublicKey(apiKey), token: secretToken }, { status: 201 });
};
