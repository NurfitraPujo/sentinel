import { describe, it, expect, beforeAll, afterAll, vi } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';
import { isHttpError } from '@sveltejs/kit';

/**
 * sentinel-issue-activity TASK-01 — the issue activity history against a REAL migrated Postgres: keyset paging, the
 * `occurrence_burst` collapse rule (including runs that cross a page and a read chunk), actor names (user, nameless user,
 * org agent, personal CLI agent, system, deleted), comment excerpts, the index the scan uses (EXPLAIN), the session
 * route's access rule against the issue page's own loader, and that the issue page's default load did not gain a
 * statement.
 *
 * Point DATABASE_URL at a DISPOSABLE database -- never the shared dev one (CLAUDE.md):
 *
 *   podman run -d --name iah-pg -p 127.0.0.1:55543:5432 -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme \
 *     -e POSTGRES_DB=sentinel postgres:16-alpine
 *   cd packages/db-migrations && GOWORK=off DB_URL_DASHBOARD='postgres://sentinel:changeme@127.0.0.1:55543/sentinel?sslmode=disable' \
 *     go run ./cmd/migrate up -target dashboard
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:changeme@127.0.0.1:55543/sentinel \
 *     ISSUE_ACTIVITY_INTEGRATION_REQUIRED=1 pnpm vitest run src/lib/db/queries/issue-activity.integration.test.ts
 *
 * Skipped when nothing answers; ISSUE_ACTIVITY_INTEGRATION_REQUIRED=1 (set in CI) makes that a failure.
 */

const connectionString = process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.ISSUE_ACTIVITY_INTEGRATION_REQUIRED === '1';

// Every statement the app's `db` sends, so the issue page's default load can be counted (and searched).
const statements = vi.hoisted(() => ({ log: [] as string[], on: false }));
vi.mock('$env/dynamic/private', () => ({ env: process.env }));
vi.mock('$lib/server/db', async () => {
	const { default: pg } = await import('postgres');
	const { drizzle } = await import('drizzle-orm/postgres-js');
	const types = {
		jsonb: {
			to: 3802,
			from: [3802],
			serialize: (v: unknown) => (typeof v === 'string' ? v : JSON.stringify(v)),
			parse: (v: string) => JSON.parse(v),
		},
	};
	const client = pg(process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel', {
		types,
		max: 4,
		debug: (_conn: number, query: string) => {
			if (statements.on) statements.log.push(query);
		},
	});
	return { db: drizzle(client), openRetentionRunClient: () => { throw new Error('not in this test'); } };
});

let dbReady = false;
const probe = postgres(connectionString, { connect_timeout: 3, max: 1 });
try {
	const [row] = await probe`select to_regclass('issue_activity')::text as t, to_regclass('cli_device_authorizations')::text as c`;
	dbReady = row?.t === 'issue_activity' && row?.c === 'cli_device_authorizations';
} catch {
	dbReady = false;
} finally {
	await probe.end({ timeout: 1 }).catch(() => {});
}
if (required && !dbReady) {
	throw new Error(`ISSUE_ACTIVITY_INTEGRATION_REQUIRED=1 but no migrated database answered at ${connectionString}.`);
}

const { listIssueActivityPage, activityChunkQuery, burstTailQuery, decodeActivityCursor, InvalidActivityCursorError } = await import(
	'./issue-activity'
);
const { db } = await import('$lib/server/db');
const { sql: dsql } = await import('drizzle-orm');

const suffix = randomUUID().slice(0, 8);
const orgA = randomUUID();
const orgB = randomUUID();
const project = randomUUID();
const projectB = randomUUID();
const users = {
	owner: randomUUID(), // "Ada Lovelace", owns the personal agent
	engineer: randomUUID(), // named
	nameless: randomUUID(), // no name: shown by email
	narrow: randomUUID(), // engineer granted staging only
	outsider: randomUUID(), // member of org B only
	gone: randomUUID(), // deleted before the read
};
const orgAgent = randomUUID();
const personalAgent = randomUUID();
const issue = randomUUID(); // production, the 300-burst timeline
const ties = randomUUID(); // production, same-timestamp and microsecond rows
const filler = randomUUID(); // production, bulk rows for the planner
const foreignIssue = randomUUID(); // org B
const comment = randomUUID();
const deletedComment = randomUUID();

