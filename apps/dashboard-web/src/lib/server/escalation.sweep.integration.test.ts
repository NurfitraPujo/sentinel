import { describe, it, expect, beforeAll, afterAll, beforeEach, vi } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

/**
 * sentinel-user-report-issue §4.4 / §4.4b — the escalation chain, against a REAL migrated
 * Postgres, driven through `POST /api/cron/oncall-escalation`.
 *
 * This is §11's named boundary, quoted: *"The escalation timer sweep (§4.4b) | new — an
 * integration boundary at the cron route | Whether a deadline reassigns, whether a human
 * reassignment stops the chain, and whether `max_hops` terminates it are transitions across rows,
 * not a pure function. Driven through `POST /api/cron/oncall-escalation` with a controlled
 * clock."*
 *
 * **The controlled clock is the data's.** A 15-minute deadline cannot be waited out in a test, and
 * `now()` cannot be moved against a real Postgres — so every `ack_deadline_at` is written into the
 * past by the seeding helpers, which is the same technique `retention.claims.integration.test.ts`
 * uses for `created_at` and the same one that makes a 45-minute three-hop walk assertable in
 * milliseconds. The route itself is called exactly as the CronJob calls it, secret header and all.
 *
 * **A Drizzle double could not prove any of this.** Every claim below is about rows AFTER a
 * transition — `issues.assigned_to` moved, exactly one new `assigned` activity row exists, the
 * `notifications` row is of kind `escalated`, `hop` advanced by one, `exhausted` flipped — and a
 * mock that returns itself for every call cannot tell "did the right thing" from "did nothing"
 * (MANUAL_ISSUES_DESIGN §1, B10 addendum).
 *
 * It must run against a DISPOSABLE Postgres, never the shared dev database (CLAUDE.md):
 *
 *   podman run --rm -d --name sentinel-task06-pg -p 55432:5432 \
 *     -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme -e POSTGRES_DB=sentinel postgres:15-alpine
 *   cd packages/db-migrations && GOWORK=off \
 *     DB_URL_DASHBOARD=postgres://sentinel:changeme@localhost:55432/sentinel?sslmode=disable \
 *     go run ./cmd/migrate up -target dashboard
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:changeme@localhost:55432/sentinel \
 *     ESCALATION_INTEGRATION_REQUIRED=1 pnpm vitest run src/lib/server/escalation.sweep.integration.test.ts
 *
 * `DATABASE_URL` must be set EXPLICITLY, which is stricter than "probe the default and skip": the
 * default IS the compose stack's shared dev database, and probing alone would silently point these
 * writes at it.
 */
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.ESCALATION_INTEGRATION_REQUIRED === '1';
const explicitlyTargeted = process.env.DATABASE_URL !== undefined;

let dbReachable = false;
const probeSql = postgres(connectionString, { connect_timeout: 3, max: 1 });
try {
	await probeSql`select 1 from issue_escalations limit 1`;
	dbReachable = true;
} catch {
	dbReachable = false;
} finally {
	await probeSql.end({ timeout: 1 }).catch(() => {});
}

if (required && !(dbReachable && explicitlyTargeted)) {
	throw new Error(
		`ESCALATION_INTEGRATION_REQUIRED=1 but no migrated database was EXPLICITLY targeted at ${connectionString} (set DATABASE_URL). ` +
			'This file is the committed proof of §4.4b — that a deadline reassigns, that an ack stops ' +
			'the chain, that a human reassignment stops it, that max_hops terminates it and that a ' +
			'doubled sweep does not double-reassign. Skipping it silently would leave all five unproven.'
	);
}

/**
 * `$env/dynamic/private` is a SNAPSHOT of `process.env` taken when SvelteKit's virtual module is
 * first generated, not a live view of it — so a `process.env.CRON_SECRET = …` in `beforeAll`
 * reaches the route as `undefined` and the sweep answers `500 Cron endpoint not configured`. That
 * is the correct behaviour (the guard refuses rather than accepting anything), and it is also why
 * this file hands the module `process.env` itself: the values below have to be readable by the
 * route, and `DATABASE_URL` has to keep pointing at the disposable Postgres this run targeted
 * rather than silently falling back to the compose stack's shared dev database.
 */
