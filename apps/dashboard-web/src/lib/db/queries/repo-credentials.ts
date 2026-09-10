import { db } from '$lib/server/db';
import { repoCredentials, auditLogs, agentRepoCredentialGrants } from '$lib/db/schema';
import { eq, and, or, desc, isNull, inArray } from 'drizzle-orm';
import {
	encryptRepoCredentialSecret,
	decryptRepoCredentialSecret,
	type RepoCredentialSecret,
} from '$lib/server/repo-credential-crypto';

/**
 * N10 part 2 (docs/plans/AGENT_WORKER_PLAN.md §4.5): org-scoped git credentials. Every mutation
 * writes an audit_logs row (same convention as agents.ts). Tenant scope (`orgId`) MUST always
 * come from the caller's authenticated membership or agent credential, never from the request
 * body (B7).
 *
 * INVARIANT: no function in this module ever returns, logs, or audits the plaintext secret. The
 * only decrypting caller is fetchDecryptedCredentialsForAgent, whose result goes straight to the
 * flag-gated agent delivery endpoint's response body. List/get shapes for the dashboard are
 * metadata-only (label + secretPrefix) -- the write-only UI guarantee lives here, not in the UI.
 */

export type RepoCredentialProvider = 'github' | 'bitbucket';

export interface RepoCredentialListItem {
	id: string;
	organizationId: string;
	provider: RepoCredentialProvider;
	label: string;
	secretPrefix: string;
	/** null = org-wide. */
	projectId: string | null;
	status: 'active' | 'revoked';
	createdBy: string;
	createdAt: Date | null;
	revokedAt: Date | null;
	lastFetchedAt: Date | null;
}

// The metadata-only projection. Deliberately a column allowlist rather than select() + omit:
// encrypted_secret/nonce must be unselectable by construction from any dashboard-facing path.
const listColumns = {
	id: repoCredentials.id,
	organizationId: repoCredentials.organizationId,
	provider: repoCredentials.provider,
	label: repoCredentials.label,
	secretPrefix: repoCredentials.secretPrefix,
	projectId: repoCredentials.projectId,
	status: repoCredentials.status,
	createdBy: repoCredentials.createdBy,
	createdAt: repoCredentials.createdAt,
	revokedAt: repoCredentials.revokedAt,
	lastFetchedAt: repoCredentials.lastFetchedAt,
};

/** Display fragment for the write-only UI: token prefix, or the bitbucket username. */
export function secretPrefixFor(secret: RepoCredentialSecret): string {
	if ('token' in secret) {
		return secret.token.slice(0, 8);
	}
	return secret.username.slice(0, 16);
}

export async function listRepoCredentials(orgId: string): Promise<RepoCredentialListItem[]> {
	const rows = await db
		.select(listColumns)
		.from(repoCredentials)
		.where(eq(repoCredentials.organizationId, orgId))
		.orderBy(desc(repoCredentials.createdAt));
	return rows as RepoCredentialListItem[];
}

export async function createRepoCredential(
	actorUserId: string,
	data: {
		orgId: string;
		provider: RepoCredentialProvider;
		label: string;
		secret: RepoCredentialSecret;
		/** Bind the credential to one project. Omitted/null = org-wide, the pre-1724300000 shape. */
		projectId?: string | null;
	}
): Promise<RepoCredentialListItem> {
	const enc = encryptRepoCredentialSecret(data.orgId, data.secret);
	const [row] = await db
		.insert(repoCredentials)
		.values({
			organizationId: data.orgId,
			provider: data.provider,
			label: data.label,
			secretPrefix: secretPrefixFor(data.secret),
			encryptedSecret: enc.encryptedSecret,
			nonce: enc.nonce,
			keyVersion: enc.keyVersion,
			status: 'active',
			createdBy: actorUserId,
			projectId: data.projectId ?? null,
		})
		.returning(listColumns);

	await db.insert(auditLogs).values({
		action: 'repo_credential.created',
		resourceType: 'repo_credential',
		resourceId: row.id,
		actorId: actorUserId,
		metadata: {
			orgId: data.orgId,
			provider: data.provider,
			label: data.label,
			// Recorded because it is the scope decision — an auditor reading this line needs to know
			// whether the credential was created org-wide or bound to one project.
			projectId: data.projectId ?? null,
		},
	});

	return row as RepoCredentialListItem;
}

/**
 * Replace the secret in place (rotate a token without changing the credential id that repo
 * connections reference). Only active credentials can be replaced -- a revoked credential's
 * ciphertext was destroyed and the row is an audit tombstone.
 */
export async function replaceRepoCredentialSecret(
	actorUserId: string,
	orgId: string,
	credentialId: string,
	secret: RepoCredentialSecret
): Promise<RepoCredentialListItem> {
	const enc = encryptRepoCredentialSecret(orgId, secret);
	const [row] = await db
		.update(repoCredentials)
		.set({
			secretPrefix: secretPrefixFor(secret),
			encryptedSecret: enc.encryptedSecret,
			nonce: enc.nonce,
			keyVersion: enc.keyVersion,
		})
		.where(
			and(
				eq(repoCredentials.id, credentialId),
				eq(repoCredentials.organizationId, orgId),
				eq(repoCredentials.status, 'active')
			)
		)
		.returning(listColumns);

	if (!row) {
		throw new Error('Credential not found');
	}

	await db.insert(auditLogs).values({
		action: 'repo_credential.replaced',
		resourceType: 'repo_credential',
		resourceId: credentialId,
		actorId: actorUserId,
		metadata: { orgId },
	});

	return row as RepoCredentialListItem;
}

