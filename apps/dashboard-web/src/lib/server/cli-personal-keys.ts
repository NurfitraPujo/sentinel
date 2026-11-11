import { and, desc, eq, isNull, sql } from 'drizzle-orm';
import { db } from '$lib/server/db';
import { agents, auditLogs, organizations, projectApiKeys, users } from '$lib/db/schema';
import { generateApiKeySecret } from '$lib/db/queries/apikeys';

/**
 * sentinel-cli-browser-login (docs/plans/CLI_BROWSER_LOGIN.md): personal agents and their keys.
 *
 * A personal agent is an `agents` row of kind 'personal' with `owner_user_id` set -- ONE per (organization, user), shared
 * by every device of that user, so their claims and comments read as one agent ("<name> (CLI)"). A personal KEY is an
 * ordinary `scope='agent'` `project_api_keys` row pointing at that agent, whose `name` is the device name. What the key may
 * do is never stored here: `authenticateAgentRequest` resolves the owner's live access on every request.
 */

type Tx = Parameters<Parameters<typeof db.transaction>[0]>[0];
type Executor = typeof db | Tx;

export const PERSONAL_KEY_PREFIX = 'sent_agent_';
export const DEFAULT_PERSONAL_KEY_TTL_DAYS = 90;
const MAX_PERSONAL_KEY_TTL_DAYS = 365;

/**
 * Lifetime of a personal key in days: `CLI_PERSONAL_KEY_TTL_DAYS` (helm `config.dashboard.cliPersonalKeyTtlDays`), an
 * integer in 1..365, default 90. Anything else falls back to the default -- there is deliberately no way to configure
 * a non-expiring personal key.
 */
export function personalKeyTtlDays(raw: string | undefined = process.env.CLI_PERSONAL_KEY_TTL_DAYS): number {
	if (raw === undefined || raw.trim() === '') return DEFAULT_PERSONAL_KEY_TTL_DAYS;
	const n = Number(raw);
	if (!Number.isInteger(n) || n < 1 || n > MAX_PERSONAL_KEY_TTL_DAYS) return DEFAULT_PERSONAL_KEY_TTL_DAYS;
	return n;
}

/** "<user name> (CLI)" -- what the dashboard's agent-name lookups render for the person's claims and comments. */
export function personalAgentName(user: { name: string | null; email: string }): string {
	const base = (user.name ?? '').trim() || user.email;
	return `${base} (CLI)`.slice(0, 255);
}

export interface MintedPersonalKey {
	keyId: string;
	keyPrefix: string;
	secretToken: string;
	expiresAt: Date;
	agentId: string;
	replacedKeyIds: string[];
}

/**
 * Mints a personal key for (`userId`, `organizationId`, `deviceName`) INSIDE the caller's transaction:
 *
 *   1. get-or-create the personal agent with one `INSERT ... ON CONFLICT DO UPDATE` -- which also row-locks it, so two
 *      concurrent logins of the same user serialize here (the name is refreshed to the user's current name);
 *   2. revoke that agent's active keys with the same device name (re-login from a device replaces its key);
 *   3. insert the new key, expiring after `personalKeyTtlDays()`.
 *
 * Membership is NOT checked here; the caller has just checked it in the same transaction. Returns the raw secret ONCE --
 * the caller hands it to exactly one HTTP response and never logs or stores it.
 */