process.env.CRON_SECRET = 'integration-cron-secret';
process.env.ESCALATION_ACK_MINUTES = '15';
process.env.ESCALATION_MAX_HOPS = '3';
vi.mock('$env/dynamic/private', () => ({ env: process.env }));

const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const projectId = randomUUID();
const scheduleId = randomUUID();
/**
 * FOUR engineers for a `max_hops` of three, deliberately. With exactly three the bound is
 * unfalsifiable: "the chain stopped because it hit max_hops" and "the chain stopped because it ran
 * out of untried members" produce identical rows, so an off-by-one in the bound would pass. The
 * fourth member is what makes the terminal test about §4.4's bound and nothing else.
 */
const ENGINEERS = ['esc-a-' + suffix, 'esc-b-' + suffix, 'esc-c-' + suffix, 'esc-d-' + suffix];
const CRON_SECRET = 'integration-cron-secret';

/**
 * The escalation instant every fixture is anchored on — two hours BEFORE the wall clock, not a
 * fixed literal. The sweep's due predicate is `ack_deadline_at <= now()`, and `now()` is the real
 * one against a real Postgres; a hard-coded date is due or not due depending on what day the suite
 * is run, which is the difference between "this test asserts a deadline reassigns" and "this test
 * asserts nothing on Tuesdays". The CONTROL is still the data's: every deadline below is placed
 * relative to this anchor, so a 45-minute three-hop walk still takes milliseconds.
 */
const T0 = new Date(Date.now() - 2 * 60 * 60 * 1000);
const minutes = (n: number) => new Date(T0.getTime() + n * 60_000);

let sql: ReturnType<typeof postgres>;
let SWEEP: (event: { request: Request; url: URL }) => Promise<Response>;
let escalation: typeof import('./escalation');

/** The CronJob's own call, header and all. */
function sweepRequest() {
	return {
		request: new Request('http://x/api/cron/oncall-escalation', {
			method: 'POST',
			headers: { 'x-cron-secret': CRON_SECRET },
		}),
		url: new URL('http://x/api/cron/oncall-escalation'),
	};
}

async function seedReport(): Promise<string> {
	const issueId = randomUUID();
	await sql`
		insert into issues (id, project_id, fingerprint, message, error_class, issue_type, source_channel)
		values (${issueId}, ${projectId}, ${'fp-' + issueId}, ${'Save spins forever'},
		        ${'UserReport'}, ${'user_report'}, ${'user_report_api'})
	`;
	return issueId;
}

/**
 * A chain already open at `hop`, with its deadline written into the past — the controlled clock.
 * Mirrors what `escalateReportAtIntake` leaves behind, including the assignment, so the sweep sees
 * exactly the state a live intake would have produced.
 */
async function seedChain(options: {
	issueId: string;
	hops: string[];
	deadlineAt: Date;
	acknowledgedAt?: Date | null;
}) {
	for (let i = 0; i < options.hops.length; i++) {
		const hop = i + 1;
		const last = i === options.hops.length - 1;
		await sql`
			insert into issue_escalations
				(id, issue_id, schedule_id, assignee_id, hop, escalated_at, ack_deadline_at, acknowledged_at)
			values (${randomUUID()}, ${options.issueId}, ${scheduleId}, ${options.hops[i]}, ${hop},
			        ${minutes(-60 + hop)}, ${last ? options.deadlineAt : minutes(-59 + hop)},
			        ${last ? (options.acknowledgedAt ?? null) : minutes(-58 + hop)})
		`;
	}
	await sql`
		update issues set assignee_type = 'user', assigned_to = ${options.hops[options.hops.length - 1]}
		where id = ${options.issueId}
	`;
}

async function escalationRows(issueId: string) {
	return await sql`
		select assignee_id, hop, acknowledged_at, exhausted, ack_deadline_at
		from issue_escalations where issue_id = ${issueId} order by hop
	`;
}

async function assignment(issueId: string) {
	const [row] = await sql`select assignee_type, assigned_to from issues where id = ${issueId}`;
	return row;
}

