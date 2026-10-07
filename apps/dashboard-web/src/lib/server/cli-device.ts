import crypto from 'node:crypto';
import { and, desc, eq } from 'drizzle-orm';
import { db } from '$lib/server/db';
import { auditLogs, cliDeviceAuthorizations, organizationMembers, organizations, users } from '$lib/db/schema';
import { mintPersonalKey } from '$lib/server/cli-personal-keys';

/**
 * sentinel-cli-browser-login (docs/plans/CLI_BROWSER_LOGIN.md): the RFC 8628 device authorization flow behind
 * `sentinel login`.
 *
 *   POST /api/cli/device         -> startDeviceAuthorization   (unauthenticated; rate-limited by the route)
 *   GET/POST /cli/approve         -> findDeviceAuthorization / decideDeviceAuthorization   (session-authenticated)
 *   POST /api/cli/device/token   -> pollDeviceToken             (unauthenticated; the device_code is the credential)
 *
 * Secrets at rest: the device_code is stored only as sha256 hex; the key is minted INSIDE the transaction that marks the
 * row delivered, so neither the device_code nor the key is ever readable from the database.
 */

export const DEVICE_CODE_TTL_SECONDS = 600;
export const DEVICE_POLL_INTERVAL_SECONDS = 5;
/** RFC 8628 §3.5: on `slow_down` the client adds 5 s; the server tracks the same and caps it. */
const SLOW_DOWN_STEP_SECONDS = 5;
const MAX_POLL_INTERVAL_SECONDS = 60;
/** Clock/network jitter forgiven before a poll counts as "too fast". */
const POLL_JITTER_MS = 1000;

/** RFC 8628 §6.1's example alphabet: 20 consonants, no vowels (no words), no digits or look-alikes. 20^8 ≈ 2^34.6. */
export const USER_CODE_ALPHABET = 'BCDFGHJKLMNPQRSTVWXZ';
const USER_CODE_LENGTH = 8;

export function generateUserCode(): string {
	let out = '';
	for (let i = 0; i < USER_CODE_LENGTH; i++) {
		out += USER_CODE_ALPHABET[crypto.randomInt(USER_CODE_ALPHABET.length)];
	}
	return out;
}

/** `BCDFGHJK` -> `BCDF-GHJK`, the form shown to people. */
export function formatUserCode(code: string): string {
	return `${code.slice(0, 4)}-${code.slice(4)}`;
}

/**
 * What a person typed (any case, with or without the dash or spaces) -> the stored 8-letter form, or null when it cannot
 * be a code. Rejecting early means a malformed input never reaches the database or the per-user lookup budget.
 */
export function normalizeUserCode(input: unknown): string | null {
	if (typeof input !== 'string' || input.length > 32) return null;
	const code = input.toUpperCase().replace(/[\s-]/g, '');
	if (code.length !== USER_CODE_LENGTH) return null;
	for (const ch of code) {
		if (!USER_CODE_ALPHABET.includes(ch)) return null;
	}
	return code;
}

/** 32 random bytes, base64url: 256 bits, so guessing a live device_code is infeasible. */
export function generateDeviceCode(): string {
	return crypto.randomBytes(32).toString('base64url');
}

export function hashDeviceCode(code: string): string {
	return crypto.createHash('sha256').update(code).digest('hex');
}

/** Printable single-line text: control characters removed, whitespace collapsed, clamped to `max`. */
export function cleanText(value: unknown, max: number): string {
	if (typeof value !== 'string') return '';
	// eslint-disable-next-line no-control-regex
	return value.replace(/[\u0000-\u001f\u007f-\u009f\u2028\u2029]/g, ' ').replace(/\s+/g, ' ').trim().slice(0, max);
}

/** The CLI's own profile-name rule (tools/sentinel/config.go `profileNameRE`); anything else is dropped. */
export function cleanProfile(value: unknown): string {
	return typeof value === 'string' && /^[a-z0-9][a-z0-9_-]{0,31}$/.test(value) ? value : '';
}

