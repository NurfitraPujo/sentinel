import { describe, it, expect, beforeAll, afterAll } from 'vitest';
import postgres from 'postgres';
import { PgDialect } from 'drizzle-orm/pg-core';
import { randomBytes, randomUUID } from 'node:crypto';

/**
 * sentinel-occurrence-spans TASK-01/02 — the spans status helper and the on-demand pull, against a REAL migrated Postgres,
 * because every claim is a claim about SQL: that "has spans" is the waterfall's own guard, that the EXISTS probes the spans
 * primary key on every partition, that a request writes (or declines to write) exactly the queue row each state calls for,
 * that nothing touches the per-issue throttle, and that the hourly limit holds when requests race.
 *
 * Point DATABASE_URL at a DISPOSABLE database — never the shared dev one (CLAUDE.md):
 *
 *   cd packages/db-migrations && GOWORK=off DB_URL_DASHBOARD='postgres://sentinel:<pw>@127.0.0.1:<port>/sentinel?sslmode=disable' \
 *     go run ./cmd/migrate up -target dashboard
 *   cd apps/dashboard-web && DATABASE_URL=postgres://sentinel:<pw>@127.0.0.1:<port>/sentinel \
 *     SPAN_PULL_INTEGRATION_REQUIRED=1 pnpm vitest run src/lib/server/span-pull.integration.test.ts
 *
 * Skipped when nothing answers (or the on-demand migration 1725800000 is missing); SPAN_PULL_INTEGRATION_REQUIRED=1 makes
 * that a hard failure.
 */
const connectionString = process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.SPAN_PULL_INTEGRATION_REQUIRED === '1';

let ready = false;
const probe = postgres(connectionString, { connect_timeout: 3, max: 1 });
try {
	const [r] = await probe`select to_regclass('span_pull_requests') is not null as ok`;
	ready = !!r?.ok;
} catch {
	ready = false;
} finally {
	await probe.end({ timeout: 1 }).catch(() => {});
}
if (required && !ready) {
	throw new Error(`SPAN_PULL_INTEGRATION_REQUIRED=1 but no migrated database (with span_pull_requests) answered at ${connectionString}.`);
}

const suffix = randomUUID().slice(0, 8);
const hex32 = () => randomBytes(32 / 2).toString('hex');
const hex16 = () => randomBytes(8).toString('hex');

const orgA = randomUUID();
const orgB = randomUUID();
const core = randomUUID(); // orgA's core (scope) project
const mobile = randomUUID(); // orgA's mobile project
const projB = randomUUID();
const member = randomUUID();
const stagingOnly = randomUUID(); // narrowed to {staging} on the mobile project
const outsider = randomUUID();

const T = {
	available: hex32(), // mobile span stored
	pulledVisible: hex32(), // pulled span, scope project core
	pending: hex32(), // queue row stage 1
	none: hex32(),
	staging: hex32(), // mobile span in staging
	foreign: hex32(), // a span of org B
};

