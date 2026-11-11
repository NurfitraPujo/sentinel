import { error } from '@sveltejs/kit';
import type { RepoCredentialSecret } from '$lib/server/repo-credential-crypto';
import type { RepoCredentialProvider } from '$lib/db/queries/repo-credentials';

/**
 * N10 part 2: request-body → secret shape validation, shared by the create and replace routes.
 * Lives in $lib/server (not exported from a +server.ts) because of SvelteKit's route-export
 * allowlist -- `pnpm build` is the only gate that enforces it (B12).
 *
 * Error messages NEVER include secret material -- only which field is missing/conflicting.
 * github: token only. bitbucket: access token OR username+appPassword pair.
 */
export function parseRepoCredentialSecret(
	provider: RepoCredentialProvider,
	body: unknown
): RepoCredentialSecret {
	const b = (body ?? {}) as Record<string, unknown>;
	const token = typeof b.token === 'string' ? b.token.trim() : '';
	const username = typeof b.username === 'string' ? b.username.trim() : '';
	const appPassword = typeof b.appPassword === 'string' ? b.appPassword.trim() : '';

	if (provider === 'github') {
		if (!token) throw error(400, 'token is required for github credentials');
		if (username || appPassword) {
			throw error(400, 'github credentials take a token only');
		}
		return { token };
	}
	if (token) {
		if (username || appPassword) {
			throw error(400, 'provide either token or username+appPassword, not both');
		}
		return { token };
	}
	if (!username || !appPassword) {
		throw error(400, 'bitbucket credentials require token, or username and appPassword');
	}
	return { username, appPassword };
}
