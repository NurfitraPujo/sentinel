import { describe, it, expect, beforeAll, afterAll, vi } from 'vitest';
import postgres from 'postgres';
import { randomBytes, randomUUID, createHash } from 'node:crypto';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { isHttpError } from '@sveltejs/kit';

/**
 * sentinel-cli-browser-login (docs/plans/CLI_BROWSER_LOGIN.md, "Personal-key authorization rule") -- the authorization
 * matrix for PERSONAL agent keys, against a REAL migrated Postgres and the REAL route handlers, because every claim here is
 * a claim about SQL guards and live membership reads that a mocked Drizzle chain cannot make.
 *
 * Every key is minted through the real device flow (start -> approve -> poll), then driven through EVERY `/api/agent/*`
 * route family (the last test fails if a route is added without a row here), for each org role:
 *
 *   owner, admin, engineer      every project, may write
 *   narrow                       engineer with an environment grant (web: production only) -- grants apply
 *   support                      every project, READ-ONLY (rbac.ts `write` is owner/admin/engineer)
 *   viewerWeb                    viewer + project_members(web): web only, read-only
 *   viewerNone                   viewer with no project: sees nothing, read-only
 *
 * plus an ORG agent key as the regression baseline (whole org, writes, members list), and the lifecycle: removal from the
 * org, a role downgrade and a new grant take effect on the NEXT request; logout and a re-login from the same device kill
 * the old key.
 *
 * Point DATABASE_URL at a DISPOSABLE database -- never the shared dev one (CLAUDE.md):
 *
 *   podman run -d --name cbl-pg -p 127.0.0.1:55533:5432 -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=<pw> \
 *     -e POSTGRES_DB=sentinel postgres:16-alpine
 *   cd packages/db-migrations && GOWORK=off DB_URL_DASHBOARD='postgres://sentinel:<pw>@127.0.0.1:55533/sentinel?sslmode=disable' \
 *     go run ./cmd/migrate up -target dashboard
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:<pw>@127.0.0.1:55533/sentinel \
 *     CLI_LOGIN_INTEGRATION_REQUIRED=1 pnpm vitest run src/routes/api/agent/personal-keys.integration.test.ts
 *
 * Skipped when nothing answers (or before migration 1725900000); CLI_LOGIN_INTEGRATION_REQUIRED=1 makes that a failure.
 */
// Live process.env, so the SPAN_PULL_SCOPES set in beforeAll reaches span-pull.ts's getSpanPullConfig.
vi.mock('$env/dynamic/private', () => ({ env: process.env }));

const connectionString = process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.CLI_LOGIN_INTEGRATION_REQUIRED === '1';

let dbReady = false;
const probe = postgres(connectionString, { connect_timeout: 3, max: 1 });
try {
	const [row] = await probe`select to_regclass('cli_device_authorizations')::text as t`;
	dbReady = row?.t === 'cli_device_authorizations';
} catch {
	dbReady = false;
} finally {
	await probe.end({ timeout: 1 }).catch(() => {});
}
if (required && !dbReady) {
	throw new Error(`CLI_LOGIN_INTEGRATION_REQUIRED=1 but no migrated (>= 1725900000) database answered at ${connectionString}.`);
}

const suffix = randomUUID().slice(0, 8);
const orgA = randomUUID();
const orgB = randomUUID();
const pWeb = randomUUID();
const pCore = randomUUID();
const pB = randomUUID();

const users = {
	owner: randomUUID(),
	admin: randomUUID(),
	engineer: randomUUID(),
	narrow: randomUUID(),
	support: randomUUID(),
	viewerWeb: randomUUID(),
	viewerNone: randomUUID(),
	lifecycle: randomUUID(),
	removed: randomUUID(),
	outsider: randomUUID(),
};
type Persona = 'owner' | 'admin' | 'engineer' | 'narrow' | 'support' | 'viewerWeb' | 'viewerNone';
const ROLE: Record<Persona | 'lifecycle' | 'removed', string> = {
	owner: 'owner',
	admin: 'admin',
	engineer: 'engineer',
	narrow: 'engineer',
	support: 'support',
	viewerWeb: 'viewer',
	viewerNone: 'viewer',
	lifecycle: 'engineer',
	removed: 'engineer',
};

// web/production, web/staging, core/production in org A; one issue in org B.
const issue = { webProd: randomUUID(), webStaging: randomUUID(), coreProd: randomUUID(), foreign: randomUUID() };
const occ = { webProd: randomUUID(), webStaging: randomUUID(), coreProd: randomUUID(), foreign: randomUUID() };
const TRACE = randomBytes(16).toString('hex'); // in all four issues
const TRACE_STAGING = randomBytes(16).toString('hex'); // web/staging only
type IssueKey = keyof typeof issue;
const ORG_A_ISSUES: IssueKey[] = ['webProd', 'webStaging', 'coreProd'];