describe.skipIf(!ready)('spans status and on-demand pull (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;
	const NOW = Date.now();
	const iso = (msAgo: number) => new Date(NOW - msAgo).toISOString();
	let issueId: string;
	let issueB: string;

	const insertSpan = (org: string, project: string | null, source: 'mobile' | 'pulled', env: string, trace: string, scope: string | null = null) => sql`
		insert into spans (org_id, project_id, environment, source, service_name, trace_id, span_id, name, op, start_ts, duration_us, status, attributes, scope_project_id)
		values (${org}, ${project}, ${env}, ${source}, 'svc', ${trace}, ${hex16()}, 'n', 'op', ${iso(60 * 60 * 1000)}::timestamptz, 10, 'ok', '{}'::jsonb, ${scope})`;

	/** An occurrence of `trace` on the orgA mobile project's issue, `agoMs` old. */
	const occurrence = (trace: string | null, agoMs = 60 * 60 * 1000, issue = issueId, env = 'production') => sql`
		insert into error_occurrences (id, issue_id, environment, platform, trace_id, created_at)
		values (${randomUUID()}, ${issue}, ${env}, 'node', ${trace}, ${iso(agoMs)}::timestamptz)`;

	const queueRow = async (trace: string, org = orgA) => {
		const [r] = await sql`select stage, attempts, next_fetch_at, on_demand, requested_by, requested_by_kind, requested_at, outcome, last_error
			from span_pull_queue where org_id = ${org} and trace_id = ${trace}`;
		return r;
	};
	const ledger = (org = orgA) => sql`select * from span_pull_requests where org_id = ${org} order by requested_at`;

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 10 });
		await sql`select spans_ensure_partitions(3, 2)`;
		for (const [oid, label] of [[orgA, 'a'], [orgB, 'b']] as const) {
			await sql`insert into organizations (id, name, slug) values (${oid}, ${`sp-${label}-${suffix}`}, ${`sp-${label}-${suffix}`})`;
		}
		for (const [pid, org, label] of [[core, orgA, 'core'], [mobile, orgA, 'mobile'], [projB, orgB, 'b']] as const) {
			await sql`insert into projects (id, name, organization_id, api_key, api_key_hash)
				values (${pid}, ${`sp-${label}-${suffix}`}, ${org}, ${`k_${label}_${suffix}`}, ${`h_${label}_${suffix}`})`;
		}
		for (const [uid, label] of [[member, 'member'], [stagingOnly, 'staging'], [outsider, 'outsider']] as const) {
			await sql`insert into "user" (id, name, email) values (${uid}, ${label}, ${`sp-${label}-${suffix}@example.test`})`;
		}
		for (const uid of [member, stagingOnly]) {
			await sql`insert into organization_members (organization_id, user_id, role) values (${orgA}, ${uid}, 'engineer')`;
		}
		await sql`insert into project_environment_grants (user_id, project_id, environments, created_by)
			values (${stagingOnly}, ${mobile}, ${sql.array(['staging'])}, ${member})`;

		issueId = randomUUID();
		issueB = randomUUID();
		await sql`insert into issues (id, project_id, environment, fingerprint, message, error_class) values (${issueId}, ${mobile}, 'production', ${`fp-${issueId}`}, 'm', 'E')`;
		await sql`insert into issues (id, project_id, environment, fingerprint, message, error_class) values (${issueB}, ${projB}, 'production', ${`fp-${issueB}`}, 'm', 'E')`;

		await insertSpan(orgA, mobile, 'mobile', 'production', T.available);
		await insertSpan(orgA, null, 'pulled', 'production', T.pulledVisible, core);
		await insertSpan(orgA, mobile, 'mobile', 'staging', T.staging);
		await insertSpan(orgB, projB, 'mobile', 'production', T.foreign);
		await sql`insert into span_pull_queue (org_id, trace_id, stage, next_fetch_at) values (${orgA}, ${T.pending}, 1, now() + interval '5 minutes')`;
		// A queue row of ANOTHER org for a trace orgA also has: must not make orgA's trace pending.
		await sql`insert into span_pull_queue (org_id, trace_id, stage, next_fetch_at) values (${orgB}, ${T.none}, 0, now())`;
		for (const t of [T.available, T.pulledVisible, T.pending, T.none, T.staging]) await occurrence(t);
		await occurrence(T.foreign, 3600_000, issueB);
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from spans where org_id in ${sql([orgA, orgB])}`;
			await sql`delete from span_pull_queue where org_id in ${sql([orgA, orgB])}`;
			await sql`delete from span_pull_requests where org_id in ${sql([orgA, orgB])}`;
			await sql`delete from span_pull_issue_window where issue_id in ${sql([issueId, issueB])}`;
			await sql`delete from error_occurrences where issue_id in ${sql([issueId, issueB])}`;
			await sql`delete from issues where id in ${sql([issueId, issueB])}`;
			await sql`delete from project_environment_grants where user_id in ${sql([stagingOnly])}`;
			await sql`delete from organization_members where organization_id in ${sql([orgA, orgB])}`;
			await sql`delete from projects where organization_id in ${sql([orgA, orgB])}`;
			await sql`delete from "user" where id in ${sql([member, stagingOnly, outsider])}`;
			await sql`delete from organizations where id in ${sql([orgA, orgB])}`;
			// A database-wide DROP of expired `spans` partitions, so it runs under the retention.*.integration
			// family's lock: span-waterfall creates an expired partition and inserts into it, and this drop
			// landing in between failed it under --sequence.shuffle (B13).
			await sql`select pg_advisory_lock(hashtext('sentinel.retention.integration'))`;
			await sql`select spans_drop_expired(14)`;
		} finally {
			await sql`select pg_advisory_unlock_all()`.catch(() => {});
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	}, 120_000);

	async function scopeFor(userId: string, orgId = orgA) {
		const { resolveViewerSpanScope } = await import('./span-scope');
		return (await resolveViewerSpanScope(userId, orgId))!;
	}

	// ---- TASK-01: the status helper ---------------------------------------------------------------------------------

	it('available | pending | none for a batch, in one call; a non-canonical id is none; another org’s spans and queue rows do not leak', async () => {
		const { getSpansStatuses } = await import('./queries/span-queries');
		const r = await getSpansStatuses([T.available, T.pulledVisible, T.pending, T.none, T.foreign, 'NOT-A-TRACE', null, T.available], await scopeFor(member));
		expect(Object.fromEntries(r)).toEqual({
			[T.available]: 'available',
			[T.pulledVisible]: 'available',
			[T.pending]: 'pending',
			[T.none]: 'none', // orgB's queue row for it is not orgA's
			[T.foreign]: 'none', // orgB's span
		});
	});

	it('is the waterfall’s own guard: status is available exactly when listTraceSpans returns spans, for every viewer', async () => {
		const { getSpansStatuses, listTraceSpans } = await import('./queries/span-queries');
		for (const user of [member, stagingOnly]) {
			const scope = await scopeFor(user);
			const statuses = await getSpansStatuses(Object.values(T), scope);
			for (const trace of Object.values(T)) {
				const spans = (await listTraceSpans(trace, scope)).spans;
				expect(statuses.get(trace) === 'available', `trace ${trace} for ${user === member ? 'member' : 'stagingOnly'}`).toBe(spans.length > 0);
			}
		}
		// The environment grant hides the production mobile span from stagingOnly but not the staging one.
		const s = await getSpansStatuses([T.available, T.staging], await scopeFor(stagingOnly));
		expect(s.get(T.available)).toBe('none');
		expect(s.get(T.staging)).toBe('available');
	});

	it('getSpansState says WHY a none is a none, from the queue row', async () => {
		const { getSpansState } = await import('./queries/span-queries');
		const scope = await scopeFor(member);
		const fresh = hex32();
		expect(await getSpansState(fresh, scope)).toEqual({ status: 'none', reason: 'not_pulled' });
		const finished = async (outcome: string) => {
			const t = hex32();
			await sql`insert into span_pull_queue (org_id, trace_id, stage, next_fetch_at, outcome) values (${orgA}, ${t}, 2, now(), ${outcome})`;
			return (await getSpansState(t, scope)).reason;
		};
		expect(await finished('not_found')).toBe('not_found');
		expect(await finished('')).toBe('not_found'); // finished before the outcome column existed
		expect(await finished('too_large')).toBe('too_large');
		expect(await finished('gave_up')).toBe('failed');
		expect(await finished('not_allowed')).toBe('not_allowed');
		expect(await finished('stored')).toBe('not_visible');
		expect(await getSpansState(T.pending, scope)).toEqual({ status: 'pending', reason: null });
	});

	it('EXPLAIN: the EXISTS probes the spans primary key on EVERY partition (no seq scan of spans)', async () => {
		const { statusQuery } = await import('./queries/span-queries');
		const scope = await scopeFor(member);
		const q = new PgDialect().sqlToQuery(statusQuery([T.available, T.none, T.pending, hex32()], scope));
		const plan = await sql.begin(async (tx) => {
			// Tables this small are always seq-scanned by a planner that is free to choose; this asks whether the index CAN be
			// used by the predicates (org_id, trace_id lead the primary key), which is what matters at 9 GB.
			await tx`set local enable_seqscan = off`;
			const rows = await tx.unsafe(`EXPLAIN (FORMAT TEXT) ${q.sql}`, q.params as never[]);
			return rows.map((r) => String(r['QUERY PLAN'])).join('\n');
		});
		const [{ n }] = await sql`select count(*)::int as n from pg_inherits where inhparent = 'spans'::regclass`;
		expect(n).toBeGreaterThanOrEqual(6);
		// Both spans indexes lead with the correlated pair — the primary key (org_id, trace_id, span_id, start_ts) and
		// idx_spans_trace_id (trace_id, org_id) — and the planner may pick either: what matters is an INDEX probe per partition.
		const probed = new Set((plan.match(/Index (?:Only )?Scan using spans_p\d{8}_(?:pkey|trace_id_org_id_idx) on (spans_p\d{8})/g) ?? []).map((m) => m.match(/spans_p\d{8}$/)![0]));
		expect(probed.size).toBe(n);
		expect(plan).not.toMatch(/Seq Scan on spans_p/);
		expect(plan).toMatch(/Index Cond: \(\(trace_id = t\.trace_id\) AND \(org_id = '[0-9a-f-]+'::uuid\)\)/);
	});

	it('the trace page loader says why there are no spans (and stays silent about a trace the viewer cannot see)', async () => {
		const { load } = await import('../../routes/[orgSlug]/traces/[traceId]/+page.server');
		const org = { id: orgA, slug: `sp-a-${suffix}` };
		const run = async (userId: string, traceId: string) =>
			(await (load as any)({
				params: { orgSlug: org.slug, traceId },
				locals: { auth: async () => ({ user: { id: userId } }), currentOrg: org },
				url: new URL(`http://localhost/${org.slug}/traces/${traceId}`),
			})) as any;
		expect((await run(member, T.available)).spansState).toEqual({ status: 'available', reason: null });
		expect((await run(member, T.pending)).spansState).toEqual({ status: 'pending', reason: null });
		const none = await run(member, T.none);
		expect(none.spansState).toEqual({ status: 'none', reason: 'not_pulled' });
		expect(none.organizationId).toBe(orgA);
		// A trace id nobody in this org recorded an error for: the same empty page whatever the cause (D2), no state to learn.
		expect((await run(member, hex32())).spansState).toEqual({ status: 'none', reason: null });
	});

	// ---- TASK-02: the on-demand pull ---------------------------------------------------------------------------------

	const configFor = (limitPerHour = 30, orgs: string[] = [orgA]) => ({
		scopes: new Map(orgs.map((o) => [o, core] as [string, string])),
		limitPerHour,
	});
	const asUser = (id: string) => ({ kind: 'user' as const, id });

	it('refuses an organization that is not allow-listed, writes nothing, and says so as a 403-class error', async () => {
		const { requestSpanPull, SpanPullError } = await import('./span-pull');
		const trace = hex32();
		await occurrence(trace);
		await expect(
			requestSpanPull({ traceId: trace, scope: await scopeFor(member), requester: asUser(member), config: configFor(30, [orgB]) })
		).rejects.toMatchObject({ code: 'not_allowed' });
		expect(await queueRow(trace)).toBeUndefined();
		expect((await ledger()).filter((l) => l.trace_id === trace)).toHaveLength(0);
		await expect(
			requestSpanPull({ traceId: trace, scope: await scopeFor(member), requester: asUser(member), config: configFor(30, []) })
		).rejects.toBeInstanceOf(SpanPullError);
	});

	it('404-class (not_found) for a viewer who cannot see the occurrence, cannot reach the core project, or for a trace with no occurrence', async () => {
		const { requestSpanPull } = await import('./span-pull');
		const trace = hex32();
		await occurrence(trace);
		const ask = async (scope: Awaited<ReturnType<typeof scopeFor>>, traceId = trace) =>
			requestSpanPull({ traceId, scope, requester: asUser(member), config: configFor() }).catch((e) => e);
		// no accessible project at all
		expect(await ask({ orgId: orgA, projects: [], environmentScopes: new Map() })).toMatchObject({ code: 'not_found' });
		// reaches the mobile project but not the core project that governs pulled spans
		expect(await ask({ orgId: orgA, projects: [{ id: mobile }], environmentScopes: new Map() })).toMatchObject({ code: 'not_found' });
		// narrowed to staging: the production occurrence is invisible to them
		expect(await ask(await scopeFor(stagingOnly))).toMatchObject({ code: 'not_found' });
		// a trace nobody recorded an error for
		expect(await ask(await scopeFor(member), hex32())).toMatchObject({ code: 'not_found' });
		expect(await queueRow(trace)).toBeUndefined();
		expect((await ledger()).filter((l) => l.trace_id === trace)).toHaveLength(0);
	});

	it('a new trace: one immediate single-fetch row (stage 1), audited, and the per-issue throttle is not touched', async () => {
		const { requestSpanPull } = await import('./span-pull');
		const trace = hex32();
		await occurrence(trace, 60 * 60 * 1000);
		const before = await sql`select count(*)::int as n from span_pull_issue_window`;
		const res = await requestSpanPull({ traceId: trace, scope: await scopeFor(member), requester: asUser(member), config: configFor() });
		expect(res).toMatchObject({ status: 'pending', reason: null, action: 'queued' });
		const row = await queueRow(trace);
		expect(row).toMatchObject({ stage: 1, attempts: 0, on_demand: true, requested_by: member, requested_by_kind: 'user', outcome: '' });
		expect(Math.abs(new Date(row.next_fetch_at).getTime() - Date.now())).toBeLessThan(5000); // an hour-old error: due now
		expect(row.requested_at).toBeTruthy();
		const [entry] = (await ledger()).filter((l) => l.trace_id === trace);
		expect(entry).toMatchObject({ id: res.requestId, action: 'queued', requested_by: member, requested_by_kind: 'user' });
		expect(await sql`select count(*)::int as n from span_pull_issue_window`).toEqual(before);
		expect(await sql`select 1 from span_pull_issue_window where issue_id = ${issueId}`).toHaveLength(0);
	});

	it('a trace whose first error is younger than the store’s flush time is not fetched before error + 2 minutes', async () => {
		const { requestSpanPull } = await import('./span-pull');
		const trace = hex32();
		await occurrence(trace, 20 * 1000);
		await requestSpanPull({ traceId: trace, scope: await scopeFor(member), requester: asUser(member), config: configFor() });
		const due = new Date((await queueRow(trace)).next_fetch_at).getTime();
		expect(due - Date.now()).toBeGreaterThan(90 * 1000);
		expect(due - Date.now()).toBeLessThan(110 * 1000);
	});

	it('is idempotent: asking again while it is pending writes nothing and counts nothing', async () => {
		const { requestSpanPull } = await import('./span-pull');
		const trace = hex32();
		await occurrence(trace);
		const scope = await scopeFor(member);
		await requestSpanPull({ traceId: trace, scope, requester: asUser(member), config: configFor() });
		const first = await queueRow(trace);
		const ledgerBefore = (await ledger()).length;
		const again = await requestSpanPull({ traceId: trace, scope, requester: asUser(stagingOnly), config: configFor() });
		expect(again).toMatchObject({ status: 'pending', action: 'already_pending', requestId: null });
		expect(await queueRow(trace)).toEqual(first); // including requested_by: the second asker did not overwrite the first
		expect((await ledger()).length).toBe(ledgerBefore);
	});

	it('an automatic stage-0 row that is not yet due is pulled forward KEEPING its stage (so the +10 min late-span fetch survives); one in flight is left alone', async () => {
		const { requestSpanPull } = await import('./span-pull');
		const scope = await scopeFor(member);
		const waiting = hex32();
		const inFlight = hex32();
		for (const t of [waiting, inFlight]) await occurrence(t);
		await sql`insert into span_pull_queue (org_id, trace_id, stage, attempts, next_fetch_at, trigger_project_id) values (${orgA}, ${waiting}, 0, 0, now() + interval '8 minutes', ${mobile})`;
		await sql`insert into span_pull_queue (org_id, trace_id, stage, attempts, next_fetch_at) values (${orgA}, ${inFlight}, 0, 1, now() + interval '2 minutes')`;
		const flightBefore = await queueRow(inFlight);

		expect(await requestSpanPull({ traceId: waiting, scope, requester: asUser(member), config: configFor() })).toMatchObject({ status: 'pending', action: 'pulled_forward' });
		const row = await queueRow(waiting);
		expect(row).toMatchObject({ stage: 0, on_demand: true, requested_by: member });
		expect(new Date(row.next_fetch_at).getTime()).toBeLessThan(Date.now() + 5000);

		expect(await requestSpanPull({ traceId: inFlight, scope, requester: asUser(member), config: configFor() })).toMatchObject({ status: 'pending', action: 'already_pending' });
		expect(await queueRow(inFlight)).toEqual(flightBefore);
	});

	it('a finished pull with no visible spans is re-armed for one more fetch; spans already available are a no-op', async () => {
		const { requestSpanPull } = await import('./span-pull');
		const scope = await scopeFor(member);
		const done = hex32();
		await occurrence(done);
		await sql`insert into span_pull_queue (org_id, trace_id, stage, attempts, next_fetch_at, outcome, last_error)
			values (${orgA}, ${done}, 2, 2, now() - interval '1 hour', 'not_found', 'boom')`;
		expect(await requestSpanPull({ traceId: done, scope, requester: asUser(member), config: configFor() })).toMatchObject({ status: 'pending', action: 'rearmed' });
		expect(await queueRow(done)).toMatchObject({ stage: 1, attempts: 0, outcome: '', last_error: '', on_demand: true, requested_by: member });
		// m4: created_at was reset, so the retention reaper (which deletes by created_at) cannot take it before the fetch.
		const [age] = await sql`select created_at > now() - interval '1 minute' as fresh from span_pull_queue where org_id = ${orgA} and trace_id = ${done}`;
		expect(age.fresh).toBe(true);

		// available: nothing written, the (finished) row is untouched
		const availableRowBefore = hex32();
		await insertSpan(orgA, mobile, 'mobile', 'production', availableRowBefore);
		await occurrence(availableRowBefore);
		await sql`insert into span_pull_queue (org_id, trace_id, stage, next_fetch_at, outcome) values (${orgA}, ${availableRowBefore}, 2, now(), 'stored')`;
		const rowBefore = await queueRow(availableRowBefore);
		const ledgerBefore = (await ledger()).length;
		expect(await requestSpanPull({ traceId: availableRowBefore, scope, requester: asUser(member), config: configFor() })).toMatchObject({ status: 'available', action: 'none', requestId: null });
		expect(await queueRow(availableRowBefore)).toEqual(rowBefore);
		expect((await ledger()).length).toBe(ledgerBefore);
	});

	it('a trace that finished too_large is not re-armed: status reported, nothing written, no budget spent', async () => {
		const { requestSpanPull } = await import('./span-pull');
		const scope = await scopeFor(member);
		const big = hex32();
		await occurrence(big);
		await sql`insert into span_pull_queue (org_id, trace_id, stage, attempts, next_fetch_at, outcome) values (${orgA}, ${big}, 2, 1, now() - interval '1 hour', 'too_large')`;
		const before = await queueRow(big);
		const ledgerBefore = (await ledger()).length;
		expect(await requestSpanPull({ traceId: big, scope, requester: asUser(member), config: configFor() })).toMatchObject({ status: 'none', reason: 'too_large', action: 'none', requestId: null });
		expect(await queueRow(big)).toEqual(before);
		expect((await ledger()).length).toBe(ledgerBefore);
	});

	it('the hourly limit is per organization, counts only accepted writes, rolls the refused one back, and holds when requests race', async () => {
		const { requestSpanPull } = await import('./span-pull');
		const scope = await scopeFor(member);
		// Isolate this test's budget: age out everything earlier tests of this file counted.
		await sql`update span_pull_requests set requested_at = now() - interval '3 hours' where org_id = ${orgA}`;
		const limit = 3;
		const traces = Array.from({ length: 8 }, () => hex32());
		for (const t of traces) await occurrence(t);

		// "Two replicas": concurrent calls on separate connections, more than the budget. Exactly `limit` get through.
		const results = await Promise.all(
			traces.map((traceId) => requestSpanPull({ traceId, scope, requester: asUser(member), config: configFor(limit) }).then((r) => r.action, (e) => e.code))
		);
		expect(results.filter((r) => r === 'queued')).toHaveLength(limit);
		expect(results.filter((r) => r === 'rate_limited')).toHaveLength(traces.length - limit);
		const recent = (await ledger()).filter((l) => Date.now() - new Date(l.requested_at).getTime() < 3600_000);
		expect(recent).toHaveLength(limit);
		// The refused requests left no queue row behind (the transaction rolled back).
		const queued = await sql`select trace_id from span_pull_queue where org_id = ${orgA} and trace_id in ${sql(traces)}`;
		expect(queued).toHaveLength(limit);

		// Over budget: a NEW trace is refused with a Retry-After, an already-pending one is still answered (it writes nothing).
		const fresh = hex32();
		await occurrence(fresh);
		const refused = await requestSpanPull({ traceId: fresh, scope, requester: asUser(member), config: configFor(limit) }).catch((e) => e);
		expect(refused).toMatchObject({ code: 'rate_limited' });
		expect(refused.retryAfterSeconds).toBeGreaterThan(0);
		expect(refused.retryAfterSeconds).toBeLessThanOrEqual(3600);
		const pendingTrace = queued[0].trace_id as string;
		expect(await requestSpanPull({ traceId: pendingTrace, scope, requester: asUser(member), config: configFor(limit) })).toMatchObject({ action: 'already_pending' });

		// Sequentially, after the window passes, the budget is back.
		await sql`update span_pull_requests set requested_at = now() - interval '2 hours' where org_id = ${orgA}`;
		expect(await requestSpanPull({ traceId: fresh, scope, requester: asUser(member), config: configFor(limit) })).toMatchObject({ action: 'queued' });
	});

	it('the budget is per organization: another organization’s requests do not spend it', async () => {
		const { requestSpanPull } = await import('./span-pull');
		await sql`update span_pull_requests set requested_at = now() - interval '3 hours' where org_id = ${orgA}`;
		const t = hex32();
		await occurrence(t, 3600_000, issueB);
		const scopeB = { orgId: orgB, projects: [{ id: projB }], environmentScopes: new Map() };
		await expect(
			requestSpanPull({ traceId: t, scope: scopeB, requester: { kind: 'agent', id: 'agent-b' }, config: { scopes: new Map([[orgB, projB]]), limitPerHour: 1 } })
		).resolves.toMatchObject({ action: 'queued' });
		const [entry] = await sql`select * from span_pull_requests where org_id = ${orgB} and trace_id = ${t}`;
		expect(entry).toMatchObject({ requested_by: 'agent-b', requested_by_kind: 'agent' });
	});
});