export interface StartDeviceInput {
	client: string;
	clientVersion: string;
	deviceName: string;
	profile: string;
	ip: string;
	userAgent: string;
}

export interface StartedDevice {
	deviceCode: string;
	userCode: string;
	expiresIn: number;
	interval: number;
}

function isUniqueViolation(err: unknown): boolean {
	return typeof err === 'object' && err !== null && (err as { code?: string }).code === '23505';
}

/** Creates a pending device authorization. A user_code collision with another PENDING row is retried with a new code. */
export async function startDeviceAuthorization(input: StartDeviceInput): Promise<StartedDevice> {
	for (let attempt = 0; attempt < 5; attempt++) {
		const deviceCode = generateDeviceCode();
		const userCode = generateUserCode();
		try {
			await db.insert(cliDeviceAuthorizations).values({
				deviceCodeHash: hashDeviceCode(deviceCode),
				userCode,
				status: 'pending',
				client: input.client,
				clientVersion: input.clientVersion,
				deviceName: input.deviceName,
				requestedProfile: input.profile,
				requestedIp: input.ip,
				requestedUserAgent: input.userAgent,
				expiresAt: new Date(Date.now() + DEVICE_CODE_TTL_SECONDS * 1000),
				pollIntervalSeconds: DEVICE_POLL_INTERVAL_SECONDS,
			});
			return { deviceCode, userCode, expiresIn: DEVICE_CODE_TTL_SECONDS, interval: DEVICE_POLL_INTERVAL_SECONDS };
		} catch (err) {
			if (isUniqueViolation(err)) continue;
			throw err;
		}
	}
	throw new Error('could not allocate a unique user code');
}

export type DeviceAuthorizationState = 'pending' | 'approved' | 'denied' | 'delivered' | 'expired';

export interface DeviceAuthorizationView {
	id: string;
	userCode: string;
	state: DeviceAuthorizationState;
	client: string;
	clientVersion: string;
	deviceName: string;
	requestedProfile: string;
	requestedIp: string;
	createdAt: Date;
	expiresAt: Date;
}

function stateOf(row: { status: string; expiresAt: Date }, now = Date.now()): DeviceAuthorizationState {
	if ((row.status === 'pending' || row.status === 'approved') && row.expiresAt.getTime() <= now) return 'expired';
	return row.status as DeviceAuthorizationState;
}

/**
 * The approval page's lookup: the PENDING request with this code if there is one, else the most recent row that used it
 * (so the page can say "already used" / "denied" / "expired" instead of a bare "not found"). Null when no row ever had it.
 */
export async function findDeviceAuthorization(userCode: string): Promise<DeviceAuthorizationView | null> {
	const [row] = await db
		.select()
		.from(cliDeviceAuthorizations)
		.where(eq(cliDeviceAuthorizations.userCode, userCode))
		.orderBy(desc(eq(cliDeviceAuthorizations.status, 'pending')), desc(cliDeviceAuthorizations.createdAt))
		.limit(1);
	if (!row) return null;
	return {
		id: row.id,
		userCode: row.userCode,
		state: stateOf(row),
		client: row.client,
		clientVersion: row.clientVersion,
		deviceName: row.deviceName,
		requestedProfile: row.requestedProfile,
		requestedIp: row.requestedIp,
		createdAt: row.createdAt,
		expiresAt: row.expiresAt,
	};
}

export type DecideResult =
	| { ok: true; state: 'approved' | 'denied' }
	| { ok: false; reason: 'not_found' | 'expired' | 'already_decided' | 'not_a_member' };

/**
 * Approves or denies the PENDING request with `userCode`, as `userId`. Approval binds the request to `organizationId`,
 * which must be an organization `userId` is a member of RIGHT NOW (re-checked in the transaction, never taken from the
 * page's earlier render). Single use: only a pending, unexpired row can be decided, and the row is locked while it is.
 * Writes `cli.device.approved` / `cli.device.denied` to audit_logs.
 */