async function countWhere(table: string, issueId: string, extra = sql``) {
	const [row] = await sql`
		select count(*)::int as count from ${sql(table)} where issue_id = ${issueId} ${extra}
	`;
	return row.count as number;
}

describe.skipIf(!(dbReachable && explicitlyTargeted))(
	'§4.4b the escalation chain, driven through POST /api/cron/oncall-escalation',
	() => {
		beforeAll(async () => {
			sql = postgres(connectionString, { connect_timeout: 5, max: 4 });

			await sql`insert into organizations (id, name, slug) values (${orgId}, ${'esc-' + suffix}, ${'esc-' + suffix})`;
			await sql`
				insert into projects (id, name, organization_id, api_key, api_key_hash)
				values (${projectId}, ${'proj-' + suffix}, ${orgId}, ${'pk_' + suffix}, ${'hash_' + suffix})
			`;
			for (const userId of ENGINEERS) {
				await sql`insert into "user" (id, name, email) values (${userId}, ${userId}, ${userId + '@example.test'})`;
				await sql`
					insert into organization_members (id, organization_id, user_id, role)
					values (${randomUUID()}, ${orgId}, ${userId}, ${'engineer'})
				`;
			}
			// A daily rotation anchored well before T0, so `whoIsOnCall` resolves deterministically
			// and the rota's ORDER — which is what the chain walks — is the only thing under test.
			await sql`
				insert into oncall_schedules
					(id, organization_id, name, timezone, kind, handoff_time, active, created_by, rotation_anchor_at)
				values (${scheduleId}, ${orgId}, ${'Primary'}, ${'UTC'}, ${'daily'}, ${'09:00'}, true,
				        ${ENGINEERS[0]}, ${new Date(T0.getTime() - 7 * 24 * 60 * 60 * 1000)})
			`;
			for (let i = 0; i < ENGINEERS.length; i++) {
				await sql`
					insert into oncall_schedule_members (schedule_id, user_id, position)
					values (${scheduleId}, ${ENGINEERS[i]}, ${i})
				`;
			}

			({ POST: SWEEP } = (await import('../../routes/api/cron/oncall-escalation/+server')) as unknown as {
				POST: typeof SWEEP;
			});
			escalation = await import('./escalation');
		});

		afterAll(async () => {
			if (!sql) return;
			await sql`
				delete from issues where project_id = ${projectId}
			`;
			await sql`delete from oncall_schedules where organization_id = ${orgId}`;
			await sql`delete from organizations where id = ${orgId}`;
			await sql`delete from "user" where id in ${sql(ENGINEERS)}`;
			await sql.end({ timeout: 5 });
		});

		describe('escalation at intake (§4.4)', () => {
			it('writes all five rows, and running it again changes nothing', async () => {
				const issueId = await seedReport();

				const first = await escalation.escalateReportAtIntake({
					issueId,
					organizationId: orgId,
					at: T0,
					ackMinutes: 15,
				});

				expect(first.escalation).not.toBeNull();
				expect(first.escalation?.hop).toBe(1);
				// §4.4: `ack_deadline_at = escalated_at + ESCALATION_ACK_MINUTES`.
				expect(first.escalation?.ackDeadlineAt.getTime()).toBe(T0.getTime() + 15 * 60_000);
				expect(first.notified).toEqual([{ issueId, userId: first.escalation?.assigneeId }]);

				const assignee = first.escalation!.assigneeId;
				expect(await assignment(issueId)).toEqual({ assignee_type: 'user', assigned_to: assignee });

				const before = {
					escalations: await escalationRows(issueId),
					activity: await countWhere('issue_activity', issueId, sql`and event_type = 'assigned'`),
					subscriptions: await countWhere('issue_subscriptions', issueId),
					notifications: await countWhere('notifications', issueId, sql`and kind = 'escalated'`),
				};
				expect(before.escalations).toHaveLength(1);
				expect(before.activity).toBe(1);
				expect(before.subscriptions).toBe(1);
				expect(before.notifications).toBe(1);

				// The `assigned` row is attributed to `system`, not to a person nobody can name.
				const [activityRow] = await sql`
					select actor_type, actor_id from issue_activity
					where issue_id = ${issueId} and event_type = 'assigned'
				`;
				expect(activityRow).toEqual({ actor_type: 'system', actor_id: 'system' });

				// §4.4: "one of the four values the existing check allows. No new reason value."
				const [subscription] = await sql`
					select subscriber_type, subscriber_id, reason from issue_subscriptions where issue_id = ${issueId}
				`;
				expect(subscription).toEqual({
					subscriber_type: 'user',
					subscriber_id: assignee,
					reason: 'claimant',
				});

				// §4.4: "running it again on a report that already has an `issue_escalations` row is a
				// no-op" — asserted as SELECT-before and SELECT-after over all four tables, because
				// "no-op" has to mean no second activity row and no second notification either.
				const second = await escalation.escalateReportAtIntake({
					issueId,
					organizationId: orgId,
					at: minutes(30),
					ackMinutes: 15,
				});
				expect(second.escalation).toBeNull();
				expect(second.notified).toEqual([]);
				expect(await escalationRows(issueId)).toEqual(before.escalations);
				expect(
					await countWhere('issue_activity', issueId, sql`and event_type = 'assigned'`)
				).toBe(before.activity);
				expect(await countWhere('issue_subscriptions', issueId)).toBe(before.subscriptions);
				expect(await countWhere('notifications', issueId, sql`and kind = 'escalated'`)).toBe(
					before.notifications
				);
			});

			it('is a non-error for an organization with no active schedule', async () => {
				// §4.4: "No active schedule … escalation does not run; `escalation` is null in §4.2, a
				// metric increments, and the report is left unassigned for the existing pull loop."
				const otherOrg = randomUUID();
				const otherProject = randomUUID();
				await sql`insert into organizations (id, name, slug) values (${otherOrg}, ${'no-rota-' + suffix}, ${'no-rota-' + suffix})`;
				await sql`
					insert into projects (id, name, organization_id, api_key, api_key_hash)
					values (${otherProject}, ${'p2-' + suffix}, ${otherOrg}, ${'pk2_' + suffix}, ${'hash2_' + suffix})
				`;
				const issueId = randomUUID();
				await sql`
					insert into issues (id, project_id, fingerprint, message, error_class, issue_type, source_channel)
					values (${issueId}, ${otherProject}, ${'fp-' + issueId}, ${'no rota'}, ${'UserReport'},
					        ${'user_report'}, ${'user_report_api'})
				`;

				const metrics = await import('./metrics');
				metrics.resetMetricsForTest();

				const result = await escalation.escalateReportAtIntake({
					issueId,
					organizationId: otherOrg,
					at: T0,
				});

				expect(result.escalation).toBeNull();
				expect(await assignment(issueId)).toEqual({ assignee_type: null, assigned_to: null });
				expect(await escalationRows(issueId)).toHaveLength(0);

				const body = (await metrics.renderMetrics()).body;
				expect(body).toContain('sentinel_oncall_schedule_unresolved_total 1');

				await sql`delete from issues where id = ${issueId}`;
				await sql`delete from organizations where id = ${otherOrg}`;
			});
		});

		describe('the sweep (§4.4b)', () => {
			let metrics: typeof import('./metrics');

			beforeEach(async () => {
				metrics = await import('./metrics');
				metrics.resetMetricsForTest();

				// The sweep is GLOBAL — §4.4b gives it no per-issue scope, and correctly so. This
				// suite shares one database, so without this every test would also sweep whatever
				// its neighbours left open and its `due`/`reassigned` counters would depend on test
				// ORDER, which `--sequence.shuffle` deliberately randomises (MANUAL_ISSUES_DESIGN
				// §1, B13). Quiescing every open chain first makes each test's counters about its
				// own fixture, which is the only thing it is entitled to assert.
				await sql`update issue_escalations set acknowledged_at = now() where acknowledged_at is null`;
			});

			it('reassigns an unacknowledged deadline to the next untried member at hop+1', async () => {
				const issueId = await seedReport();
				await seedChain({ issueId, hops: [ENGINEERS[0]], deadlineAt: minutes(-5) });

				const res = await SWEEP(sweepRequest());
				expect(res.status).toBe(200);
				expect((await res.json()).result).toMatchObject({ due: 1, reassigned: 1 });

				const chain = await escalationRows(issueId);
				expect(chain).toHaveLength(2);
				expect(chain[1].hop).toBe(2);
				expect(chain[1].assignee_id).toBe(ENGINEERS[1]);
				expect(chain[1].acknowledged_at).toBeNull();
				// A FRESH deadline, not the old one carried forward.
				expect(new Date(chain[1].ack_deadline_at).getTime()).toBeGreaterThan(minutes(-5).getTime());

				// The assignment moved, and the new hop brought its own activity row and notification.
				expect(await assignment(issueId)).toEqual({
					assignee_type: 'user',
					assigned_to: ENGINEERS[1],
				});
				expect(await countWhere('issue_activity', issueId, sql`and event_type = 'assigned'`)).toBe(1);
				expect(await countWhere('notifications', issueId, sql`and kind = 'escalated'`)).toBe(1);

				// §4.4: "the previous assignee's subscription stays — they were told, and un-telling
				// them is worse than a stale row."
				const subs = await sql`
					select subscriber_id from issue_subscriptions where issue_id = ${issueId} order by created_at
				`;
				expect(subs.map((r) => r.subscriber_id)).toContain(ENGINEERS[1]);
			});

			it('running the sweep TWICE in a row does not double-reassign', async () => {
				const issueId = await seedReport();
				await seedChain({ issueId, hops: [ENGINEERS[0]], deadlineAt: minutes(-5) });

				await SWEEP(sweepRequest());
				const afterFirst = await escalationRows(issueId);
				const second = await SWEEP(sweepRequest());
				const afterSecond = await escalationRows(issueId);

				// The second run finds nothing due: the new hop's deadline is in the future, and the
				// superseded hop is no longer the latest.
				expect((await second.json()).result).toMatchObject({ due: 0, reassigned: 0 });
				expect(afterSecond).toEqual(afterFirst);
				expect(afterSecond).toHaveLength(2);
			});

			it('reassigns a row whose deadline was written by SQL, microseconds and all', async () => {
				// The regression guard for a defect a self-seeding suite cannot find on its own.
				// `timestamptz` keeps MICROSECONDS and a JS `Date` keeps milliseconds, so a claim
				// expressed as `ack_deadline_at = <the value we read back>` matches nothing for any
				// row this module did not write itself — an operator's backfill, a migration, this
				// very statement. Every other fixture here seeds its deadline from a JS Date and
				// would have gone on passing; this one writes it the way Postgres does.
				const issueId = await seedReport();
				await seedChain({ issueId, hops: [ENGINEERS[0]], deadlineAt: minutes(-5) });
				await sql`
					update issue_escalations
					set ack_deadline_at = now() - interval '5 minutes' - interval '137 microseconds'
					where issue_id = ${issueId}
				`;
				const [seeded] = await sql`
					select ack_deadline_at from issue_escalations where issue_id = ${issueId}
				`;
				// Prove the fixture is actually sub-millisecond, or the test proves nothing.
				expect(String(seeded.ack_deadline_at)).not.toBe(
					new Date(seeded.ack_deadline_at as string).toISOString()
				);

				const res = await SWEEP(sweepRequest());
				expect((await res.json()).result).toMatchObject({ due: 1, reassigned: 1 });
				expect(await escalationRows(issueId)).toHaveLength(2);
			});

			it('two sweeps running CONCURRENTLY reassign exactly once', async () => {
				// §4.4b: "the sweep is idempotent and safe to run concurrently with itself: it claims
				// each due row with a conditional update before acting, so a doubled schedule cannot
				// double-reassign." The sequential test above cannot see that guard — the second run
				// finds nothing due because the superseded hop is no longer the latest. Only two runs
				// racing on the SAME due row exercise the compare-and-set, and a doubled schedule
				// (the CronJob and the compose loop both pointed at one dashboard) is exactly that.
				const issueId = await seedReport();
				await seedChain({ issueId, hops: [ENGINEERS[0]], deadlineAt: minutes(-5) });

				const [a, b] = await Promise.all([SWEEP(sweepRequest()), SWEEP(sweepRequest())]);
				expect(a.status).toBe(200);
				expect(b.status).toBe(200);

				const chain = await escalationRows(issueId);
				expect(chain).toHaveLength(2);
				expect(chain[1].hop).toBe(2);
				// One reassignment means ONE of everything downstream of it — a second `assigned`
				// row or a second `escalated` notification would mean the pager moved twice.
				expect(await countWhere('issue_activity', issueId, sql`and event_type = 'assigned'`)).toBe(1);
				expect(await countWhere('notifications', issueId, sql`and kind = 'escalated'`)).toBe(1);

				// Between them the two runs report one reassignment and one loser.
				const reassigned =
					(await a.json()).result.reassigned + (await b.json()).result.reassigned;
				expect(reassigned).toBe(1);
			});

			it('an explicit acknowledgement stops the chain', async () => {
				const issueId = await seedReport();
				await seedChain({ issueId, hops: [ENGINEERS[0]], deadlineAt: minutes(-5) });

				const stamped = await escalation.acknowledgeEscalation(issueId, ENGINEERS[0], minutes(-4));
				expect(stamped?.acknowledgedAt?.getTime()).toBe(minutes(-4).getTime());

				const res = await SWEEP(sweepRequest());
				expect((await res.json()).result).toMatchObject({ due: 0, reassigned: 0 });
				expect(await escalationRows(issueId)).toHaveLength(1);
				expect(await assignment(issueId)).toEqual({
					assignee_type: 'user',
					assigned_to: ENGINEERS[0],
				});
			});

			it('an ack-by-comment from the assignee stops the chain, and one from anybody else does not', async () => {
				// §4.4's implicit acknowledgement, exercised through the same helper the comment,
				// claim and status routes call. The negative half is the load-bearing one: a comment
				// by a bystander must NOT stop somebody else's pager.
				const issueId = await seedReport();
				await seedChain({ issueId, hops: [ENGINEERS[0]], deadlineAt: minutes(-5) });

				await escalation.acknowledgeEscalationQuietly(issueId, ENGINEERS[2]);
				expect((await escalationRows(issueId))[0].acknowledged_at).toBeNull();

				await escalation.acknowledgeEscalationQuietly(issueId, ENGINEERS[0]);
				expect((await escalationRows(issueId))[0].acknowledged_at).not.toBeNull();

				const res = await SWEEP(sweepRequest());
				expect((await res.json()).result).toMatchObject({ due: 0, reassigned: 0 });
				expect(await escalationRows(issueId)).toHaveLength(1);
			});

			it('a human reassignment stops the chain and the timer writes nothing', async () => {
				const issueId = await seedReport();
				await seedChain({ issueId, hops: [ENGINEERS[0]], deadlineAt: minutes(-5) });
				// A person took it — §4.4: "If `issues.assigned_to` no longer matches the latest
				// `issue_escalations` row's assignee, a person has taken it."
				await sql`update issues set assigned_to = ${ENGINEERS[2]} where id = ${issueId}`;

				const before = await escalationRows(issueId);
				const res = await SWEEP(sweepRequest());
				const body = await res.json();

				expect(body.result).toMatchObject({ due: 0, reassigned: 0 });
				// Counted, so an operator can tell "a person answered" from "nothing was due" …
				expect(body.result.stoppedByReassignment).toBeGreaterThanOrEqual(1);
				// … and NOTHING was written.
				expect(await escalationRows(issueId)).toEqual(before);
				expect(await assignment(issueId)).toEqual({
					assignee_type: 'user',
					assigned_to: ENGINEERS[2],
				});
			});

			it('terminates at ESCALATION_MAX_HOPS: exhausted, the assignment stops moving, the counter increments', async () => {
				const issueId = await seedReport();
				// Two deadlines already walked; this is the third, and max_hops is 3.
				await seedChain({
					issueId,
					hops: [ENGINEERS[0], ENGINEERS[1], ENGINEERS[2]],
					deadlineAt: minutes(-5),
				});

				const res = await SWEEP(sweepRequest());
				expect((await res.json()).result).toMatchObject({ due: 1, exhausted: 1, reassigned: 0 });

				const chain = await escalationRows(issueId);
				expect(chain).toHaveLength(3);
				expect(chain[2].exhausted).toBe(true);
				// §4.4: "`assigned_to` stays on the last engineer tried."
				expect(await assignment(issueId)).toEqual({
					assignee_type: 'user',
					assigned_to: ENGINEERS[2],
				});

				const body = (await metrics.renderMetrics()).body;
				expect(body).toContain('sentinel_report_escalations_exhausted_total 1');

				// And it does not loop the rotation forever: a further sweep finds nothing.
				const again = await SWEEP(sweepRequest());
				expect((await again.json()).result).toMatchObject({ due: 0, exhausted: 0 });
				expect(await escalationRows(issueId)).toHaveLength(3);
			});

			it('stops at ESCALATION_MAX_HOPS even though an untried member is still available', async () => {
				// The bound is §4.4's, not an accident of rota size: four eligible members are on the
				// rota, three have been tried, and the chain must still stop. "It does not loop the
				// rotation forever — a pager that keeps moving is a pager nobody owns."
				const issueId = await seedReport();
				await seedChain({
					issueId,
					hops: [ENGINEERS[0], ENGINEERS[1], ENGINEERS[2]],
					deadlineAt: minutes(-5),
				});

				await SWEEP(sweepRequest());

				const chain = await escalationRows(issueId);
				expect(chain).toHaveLength(3);
				expect(chain[2].exhausted).toBe(true);
				// ENGINEERS[3] is eligible, untried, and next in the rota — and is never reached.
				expect(chain.map((r) => r.assignee_id)).not.toContain(ENGINEERS[3]);
				expect(await assignment(issueId)).toEqual({
					assignee_type: 'user',
					assigned_to: ENGINEERS[2],
				});
			});

			it('two concurrent sweeps count ONE exhaustion, not two', async () => {
				// The terminal transition has no unique index behind it — nothing stops a second
				// `SET exhausted = true` from succeeding — so its conditional claim is the ONLY
				// guard, and §12's "nobody answered" alert fires on the counter this would inflate.
				const issueId = await seedReport();
				await seedChain({
					issueId,
					hops: [ENGINEERS[0], ENGINEERS[1], ENGINEERS[2]],
					deadlineAt: minutes(-5),
				});

				const [a, b] = await Promise.all([SWEEP(sweepRequest()), SWEEP(sweepRequest())]);
				const exhausted =
					(await a.json()).result.exhausted + (await b.json()).result.exhausted;
				expect(exhausted).toBe(1);
				expect((await metrics.renderMetrics()).body).toContain(
					'sentinel_report_escalations_exhausted_total 1'
				);
			});

			it('walks the whole chain hop by hop, never handing a report back to somebody already tried', async () => {
				const issueId = await seedReport();
				await seedChain({ issueId, hops: [ENGINEERS[0]], deadlineAt: minutes(-5) });

				// Three sweeps, each preceded by expiring the current deadline — the controlled clock
				// standing in for 45 real minutes.
				const seen: string[] = [ENGINEERS[0]];
				for (let i = 0; i < 3; i++) {
					await sql`
						update issue_escalations set ack_deadline_at = ${minutes(-5)}
						where issue_id = ${issueId}
						  and hop = (select max(hop) from issue_escalations where issue_id = ${issueId})
					`;
					await SWEEP(sweepRequest());
					const chain = await escalationRows(issueId);
					seen.push(chain[chain.length - 1].assignee_id as string);
				}

				const chain = await escalationRows(issueId);
				expect(chain).toHaveLength(3);
				expect(chain.map((r) => r.hop)).toEqual([1, 2, 3]);
				// Every hop is a DIFFERENT engineer, and the last state is terminal — three of the
				// four on the rota, because the bound is max_hops and not the rota's length.
				expect(new Set(chain.map((r) => r.assignee_id)).size).toBe(3);
				expect(chain[2].exhausted).toBe(true);
			});
		});
	}
);