const VISIBLE: Record<Persona, IssueKey[]> = {
	owner: ['webProd', 'webStaging', 'coreProd'],
	admin: ['webProd', 'webStaging', 'coreProd'],
	engineer: ['webProd', 'webStaging', 'coreProd'],
	narrow: ['webProd', 'coreProd'],
	support: ['webProd', 'webStaging', 'coreProd'],
	viewerWeb: ['webProd', 'webStaging'],
	viewerNone: [],
};
const CAN_WRITE: Record<Persona, boolean> = {
	owner: true,
	admin: true,
	engineer: true,
	narrow: true,
	support: false,
	viewerWeb: false,
	viewerNone: false,
};
const PROJECTS: Record<Persona, string[]> = {
	owner: [pWeb, pCore],
	admin: [pWeb, pCore],
	engineer: [pWeb, pCore],
	narrow: [pWeb, pCore],
	support: [pWeb, pCore],
	viewerWeb: [pWeb],
	viewerNone: [],
};
const PERSONAS = Object.keys(VISIBLE) as Persona[];

// ---- route plumbing ---------------------------------------------------------------------------------------------------

type Handler = (event: any) => Promise<Response>;
const ROUTE_MODULES = import.meta.glob('./**/+server.ts');

/**
 * Every `/api/agent/*` route and method this file drives. The last test asserts it equals what the route tree exports, so
 * a route added without a personal-key row here fails CI -- and `call()` refuses a route that is not listed.
 */
const ROUTE_TABLE = [
	'/api/agent/batch POST',
	'/api/agent/events GET',
	'/api/agent/issues GET',
	'/api/agent/issues/claim-next POST',
	'/api/agent/issues/{issueId} GET',
	'/api/agent/issues/{issueId}/activity GET',
	'/api/agent/issues/{issueId}/claim POST',
	'/api/agent/issues/{issueId}/claim DELETE',
	'/api/agent/issues/{issueId}/comments GET',
	'/api/agent/issues/{issueId}/comments POST',
	'/api/agent/issues/{issueId}/comments/{commentId} PATCH',
	'/api/agent/issues/{issueId}/comments/{commentId} DELETE',
	'/api/agent/issues/{issueId}/occurrences GET',
	'/api/agent/issues/{issueId}/progress POST',
	'/api/agent/issues/{issueId}/questions POST',
	'/api/agent/issues/{issueId}/relations POST',
	'/api/agent/issues/{issueId}/relations DELETE',
	'/api/agent/issues/{issueId}/report/severity PATCH',
	'/api/agent/issues/{issueId}/status PATCH',
	'/api/agent/key/revoke POST',
	'/api/agent/key/rotate POST',
	'/api/agent/members GET',
	'/api/agent/members POST',
	'/api/agent/members/{id} DELETE',
	'/api/agent/occurrences/{occurrenceId} GET',
	'/api/agent/projects GET',
	'/api/agent/repo-credentials GET',
	'/api/agent/self GET',
	'/api/agent/traces/{traceId} GET',
	'/api/agent/traces/{traceId}/pull POST',
	'/api/agent/uploads POST',
];

async function call(
	routePath: string,
	method: 'GET' | 'POST' | 'PATCH' | 'DELETE',
	opts: { key: string; params?: Record<string, string>; body?: unknown; query?: string; form?: FormData }
): Promise<{ status: number; body: any }> {
	if (!ROUTE_TABLE.includes(`${routePath} ${method}`)) throw new Error(`not in ROUTE_TABLE: ${routePath} ${method}`);
	// `{issueId}` is the directory `[issueId]` or, with a param matcher, `[issueId=uuid]` (src/params/uuid.ts).
	const dir = routePath.replace(/^\/api\/agent/, '').replace(/\{(\w+)\}/g, '[$1]');
	const loader = Object.entries(ROUTE_MODULES).find(([k]) => k.replace(/\[(\w+)=\w+\]/g, '[$1]') === `.${dir}/+server.ts`)?.[1];
	if (!loader) throw new Error(`no route module for ${routePath}`);
	const mod = (await loader()) as Record<string, Handler>;
	const handler = mod[method];
	let concrete = routePath;
	for (const [k, v] of Object.entries(opts.params ?? {})) concrete = concrete.replace(`{${k}}`, v);
	const url = new URL(`http://localhost${concrete}${opts.query ?? ''}`);
	const headers: Record<string, string> = { authorization: `Bearer ${opts.key}` };
	let body: BodyInit | undefined;
	if (opts.form) body = opts.form;
	else if (opts.body !== undefined) {
		headers['content-type'] = 'application/json';
		body = JSON.stringify(opts.body);
	}
	const request = new Request(url, { method, headers, body });
	try {
		const res = await handler({ request, url, params: opts.params ?? {}, getClientAddress: () => '127.0.0.1', locals: {} });
		const text = await res.text();
		return { status: res.status, body: text ? JSON.parse(text) : null };
	} catch (err) {
		if (isHttpError(err)) return { status: err.status, body: err.body };
		if (err instanceof Response) return { status: err.status, body: await err.json().catch(() => null) };
		throw err;
	}
}

// ---- seed -------------------------------------------------------------------------------------------------------------

let sql: ReturnType<typeof postgres>;
const keys = {} as Record<Persona | 'lifecycle' | 'removed', string>;
let orgAgentKey = '';
let orgAgentId = '';