/**
 * Revoke: stop serving AND destroy the ciphertext (overwrite with ''), keeping the row as an
 * audit tombstone. Crypto-shredding lite -- even a later master-key compromise cannot recover a
 * revoked credential from a backup of the current row.
 */
export async function revokeRepoCredential(
	actorUserId: string,
	orgId: string,
	credentialId: string
): Promise<RepoCredentialListItem> {
	const [row] = await db
		.update(repoCredentials)
		.set({
			status: 'revoked',
			revokedAt: new Date(),
			encryptedSecret: '',
			nonce: '',
		})
		.where(
			and(
				eq(repoCredentials.id, credentialId),
				eq(repoCredentials.organizationId, orgId),
				eq(repoCredentials.status, 'active')
			)
		)
		.returning(listColumns);

	if (!row) {
		throw new Error('Credential not found');
	}

	await db.insert(auditLogs).values({
		action: 'repo_credential.revoked',
		resourceType: 'repo_credential',
		resourceId: credentialId,
		actorId: actorUserId,
		metadata: { orgId },
	});

	return row as RepoCredentialListItem;
}

export interface DecryptedRepoCredential {
	id: string;
	provider: RepoCredentialProvider;
	label: string;
	secret: RepoCredentialSecret;
}

/**
 * The ONLY decrypting read path. Caller (GET /api/agent/repo-credentials) is responsible for the
 * can_access_repo_credentials master switch and for auditing each served credential id. Serves
 * active rows only; stamps last_fetched_at.
 *
 * SCOPE (1724300000_add_repo_credential_project_scope.sql). This used to return every active
 * credential in the organization, so an agent working on one project received the git tokens for
 * all of them. It now returns:
 *
 *   - org-wide credentials (`project_id IS NULL`) — unchanged behaviour, and what every row
 *     created before the migration is, so no deployed worker loses access;
 *   - project-scoped credentials ONLY for projects this agent holds a grant for.
 *
 * `agentId` is taken from the authenticated agent context, never from the request — the grant set
 * is read here rather than passed in, so no caller can widen its own scope.
 */
export async function fetchDecryptedCredentialsForAgent(
	orgId: string,
	agentId: string
): Promise<DecryptedRepoCredential[]> {
	const grants = await db
		.select({ projectId: agentRepoCredentialGrants.projectId })
		.from(agentRepoCredentialGrants)
		.where(eq(agentRepoCredentialGrants.agentId, agentId));
	const grantedProjectIds = grants.map((g) => g.projectId);

	// `inArray` with an empty list is not a safe way to express "match nothing" across drizzle
	// versions, so an agent with no grants is expressed as the org-wide clause alone.
	const scope =
		grantedProjectIds.length > 0
			? or(
					isNull(repoCredentials.projectId),
					inArray(repoCredentials.projectId, grantedProjectIds)
				)
			: isNull(repoCredentials.projectId);

	const rows = await db
		.select()
		.from(repoCredentials)
		.where(
			and(
				eq(repoCredentials.organizationId, orgId),
				eq(repoCredentials.status, 'active'),
				scope
			)
		)
		.orderBy(desc(repoCredentials.createdAt));

	const now = new Date();
	const out: DecryptedRepoCredential[] = [];
	for (const row of rows) {
		const secret = decryptRepoCredentialSecret(orgId, {
			encryptedSecret: row.encryptedSecret,
			nonce: row.nonce,
			keyVersion: row.keyVersion,
		});
		await db
			.update(repoCredentials)
			.set({ lastFetchedAt: now })
			.where(eq(repoCredentials.id, row.id));
		out.push({
			id: row.id,
			provider: row.provider as RepoCredentialProvider,
			label: row.label,
			secret,
		});
	}
	return out;
}

/**
 * 1724300000: an agent's project grants — the scope for project-bound credentials.
 * `agents.can_access_repo_credentials` remains the outer master switch; these say WHICH projects.
 */
export async function listAgentCredentialGrants(agentId: string): Promise<string[]> {
	const rows = await db
		.select({ projectId: agentRepoCredentialGrants.projectId })
		.from(agentRepoCredentialGrants)
		.where(eq(agentRepoCredentialGrants.agentId, agentId));
	return rows.map((r) => r.projectId);
}

/**
 * Replaces an agent's grants wholesale. A set-semantics write rather than add/remove verbs: the UI
 * edits a checklist, and a partial apply would leave an agent holding a scope nobody chose.
 *
 * Callers MUST have already verified that `agentId` and every id in `projectIds` belong to `orgId`
 * — this function does not re-derive org membership, and a project id from another tenant written
 * here would be a cross-tenant grant.
 */
export async function replaceAgentCredentialGrants(
	actorUserId: string,
	agentId: string,
	projectIds: string[]
): Promise<string[]> {
	const unique = [...new Set(projectIds)];

	await db.transaction(async (tx) => {
		await tx.delete(agentRepoCredentialGrants).where(eq(agentRepoCredentialGrants.agentId, agentId));
		if (unique.length > 0) {
			await tx.insert(agentRepoCredentialGrants).values(
				unique.map((projectId) => ({ agentId, projectId, createdBy: actorUserId }))
			);
		}
	});

	await db.insert(auditLogs).values({
		action: 'agent.repo_credential_grants_replaced',
		resourceType: 'agent',
		resourceId: agentId,
		actorId: actorUserId,
		metadata: { projectIds: unique },
	});

	return unique;
}