export async function mintPersonalKey(
	tx: Tx,
	params: { userId: string; organizationId: string; deviceName: string; actorUserId: string }
): Promise<MintedPersonalKey> {
	const [user] = await tx
		.select({ name: users.name, email: users.email })
		.from(users)
		.where(eq(users.id, params.userId));
	if (!user) {
		throw new Error('personal key owner not found');
	}

	const name = personalAgentName(user);
	const upserted = await tx.execute<{ id: string }>(sql`
		INSERT INTO agents (org_id, name, kind, status, created_by, owner_user_id)
		VALUES (${params.organizationId}, ${name}, 'personal', 'active', ${params.actorUserId}, ${params.userId})
		ON CONFLICT (org_id, owner_user_id) WHERE owner_user_id IS NOT NULL
		DO UPDATE SET name = EXCLUDED.name
		RETURNING id
	`);
	const agentId = (upserted as unknown as Array<{ id: string }>)[0].id;

	const now = new Date();
	const replaced = await tx
		.update(projectApiKeys)
		.set({ status: 'revoked', revokedAt: now, expiresAt: now })
		.where(
			and(
				eq(projectApiKeys.agentId, agentId),
				eq(projectApiKeys.name, params.deviceName),
				eq(projectApiKeys.status, 'active'),
				isNull(projectApiKeys.revokedAt)
			)
		)
		.returning({ id: projectApiKeys.id });

	const { secretToken, keyHash } = generateApiKeySecret(PERSONAL_KEY_PREFIX);
	const expiresAt = new Date(now.getTime() + personalKeyTtlDays() * 24 * 60 * 60 * 1000);
	const [key] = await tx
		.insert(projectApiKeys)
		.values({
			organizationId: params.organizationId,
			projectId: null,
			name: params.deviceName,
			keyPrefix: PERSONAL_KEY_PREFIX,
			keyHash,
			scope: 'agent',
			status: 'active',
			createdBy: params.actorUserId,
			expiresAt,
			agentId,
		})
		.returning({ id: projectApiKeys.id });

	return {
		keyId: key.id,
		keyPrefix: PERSONAL_KEY_PREFIX,
		secretToken,
		expiresAt,
		agentId,
		replacedKeyIds: replaced.map((r) => r.id),
	};
}

export interface PersonalKeyRow {
	keyId: string;
	deviceName: string;
	keyPrefix: string;
	status: string;
	createdAt: Date | null;
	expiresAt: Date | null;
	revokedAt: Date | null;
	agentId: string;
	organizationId: string;
	organizationName: string;
	organizationSlug: string;
	ownerUserId: string;
	ownerName: string | null;
	ownerEmail: string;
}

function personalKeysQuery(executor: Executor) {
	return executor
		.select({
			keyId: projectApiKeys.id,
			deviceName: projectApiKeys.name,
			keyPrefix: projectApiKeys.keyPrefix,
			status: projectApiKeys.status,
			createdAt: projectApiKeys.createdAt,
			expiresAt: projectApiKeys.expiresAt,
			revokedAt: projectApiKeys.revokedAt,
			agentId: agents.id,
			organizationId: organizations.id,
			organizationName: organizations.name,
			organizationSlug: organizations.slug,
			ownerUserId: sql<string>`${agents.ownerUserId}`,
			ownerName: users.name,
			ownerEmail: users.email,
		})
		.from(projectApiKeys)
		.innerJoin(agents, eq(agents.id, projectApiKeys.agentId))
		.innerJoin(organizations, eq(organizations.id, agents.orgId))
		.innerJoin(users, eq(users.id, agents.ownerUserId));
}

/** True while the key still authenticates (active, not revoked, not past expiry). */
export function isLivePersonalKey(row: Pick<PersonalKeyRow, 'status' | 'revokedAt' | 'expiresAt'>, now = Date.now()): boolean {
	return row.status === 'active' && !row.revokedAt && (!row.expiresAt || row.expiresAt.getTime() > now);
}

/** The signed-in user's own CLI sessions, across every organization, newest first (`/settings/cli`). */
export async function listPersonalKeysForUser(userId: string): Promise<PersonalKeyRow[]> {
	return await personalKeysQuery(db)
		.where(and(eq(agents.kind, 'personal'), eq(agents.ownerUserId, userId)))
		.orderBy(desc(projectApiKeys.createdAt));
}

