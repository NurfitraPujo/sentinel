import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import {
	listRepoCredentials,
	createRepoCredential,
	type RepoCredentialProvider,
} from '$lib/db/queries/repo-credentials';
import { EncryptionKeyUnavailableError } from '$lib/server/repo-credential-crypto';
import { parseRepoCredentialSecret } from '$lib/server/repo-credential-input';
import { hasPermission } from '$lib/rbac';
import { requireOrgMembership } from '../keys/_shared';
import { db } from '$lib/server/db';
import { projects } from '$lib/db/schema';
import { eq, and } from 'drizzle-orm';

/**
 * N10 part 2: org git-credentials management. WRITE-ONLY -- the response of every handler in
 * this tree is metadata (label + secretPrefix); the secret is never echoed back, not even in the
 * create response. Owner/admin (`manage_agents`) only, same gate as agent management.
 */

const VALID_PROVIDERS: RepoCredentialProvider[] = ['github', 'bitbucket'];

async function requireManageAgents(locals: App.Locals, orgId: string) {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}
	const membership = await requireOrgMembership(session.user.id, orgId);
	if (!membership) {
		throw error(403, 'Forbidden: not a member of this organization');
	}
	if (!hasPermission(membership.role, 'manage_agents')) {
		throw error(403, 'Forbidden: only owners and admins can manage repo credentials');
	}
	return session.user.id;
}

export const GET: RequestHandler = async ({ params, locals }) => {
	await requireManageAgents(locals, params.orgId!);
	const credentials = await listRepoCredentials(params.orgId!);
	return json({ credentials });
};

export const POST: RequestHandler = async ({ params, request, locals }) => {
	const userId = await requireManageAgents(locals, params.orgId!);

	const body = await request.json().catch(() => ({}) as any);
	const provider = body?.provider;
	if (!VALID_PROVIDERS.includes(provider)) {
		throw error(400, `provider is required and must be one of ${VALID_PROVIDERS.join(', ')}`);
	}
	const label = typeof body?.label === 'string' ? body.label.trim() : '';
	if (!label || label.length > 255) {
		throw error(400, 'label is required (max 255 chars)');
	}
	const secret = parseRepoCredentialSecret(provider, body);

	// 1724300000: optional project binding. Absent/null keeps the pre-migration org-wide shape.
	// The project must belong to this org — a DB trigger enforces it too, but a 400 here is a
	// better answer than a raised exception, and the trigger is the backstop rather than the gate.
	const rawProjectId = body?.projectId;
	if (rawProjectId !== undefined && rawProjectId !== null && typeof rawProjectId !== 'string') {
		throw error(400, 'projectId must be a string or null');
	}
	const projectId = typeof rawProjectId === 'string' && rawProjectId ? rawProjectId : null;
	if (projectId) {
		const [owned] = await db
			.select({ id: projects.id })
			.from(projects)
			.where(and(eq(projects.id, projectId), eq(projects.organizationId, params.orgId!)));
		if (!owned) {
			throw error(400, 'projectId must belong to this organization');
		}
	}

	try {
		const credential = await createRepoCredential(userId, {
			orgId: params.orgId!,
			provider,
			label,
			projectId,
			secret,
		});
		return json({ credential }, { status: 201 });
	} catch (e) {
		if (e instanceof EncryptionKeyUnavailableError) {
			// Fail closed and say why in operational (not secret) terms: the server will not
			// store what it cannot encrypt.
			throw error(503, 'Server encryption key is not configured; refusing to store credentials');
		}
		throw e;
	}
};