async function deviceLogin(userId: string, organizationId: string, deviceName: string): Promise<string> {
	const { startDeviceAuthorization, decideDeviceAuthorization, pollDeviceToken, hashDeviceCode } = await import(
		'$lib/server/cli-device'
	);
	const started = await startDeviceAuthorization({
		client: 'sentinel-cli',
		clientVersion: 'test',
		deviceName,
		profile: '',
		ip: '127.0.0.1',
		userAgent: 'vitest',
	});
	const decided = await decideDeviceAuthorization({
		userCode: started.userCode,
		userId,
		organizationId,
		decision: 'approve',
		ip: '127.0.0.1',
	});
	if (!decided.ok) throw new Error(`approve failed: ${decided.reason}`);
	const polled = await pollDeviceToken(started.deviceCode, '127.0.0.1');
	if (polled.kind !== 'token') throw new Error(`poll failed: ${polled.error}`);
	// The row never stores the device code or the key -- only the code's hash.
	const [row] = await sql`select device_code_hash, status from cli_device_authorizations where user_code = ${started.userCode} and status = 'delivered'`;
	expect(row.device_code_hash).toBe(hashDeviceCode(started.deviceCode));
	return polled.key;
}

describe.skipIf(!dbReady)('personal agent keys: authorization matrix (integration, real Postgres)', () => {
	beforeAll(async () => {
		process.env.SPAN_PULL_SCOPES = `${orgA}:${pWeb}`;
		sql = postgres(connectionString, { connect_timeout: 5, max: 2 });
		for (const [id, label] of [[orgA, 'a'], [orgB, 'b']] as const) {
			await sql`insert into organizations (id, name, slug) values (${id}, ${`cbl-${label}-${suffix}`}, ${`cbl-${label}-${suffix}`})`;
		}
		for (const [id, org, label] of [[pWeb, orgA, 'web'], [pCore, orgA, 'core'], [pB, orgB, 'b']] as const) {
			await sql`insert into projects (id, name, organization_id, api_key, api_key_hash)
			          values (${id}, ${`cbl-${label}-${suffix}`}, ${org}, ${`k_${label}_${suffix}`}, ${`h_${label}_${suffix}`})`;
		}
		for (const [label, id] of Object.entries(users)) {
			await sql`insert into "user" (id, name, email) values (${id}, ${`CBL ${label}`}, ${`cbl-${label}-${suffix}@example.test`})`;
		}
		for (const [persona, role] of Object.entries(ROLE)) {
			await sql`insert into organization_members (organization_id, user_id, role) values (${orgA}, ${users[persona as Persona]}, ${role})`;
		}
		await sql`insert into organization_members (organization_id, user_id, role) values (${orgB}, ${users.outsider}, 'owner')`;
		await sql`insert into project_members (project_id, user_id, role) values (${pWeb}, ${users.viewerWeb}, 'viewer')`;
		await sql`insert into project_environment_grants (user_id, project_id, environments, created_by)
		          values (${users.narrow}, ${pWeb}, ${sql.array(['production'])}, ${users.owner})`;

		for (const [key, project, env] of [
			['webProd', pWeb, 'production'],
			['webStaging', pWeb, 'staging'],
			['coreProd', pCore, 'production'],
			['foreign', pB, 'production'],
		] as const) {
			await sql`insert into issues (id, project_id, environment, fingerprint, message, error_class)
			          values (${issue[key]}, ${project}, ${env}, ${`fp-${issue[key]}`}, ${`cbl ${key} ${suffix}`}, 'CblSeedError')`;
			await sql`insert into error_occurrences (id, issue_id, environment, platform, trace_id, created_at, received_at, message, level, error_class)
			          values (${occ[key]}, ${issue[key]}, ${env}, 'node', ${TRACE}, now() - interval '1 hour', now() - interval '1 hour', ${`cbl ${key}`}, 'error', 'CblSeedError')`;
			await sql`insert into issue_activity (issue_id, event_type, actor_type, actor_id, created_at)
			          values (${issue[key]}, 'commented', 'user', ${users.owner}, now() - interval '1 hour')`;
		}
		await sql`insert into error_occurrences (id, issue_id, environment, platform, trace_id, created_at, received_at, message, level, error_class)
		          values (${randomUUID()}, ${issue.webStaging}, 'staging', 'node', ${TRACE_STAGING}, now() - interval '1 hour', now() - interval '1 hour', 'staging only', 'error', 'CblSeedError')`;
		// Two `linked` events on web/production whose TARGETS sit in core/production and web/staging: the feed is filtered
		// by the source issue, so the target id is what has to be redacted for an owner who cannot see it.
		for (const target of [issue.coreProd, issue.webStaging]) {
			await sql`insert into issue_activity (issue_id, event_type, actor_type, actor_id, new_value, created_at)
			          values (${issue.webProd}, 'linked', 'user', ${users.owner}, ${sql.json({ targetIssueId: target, relationType: 'related' })}, now() - interval '1 hour')`;
		}

		// An org agent + key, the pre-existing shape, as the regression baseline.
		const [agent] = await sql`insert into agents (org_id, name, kind, created_by) values (${orgA}, 'cbl-org-agent', 'ai', ${users.owner}) returning id`;
		orgAgentId = agent.id;
		orgAgentKey = `sent_agent_${randomBytes(32).toString('hex')}`;
		await sql`insert into project_api_keys (organization_id, name, key_prefix, key_hash, scope, created_by, agent_id)
		          values (${orgA}, 'ci', 'sent_agent_', ${createHash('sha256').update(orgAgentKey).digest('hex')}, 'agent', ${users.owner}, ${orgAgentId})`;

		for (const persona of [...PERSONAS, 'lifecycle', 'removed'] as const) {
			keys[persona] = await deviceLogin(users[persona], orgA, `laptop-${persona}`);
		}
	});

	afterAll(async () => {
		delete process.env.SPAN_PULL_SCOPES;
		if (!sql) return;
		try {
			await sql`delete from span_pull_requests where org_id = ${orgA}`;
			await sql`delete from span_pull_queue where org_id = ${orgA}`;
			const userIds: string[] = Object.values(users);
			const actorIds: string[] = [...userIds, orgAgentId];
			await sql`delete from audit_logs where actor_id in ${sql(actorIds)} or (metadata->>'organizationId') in ${sql([orgA, orgB])}`;
			await sql`delete from cli_device_authorizations where approved_by_user_id in ${sql(userIds)} or client_version = 'test'`;
			await sql`delete from organizations where id in ${sql([orgA, orgB])}`;
			await sql`delete from "user" where id in ${sql(userIds)}`;
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	const visibleIssueIds = (p: Persona) => VISIBLE[p].map((k) => issue[k]).sort();
	const expectFor = (p: Persona, k: IssueKey, ifVisible: number) => (VISIBLE[p].includes(k) ? ifVisible : 404);
	const expectWrite = (p: Persona, k: IssueKey, ok: number) =>
		!VISIBLE[p].includes(k) ? 404 : CAN_WRITE[p] ? ok : 403;

	describe.each(PERSONAS)('%s', (p) => {
		it('GET /self reports the live owner, role and canWrite', async () => {
			const r = await call('/api/agent/self', 'GET', { key: keys[p] });
			expect(r.status).toBe(200);
			expect(r.body.kind).toBe('personal');
			expect(r.body.owner).toMatchObject({ userId: users[p], role: ROLE[p], canWrite: CAN_WRITE[p] });
			expect(r.body.name).toBe(`CBL ${p} (CLI)`);
		});

		it('GET /issues lists exactly the visible issues (never another org)', async () => {
			const r = await call('/api/agent/issues', 'GET', { key: keys[p] });
			expect(r.status).toBe(200);
			expect(r.body.issues.map((i: { id: string }) => i.id).sort()).toEqual(visibleIssueIds(p));
		});

		it('GET /projects lists exactly the reachable projects', async () => {
			const r = await call('/api/agent/projects', 'GET', { key: keys[p] });
			expect(r.status).toBe(200);
			expect(r.body.projects.map((x: { id: string }) => x.id).sort()).toEqual([...PROJECTS[p]].sort());
		});

		it('GET /events carries only events of visible issues', async () => {
			const r = await call('/api/agent/events', 'GET', { key: keys[p], query: '?limit=500' });
			expect(r.status).toBe(200);
			const seen = new Set(r.body.events.map((e: { issue: { id: string } }) => e.issue.id));
			const seeded = new Set<string>(Object.values(issue));
			expect([...seen].filter((id) => seeded.has(id as string)).sort()).toEqual(visibleIssueIds(p));
		});

		it("GET /events redacts a linked event's target issue the owner cannot see, as the issue page does", async () => {
			const r = await call('/api/agent/events', 'GET', { key: keys[p], query: '?limit=500&type=linked' });
			expect(r.status).toBe(200);
			const linked = r.body.events.filter((e: { issue: { id: string } }) => e.issue.id === issue.webProd);
			if (!VISIBLE[p].includes('webProd')) {
				expect(linked).toEqual([]); // the source itself is invisible: the whole event is filtered
				return;
			}
			const targets = linked.map((e: { newValue: Record<string, unknown> }) => e.newValue);
			for (const k of ['coreProd', 'webStaging'] as const) {
				const expected = VISIBLE[p].includes(k)
					? { targetIssueId: issue[k], relationType: 'related' }
					: { targetIssueId: null, targetRedacted: true, relationType: 'related' };
				expect(targets).toContainEqual(expected);
			}
			// Nothing else in the event carries a hidden target's id.
			for (const k of ['coreProd', 'webStaging'] as const) {
				if (!VISIBLE[p].includes(k)) expect(JSON.stringify(linked)).not.toContain(issue[k]);
			}
		});

		it("GET /issues/{id}/activity redacts a linked entry's target the owner cannot see, like the events feed", async () => {
			const r = await call('/api/agent/issues/{issueId}/activity', 'GET', { key: keys[p], params: { issueId: issue.webProd } });
			if (!VISIBLE[p].includes('webProd')) {
				expect(r.status).toBe(404);
				return;
			}
			expect(r.status).toBe(200);
			// The two seeded `related` links (earlier personas' relation tests add `linked_to` ones to this issue).
			const linked = r.body.entries.filter(
				(e: { eventType: string; newValue: { relationType?: string } }) => e.eventType === 'linked' && e.newValue.relationType === 'related'
			);
			expect(linked).toHaveLength(2);
			for (const k of ['coreProd', 'webStaging'] as const) {
				const expected = VISIBLE[p].includes(k)
					? { targetIssueId: issue[k], relationType: 'related' }
					: { targetIssueId: null, targetRedacted: true, relationType: 'related' };
				expect(linked.map((e: { newValue: unknown }) => e.newValue)).toContainEqual(expected);
				if (!VISIBLE[p].includes(k)) expect(JSON.stringify(linked)).not.toContain(issue[k]);
			}
			// The actor is the owner's NAME, never the raw user id.
			expect(linked[0].actor).toEqual({ type: 'user', id: users.owner, name: 'CBL owner', personal: false });
		});

		it.each([...ORG_A_ISSUES, 'foreign'] as IssueKey[])('single-issue reads of %s: 200 visible, 404 otherwise', async (k) => {
			const params = { issueId: issue[k] };
			const want = k === 'foreign' ? 404 : expectFor(p, k, 200);
			expect((await call('/api/agent/issues/{issueId}', 'GET', { key: keys[p], params })).status).toBe(want);
			expect((await call('/api/agent/issues/{issueId}/comments', 'GET', { key: keys[p], params })).status).toBe(want);
			expect((await call('/api/agent/issues/{issueId}/occurrences', 'GET', { key: keys[p], params })).status).toBe(want);
			expect((await call('/api/agent/issues/{issueId}/activity', 'GET', { key: keys[p], params })).status).toBe(want);
			const o = await call('/api/agent/occurrences/{occurrenceId}', 'GET', { key: keys[p], params: { occurrenceId: occ[k] } });
			expect(o.status).toBe(want);
		});

		it('GET /traces/{id} shows only visible occurrences', async () => {
			const r = await call('/api/agent/traces/{traceId}', 'GET', { key: keys[p], params: { traceId: TRACE } });
			expect(r.status).toBe(200);
			expect(r.body.occurrences.map((o: { id: string }) => o.id).sort()).toEqual(VISIBLE[p].map((k) => occ[k]).sort());
		});

		it('POST /traces/{id}/pull runs under the owner scope: a trace with no visible occurrence is 404', async () => {
			// SPAN_PULL_SCOPES allow-lists org A with `web` as the core project (beforeAll).
			const hidden = await call('/api/agent/traces/{traceId}/pull', 'POST', { key: keys[p], params: { traceId: TRACE_STAGING } });
			expect(hidden.status).toBe(VISIBLE[p].includes('webStaging') ? 200 : 404);
			const shared = await call('/api/agent/traces/{traceId}/pull', 'POST', { key: keys[p], params: { traceId: TRACE } });
			expect(shared.status).toBe(PROJECTS[p].includes(pWeb) && VISIBLE[p].length > 0 ? 200 : 404);
		});

		it.each(ORG_A_ISSUES)('mutations on %s: write -> ok, read-only -> 403, invisible -> 404', async (k) => {
			const params = { issueId: issue[k] };
			const key = keys[p];
			const comment = await call('/api/agent/issues/{issueId}/comments', 'POST', { key, params, body: { body_md: `hi from ${p}` } });
			expect(comment.status).toBe(expectWrite(p, k, 201));
			if (comment.status === 201) {
				const commentId = comment.body.comment.id;
				const edit = await call('/api/agent/issues/{issueId}/comments/{commentId}', 'PATCH', {
					key,
					params: { ...params, commentId },
					body: { body_md: 'edited' },
				});
				expect(edit.status).toBe(200);
				const del = await call('/api/agent/issues/{issueId}/comments/{commentId}', 'DELETE', { key, params: { ...params, commentId } });
				expect(del.status).toBe(200);
			} else {
				const anyComment = randomUUID();
				const edit = await call('/api/agent/issues/{issueId}/comments/{commentId}', 'PATCH', {
					key,
					params: { ...params, commentId: anyComment },
					body: { body_md: 'x' },
				});
				expect(edit.status).toBe(expectWrite(p, k, 404));
				const del = await call('/api/agent/issues/{issueId}/comments/{commentId}', 'DELETE', { key, params: { ...params, commentId: anyComment } });
				expect(del.status).toBe(expectWrite(p, k, 404));
			}

			expect((await call('/api/agent/issues/{issueId}/status', 'PATCH', { key, params, body: { status: 'unresolved' } })).status).toBe(
				expectWrite(p, k, 200)
			);
			expect((await call('/api/agent/issues/{issueId}/claim', 'POST', { key, params })).status).toBe(expectWrite(p, k, 200));
			expect((await call('/api/agent/issues/{issueId}/claim', 'DELETE', { key, params })).status).toBe(expectWrite(p, k, 200));
			expect((await call('/api/agent/issues/{issueId}/progress', 'POST', { key, params, body: { message_md: 'working' } })).status).toBe(
				expectWrite(p, k, 201)
			);
			expect(
				(await call('/api/agent/issues/{issueId}/questions', 'POST', { key, params, body: { body_md: 'why?', audience: 'team' } })).status
			).toBe(expectWrite(p, k, 201));
			// system_error issue: a writer gets past the gate and is refused by validation (400), never by authorization.
			expect((await call('/api/agent/issues/{issueId}/report/severity', 'PATCH', { key, params, body: { severity: 'high' } })).status).toBe(
				expectWrite(p, k, 400)
			);
		});

		it('relations: link/unlink needs write on the source and visibility of the target', async () => {
			const key = keys[p];
			const params = { issueId: issue.webProd };
			const body = { target_issue_id: issue.coreProd, relation_type: 'linked_to' };
			// Source first ('write': invisible 404, read-only 403), then the target ('read': invisible 404).
			const want = !VISIBLE[p].includes('webProd') ? 404 : !CAN_WRITE[p] ? 403 : !VISIBLE[p].includes('coreProd') ? 404 : 201;
			const add = await call('/api/agent/issues/{issueId}/relations', 'POST', { key, params, body });
			expect(add.status).toBe(want);
			const rm = await call('/api/agent/issues/{issueId}/relations', 'DELETE', { key, params, body });
			expect(rm.status).toBe(want === 201 ? 200 : want);
			if (p === 'narrow') {
				// The target is outside the owner's grant: indistinguishable from a missing issue.
				const hidden = await call('/api/agent/issues/{issueId}/relations', 'POST', {
					key,
					params,
					body: { target_issue_id: issue.webStaging, relation_type: 'linked_to' },
				});
				expect(hidden.status).toBe(404);
			}
		});

		it('POST /batch authorizes every op exactly like its single route', async () => {
			const r = await call('/api/agent/batch', 'POST', {
				key: keys[p],
				body: {
					stopOnError: false,
					operations: ORG_A_ISSUES.map((k) => ({ op: 'issues.progress', issueId: issue[k], params: { message_md: 'batch' } })).concat([
						{ op: 'issues.progress', issueId: issue.foreign, params: { message_md: 'batch' } },
					]),
				},
			});
			expect(r.status).toBe(200);
			expect(r.body.results.map((x: { status: number }) => x.status)).toEqual([
				...ORG_A_ISSUES.map((k) => expectWrite(p, k, 201)),
				404,
			]);
		});

		it('POST /issues/claim-next is a write and never claims an invisible issue', async () => {
			const r = await call('/api/agent/issues/claim-next', 'POST', { key: keys[p], body: { project: pWeb } });
			if (!CAN_WRITE[p]) {
				expect(r.status).toBe(403);
				return;
			}
			expect(r.status).toBe(200);
			expect(VISIBLE[p].map((k) => issue[k])).toContain(r.body.issue.id);
			await call('/api/agent/issues/{issueId}/claim', 'DELETE', { key: keys[p], params: { issueId: r.body.issue.id } });
		});

		it('POST /uploads is a write', async () => {
			const form = new FormData();
			form.append('file', new Blob(['hello'], { type: 'text/plain' }), 'a.txt');
			const r = await call('/api/agent/uploads', 'POST', { key: keys[p], form });
			if (CAN_WRITE[p]) expect(r.status).not.toBe(403);
			else expect(r.status).toBe(403);
		});

		it('members and repo-credentials are denied to every personal key', async () => {
			for (const [route, method, extra] of [
				['/api/agent/members', 'GET', {}],
				['/api/agent/members', 'POST', { body: { email: `x-${suffix}@example.test`, role: 'viewer' } }],
				['/api/agent/members/{id}', 'DELETE', { params: { id: users.viewerNone } }],
				['/api/agent/repo-credentials', 'GET', {}],
			] as const) {
				const r = await call(route, method, { key: keys[p], ...(extra as object) });
				expect(r.status, `${method} ${route}`).toBe(403);
			}
			// The member really is still there.
			const [m] = await sql`select 1 as ok from organization_members where organization_id = ${orgA} and user_id = ${users.viewerNone}`;
			expect(m?.ok).toBe(1);
		});
	});

	describe('org agent (regression: unchanged behaviour)', () => {
		it('sees every issue of its org, writes, and may list members; logout is refused', async () => {
			const list = await call('/api/agent/issues', 'GET', { key: orgAgentKey });
			expect(list.body.issues.map((i: { id: string }) => i.id).sort()).toEqual(ORG_A_ISSUES.map((k) => issue[k]).sort());
			const self = await call('/api/agent/self', 'GET', { key: orgAgentKey });
			expect(self.body).toMatchObject({ kind: 'ai', owner: null });
			const comment = await call('/api/agent/issues/{issueId}/comments', 'POST', {
				key: orgAgentKey,
				params: { issueId: issue.webStaging },
				body: { body_md: 'org agent' },
			});
			expect(comment.status).toBe(201);
			expect((await call('/api/agent/members', 'GET', { key: orgAgentKey })).status).toBe(200);
			const trace = await call('/api/agent/traces/{traceId}', 'GET', { key: orgAgentKey, params: { traceId: TRACE } });
			expect(trace.body.occurrences).toHaveLength(3);
			expect((await call('/api/agent/key/revoke', 'POST', { key: orgAgentKey })).status).toBe(403);
			// Org agents see every linked event's target unredacted (unchanged behaviour).
			const ev = await call('/api/agent/events', 'GET', { key: orgAgentKey, query: '?limit=500&type=linked' });
			const linkedEvents = ev.body.events.filter((e: any) => e.issue.id === issue.webProd);
			const targets = linkedEvents.map((e: any) => e.newValue.targetIssueId);
			expect(targets).toEqual(expect.arrayContaining([issue.coreProd, issue.webStaging]));
			expect(linkedEvents.filter((e: any) => e.newValue.targetRedacted || e.newValue.targetIssueId === null)).toEqual([]);
			expect((await call('/api/agent/self', 'GET', { key: orgAgentKey })).status).toBe(200);
		});
	});

	describe('lifecycle: every change applies on the next request', () => {
		it('a role downgrade removes write; a new grant hides issues; removal from the org is a 401', async () => {
			const key = keys.lifecycle;
			const params = { issueId: issue.webStaging };
			expect((await call('/api/agent/issues/{issueId}/progress', 'POST', { key, params, body: { message_md: 'a' } })).status).toBe(201);

			await sql`update organization_members set role = 'viewer' where organization_id = ${orgA} and user_id = ${users.lifecycle}`;
			// A viewer with no project_members row now sees nothing at all.
			expect((await call('/api/agent/issues/{issueId}/progress', 'POST', { key, params, body: { message_md: 'b' } })).status).toBe(404);
			await sql`insert into project_members (project_id, user_id, role) values (${pWeb}, ${users.lifecycle}, 'viewer')`;
			expect((await call('/api/agent/issues/{issueId}/progress', 'POST', { key, params, body: { message_md: 'c' } })).status).toBe(403);

			await sql`update organization_members set role = 'engineer' where organization_id = ${orgA} and user_id = ${users.lifecycle}`;
			await sql`insert into project_environment_grants (user_id, project_id, environments, created_by)
			          values (${users.lifecycle}, ${pWeb}, ${sql.array(['production'])}, ${users.owner})`;
			expect((await call('/api/agent/issues/{issueId}', 'GET', { key, params })).status).toBe(404);
			await sql`delete from project_environment_grants where user_id = ${users.lifecycle}`;
			expect((await call('/api/agent/issues/{issueId}', 'GET', { key, params })).status).toBe(200);

			await sql`delete from organization_members where organization_id = ${orgA} and user_id = ${users.lifecycle}`;
			expect((await call('/api/agent/self', 'GET', { key })).status).toBe(401);
			expect((await call('/api/agent/issues', 'GET', { key })).status).toBe(401);
		});

		it('removing a member through the dashboard revokes their keys for good: re-adding them does not revive one', async () => {
			const key = keys.removed;
			expect((await call('/api/agent/self', 'GET', { key })).status).toBe(200);
			const { DELETE } = await import('../organizations/[orgId=uuid]/members/[memberId]/+server');
			const res = await (DELETE as any)({
				params: { orgId: orgA, memberId: users.removed },
				locals: { auth: async () => ({ user: { id: users.owner } }) },
			});
			expect(res.status).toBe(200);
			expect((await call('/api/agent/self', 'GET', { key })).status).toBe(401);
			const rows = await sql`select k.status from project_api_keys k join agents a on a.id = k.agent_id where a.owner_user_id = ${users.removed}`;
			expect(rows.map((r) => r.status)).toEqual(['revoked']);
			const [audit] = await sql`select metadata from audit_logs where action = 'cli.key.revoked' and actor_id = ${users.owner} and metadata->>'ownerUserId' = ${users.removed}`;
			expect(audit.metadata.via).toBe('member_removed');

			await sql`insert into organization_members (organization_id, user_id, role) values (${orgA}, ${users.removed}, 'engineer')`;
			expect((await call('/api/agent/self', 'GET', { key })).status).toBe(401);
		});

		it("owners/admins revoke a member's key from the Agents page; an engineer cannot; another org's key is 404", async () => {
			const key = await deviceLogin(users.support, orgA, 'agents-page');
			const self = await call('/api/agent/self', 'GET', { key });
			const keyId = self.body.key.id as string;
			const { DELETE } = await import('../organizations/[orgId=uuid]/cli-keys/[keyId=uuid]/+server');
			const as = (userId: string, orgId = orgA) =>
				(DELETE as any)({ params: { orgId, keyId }, locals: { auth: async () => ({ user: { id: userId } }) } }).then(
					(r: Response) => r.status,
					(e: { status: number }) => e.status
				);
			expect(await as(users.engineer)).toBe(403);
			expect(await as(users.outsider, orgB)).toBe(404);
			expect((await call('/api/agent/self', 'GET', { key })).status).toBe(200);
			expect(await as(users.admin)).toBe(200);
			expect((await call('/api/agent/self', 'GET', { key })).status).toBe(401);
			const [audit] = await sql`select metadata from audit_logs where action = 'cli.key.revoked' and resource_id = ${keyId}`;
			expect(audit.metadata).toMatchObject({ via: 'agents_page', ownerUserId: users.support });
		});

		it('key rotate: only the calling key, no grace (old key dead at once), the ORIGINAL expiry kept, logout leaves nothing alive', async () => {
			const old = await deviceLogin(users.engineer, orgA, 'rotate-box');
			const before = await call('/api/agent/self', 'GET', { key: old });
			// Pin the old key's expiry well inside the TTL, so "kept" and "renewed to +90 days" cannot be confused.
			const pinned = new Date(Date.now() + 10 * 86_400_000);
			pinned.setMilliseconds(0);
			await sql`update project_api_keys set expires_at = ${pinned} where id = ${before.body.key.id}`;
			const otherDevice = await deviceLogin(users.engineer, orgA, 'rotate-other');

			const r = await call('/api/agent/key/rotate', 'POST', { key: old });
			expect(r.status).toBe(200);
			const fresh = r.body.newKey.secret as string;

			// No grace period: the calling key is refused on the very next request, whatever AGENT_KEY_ROTATION_GRACE_HOURS says.
			expect((await call('/api/agent/self', 'GET', { key: old })).status).toBe(401);
			const after = await call('/api/agent/self', 'GET', { key: fresh });
			expect(after.status).toBe(200);
			expect(after.body.owner.userId).toBe(users.engineer);
			// No renewal: exactly the old expiry, not now + CLI_PERSONAL_KEY_TTL_DAYS.
			expect(after.body.key.expiresAt).toBe(pinned.toISOString());

			const live = (device: string) => sql`
				select k.id from project_api_keys k join agents a on a.id = k.agent_id
				where a.owner_user_id = ${users.engineer} and a.org_id = ${orgA} and k.name = ${device}
				  and k.status = 'active' and k.revoked_at is null and k.expires_at > now()`;
			// A device never holds two live keys, and another device's key is untouched.
			expect(await live('rotate-box')).toHaveLength(1);
			expect((await call('/api/agent/self', 'GET', { key: otherDevice })).status).toBe(200);

			// `sentinel logout` after a rotate leaves nothing alive for that device.
			expect((await call('/api/agent/key/revoke', 'POST', { key: fresh })).status).toBe(200);
			expect(await live('rotate-box')).toHaveLength(0);
			expect((await call('/api/agent/self', 'GET', { key: old })).status).toBe(401);
			expect((await call('/api/agent/self', 'GET', { key: fresh })).status).toBe(401);

			const [audit] = await sql`select metadata from audit_logs where action = 'agent.key.rotated' and resource_id = ${before.body.key.id}`;
			expect(audit.metadata).toMatchObject({ graceHours: 0, personal: true, expiresAt: pinned.toISOString() });
		});

		it('logout revokes the calling key (401 afterwards) and audits it', async () => {
			const key = await deviceLogin(users.admin, orgA, 'throwaway');
			const self = await call('/api/agent/self', 'GET', { key });
			const r = await call('/api/agent/key/revoke', 'POST', { key });
			expect(r).toEqual({ status: 200, body: { success: true, revokedKeyId: self.body.key.id } });
			expect((await call('/api/agent/self', 'GET', { key })).status).toBe(401);
			const [audit] = await sql`select actor_id, metadata from audit_logs where action = 'cli.key.revoked' and resource_id = ${self.body.key.id}`;
			expect(audit.metadata).toMatchObject({ via: 'cli_logout', ownerUserId: users.admin });
		});

		it('a re-login from the same device revokes the previous key of that device only', async () => {
			const { liveKeysReplacedByDevice } = await import('$lib/server/cli-personal-keys');
			expect(await liveKeysReplacedByDevice(users.owner, 'desk')).toEqual([]);
			const first = await deviceLogin(users.owner, orgA, 'desk');
			const other = await deviceLogin(users.owner, orgA, 'phone');
			// The approval page's "This replaces your CLI key for desk" comes from exactly this: one live key, in org A.
			const replaced = await liveKeysReplacedByDevice(users.owner, 'desk');
			expect(replaced).toEqual([{ organizationId: orgA, createdAt: expect.any(String) }]);
			const second = await deviceLogin(users.owner, orgA, 'desk');
			expect((await call('/api/agent/self', 'GET', { key: first })).status).toBe(401);
			expect((await call('/api/agent/self', 'GET', { key: second })).status).toBe(200);
			expect((await call('/api/agent/self', 'GET', { key: other })).status).toBe(200);
			// One personal agent per (org, user), shared by all devices.
			const agents = await sql`select id from agents where org_id = ${orgA} and owner_user_id = ${users.owner}`;
			expect(agents).toHaveLength(1);
		});
	});

	it('covers every /api/agent route and method', () => {
		const root = path.dirname(fileURLToPath(import.meta.url));
		const discovered: string[] = [];
		const walk = (dir: string, rel: string) => {
			for (const entry of readdirSync(dir)) {
				const full = path.join(dir, entry);
				if (statSync(full).isDirectory()) walk(full, `${rel}/${entry}`);
				else if (entry === '+server.ts') {
					const routePath = `/api/agent${rel.replace(/\[(\w+)(?:=\w+)?\]/g, '{$1}')}`;
					for (const m of readFileSync(full, 'utf8').matchAll(/export\s+const\s+(GET|POST|PATCH|DELETE)\b/g)) {
						discovered.push(`${routePath} ${m[1]}`);
					}
				}
			}
		};
		walk(root, '');
		expect([...ROUTE_TABLE].sort()).toEqual(discovered.sort());
	});
});