/** Every personal key in one organization, with its owner (the Agents page, owners/admins only). */
export async function listPersonalKeysForOrg(organizationId: string): Promise<PersonalKeyRow[]> {
	return await personalKeysQuery(db)
		.where(and(eq(agents.kind, 'personal'), eq(agents.orgId, organizationId)))
		.orderBy(desc(projectApiKeys.createdAt));
}

/** One personal key with its owner and organization, or undefined when the id is not a personal key. */
export async function getPersonalKey(keyId: string): Promise<PersonalKeyRow | undefined> {
	const [row] = await personalKeysQuery(db).where(and(eq(agents.kind, 'personal'), eq(projectApiKeys.id, keyId)));
	return row;
}

export type PersonalKeyRevokeVia = 'settings' | 'agents_page' | 'cli_logout' | 'member_removed';

/**
 * Revokes ONE personal key (status 'revoked', revoked_at and expires_at = now, like `revokeApiKey`) and writes the
 * `cli.key.revoked` audit row. The caller has already decided the actor may do this:
 *   - `settings`: the key's owner (routes/settings/cli);
 *   - `agents_page`: an owner/admin of the key's organization;
 *   - `cli_logout`: the key itself (POST /api/agent/key/revoke).
 * Idempotent: revoking an already-revoked key changes nothing and returns false. Agent keys are never cached by the
 * ingestor (scope 'agent' is dashboard-only), so no invalidation publish is needed.
 */
export async function revokePersonalKey(params: {
	keyId: string;
	actorId: string;
	via: PersonalKeyRevokeVia;
	organizationId: string;
	ownerUserId: string;
}): Promise<boolean> {
	const now = new Date();
	const updated = await db
		.update(projectApiKeys)
		.set({ status: 'revoked', revokedAt: now, expiresAt: now })
		.where(and(eq(projectApiKeys.id, params.keyId), eq(projectApiKeys.status, 'active'), isNull(projectApiKeys.revokedAt)))
		.returning({ id: projectApiKeys.id });

	if (updated.length === 0) return false;

	await db.insert(auditLogs).values({
		action: 'cli.key.revoked',
		resourceType: 'api_key',
		resourceId: params.keyId,
		actorId: params.actorId,
		metadata: { via: params.via, organizationId: params.organizationId, ownerUserId: params.ownerUserId },
	});
	return true;
}

/**
 * Revokes every live personal key of `userId` in `organizationId`, inside the caller's member-removal transaction (D18:
 * one rollback for both). Authorization already refuses these keys the moment the membership row is gone -- that check
 * is LIVE -- so this is defense in depth with one concrete job: if the person is re-added later, their old laptop keys do
 * not come back to life with them. Audited per key as `cli.key.revoked` (via: member_removed). Returns the revoked ids.
 */
export async function revokePersonalKeysOfMember(
	tx: Executor,
	params: { organizationId: string; userId: string; actorId: string }
): Promise<string[]> {
	const now = new Date();
	const revoked = await tx
		.update(projectApiKeys)
		.set({ status: 'revoked', revokedAt: now, expiresAt: now })
		.where(
			and(
				eq(projectApiKeys.organizationId, params.organizationId),
				eq(projectApiKeys.status, 'active'),
				isNull(projectApiKeys.revokedAt),
				sql`${projectApiKeys.agentId} IN (SELECT id FROM agents WHERE kind = 'personal' AND org_id = ${params.organizationId} AND owner_user_id = ${params.userId})`
			)
		)
		.returning({ id: projectApiKeys.id });

	for (const { id } of revoked) {
		await tx.insert(auditLogs).values({
			action: 'cli.key.revoked',
			resourceType: 'api_key',
			resourceId: id,
			actorId: params.actorId,
			metadata: { via: 'member_removed', organizationId: params.organizationId, ownerUserId: params.userId },
		});
	}
	return revoked.map((r) => r.id);
}

export class PersonalKeyRotationError extends Error {}