// The timeline, oldest first. 300 bursts in three runs (120, 10, 170) between seven other events.
type Ev = { type: string; actorType: string; actorId: string; old?: unknown; new?: unknown; burstCount?: number };
const TIMELINE: Ev[] = [];
TIMELINE.push({ type: 'created', actorType: 'system', actorId: 'sentinel-processor', new: { errorClass: 'TypeError', projectId: project } });
for (let i = 0; i < 120; i++) TIMELINE.push({ type: 'occurrence_burst', actorType: 'system', actorId: 'sentinel-processor', burstCount: 2 + i });
TIMELINE.push({ type: 'claimed', actorType: 'agent', actorId: personalAgent, new: { assigneeType: 'agent', assignedTo: personalAgent } });
for (let i = 0; i < 10; i++) TIMELINE.push({ type: 'occurrence_burst', actorType: 'system', actorId: 'sentinel-processor', burstCount: 200 + i });
TIMELINE.push({ type: 'commented', actorType: 'agent', actorId: personalAgent, new: { commentId: comment, parentId: null } });
TIMELINE.push({ type: 'status_changed', actorType: 'user', actorId: users.engineer, old: { status: 'unresolved', waitingOn: null }, new: { status: 'resolved', resolvedInVersion: '1.4.0' } });
TIMELINE.push({ type: 'regressed', actorType: 'system', actorId: 'sentinel-regression-detector', new: { releaseVersion: '1.5.0', previousResolvedVersion: '1.4.0' } });
for (let i = 0; i < 170; i++) TIMELINE.push({ type: 'occurrence_burst', actorType: 'system', actorId: 'sentinel-processor', burstCount: 300 + i });
TIMELINE.push({ type: 'commented', actorType: 'user', actorId: users.nameless, new: { commentId: deletedComment, parentId: null } });
TIMELINE.push({ type: 'progress_update', actorType: 'agent', actorId: orgAgent, new: { messageMd: 'Bisected to the cache layer' } });
TIMELINE.push({ type: 'claim_released', actorType: 'system', actorId: 'system', new: { previousAssignee: personalAgent, reason: 'stale' } });

// What the collapsed history must be, newest first.
const EXPECTED = [
	['claim_released', 1],
	['progress_update', 1],
	['commented', 1],
	['occurrence_burst', 170],
	['regressed', 1],
	['status_changed', 1],
	['commented', 1],
	['occurrence_burst', 10],
	['claimed', 1],
	['occurrence_burst', 120],
	['created', 1],
] as const;

let sql: ReturnType<typeof postgres>;

async function readAll(limit: number, issueId = issue) {
	const pages = [];
	let before: string | null = null;
	for (let i = 0; i < 1000; i++) {
		const page = await listIssueActivityPage({ issueId, organizationId: orgA, limit, before });
		pages.push(page);
		if (!page.hasMore) break;
		before = page.nextCursor;
	}
	return pages;
}

const shape = (entries: { eventType: string; burst?: { rows: number } }[]) => entries.map((e) => [e.eventType, e.burst?.rows ?? 1]);

