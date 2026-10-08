import { createRun, type DatabendConfig, type Run } from './databend';

/**
 * Test support for the analytics module: the CDC-shaped Databend tables (the columns the D10 queries read, with the
 * types cdc-pipeline lands: uuid → VARCHAR, timestamptz → TIMESTAMP, jsonb → VARIANT, plus `deleted_at`, ADR 0020),
 * and a seeder. Integration tests run it against a real Databend container (`DATABEND_TEST_URL`).
 */

export const DDL = [
	`CREATE OR REPLACE TABLE issues (id VARCHAR, project_id VARCHAR, environment VARCHAR, message VARCHAR, error_class VARCHAR,
	   status VARCHAR, issue_type VARCHAR, assigned_to VARCHAR NULL, first_seen TIMESTAMP, last_seen TIMESTAMP,
	   first_release_version VARCHAR NULL, deleted_at TIMESTAMP NULL)`,
	`CREATE OR REPLACE TABLE error_occurrences (id VARCHAR, issue_id VARCHAR, project_id VARCHAR NULL, environment VARCHAR,
	   release_version VARCHAR NULL, created_at TIMESTAMP, received_at TIMESTAMP NULL, level VARCHAR NULL, service VARCHAR NULL,
	   tag_source VARCHAR NULL, request_scoped BOOLEAN, event_user VARIANT NULL, deleted_at TIMESTAMP NULL)`,
	`CREATE OR REPLACE TABLE issue_activity (id VARCHAR, issue_id VARCHAR, event_type VARCHAR, old_value VARIANT NULL,
	   new_value VARIANT NULL, created_at TIMESTAMP, deleted_at TIMESTAMP NULL)`,
	`CREATE OR REPLACE TABLE sli_hourly (sli_id VARCHAR, environment VARCHAR, hour_start TIMESTAMP, total BIGINT, errors BIGINT,
	   fast BIGINT NULL, deleted_at TIMESTAMP NULL)`,
];

export function testRun(url: string, database = 'sentinel_test'): { run: Run; admin: Run } {
	const cfg = (db: string): DatabendConfig => ({ url, user: 'root', password: '', database: db });
	return { run: createRun(cfg(database)), admin: createRun(cfg('default')) };
}

export async function resetSchema(url: string, database = 'sentinel_test'): Promise<Run> {
	const { run, admin } = testRun(url, database);
	await admin(`CREATE DATABASE IF NOT EXISTS ${database}`);
	for (const ddl of DDL) await run(ddl);
	return run;
}

const q = (v: string | number | boolean | null | object): string => {
	if (v === null) return 'NULL';
	if (typeof v === 'number' || typeof v === 'boolean') return String(v);
	if (typeof v === 'object') return `parse_json('${JSON.stringify(v).replace(/'/g, "''")}')`;
	return `'${v.replace(/'/g, "''")}'`;
};
const ts = (d: Date | null) => (d ? `'${d.toISOString().slice(0, 19).replace('T', ' ')}'` : 'NULL');

export interface SeedIssue {
	id: string; projectId: string; environment: string; message?: string; status?: string; issueType?: string;
	assignedTo?: string | null; firstSeen: Date; firstRelease?: string | null; deletedAt?: Date | null;
}
export interface SeedOcc {
	id: string; issueId: string; environment: string; createdAt: Date; release?: string | null; level?: string;
	service?: string | null; tagSource?: string | null; requestScoped?: boolean; userId?: string | null; receivedAt?: Date;
}
export interface SeedActivity { issueId: string; eventType: string; createdAt: Date; newValue?: object | null; oldValue?: object | null }

export async function seed(run: Run, d: { issues?: SeedIssue[]; occs?: SeedOcc[]; activity?: SeedActivity[]; sli?: [string, string, Date, number, number, number | null][] }) {
	for (const i of d.issues ?? []) {
		await run(`INSERT INTO issues VALUES (${q(i.id)}, ${q(i.projectId)}, ${q(i.environment)}, ${q(i.message ?? 'boom')}, 'Err', ${q(i.status ?? 'unresolved')},
		  ${q(i.issueType ?? 'system_error')}, ${i.assignedTo ? q(i.assignedTo) : 'NULL'}, ${ts(i.firstSeen)}, ${ts(i.firstSeen)}, ${i.firstRelease ? q(i.firstRelease) : 'NULL'}, ${ts(i.deletedAt ?? null)})`);
	}
	for (const o of d.occs ?? []) {
		await run(`INSERT INTO error_occurrences VALUES (${q(o.id)}, ${q(o.issueId)}, NULL, ${q(o.environment)}, ${o.release ? q(o.release) : 'NULL'}, ${ts(o.createdAt)},
		  ${ts(o.receivedAt ?? o.createdAt)}, ${q(o.level ?? 'error')}, ${o.service ? q(o.service) : 'NULL'}, ${o.tagSource ? q(o.tagSource) : 'NULL'},
		  ${o.requestScoped ?? false}, ${o.userId ? q({ id: o.userId }) : 'NULL'}, NULL)`);
	}
	for (const a of d.activity ?? []) {
		await run(`INSERT INTO issue_activity VALUES ('${Math.random().toString(36).slice(2)}', ${q(a.issueId)}, ${q(a.eventType)}, ${a.oldValue ? q(a.oldValue) : 'NULL'}, ${a.newValue ? q(a.newValue) : 'NULL'}, ${ts(a.createdAt)}, NULL)`);
	}
	for (const s of d.sli ?? []) {
		await run(`INSERT INTO sli_hourly VALUES (${q(s[0])}, ${q(s[1])}, ${ts(s[2])}, ${s[3]}, ${s[4]}, ${s[5] === null ? 'NULL' : s[5]}, NULL)`);
	}
}
