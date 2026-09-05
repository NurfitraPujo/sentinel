import crypto from 'crypto';
import { and, eq, lt } from 'drizzle-orm';
import { db, type Tx } from '$lib/server/db';
import { reportIdempotencyKeys } from '$lib/db/schema';

/**
 * sentinel-user-report-issue contract §4.1 / §8 — the 7-day idempotency of `POST /api/report`.
 *
 * §4.1 fixes the observable behaviour and hands the storage shape to the repo: "a repeat within
 * the dedupe window returns the first call's result and creates nothing … this endpoint's key
 * space is the calling API key row's id, so it needs its own table or a widened one. That is a
 * repo-side implementation call; the observable behaviour above is fixed here, as is the dedupe
 * window: 7 days."
 *
 * TASK-01 made that call: `report_idempotency_keys`, `UNIQUE (api_key_id, idempotency_key)`, with
 * `request_hash` (without a stored digest of the first body, §7's `409 idempotency_conflict` --
 * "the same key used with a DIFFERENT body" -- is not detectable at all) and `issue_id` (no FK,
 * for the reason `agent_idempotency_keys.comment_id` gives: a retry must not resurrect a deleted
 * row).
 *
 * **The row is written inside the SAME transaction as the report**, by `createExternalIssueReport`
 * -- never before it and never after. That is what closes the window a two-step
 * "reserve-then-create" would leave open: two concurrent requests carrying one key would both find
 * no row, both create a report, and the endpoint would have deduplicated nothing. With the insert
 * inside the transaction, the second writer BLOCKS on the unique index until the first commits and
 * then fails it, its own report insert rolls back, and it replays the winner's `report_id`.
 * `isDuplicateKeyViolation` below is how the route recognises that it lost, and it is the whole
 * concurrency story.
 */

/** §8, matching `reapExpiredIdempotencyKeys`' existing cutoff for the agent-side table. */
export const REPORT_IDEMPOTENCY_RETENTION_DAYS = 7;

/** Postgres `unique_violation`. */
const PG_UNIQUE_VIOLATION = '23505';

export interface ReportIdempotencyRecord {
	id: string;
	requestHash: string;
	issueId: string | null;
	createdAt: Date;
}

/**
 * The digest §7's `409 idempotency_conflict` compares against.
 *
 * Hashes a CANONICAL form (object keys sorted at every depth) rather than the caller's raw bytes,
 * so a client that re-serialises the same report on retry -- different key order, different
 * whitespace -- is deduplicated instead of being told its body changed. Unknown top-level keys are
 * INCLUDED: §3 says the server ignores them, but a retry that adds or changes one is a different
 * body by any reading a caller would recognise, and silently replaying the first result would hide
 * that from them.
 *
 * `projectId` is folded in because it is not part of the body on an org-wide key -- it comes from
 * `X-Project-Key`. Without it, one key plus one `idempotency_key` plus two different
 * `X-Project-Key` headers would collide into one report, and the second project would silently get
 * the first project's `report_id` back.
 */
export function hashReportRequest(raw: unknown, projectId: string): string {
	return crypto
		.createHash('sha256')
		.update(`${projectId} ${canonicalize(raw)}`)
		.digest('hex');
}

/** Stable JSON: object keys sorted at every depth, arrays left in order (order is meaning there). */
function canonicalize(value: unknown): string {
	if (value === null || typeof value !== 'object') return JSON.stringify(value) ?? 'null';
	if (Array.isArray(value)) return `[${value.map(canonicalize).join(',')}]`;
	const entries = Object.entries(value as Record<string, unknown>)
		.filter(([, v]) => v !== undefined)
		.sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
		.map(([k, v]) => `${JSON.stringify(k)}:${canonicalize(v)}`);
	return `{${entries.join(',')}}`;
}

/** The instant before which a stored key no longer deduplicates anything (§8's 7-day window). */
export function idempotencyCutoff(now: Date = new Date()): Date {
	const cutoff = new Date(now);
	cutoff.setDate(cutoff.getDate() - REPORT_IDEMPOTENCY_RETENTION_DAYS);
	return cutoff;
}

/** Reads the stored record for this key space, expired rows included -- the caller decides. */
export async function findReportIdempotency(
	apiKeyId: string,
	idempotencyKey: string
): Promise<ReportIdempotencyRecord | null> {
	const [row] = await db
		.select({
			id: reportIdempotencyKeys.id,
			requestHash: reportIdempotencyKeys.requestHash,
			issueId: reportIdempotencyKeys.issueId,
			createdAt: reportIdempotencyKeys.createdAt,
		})
		.from(reportIdempotencyKeys)
		.where(
			and(
				eq(reportIdempotencyKeys.apiKeyId, apiKeyId),
				eq(reportIdempotencyKeys.idempotencyKey, idempotencyKey)
			)
		);
	return row ?? null;
}

/**
 * Deletes one EXPIRED record so the key can be reused for a fresh report.
 *
 * §4.1's window is 7 days, and past it "a repeat … creates a second report" -- but the row
 * survives until the retention sweep runs, and the unique index would otherwise refuse the new
 * insert. The `created_at < cutoff` predicate is inside the DELETE, not evaluated before it, so a
 * row a concurrent request refreshed in the meantime is left alone rather than dropped. Returns
 * whether the expired row was actually removed.
 */
export async function deleteExpiredReportIdempotency(
	apiKeyId: string,
	idempotencyKey: string,
	now: Date = new Date()
): Promise<boolean> {
	const deleted = await db
		.delete(reportIdempotencyKeys)
		.where(
			and(
				eq(reportIdempotencyKeys.apiKeyId, apiKeyId),
				eq(reportIdempotencyKeys.idempotencyKey, idempotencyKey),
				lt(reportIdempotencyKeys.createdAt, idempotencyCutoff(now))
			)
		)
		.returning({ id: reportIdempotencyKeys.id });
	return deleted.length > 0;
}

/** Writes the record inside the report's own transaction. See the header on why it lives there. */
export async function insertReportIdempotency(
	tx: Tx,
	input: { apiKeyId: string; idempotencyKey: string; requestHash: string; issueId: string }
): Promise<void> {
	await tx.insert(reportIdempotencyKeys).values({
		apiKeyId: input.apiKeyId,
		idempotencyKey: input.idempotencyKey,
		requestHash: input.requestHash,
		issueId: input.issueId,
	});
}

/**
 * True when `err` is the `(api_key_id, idempotency_key)` unique violation -- i.e. a concurrent
 * request carrying the same key committed first and this transaction rolled back having created
 * nothing. Matched on the SQLSTATE, which postgres-js surfaces as `code`, rather than on the
 * message text, which is locale- and version-dependent.
 */
export function isDuplicateKeyViolation(err: unknown): boolean {
	const code = (err as { code?: unknown })?.code;
	if (code !== PG_UNIQUE_VIOLATION) return false;
	const source = (err ?? {}) as { constraint_name?: unknown; constraint?: unknown };
	const name = source.constraint_name ?? source.constraint;
	// A unique violation from any OTHER index in the same transaction is a real error, not a lost
	// idempotency race, and must not be reported to the caller as a successful replay.
	return typeof name !== 'string' || name === 'report_idempotency_keys_key_unique';
}