describe.skipIf(!dbReady)('issue activity history (integration, real Postgres)', () => {
	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 2 });
		for (const [id, label] of [[orgA, 'a'], [orgB, 'b']] as const) {
			await sql`insert into organizations (id, name, slug) values (${id}, ${`iah-${label}-${suffix}`}, ${`iah-${label}-${suffix}`})`;
		}
		for (const [id, org, label] of [[project, orgA, 'web'], [projectB, orgB, 'b']] as const) {
			await sql`insert into projects (id, name, organization_id, api_key, api_key_hash)
			          values (${id}, ${`iah-${label}-${suffix}`}, ${org}, ${`k_${label}_${suffix}`}, ${`h_${label}_${suffix}`})`;
		}
		const names: Record<keyof typeof users, string | null> = {
			owner: 'Ada Lovelace', engineer: 'Grace Hopper', nameless: null, narrow: 'Narrow Nora', outsider: 'Out Sider', gone: 'Gone Gary',
		};
		for (const [k, id] of Object.entries(users)) {
			await sql`insert into "user" (id, name, email) values (${id}, ${names[k as keyof typeof users]}, ${`iah-${k}-${suffix}@example.test`})`;
		}
		for (const k of ['owner', 'engineer', 'nameless', 'narrow', 'gone'] as const) {
			await sql`insert into organization_members (organization_id, user_id, role) values (${orgA}, ${users[k]}, ${k === 'owner' ? 'owner' : 'engineer'})`;
		}
		await sql`insert into organization_members (organization_id, user_id, role) values (${orgB}, ${users.outsider}, 'owner')`;
		await sql`insert into project_environment_grants (user_id, project_id, environments, created_by)
		          values (${users.narrow}, ${project}, ${sql.array(['staging'])}, ${users.owner})`;
		await sql`insert into agents (id, org_id, name, kind, created_by) values (${orgAgent}, ${orgA}, 'Triage Bot', 'ai', ${users.owner})`;
		// The stored name is stale on purpose: the owner's CURRENT name is what renders.
		await sql`insert into agents (id, org_id, name, kind, created_by, owner_user_id)
		          values (${personalAgent}, ${orgA}, 'Ada Byron (CLI)', 'personal', ${users.owner}, ${users.owner})`;

		for (const [id, proj, env] of [[issue, project, 'production'], [ties, project, 'production'], [filler, project, 'production'], [foreignIssue, projectB, 'production']] as const) {
			await sql`insert into issues (id, project_id, environment, fingerprint, message, error_class)
			          values (${id}, ${proj}, ${env}, ${`fp-${id}`}, ${`iah ${suffix}`}, 'TypeError')`;
		}
		await sql`insert into issue_comments (id, issue_id, author_type, author_id, body_md)
		          values (${comment}, ${issue}, 'agent', ${personalAgent}, ${'Looking into this now. ' + 'x'.repeat(400)})`;

		// One row per minute, oldest first, so the expected order is the insertion order.
		const rows = TIMELINE.map((e, i) => ({
			issue_id: issue,
			event_type: e.type,
			actor_type: e.actorType,
			actor_id: e.actorId,
			old_value: e.old === undefined ? null : sql.json(e.old as never),
			new_value:
				e.type === 'occurrence_burst'
					? sql.json({ count: e.burstCount!, lastSeen: '2026-01-01T00:00:00Z' })
					: e.new === undefined
						? null
						: sql.json(e.new as never),
			minutes: TIMELINE.length - i,
		}));
		for (const r of rows) {
			await sql`insert into issue_activity (issue_id, event_type, actor_type, actor_id, old_value, new_value, created_at)
			          values (${r.issue_id}, ${r.event_type}, ${r.actor_type}, ${r.actor_id}, ${r.old_value}, ${r.new_value},
			                  now() - make_interval(mins => ${r.minutes}))`;
		}

		// `ties`: three rows written in ONE statement share created_at (seq breaks the tie, newest = highest seq), then two
		// rows one MICROSECOND apart (a JS Date would truncate both to the same millisecond).
		await sql`insert into issue_activity (issue_id, event_type, actor_type, actor_id, new_value, created_at)
		          select ${ties}, 'progress_update', 'agent', ${orgAgent}, jsonb_build_object('messageMd', 'tie ' || g), '2026-03-01T10:00:00.500000Z'::timestamptz
		            from generate_series(1, 3) g`;
		await sql`insert into issue_activity (issue_id, event_type, actor_type, actor_id, new_value, created_at)
		          values (${ties}, 'progress_update', 'agent', ${orgAgent}, ${sql.json({ messageMd: 'micro 1' })}, '2026-03-01T10:00:00.123456Z'),
		                 (${ties}, 'progress_update', 'agent', ${orgAgent}, ${sql.json({ messageMd: 'micro 2' })}, '2026-03-01T10:00:00.123457Z')`;

		// Bulk rows on other issues, so the planner sees a table worth an index.
		await sql`insert into issue_activity (issue_id, event_type, actor_type, actor_id, new_value, created_at)
		          select ${filler}, 'occurrence_burst', 'system', 'sentinel-processor', jsonb_build_object('count', g), now() - make_interval(mins => g)
		            from generate_series(1, 20000) g`;
		await sql`insert into issue_activity (issue_id, event_type, actor_type, actor_id, created_at)
		          values (${foreignIssue}, 'created', 'system', 'sentinel-processor', now())`;
		await sql`analyze issue_activity`;

		// A user that has since been deleted: their activity row stays (actor_id is not a foreign key).
		await sql`insert into issue_activity (issue_id, event_type, actor_type, actor_id, created_at)
		          values (${ties}, 'question_answered', 'user', ${users.gone}, '2026-02-01T00:00:00Z')`;
		await sql`delete from "user" where id = ${users.gone}`;
	}, 120_000);

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from organizations where id in ${sql([orgA, orgB])}`;
			await sql`delete from "user" where id in ${sql(Object.values(users))}`;
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	it('one page holds the whole history, newest first, every burst run collapsed (300 rows -> 3 entries)', async () => {
		const page = await listIssueActivityPage({ issueId: issue, organizationId: orgA, limit: 50 });
		expect(shape(page.entries)).toEqual(EXPECTED);
		expect(page.hasMore).toBe(false);
		expect(page.nextCursor).toBeNull();
		const runs = page.entries.filter((e) => e.burst);
		expect(runs.reduce((n, e) => n + e.burst!.rows, 0)).toBe(300);
		// The newest run: rows 300..469 of the processor's count, and its window spans the run.
		const newest = runs[0].burst!;
		expect([newest.occurrencesFrom, newest.occurrencesTo]).toEqual([300, 469]);
		expect(Date.parse(newest.firstAt)).toBeLessThan(Date.parse(newest.lastAt));
		expect(runs[0].createdAt).toBe(newest.lastAt);
		expect(runs.map((r) => [r.burst!.occurrencesFrom, r.burst!.occurrencesTo])).toEqual([[300, 469], [200, 209], [2, 121]]);
	});

	it.each([1, 2, 3, 4, 7, 11, 200])('limit=%i: pages concatenate to exactly the one-page history; no run is split or counted twice', async (limit) => {
		const pages = await readAll(limit);
		const all = pages.flatMap((p) => p.entries);
		expect(shape(all)).toEqual(EXPECTED);
		expect(new Set(all.map((e) => e.id)).size).toBe(all.length);
		for (const p of pages.slice(0, -1)) {
			expect(p.entries).toHaveLength(limit);
			expect(p.hasMore).toBe(true);
			expect(p.nextCursor).toEqual(expect.any(String));
		}
		expect(pages.at(-1)!.hasMore).toBe(false);
		expect(pages).toHaveLength(Math.ceil(EXPECTED.length / limit));
	});

	it('a run that outlasts the read chunk is finished by the tail aggregate, not split (limit=2: 3-row chunks, a 170-row run)', async () => {
		const first = await listIssueActivityPage({ issueId: issue, organizationId: orgA, limit: 3 });
		expect(shape(first.entries)).toEqual(EXPECTED.slice(0, 3));
		const second = await listIssueActivityPage({ issueId: issue, organizationId: orgA, limit: 2, before: first.nextCursor });
		expect(shape(second.entries)).toEqual([['occurrence_burst', 170], ['regressed', 1]]);
		expect([second.entries[0].burst!.occurrencesFrom, second.entries[0].burst!.occurrencesTo]).toEqual([300, 469]);
		// The cursor sits past the whole run: the next page starts at the next non-burst row.
		const third = await listIssueActivityPage({ issueId: issue, organizationId: orgA, limit: 1, before: second.nextCursor });
		expect(third.entries[0].eventType).toBe('status_changed');
	});

	it('orders same-timestamp rows by seq and pages across them, and across microsecond-apart rows, without loss', async () => {
		const pages = await readAll(1, ties);
		const texts = pages.flatMap((p) => p.entries).map((e) => (e.newValue as { messageMd?: string } | null)?.messageMd ?? e.eventType);
		expect(texts).toEqual(['tie 3', 'tie 2', 'tie 1', 'micro 2', 'micro 1', 'question_answered']);
		const cursor = decodeActivityCursor(pages[3].nextCursor!);
		expect(cursor.at).toBe('2026-03-01T10:00:00.123457Z');
	});

	it('names every actor: personal agent as "<owner> (CLI)" from the live name, org agent, user, nameless user, system, deleted', async () => {
		const page = await listIssueActivityPage({ issueId: issue, organizationId: orgA, limit: 50 });
		const by = (t: string, i = 0) => page.entries.filter((e) => e.eventType === t)[i];
		expect(by('claimed').actor).toEqual({ type: 'agent', id: personalAgent, name: 'Ada Lovelace (CLI)', personal: true });
		expect(by('progress_update').actor).toEqual({ type: 'agent', id: orgAgent, name: 'Triage Bot', personal: false });
		expect(by('status_changed').actor).toEqual({ type: 'user', id: users.engineer, name: 'Grace Hopper', personal: false });
		expect(by('commented', 0).actor.name).toBe(`iah-nameless-${suffix}@example.test`);
		expect(by('regressed').actor.name).toBe('Regression detector');
		expect(by('occurrence_burst').actor.name).toBe('Sentinel');
		expect(by('claim_released').actor.name).toBe('System');
		const gone = (await readAll(50, ties)).flatMap((p) => p.entries).find((e) => e.eventType === 'question_answered')!;
		expect(gone.actor).toEqual({ type: 'user', id: users.gone, name: null, personal: false });
		// No entry shows a raw id where a name exists.
		for (const e of page.entries) expect(e.actor.name).not.toBe(e.actorId);
	});

	it('an agent of ANOTHER organization is not resolved to a name', async () => {
		const page = await listIssueActivityPage({ issueId: issue, organizationId: orgB, limit: 50 });
		expect(page.entries.find((e) => e.eventType === 'claimed')!.actor.name).toBeNull();
	});

	it('carries a comment excerpt (280 chars) for a live comment and marks a deleted one', async () => {
		const page = await listIssueActivityPage({ issueId: issue, organizationId: orgA, limit: 50 });
		const [deleted, live] = page.entries.filter((e) => e.eventType === 'commented');
		expect(live.comment).toEqual({ id: comment, excerpt: expect.stringMatching(/^Looking into this now\. x+$/), deleted: false });
		expect(live.comment!.excerpt).toHaveLength(280);
		expect(deleted.comment).toEqual({ id: deletedComment, excerpt: null, deleted: true });
	});

	it('refuses a cursor it did not issue', async () => {
		for (const bad of ['garbage', Buffer.from('2026-01-01|1').toString('base64url'), Buffer.from('x|y|z').toString('base64url')]) {
			await expect(listIssueActivityPage({ issueId: issue, organizationId: orgA, limit: 5, before: bad })).rejects.toBeInstanceOf(
				InvalidActivityCursorError
			);
		}
	});

	it('EXPLAIN: the page scan and the burst-tail aggregate both use idx_issue_activity_issue_created', async () => {
		const pos = { at: '2026-12-31T00:00:00.000000Z', seq: 9_000_000_000 };
		const plans: string[] = [];
		for (const q of [activityChunkQuery(issue, null, 51), activityChunkQuery(issue, pos, 51), burstTailQuery(issue, pos)]) {
			const rows = (await db.execute(dsql`EXPLAIN (FORMAT TEXT) ${q}`)) as unknown as { 'QUERY PLAN': string }[];
			const plan = rows.map((r) => r['QUERY PLAN']).join('\n');
			plans.push(plan);
			expect(plan).toContain('idx_issue_activity_issue_created');
			expect(plan).not.toMatch(/Seq Scan on issue_activity/);
		}
		// Printed for the change's record (the walkthrough quotes it).
		console.log(plans.join('\n----\n'));
	});

	describe('GET /api/issues/{issueId}/activity: exactly the issue page\'s access rule', () => {
		const route = () => import('../../../routes/api/issues/[issueId=uuid]/activity/+server');
		const page = () => import('../../../routes/issues/[id=uuid]/+page.server');

		async function status(fn: () => Promise<unknown>): Promise<number> {
			try {
				const r = await fn();
				return r instanceof Response ? r.status : 200;
			} catch (err) {
				if (isHttpError(err)) return err.status;
				throw err;
			}
		}
		const locals = (userId: string) => ({ auth: async () => ({ user: { id: userId } }) });
		const callRoute = async (userId: string, issueId: string, query = '') => {
			const { GET } = await route();
			const url = new URL(`http://localhost/api/issues/${issueId}/activity${query}`);
			return (GET as any)({ params: { issueId }, locals: locals(userId), url });
		};
		const callPage = async (userId: string, issueId: string) => {
			const { load } = await page();
			return (load as any)({ params: { id: issueId }, locals: locals(userId), url: new URL(`http://localhost/issues/${issueId}`) });
		};

		// The page's own answers: the environment dimension is a 404 (decision 20); a caller outside the issue's
		// organization fails the PROJECT dimension, which keeps requireProjectAccess's 403 on the page too.
		it.each([
			['a member', 'engineer', 'issue', 200],
			['a member narrowed to staging (production issue)', 'narrow', 'issue', 404],
			['a member of another organization', 'outsider', 'issue', 403],
			['an org-A member, org-B issue', 'engineer', 'foreignIssue', 403],
		] as const)('%s: the route answers what the page answers (%s on %s -> %i)', async (_label, who, which, want) => {
			const issueId = which === 'issue' ? issue : foreignIssue;
			const routeStatus = await status(() => callRoute(users[who], issueId));
			const pageStatus = await status(() => callPage(users[who], issueId));
			expect(routeStatus).toBe(pageStatus);
			expect(routeStatus).toBe(want);
		});

		it('serves pages, collapsed, with names; rejects a bad limit/cursor with 400', async () => {
			const res: Response = await callRoute(users.engineer, issue, '?limit=4');
			expect(res.status).toBe(200);
			const body = await res.json();
			expect(shape(body.entries)).toEqual(EXPECTED.slice(0, 4));
			expect(body.hasMore).toBe(true);
			const next: Response = await callRoute(users.engineer, issue, `?limit=4&before=${body.nextCursor}`);
			expect(shape((await next.json()).entries)).toEqual(EXPECTED.slice(4, 8));
			expect(await status(() => callRoute(users.engineer, issue, '?limit=abc'))).toBe(400);
			expect(await status(() => callRoute(users.engineer, issue, '?before=nope'))).toBe(400);
		});

		it("the issue page's default load runs NO activity statement, and exactly as many statements as before this change", async () => {
			statements.log.length = 0;
			statements.on = true;
			try {
				await callPage(users.engineer, issue);
			} finally {
				statements.on = false;
			}
			expect(statements.log.filter((q) => /issue_activity/i.test(q))).toEqual([]);
			// Measured on the loader before the Activity section existed (f5c04be) and unchanged by it: the section reads
			// /api/issues/{id}/activity from the browser, only when opened.
			// postgres.js's own per-connection type lookup is not an application statement (and varies with pool warmth).
			const app = statements.log.filter((q) => !q.includes('pg_catalog.pg_type'));
			expect(app).toHaveLength(DEFAULT_LOAD_STATEMENTS);
		});
	});
});

/**
 * The issue page's default SSR load (an issue with no occurrences; span pull not allow-listed): the issue; project access
 * (project, membership); environment scope; the releases picker; the canWrite probe (membership); then the parallel
 * reads -- project, occurrence total, environment display name, environment scopes + relations (2), tag facets, trend,
 * occurrences. 15, measured identically on the f5c04be loader (sentinel-issue-activity TASK-01 adds none: the Activity
 * section reads from the browser).
 */
const DEFAULT_LOAD_STATEMENTS = 15;
