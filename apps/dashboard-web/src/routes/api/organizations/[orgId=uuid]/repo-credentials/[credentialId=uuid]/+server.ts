import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import {
	replaceRepoCredentialSecret,
	revokeRepoCredential,
} from '$lib/db/queries/repo-credentials';
import { EncryptionKeyUnavailableError } from '$lib/server/repo-credential-crypto';
import { parseRepoCredentialSecret } from '$lib/server/repo-credential-input';
import { hasPermission } from '$lib/rbac';
import { requireOrgMembership } from '../../keys/_shared';

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

// PUT: replace the secret in place (rotation). The credential id -- which repo connections
// reference -- is stable; the response is metadata only, never the secret.
export const PUT: RequestHandler = async ({ params, request, locals }) => {
	const userId = await requireManageAgents(locals, params.orgId!);

	const body = await request.json().catch(() => ({}) as any);
	const provider = body?.provider;
	if (provider !== 'github' && provider !== 'bitbucket') {
		throw error(400, 'provider is required and must be one of github, bitbucket');
	}
	const secret = parseRepoCredentialSecret(provider, body);

	try {
		const credential = await replaceRepoCredentialSecret(
			userId,
			params.orgId!,
			params.credentialId!,
			secret
		);
		return json({ credential });
	} catch (e) {
		if (e instanceof EncryptionKeyUnavailableError) {
			throw error(503, 'Server encryption key is not configured; refusing to store credentials');
		}
		if (e instanceof Error && e.message === 'Credential not found') {
			// Cross-org ids 404 exactly like nonexistent ones (S6's class of bug).
			throw error(404, 'Credential not found');
		}
		throw e;
	}
};

// DELETE: revoke -- stops delivery and destroys the ciphertext; the row stays as an audit
// tombstone, so this is not a hard delete.
export const DELETE: RequestHandler = async ({ params, locals }) => {
	const userId = await requireManageAgents(locals, params.orgId!);
	try {
		const credential = await revokeRepoCredential(userId, params.orgId!, params.credentialId!);
		return json({ credential });
	} catch (e) {
		if (e instanceof Error && e.message === 'Credential not found') {
			throw error(404, 'Credential not found');
		}
		throw e;
	}
};