export async function decideDeviceAuthorization(params: {
	userCode: string;
	userId: string;
	organizationId: string | null;
	decision: 'approve' | 'deny';
	ip: string;
}): Promise<DecideResult> {
	return await db.transaction(async (tx) => {
		const [row] = await tx
			.select()
			.from(cliDeviceAuthorizations)
			.where(and(eq(cliDeviceAuthorizations.userCode, params.userCode), eq(cliDeviceAuthorizations.status, 'pending')))
			.for('update');

		if (!row) {
			const [any] = await tx
				.select({ id: cliDeviceAuthorizations.id })
				.from(cliDeviceAuthorizations)
				.where(eq(cliDeviceAuthorizations.userCode, params.userCode))
				.limit(1);
			return { ok: false as const, reason: any ? ('already_decided' as const) : ('not_found' as const) };
		}
		if (row.expiresAt.getTime() <= Date.now()) {
			return { ok: false as const, reason: 'expired' as const };
		}

		let organizationId: string | null = null;
		if (params.decision === 'approve') {
			if (!params.organizationId) return { ok: false as const, reason: 'not_a_member' as const };
			const [member] = await tx
				.select({ role: organizationMembers.role })
				.from(organizationMembers)
				.where(
					and(eq(organizationMembers.userId, params.userId), eq(organizationMembers.organizationId, params.organizationId))
				);
			if (!member) return { ok: false as const, reason: 'not_a_member' as const };
			organizationId = params.organizationId;
		}

		const status = params.decision === 'approve' ? 'approved' : 'denied';
		await tx
			.update(cliDeviceAuthorizations)
			.set({ status, approvedByUserId: params.userId, organizationId, decidedAt: new Date() })
			.where(eq(cliDeviceAuthorizations.id, row.id));

		await tx.insert(auditLogs).values({
			action: status === 'approved' ? 'cli.device.approved' : 'cli.device.denied',
			resourceType: 'cli_device_authorization',
			resourceId: row.id,
			actorId: params.userId,
			metadata: {
				organizationId: organizationId ?? undefined,
				deviceName: row.deviceName,
				client: row.client,
				clientVersion: row.clientVersion,
				requestedIp: row.requestedIp,
				approverIp: params.ip,
			},
		});

		return { ok: true as const, state: status as 'approved' | 'denied' };
	});
}

export type PollResult =
	| { kind: 'error'; error: 'invalid_grant' | 'authorization_pending' | 'slow_down' | 'access_denied' | 'expired_token'; interval?: number }
	| {
			kind: 'token';
			key: string;
			keyPrefix: string;
			keyId: string;
			expiresAt: Date;
			organizationId: string;
			orgSlug: string;
			owner: { name: string | null; email: string; role: string };
	  };

/**
 * One poll of the token endpoint (RFC 8628 §3.4/§3.5). Runs in ONE transaction holding the row lock, so concurrent polls
 * with the same device_code serialize and at most one of them can ever deliver a key:
 *
 *   unknown code -> invalid_grant          denied -> access_denied          delivered (a replay) -> invalid_grant
 *   past expires_at -> expired_token       faster than the interval -> slow_down (interval += 5 s, capped)
 *   pending -> authorization_pending       approved -> mint the key, mark the row delivered, return the key once
 *
 * The approver's membership is re-checked at delivery: approval and delivery are seconds apart, but nothing should be
 * minted for someone who was removed in between (the row becomes 'denied').
 *
 * Every branch returns rather than throws so the poll bookkeeping (last_polled_at, interval) COMMITS -- none of these is
 * a failure to roll back (D18 is about failures; these are answers).
 */
