import { drizzle, type PostgresJsDatabase } from 'drizzle-orm/postgres-js';
import { RETENTION_RUN_MIN_CAP_MS, RETENTION_RUN_MIN_STATEMENT_MS } from './retention-batch';

// sentinel-issue-tag-facets §5 "One run, one connection, one deadline" rule 2: `run.db`, a drizzle over the
// retention run's one connection whose every statement is refused once the connection is lost, refused
// past the run's deadline or under a 1 ms cap, and otherwise preceded — awaited, never pipelined — by a
// `set_config` pair that sets the session's statement_timeout and lock_timeout to what is left of the run
// (or tighter, under `setCaps`). So no statement of the run can outlive its deadline, and neither can the
// run's lock, which lives on the same session.
//
// Why a hand-made client and not postgres-js's own: drizzle-orm 0.30's postgres-js session sends every
// statement through `client.unsafe(query, params)` (awaited directly, or through `.values()` for a
// builder select) and every transaction through `client.begin` (drizzle-orm/postgres-js/session.js), so
// those two functions see everything the steps run. `begin` is written out as BEGIN / COMMIT / ROLLBACK
// because postgres-js's own `begin` crashes the process when its backend is terminated mid-transaction
// (it writes `rollback` to a closed socket in a setImmediate). No step nests a transaction, so there are
// no savepoints.
//
// This module imports only drizzle-orm and the pure retention-batch constants: route tests need no mock
// for it.

/** A statement was refused, with nothing sent: the run's deadline passed (or a cap under 1 ms was set). */
export class RetentionDeadlineError extends Error {
	constructor(statement: string) {
		super(`retention run deadline: refused ${statement.replace(/\s+/g, ' ').trim().slice(0, 60)}`);
		this.name = 'RetentionDeadlineError';
	}
}

/** The run's session ended (terminated, database restart); the run refuses to reconnect without its lock. */
export class RetentionConnectionLost extends Error {
	constructor() {
		super('retention run connection lost: the run does not reconnect without its lock');
		this.name = 'RetentionConnectionLost';
	}
}

/**
 * True for an error that means the session itself is gone (a terminated backend, a closed socket), which
 * can reach the caller before postgres-js's `onclose` has run: nothing may be sent after it, because
 * postgres-js would reconnect on a new session that does not hold the run's lock.
 */
export function isConnectionLossError(e: unknown): boolean {
	const code = (e as { code?: unknown } | null)?.code;
	return (
		e instanceof RetentionConnectionLost ||
		code === 'CONNECTION_CLOSED' ||
		code === 'CONNECTION_ENDED' ||
		code === 'CONNECTION_DESTROYED' ||
		code === '57P01' ||
		code === '57P02' ||
		code === '57P03'
	);
}

export interface RetentionCaps {
	statementMs: number;
	lockMs: number;
}

export interface RetentionRun {
	/** Every statement of the run goes through this. */
	db: PostgresJsDatabase;
	/** Milliseconds until the run's deadline (negative past it). */
	left(): number;
	/** Tighten both timeouts for the statements that follow; null returns to the run's own bound. */
	setCaps(caps: RetentionCaps | null): void;
	/** Aborts at the deadline: object-storage deletes pass it as their abortSignal. */
	signal: AbortSignal;
}

/** The slice of a postgres-js Sql the run uses: tagged templates, unsafe, and its options (for drizzle). */
export interface RetentionRunClient {
	(strings: TemplateStringsArray, ...values: unknown[]): PromiseLike<unknown>;
	unsafe(query: string, params?: unknown[]): PromiseLike<unknown> & { values(): PromiseLike<unknown> };
	options: unknown;
}

type Mode = 'rows' | 'values';

export function openRetentionRun(
	client: RetentionRunClient,
	{ deadlineMs, isLost }: { deadlineMs: number; isLost: () => boolean }
): RetentionRun {
	const deadline = performance.now() + deadlineMs;
	const left = () => deadline - performance.now();
	let caps: RetentionCaps | null = null;

	// Checks happen when drizzle asks for the result, never earlier, so a refused statement sends nothing.
	function check(query: string): { st: number; lt: number } {
		if (isLost()) throw new RetentionConnectionLost();
		const l = left();
		if (l < RETENTION_RUN_MIN_STATEMENT_MS) throw new RetentionDeadlineError(query);
		const st = Math.floor(Math.min(caps?.statementMs ?? Infinity, l));
		const lt = Math.floor(Math.min(caps?.lockMs ?? Infinity, l));
		// PostgreSQL reads 0 as NO timeout and rejects a negative; neither is ever sent.
		if (!(st >= RETENTION_RUN_MIN_CAP_MS) || !(lt >= RETENTION_RUN_MIN_CAP_MS)) {
			throw new RetentionDeadlineError(query);
		}
		return { st, lt };
	}

	// Every postgres-js query created here is awaited in this frame, and the only promise handed out is
	// this function's own, so a failed pair rejects the caller's await and can never escape unhandled; and
	// the statement is sent only after its pair succeeded, never under the previous statement's timeouts.
	async function send(query: string, params: unknown[] | undefined, mode: Mode): Promise<unknown> {
		const { st, lt } = check(query);
		await client`SELECT set_config('statement_timeout', ${String(st)}, false), set_config('lock_timeout', ${String(lt)}, false)`;
		const q = client.unsafe(query, params);
		return await (mode === 'values' ? q.values() : q);
	}

	// A lazy thenable in the two shapes drizzle uses: awaited directly, or through .values().
	function unsafe(query: string, params?: unknown[]) {
		return {
			then<T1 = unknown, T2 = never>(
				onfulfilled?: ((value: unknown) => T1 | PromiseLike<T1>) | null,
				onrejected?: ((reason: unknown) => T2 | PromiseLike<T2>) | null
			): Promise<T1 | T2> {
				return send(query, params, 'rows').then(onfulfilled, onrejected);
			},
			values: () => send(query, params, 'values'),
		};
	}

	interface RunClient {
		options: unknown;
		unsafe: typeof unsafe;
		begin<T>(fn: (c: RunClient) => Promise<T>): Promise<T>;
	}
	const runClient: RunClient = {
		options: client.options,
		unsafe,
		async begin<T>(fn: (c: RunClient) => Promise<T>): Promise<T> {
			await unsafe('BEGIN');
			try {
				const result = await fn(runClient);
				await unsafe('COMMIT');
				return result;
			} catch (err) {
				// Raw, never through the checked path: a rollback is not refused by the deadline. Not sent at
				// all once the session is gone (it would reconnect, or write to a closed socket).
				if (!isLost() && !isConnectionLossError(err)) {
					try {
						await client.unsafe('ROLLBACK');
					} catch {
						// the original error is the one that matters
					}
				}
				throw err;
			}
		},
	};

	return {
		db: drizzle(runClient as never),
		left,
		setCaps(next) {
			caps = next;
		},
		signal: AbortSignal.timeout(Math.max(0, Math.floor(deadlineMs))),
	};
}
