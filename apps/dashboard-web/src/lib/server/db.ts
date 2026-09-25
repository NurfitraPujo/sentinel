import { drizzle } from 'drizzle-orm/postgres-js';
import postgres from 'postgres';
import { env } from '$env/dynamic/private';

const connectionString = env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';

// `transform: { undefined: null }` only; JSON handling is the important part below.
//
// drizzle-orm/postgres-js serializes a jsonb value with JSON.stringify before handing it to
// postgres-js, and postgres-js then serializes again — so `{ status: 'resolved' }` lands in Postgres
// as the jsonb STRING "{\"status\":\"resolved\"}" rather than an object. jsonb_typeof() returns
// 'string', `->>` returns NULL, and the Go processor (which scans these columns into
// map[string]interface{}) cannot read them at all: a JSON string does not unmarshal into a map.
//
// The cross-language consequence is why this matters beyond tidiness — the dashboard and the
// processor write the SAME tables, and only the dashboard's rows were unreadable. Declaring the
// jsonb types here makes postgres-js pass the already-serialized value through untouched.
const types = {
	// 3802 = jsonb, 114 = json. Serialize once, on our side, and tell postgres-js the value is
	// already a string so it does not encode it a second time.
	jsonb: {
		to: 3802,
		from: [3802],
		serialize: (v: unknown) => (typeof v === 'string' ? v : JSON.stringify(v)),
		parse: (v: string) => JSON.parse(v)
	}
};

const client = postgres(connectionString, { types });

export const db = drizzle(client);

/**
 * sentinel-issue-tag-facets §5 "One run, one connection, one deadline" rule 1: the retention route's
 * per-run client. A NEW client with its own single connection (`max: 1`), made per POST and ended in the
 * route's `finally`. The run takes the route's advisory lock on this connection and runs every step on
 * it through `openRetentionRun` (retention-run.ts), so the lock and the work are one session: ending the
 * session ends both, and no pooled connection of the run is left waiting on anything.
 *
 * - `application_name` names the run's backend in pg_stat_activity (the RUNBOOK's queries use it).
 * - The keepalives are startup parameters: the server drops the session of a pod whose node vanished in
 *   ~2 min (60 + 10 × 6 s), not the kernel default's ~2 h.
 * - `onclose` is how the run learns its session is gone: it sets `lost`, and run.db refuses every later
 *   statement, because postgres-js would otherwise reconnect on a new session that does not hold the lock.
 * - NOT `db`'s `reserve()` and NOT postgres-js `begin()`: each crashes the process when the backend is
 *   terminated (an uncaught write on a closed socket, spec §5 rule 1, §12).
 */
export function openRetentionRunClient(onclose: () => void) {
	return postgres(connectionString, {
		max: 1,
		types,
		onclose,
		connection: {
			application_name: 'sentinel.retention.run',
			tcp_keepalives_idle: 60,
			tcp_keepalives_interval: 10,
			tcp_keepalives_count: 6
		}
	});
}

// R15 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): the Drizzle transaction callback param type,
// exported so query modules that accept an optional/explicit `tx` (subscriptions, notify,
// reports' attachment-claim helpers) can type it precisely instead of `tx: any` -- `any` let a
// caller pass anything (or a typo'd property name) with no compile-time signal.
export type Tx = Parameters<Parameters<typeof db.transaction>[0]>[0];
