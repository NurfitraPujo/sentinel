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
const client = postgres(connectionString, {
	types: {
		// 3802 = jsonb, 114 = json. Serialize once, on our side, and tell postgres-js the value is
		// already a string so it does not encode it a second time.
		jsonb: {
			to: 3802,
			from: [3802],
			serialize: (v: unknown) => (typeof v === 'string' ? v : JSON.stringify(v)),
			parse: (v: string) => JSON.parse(v)
		}
	}
});

export const db = drizzle(client);

// R15 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): the Drizzle transaction callback param type,
// exported so query modules that accept an optional/explicit `tx` (subscriptions, notify,
// reports' attachment-claim helpers) can type it precisely instead of `tx: any` -- `any` let a
// caller pass anything (or a typo'd property name) with no compile-time signal.
export type Tx = Parameters<Parameters<typeof db.transaction>[0]>[0];