export async function pollDeviceToken(deviceCode: string, ip: string): Promise<PollResult> {
	if (typeof deviceCode !== 'string' || deviceCode.length < 20 || deviceCode.length > 128) {
		return { kind: 'error', error: 'invalid_grant' };
	}
	const hash = hashDeviceCode(deviceCode);

	return await db.transaction(async (tx): Promise<PollResult> => {
		const [row] = await tx
			.select()
			.from(cliDeviceAuthorizations)
			.where(eq(cliDeviceAuthorizations.deviceCodeHash, hash))
			.for('update');

		// The unique-index lookup already matched on the hash; compare again in constant time so the decision never rests
		// on a non-constant-time string comparison anywhere in the stack.
		if (!row || !crypto.timingSafeEqual(Buffer.from(row.deviceCodeHash, 'hex'), Buffer.from(hash, 'hex'))) {
			return { kind: 'error', error: 'invalid_grant' };
		}
		if (row.status === 'delivered') return { kind: 'error', error: 'invalid_grant' };
		if (row.status === 'denied') return { kind: 'error', error: 'access_denied' };

		const now = new Date();
		if (row.expiresAt.getTime() <= now.getTime()) return { kind: 'error', error: 'expired_token' };

		if (row.lastPolledAt && now.getTime() - row.lastPolledAt.getTime() < row.pollIntervalSeconds * 1000 - POLL_JITTER_MS) {
			const interval = Math.min(row.pollIntervalSeconds + SLOW_DOWN_STEP_SECONDS, MAX_POLL_INTERVAL_SECONDS);
			await tx
				.update(cliDeviceAuthorizations)
				.set({ lastPolledAt: now, pollIntervalSeconds: interval })
				.where(eq(cliDeviceAuthorizations.id, row.id));
			return { kind: 'error', error: 'slow_down', interval };
		}

		await tx.update(cliDeviceAuthorizations).set({ lastPolledAt: now }).where(eq(cliDeviceAuthorizations.id, row.id));

		if (row.status === 'pending') return { kind: 'error', error: 'authorization_pending' };

		// status === 'approved'
		if (!row.approvedByUserId || !row.organizationId) {
			return { kind: 'error', error: 'access_denied' };
		}
		const [member] = await tx
			.select({ role: organizationMembers.role, email: users.email, name: users.name, slug: organizations.slug })
			.from(organizationMembers)
			.innerJoin(users, eq(users.id, organizationMembers.userId))
			.innerJoin(organizations, eq(organizations.id, organizationMembers.organizationId))
			.where(
				and(
					eq(organizationMembers.userId, row.approvedByUserId),
					eq(organizationMembers.organizationId, row.organizationId)
				)
			);
		if (!member) {
			await tx
				.update(cliDeviceAuthorizations)
				.set({ status: 'denied' })
				.where(eq(cliDeviceAuthorizations.id, row.id));
			return { kind: 'error', error: 'access_denied' };
		}

		const minted = await mintPersonalKey(tx, {
			userId: row.approvedByUserId,
			organizationId: row.organizationId,
			deviceName: row.deviceName,
			actorUserId: row.approvedByUserId,
		});

		await tx
			.update(cliDeviceAuthorizations)
			.set({ status: 'delivered', deliveredAt: now, apiKeyId: minted.keyId })
			.where(eq(cliDeviceAuthorizations.id, row.id));

		await tx.insert(auditLogs).values({
			action: 'cli.key.minted',
			resourceType: 'api_key',
			resourceId: minted.keyId,
			actorId: row.approvedByUserId,
			metadata: {
				organizationId: row.organizationId,
				agentId: minted.agentId,
				deviceName: row.deviceName,
				deviceAuthorizationId: row.id,
				requestedIp: row.requestedIp,
				deliveredToIp: ip,
				expiresAt: minted.expiresAt.toISOString(),
			},
		});
		for (const replacedKeyId of minted.replacedKeyIds) {
			await tx.insert(auditLogs).values({
				action: 'cli.key.replaced',
				resourceType: 'api_key',
				resourceId: replacedKeyId,
				actorId: row.approvedByUserId,
				metadata: { organizationId: row.organizationId, deviceName: row.deviceName, replacedBy: minted.keyId },
			});
		}

		return {
			kind: 'token',
			key: minted.secretToken,
			keyPrefix: minted.keyPrefix,
			keyId: minted.keyId,
			expiresAt: minted.expiresAt,
			organizationId: row.organizationId,
			orgSlug: member.slug,
			owner: { name: member.name, email: member.email, role: member.role },
		};
	});
}
