import { db } from '$lib/server/db';
import { agentIdempotencyKeys } from '$lib/db/schema';
import { and, eq } from 'drizzle-orm';
import type { PgTransaction } from 'drizzle-orm/pg-core';

/**
 * N9 (docs/plans/AGENT_WORKER_PLAN.md C4/C5, D21): client-supplied idempotency keys for agent write
 * endpoints. The retention window a stored key survives (7 days) before retention.ts's reaper ages
 * it out. Named const, not a magic number (B12); a retry with the SAME key after this window has
 * elapsed falls through to a fresh write, exactly as it would if the key had never been sent -- the
 * documented, deliberate boundary of the guarantee.
 */
export const AGENT_IDEMPOTENCY_RETENTION_DAYS = 7;

/**
 * A key was reused for a DIFFERENT op than the one it was first recorded against (e.g. the same
 * UUID sent to both `issues.comment` and `issues.progress`). That is a client bug -- replaying the
 * stored result would be the wrong shape -- so it surfaces as a 409 at the route layer rather than
 * silently returning a mismatched body.
 */
export class IdempotencyKeyOpMismatchError extends Error {
	constructor(
		public readonly expectedOp: string,
		public readonly actualOp: string
	) {
		super(`Idempotency key already used for op "${expectedOp}", cannot reuse for "${actualOp}"`);
		this.name = 'IdempotencyKeyOpMismatchError';
	}
}

/**
 * Internal sentinel thrown by `recordIdempotencyKey` when a CONCURRENT transaction already committed
 * this (agentId, key) pair between our check and our insert. It is thrown from INSIDE the caller's
 * `db.transaction`, so it rolls back the side effects this call had staged (the comment/activity
 * inserts, the notification rows) -- the whole point: the concurrent winner's single side effect is
 * the only one that survives. The caller catches it OUTSIDE the transaction and replays the winner's
 * stored result via `replayIdempotentComment` / a bare success, so the loser still returns 200 with
 * the original result, never a 500.
 */
export class IdempotencyRaceError extends Error {
	constructor(
		public readonly agentId: string,
		public readonly key: string
	) {
		super('Idempotency key committed concurrently; roll back and replay');
		this.name = 'IdempotencyRaceError';
	}
}

export interface IdempotencyHit {
	op: string;
	commentId: string | null;
}

// The transactional helpers accept the caller's `tx`; the non-transactional replay reads use the
// pooled `db`. `Tx` is intentionally the structural `select`/`insert` slice of a real transaction
// (not the concrete PgTransaction generic soup) so BOTH a real `db.transaction` handle and a
// unit-test double that implements just those two methods satisfy it.
type Tx = Pick<PgTransaction<any, any, any>, 'select' | 'insert'>;

/**
 * Looks up an existing key WITHIN the caller's transaction. `null` ⇒ first time we have seen this
 * key, proceed with the real write. A hit whose stored `op` differs from `expectedOp` throws
 * `IdempotencyKeyOpMismatchError`.
 */
export async function findIdempotencyKey(
	tx: Tx,
	agentId: string,
	key: string,
	expectedOp: string
): Promise<IdempotencyHit | null> {
	const rows = await tx
		.select({ op: agentIdempotencyKeys.op, commentId: agentIdempotencyKeys.commentId })
		.from(agentIdempotencyKeys)
		.where(and(eq(agentIdempotencyKeys.agentId, agentId), eq(agentIdempotencyKeys.idempotencyKey, key)));
	const hit = rows[0];
	if (!hit) return null;
	if (hit.op !== expectedOp) {
		throw new IdempotencyKeyOpMismatchError(hit.op, expectedOp);
	}
	return hit;
}

/**
 * Records a freshly-produced result's key inside the caller's transaction using
 * `ON CONFLICT DO NOTHING`. If the insert affects no row a concurrent transaction beat us to this
 * key -- throw `IdempotencyRaceError` so the caller's transaction rolls back and the caller can
 * replay the winner's stored result. `ON CONFLICT DO NOTHING` (rather than letting the unique
 * violation raise) is deliberate: a raw 23505 aborts the whole Postgres transaction and cannot be
 * caught-and-continued, whereas DO NOTHING lets us detect the race via the empty RETURNING and
 * choose to throw our own sentinel.
 */
export async function recordIdempotencyKey(
	tx: Tx,
	agentId: string,
	key: string,
	op: string,
	commentId: string | null
): Promise<void> {
	const inserted = await tx
		.insert(agentIdempotencyKeys)
		.values({ agentId, idempotencyKey: key, op, commentId })
		.onConflictDoNothing()
		.returning({ id: agentIdempotencyKeys.id });
	if (inserted.length === 0) {
		throw new IdempotencyRaceError(agentId, key);
	}
}

/**
 * Non-transactional replay read used after an `IdempotencyRaceError` unwound the losing
 * transaction: fetches the winner's stored `commentId` for `(agentId, key)`. Returns null when the
 * row is somehow absent (the winner's commit should guarantee it exists; a null here means the key
 * was reaped or the caller mis-wired the replay).
 */
export async function readIdempotentCommentId(agentId: string, key: string): Promise<string | null> {
	const rows = await db
		.select({ commentId: agentIdempotencyKeys.commentId })
		.from(agentIdempotencyKeys)
		.where(and(eq(agentIdempotencyKeys.agentId, agentId), eq(agentIdempotencyKeys.idempotencyKey, key)));
	return rows[0]?.commentId ?? null;
}