export interface RotatedPersonalKey {
	oldKeyId: string;
	revokedAt: Date;
	newKeyId: string;
	keyPrefix: string;
	secretToken: string;
	expiresAt: Date;
}

/**
 * `sentinel key rotate` for a PERSONAL key, in one transaction (the org-agent path, `rotateAgentKeyWithGrace`, is
 * deliberately different on both points):
 *
 *   - **no grace period**: the calling key is revoked in the same transaction that inserts its successor, so a device
 *     never holds two live keys and `sentinel logout` after a rotate leaves nothing alive;
 *   - **no renewal**: the new key keeps the old key's EXACT `expires_at`. Only a fresh browser approval (`sentinel
 *     login`) restarts the clock -- a leaked key cannot rotate itself into a longer life.
 *
 * The old row is locked `FOR UPDATE` and must still be live, so two concurrent rotates of one key cannot both mint a
 * successor. Same agent, owner, organization and device name. Returns the raw secret ONCE.
 */
export async function rotatePersonalKey(oldKeyId: string): Promise<RotatedPersonalKey> {
	return await db.transaction(async (tx) => {
		const locked = await tx.execute<{
			id: string;
			organization_id: string;
			name: string;
			agent_id: string;
			created_by: string;
			rate_limit_rpm: number;
			expires_at: string | Date | null;
		}>(sql`
			SELECT k.id, k.organization_id, k.name, k.agent_id, k.created_by, k.rate_limit_rpm, k.expires_at
			FROM project_api_keys k
			JOIN agents a ON a.id = k.agent_id AND a.kind = 'personal'
			WHERE k.id = ${oldKeyId} AND k.status = 'active' AND k.revoked_at IS NULL
			  AND k.expires_at IS NOT NULL AND k.expires_at > now()
			FOR UPDATE OF k
		`);
		const old = (locked as unknown as Array<Record<string, unknown>>)[0];
		if (!old) {
			throw new PersonalKeyRotationError('This key is no longer active; run `sentinel login` again');
		}

		const now = new Date();
		await tx
			.update(projectApiKeys)
			.set({ status: 'revoked', revokedAt: now, expiresAt: now })
			.where(eq(projectApiKeys.id, oldKeyId));

		const expiresAt = new Date(old.expires_at as string | Date);
		const { secretToken, keyHash } = generateApiKeySecret(PERSONAL_KEY_PREFIX);
		const [key] = await tx
			.insert(projectApiKeys)
			.values({
				organizationId: old.organization_id as string,
				projectId: null,
				name: old.name as string,
				keyPrefix: PERSONAL_KEY_PREFIX,
				keyHash,
				scope: 'agent',
				status: 'active',
				createdBy: old.created_by as string,
				rateLimitRpm: old.rate_limit_rpm as number,
				expiresAt,
				agentId: old.agent_id as string,
			})
			.returning({ id: projectApiKeys.id });

		return { oldKeyId, revokedAt: now, newKeyId: key.id, keyPrefix: PERSONAL_KEY_PREFIX, secretToken, expiresAt };
	});
}

/**
 * The user's LIVE personal keys whose device name equals `deviceName`, one entry per organization (the newest): what
 * approving a login from a device of that name would replace. The approval page says so explicitly, because a phisher
 * who copies the victim's hostname otherwise looks like the victim's own re-login.
 */
export async function liveKeysReplacedByDevice(
	userId: string,
	deviceName: string
): Promise<Array<{ organizationId: string; createdAt: string | null }>> {
	const rows = await listPersonalKeysForUser(userId);
	const byOrg = new Map<string, { organizationId: string; createdAt: string | null }>();
	for (const row of rows) {
		if (row.deviceName !== deviceName || !isLivePersonalKey(row) || byOrg.has(row.organizationId)) continue;
		byOrg.set(row.organizationId, { organizationId: row.organizationId, createdAt: row.createdAt?.toISOString() ?? null });
	}
	return [...byOrg.values()];
}
