import { and, asc, eq, isNull, lte, sql } from 'drizzle-orm';
import { db } from '$lib/server/db';
import { issueActivity, issueEscalations, issues, notifications, projects } from '$lib/db/schema';
import { subscribe } from '$lib/db/queries/subscriptions';
import { getActiveSchedule, toResolutionInputs } from '$lib/db/queries/oncall';
import { whoIsOnCall } from '$lib/server/oncall';
import {
	recordEscalation,
	recordEscalationExhausted,
	recordEscalationUnscoped,
	recordOncallScheduleUnresolved,
	ESCALATION_OUTCOME,
} from '$lib/server/metrics';
import { resolveEnvironmentScope } from '$lib/server/environment-access';
import { sendIssueNotificationEmails } from '$lib/server/notify';
import { log } from '$lib/server/observability/log';
import { env } from '$env/dynamic/private';

/**
 * sentinel-user-report-issue §4.4 / §4.4b — escalation at intake, acknowledgement, and the timer
 * sweep the minute cron drives.
 *
 * Three separable things live here on purpose, because they are the three transitions of one
 * state machine and splitting them across modules would let one of them drift:
 *
 *   1. `escalateReportAtIntake` — assign, journal, subscribe, notify, and open the chain at
 *      `hop = 1`. **Idempotent per report** (§4.4): the `issue_escalations` insert is the claim,
 *      and every other write is downstream of it winning.
 *   2. `acknowledgeEscalation` — the explicit `POST /escalation/ack` and the implicit
 *      acknowledgements (§4.4: the assignee claims, comments on, or changes the status of the
 *      issue). Stamps `acknowledged_at` and stops the clock.
 *   3. `sweepEscalations` — §4.4b's sweep, run every minute. Reassigns an unacknowledged deadline
 *      to the next untried member, terminates at `ESCALATION_MAX_HOPS` — and terminates equally
 *      when the org's schedule is gone or has been replaced, because a chain that cannot move is
 *      over — and writes NOTHING when a human has taken the issue.
 *
 * **Escalation never fails a report.** §4.4 and §12 both say so: a failure here costs an
 * assignment and a metric, never the `201`. Every entry point below therefore absorbs its own
 * error rather than propagating it.
 *
 * **Concurrency.** The sweep claims each due row with a conditional UPDATE — see `stillDue` — before
 * it writes anything else, so a doubled schedule (two CronJob runs, or a CronJob and the compose
 * loop at once) cannot double-reassign. Under READ COMMITTED the second transaction blocks on the
 * row lock, re-evaluates its WHERE after the first commits, matches zero rows and rolls back with
 * `EscalationRaceLost`.
 *
 * On the REASSIGN path the unique `(issue_id, hop)` index is a second, independent guard on the
 * same property, so the claim there is belt-and-braces. On the TERMINAL path it is the only guard:
 * nothing stops a second `SET exhausted = true` from succeeding, and §12's "nobody answered" alert
 * fires on the counter a double-count would inflate.
 */

/** §8's default: `ESCALATION_ACK_MINUTES` (15). */
export const DEFAULT_ESCALATION_ACK_MINUTES = 15;
/** §8's default: `ESCALATION_MAX_HOPS` (3). §4.4: "it does not loop the rotation forever". */
export const DEFAULT_ESCALATION_MAX_HOPS = 3;

/** How many due rows one sweep will process. Bounds a single minute's work, never the chain. */
export const DEFAULT_SWEEP_LIMIT = 200;

/**
 * A positive integer from an environment value, or the default. A malformed knob degrades to the
 * contract's default rather than to `NaN` — a `NaN` deadline would be written to a NOT NULL
 * `timestamptz` and fail the whole intake, which is exactly the outcome §4.4 forbids.
 */
function positiveIntEnv(raw: string | undefined, fallback: number): number {
	if (raw === undefined || raw.trim() === '') return fallback;
	const parsed = Number.parseInt(raw, 10);
	if (!Number.isFinite(parsed) || parsed <= 0) return fallback;
	return parsed;
}

export function resolveAckMinutes(raw?: string): number {
	return positiveIntEnv(raw ?? env.ESCALATION_ACK_MINUTES, DEFAULT_ESCALATION_ACK_MINUTES);
}

export function resolveMaxHops(raw?: string): number {
	return positiveIntEnv(raw ?? env.ESCALATION_MAX_HOPS, DEFAULT_ESCALATION_MAX_HOPS);
}

/** §4.2's `escalation` object, straight off the row. */
export interface EscalationRecord {
	id: string;
	issueId: string;
	scheduleId: string;
	assigneeId: string;
	hop: number;
	escalatedAt: Date;
	ackDeadlineAt: Date;
	acknowledgedAt: Date | null;
	exhausted: boolean;
}

/**
 * One `escalated` email owed to one engineer about one report. Carried as (issue, user) pairs
 * rather than as `NotifiedUser[]` because the sweep touches MANY reports in one run, and
 * `sendIssueNotificationEmails` builds its link from a single ambient issue id — handing it one
 * issue id for a batch spanning several would send every engineer a link to somebody else's
 * report.
 */
export interface EscalationEmail {
	issueId: string;
	userId: string;
	/**
	 * sentinel-project-environments §2.2, decisions 38/41 — this page went to a rota member who does
	 * NOT hold the issue's environment, and the email must say so. It is what tells each responder
	 * why they were included, and it is the only reason a subject other than
	 * `NOTIFICATION_SUBJECTS.escalated` is ever built.
	 */
	unscoped?: boolean;
}

/** What an escalation attempt produced, plus the post-commit email work it owes. */
export interface EscalationAttempt {
	escalation: EscalationRecord | null;
	/** Fed to `sendEscalationEmails` AFTER the transaction commits (SR-2, notify.ts §8). */
	notified: EscalationEmail[];
}

/**
 * SR-2's delivery step, post-commit and best-effort — one call per (issue, user) pair so each
 * engineer gets a link to the report they were actually put on. `sendIssueNotificationEmails`
 * never throws and absorbs its own per-user failures; the `catch` here is the belt to that
 * suspenders, because escalation must never be able to fail the request that triggered it.
 */
export async function sendEscalationEmails(
	notified: readonly EscalationEmail[],
	origin: string
): Promise<void> {
	for (const entry of notified) {
		try {
			await sendIssueNotificationEmails(
				[{ userId: entry.userId, kind: 'escalated', unscoped: entry.unscoped }],
				{
					issueId: entry.issueId,
					origin,
				}
			);
		} catch (err) {
			log.error('escalation.email_failed', {
				issueId: entry.issueId,
				userId: entry.userId,
				error: err,
			});
		}
	}
}

/** Thrown inside a sweep transaction to roll it back when another sweeper claimed the row. */
class EscalationRaceLost extends Error {
	constructor() {
		super('another sweep claimed this escalation row');
		this.name = 'EscalationRaceLost';
	}
}

const ESCALATION_SELECTION = {
	id: issueEscalations.id,
	issueId: issueEscalations.issueId,
	scheduleId: issueEscalations.scheduleId,
	assigneeId: issueEscalations.assigneeId,
	hop: issueEscalations.hop,
	escalatedAt: issueEscalations.escalatedAt,
	ackDeadlineAt: issueEscalations.ackDeadlineAt,
	acknowledgedAt: issueEscalations.acknowledgedAt,
	exhausted: issueEscalations.exhausted,
} as const;

/**
 * The latest hop for an issue, or null. §4.2 renders exactly this row, and §4.4's "the latest
 * `issue_escalations` row's assignee" is the value the human-reassignment rule compares against.
 */
export async function getLatestEscalation(issueId: string): Promise<EscalationRecord | null> {
	const rows = await db
		.select(ESCALATION_SELECTION)
		.from(issueEscalations)
		.where(eq(issueEscalations.issueId, issueId))
		.orderBy(sql`${issueEscalations.hop} DESC`)
		.limit(1);
	return rows[0] ?? null;
}

/**
 * sentinel-project-environments §2.2, decisions 30, 38 and 41 — **the unscoped fallback**, and the
 * one thing in this module that reaches people who deliberately cannot see the issue.
 *
 * **There is no walk.** One candidate is resolved — `whoIsOnCall`'s at intake, `nextUntriedMember`'s
 * in the sweep — and if that candidate does not hold `issue.environment`, this fires AT ONCE. Do not
 * add a loop and do not look for a second candidate (decision 41). Over-paging is chosen
 * deliberately: a rota that can silently page NOBODY for a production incident is the worse failure,
 * and the `unscoped` label is what tells each responder why they were included.
 *
 * **Nothing new is persisted** — no column, no table, no new `kind`. `notifications.kind` is
 * `varchar(30)` under `notifications_kind_check` and a new value would need a constraint migration,
 * which decision 33 forbids for a presentation concern; the marker rides in the existing `jsonb`
 * `payload`. The cost, accepted rather than fixed: an unscoped escalation is **not queryable after
 * the fact** — it exists in email and in `notifications` rows, not as a flag anything can `SELECT`
 * by.
 *
 * **It writes no `issue_escalations` row and no `issues.assigned_to`**, so the ack clock does not
 * start. That is the point rather than a side effect: the clock must never run against somebody who
 * structurally cannot acknowledge.
 *
 * **The recipient set is `schedule.members` PLUS the override holder when one is in force.**
 * `whoIsOnCall` returns an override holder outright (`via: 'override'`) and such a holder need not
 * appear in `schedule.members` at all, so a fan-out over `members` alone would silently drop the one
 * person actually resolved.
 *
 * Contract §12 *Known limits* **5b** records the boundary crossing as a shipped property. Do not
 * "fix" it by filtering these recipients — that is the under-paging branch the operator rejected.
 */
export const UNSCOPED_ESCALATION_TEXT =
	'Escalated to the whole rota as unscoped: nobody resolved on call holds this issue’s environment, so no assignment was made and no acknowledgement deadline is running.';

/**
 * The fan-out set, exported for the unit boundary the way `nextUntriedMember` is: it is the part of
 * the fallback that is a pure function of a member list and a resolution, and asserting the union
 * rule through a seeded rota would be indirection. A build that fans out over `members` alone
 * silently drops the one person `whoIsOnCall` actually resolved.
 */
export function unscopedRecipients(
	members: readonly { userId: string }[],
	resolved: { userId: string; via: string } | null
): string[] {
	const set = new Set(members.map((m) => m.userId));
	if (resolved && resolved.via === 'override') {
		set.add(resolved.userId);
	}
	return [...set];
}

/**
 * The four facts of decision 38, written in one transaction: one `notifications` row per recipient,
 * of the existing `escalated` kind, carrying the `unscoped` marker and the text in its payload —
 * and nothing else at all.
 */
async function writeUnscopedRotaPage(input: {
	issueId: string;
	scheduleId: string;
	environment: string;
	recipients: readonly string[];
}): Promise<EscalationEmail[]> {
	if (input.recipients.length === 0) {
		return [];
	}

	await db.transaction(async (tx) => {
		await tx.insert(notifications).values(
			input.recipients.map((userId) => ({
				userId,
				issueId: input.issueId,
				// Decision 38: the EXISTING kind. The marker is the payload, not a new value.
				kind: 'escalated' as const,
				actorType: 'system' as const,
				actorId: 'system',
				payload: {
					unscoped: true,
					scheduleId: input.scheduleId,
					environment: input.environment,
					text: UNSCOPED_ESCALATION_TEXT,
				},
			}))
		);
	});

	return input.recipients.map((userId) => ({
		issueId: input.issueId,
		userId,
		unscoped: true,
	}));
}

/** The issue columns the guard needs. `environment` is a column on the issue row (decision 7). */
async function loadIssueScopeContext(
	issueId: string
): Promise<{ projectId: string; environment: string } | null> {
	const [row] = await db
		.select({ projectId: issues.projectId, environment: issues.environment })
		.from(issues)
		.where(eq(issues.id, issueId));
	return row ?? null;
}

/** Decision 41's single question: does this one candidate hold the issue's environment? */
async function candidateHoldsEnvironment(
	userId: string,
	projectId: string,
	environment: string
): Promise<boolean> {
	const scope = await resolveEnvironmentScope(userId, projectId);
	return scope === null || scope.includes(environment);
}

/**
 * §4.4, on a report that has just been created. One transaction, and idempotent per report.
 *
 * Resolution first, outside the transaction: no active schedule, no members, or nobody eligible →
 * **escalation does not run**, `sentinel_oncall_schedule_unresolved_total` increments, and the
 * report is left unassigned for the existing pull loop. That is never an error on §4.1.
 *
 * The `issue_escalations` insert is deliberately the FIRST statement inside the transaction and
 * carries `onConflictDoNothing` on `(issue_id, hop)`. It is therefore the idempotency claim: a
 * second call on a report that already has a chain — at `hop = 1` or any later hop, since `hop = 1`
 * is never deleted — inserts nothing and every downstream write is skipped. Returning from the
 * callback at that point commits an empty transaction, which is the correct no-op; D18's rule is
 * about never returning early to signal a FAILURE that needs a rollback, and there is nothing
 * here to roll back.
 */
export async function escalateReportAtIntake(input: {
	issueId: string;
	organizationId: string;
	/** The escalation instant. A parameter, never `Date.now()`, so the resolution stays clock-free. */
	at?: Date;
	ackMinutes?: number;
}): Promise<EscalationAttempt> {
	const at = input.at ?? new Date();
	const ackMinutes = input.ackMinutes ?? resolveAckMinutes();

	try {
		const schedule = await getActiveSchedule(input.organizationId);
		if (!schedule) {
			recordOncallScheduleUnresolved();
			log.info('escalation.unresolved', {
				issueId: input.issueId,
				organizationId: input.organizationId,
				reason: 'no_active_schedule',
			});
			return { escalation: null, notified: [] };
		}

		const { spec, members, overrides } = toResolutionInputs(schedule);
		const resolved = whoIsOnCall(spec, members, overrides, at);
		if (!resolved) {
			recordOncallScheduleUnresolved();
			log.info('escalation.unresolved', {
				issueId: input.issueId,
				organizationId: input.organizationId,
				scheduleId: schedule.id,
				reason: 'nobody_on_call',
			});
			return { escalation: null, notified: [] };
		}

		// sentinel-project-environments §2.2, decisions 30/38/41 — the environment guard, on the ONE
		// candidate `whoIsOnCall` resolved. Without it an engineer granted only `{staging}` is
		// assigned a production report, subscribed, emailed a deep link — and then gets a `404` when
		// they open it, because decision 20 made environment-dimension denial a `404`.
		const scopeContext = await loadIssueScopeContext(input.issueId);
		if (
			scopeContext &&
			!(await candidateHoldsEnvironment(
				resolved.userId,
				scopeContext.projectId,
				scopeContext.environment
			))
		) {
			// AT INTAKE there is no due row to terminate — nothing was written, so nothing matches the
			// sweep's due predicate and the sweep never sees this issue. The asymmetry with `sweepOne`
			// is deliberate; do not unify them. And the rows below are written INSTEAD OF a chain, not
			// instead of anything: reading "write no `issue_escalations` row" as "write nothing" is
			// the silently-page-nobody failure decision 30 exists to prevent.
			const recipients = unscopedRecipients(schedule.members, resolved);
			const notified = await writeUnscopedRotaPage({
				issueId: input.issueId,
				scheduleId: schedule.id,
				environment: scopeContext.environment,
				recipients,
			});

			log.warn('escalation.unscoped', {
				issueId: input.issueId,
				organizationId: input.organizationId,
				scheduleId: schedule.id,
				candidateId: resolved.userId,
				environment: scopeContext.environment,
				recipients: recipients.length,
				at: 'intake',
			});

			return { escalation: null, notified };
		}

		const record = await db.transaction(async (tx) => {
			const inserted = await tx
				.insert(issueEscalations)
				.values({
					issueId: input.issueId,
					scheduleId: schedule.id,
					assigneeId: resolved.userId,
					hop: 1,
					escalatedAt: at,
					ackDeadlineAt: new Date(at.getTime() + ackMinutes * 60_000),
				})
				.onConflictDoNothing({ target: [issueEscalations.issueId, issueEscalations.hop] })
				.returning(ESCALATION_SELECTION);

			const row = inserted[0];
			if (!row) {
				// Already escalated (§4.4's "running it again … is a no-op"). Nothing was written.
				return null;
			}

			await applyAssignment(tx, {
				issueId: input.issueId,
				assigneeId: resolved.userId,
				scheduleId: schedule.id,
				hop: 1,
				ackDeadlineAt: row.ackDeadlineAt,
			});

			return row;
		});

		if (!record) {
			return { escalation: null, notified: [] };
		}

		recordEscalation(ESCALATION_OUTCOME.Assigned);
		log.info('escalation.assigned', {
			issueId: input.issueId,
			organizationId: input.organizationId,
			scheduleId: schedule.id,
			assigneeId: record.assigneeId,
			via: resolved.via,
			hop: 1,
			ackDeadlineAt: record.ackDeadlineAt.toISOString(),
		});

		return {
			escalation: record,
			notified: [{ issueId: input.issueId, userId: record.assigneeId }],
		};
	} catch (err) {
		// §12: `outcome="failed"` is "a report was assigned and nobody was told". The report itself
		// is already durable — this function is called after creation commits — so the cost of this
		// branch is an unassigned report, which is exactly the pull-loop state it started in.
		recordEscalation(ESCALATION_OUTCOME.Failed);
		log.error('escalation.failed', {
			issueId: input.issueId,
			organizationId: input.organizationId,
			error: err,
		});
		return { escalation: null, notified: [] };
	}
}

/**
 * The four writes §4.4 lists beside the `issue_escalations` row, in the caller's transaction:
 * the assignment, one `assigned` activity row attributed to `system`, the `claimant`
 * subscription, and one `escalated` notification for the assignee.
 *
 * `claimed_at` is deliberately left alone. An assignment is not a claim (CONTEXT.md), a freshly
 * created report has none, and on a reassignment a claim by the current assignee would already
 * have stamped `acknowledged_at` through the implicit-acknowledgement path — so a row that
 * reaches here with a live claim cannot exist.
 */
async function applyAssignment(
	tx: Parameters<Parameters<typeof db.transaction>[0]>[0],
	input: {
		issueId: string;
		assigneeId: string;
		scheduleId: string;
		hop: number;
		ackDeadlineAt: Date;
	}
): Promise<void> {
	await tx
		.update(issues)
		.set({ assigneeType: 'user', assignedTo: input.assigneeId })
		.where(eq(issues.id, input.issueId));

	await tx.insert(issueActivity).values({
		issueId: input.issueId,
		eventType: 'assigned',
		// §4.4: "exactly one `assigned` row in `issue_activity`, `actor_type = 'system'`".
		actorType: 'system',
		actorId: 'system',
		newValue: {
			assigneeType: 'user',
			assignedTo: input.assigneeId,
			reason: 'oncall_escalation',
			hop: input.hop,
			scheduleId: input.scheduleId,
		},
	});

	// §4.4: one of the four values `issue_subscriptions_reason_check` already allows. No new
	// reason value — widening that CHECK would buy nothing here.
	await subscribe(
		{
			issueId: input.issueId,
			subscriberType: 'user',
			subscriberId: input.assigneeId,
			reason: 'claimant',
		},
		tx
	);

	await tx.insert(notifications).values({
		userId: input.assigneeId,
		issueId: input.issueId,
		// The kind the §4.6 migration added, and the one SR-2 makes emailable.
		kind: 'escalated',
		actorType: 'system',
		actorId: 'system',
		payload: {
			hop: input.hop,
			scheduleId: input.scheduleId,
			ackDeadlineAt: input.ackDeadlineAt.toISOString(),
		},
	});
}

/**
 * §4.4's acknowledgement, from either the explicit route or an implicit path.
 *
 * Returns the stamped row, or null when there was nothing to stamp — no chain, an already
 * acknowledged one, or a caller who is not the current assignee. The conditional UPDATE is the
 * whole check: it names `assignee_id` and `acknowledged_at IS NULL`, so a concurrent sweep that
 * has taken the row's lock cannot lose this stamp, and a second ack is a no-op rather than a
 * moved timestamp.
 *
 * An `exhausted` row is still acknowledgeable, deliberately: the chain has stopped moving but the
 * assignment stands, and "the engineer finally answered" is worth recording — it is what tells an
 * operator the `sentinel_report_escalations_exhausted_total` alert has been picked up.
 */
export async function acknowledgeEscalation(
	issueId: string,
	userId: string,
	at: Date = new Date()
): Promise<EscalationRecord | null> {
	const stamped = await db
		.update(issueEscalations)
		.set({ acknowledgedAt: at })
		.where(
			and(
				eq(issueEscalations.issueId, issueId),
				eq(issueEscalations.assigneeId, userId),
				isNull(issueEscalations.acknowledgedAt),
				sql`${issueEscalations.hop} = (SELECT max(e2.hop) FROM issue_escalations e2 WHERE e2.issue_id = ${issueId})`
			)
		)
		.returning(ESCALATION_SELECTION);

	return stamped[0] ?? null;
}

/**
 * §4.4's implicit acknowledgements — the assignee claims, comments on, or changes the status of
 * the issue. "Someone who is already working on it must not be reassigned for not pressing a
 * button."
 *
 * Best-effort and never throwing: it is called after the mutation it accompanies has already
 * committed, and a failure to stamp must not turn a successful comment into a 500. The cost of a
 * missed stamp is one extra reassignment, which the chain is bounded against anyway.
 */
export async function acknowledgeEscalationQuietly(
	issueId: string,
	userId: string
): Promise<void> {
	try {
		const stamped = await acknowledgeEscalation(issueId, userId);
		if (stamped) {
			log.info('escalation.acknowledged', {
				issueId,
				userId,
				hop: stamped.hop,
				implicit: true,
			});
		}
	} catch (err) {
		log.error('escalation.implicit_ack_failed', { issueId, userId, error: err });
	}
}

export interface SweepResult {
	/** Rows past their deadline whose assignment still matches the chain. */
	due: number;
	reassigned: number;
	exhausted: number;
	/** Past deadline but a human has taken the issue — §4.4: the timer writes nothing. */
	stoppedByReassignment: number;
	/**
	 * Due, but the organization's schedule is gone or has been replaced. The row is TERMINATED —
	 * `exhausted = true` through the same compare-and-set a `max_hops` termination uses — and
	 * counted here rather than under `exhausted`, because "the schedule was deactivated" is a
	 * different operator story from §12's "nobody answered".
	 *
	 * It is a terminal write and not a no-op on purpose. A row left untouched keeps its past
	 * `ack_deadline_at`, its `acknowledged_at IS NULL` and its `exhausted = false`, so it matches
	 * the due predicate again every minute forever: the report is never reassigned, never
	 * exhausted, and — because the due set is `ORDER BY ack_deadline_at ASC LIMIT
	 * DEFAULT_SWEEP_LIMIT` and these rows carry the oldest deadlines in the table — it sorts ahead
	 * of every actionable row and eats the whole bounded budget. The budget is only ever spent on
	 * rows the sweep can act on.
	 */
	skipped: number;
	/**
	 * sentinel-project-environments §2.2, decisions 38/41: due rows whose next candidate does not
	 * hold the issue's environment. The whole rota was paged with the `unscoped` label and the row
	 * was TERMINATED — counted here and never under `exhausted`, because that one is §12's "nobody
	 * answered" signal and this chain stopped on a GRANT rather than on a human.
	 */
	unscoped: number;
	/** Lost the claim race with a concurrent sweep, or failed. Nothing written by this run. */
	contended: number;
	/** Post-commit email work, one entry per engineer this sweep put on a report. */
	notified: EscalationEmail[];
}

/**
 * §4.4b's sweep. Every due row is either reassigned, terminated, or deliberately left alone.
 *
 * The due set is scoped THREE ways, and each one is load-bearing:
 *
 *   - `acknowledged_at IS NULL AND NOT exhausted AND ack_deadline_at <= now` — the sweep predicate
 *     §4.6's partial index is built for.
 *   - `hop = max(hop) for this issue` — only the LATEST hop is live. A superseded row is past its
 *     deadline forever, and without this it would be re-swept every minute for the life of the row.
 *   - `issues.assigned_to = assignee_id AND issues.assignee_type = 'user'` — §4.4's "a human
 *     reassignment wins and ends the chain … the timer stops and writes nothing". Expressing it as
 *     a predicate rather than as a per-row check is what makes "writes nothing" also mean "costs
 *     nothing": such a row never enters the due set again.
 */
export async function sweepEscalations(options?: {
	now?: Date;
	ackMinutes?: number;
	maxHops?: number;
	limit?: number;
}): Promise<SweepResult> {
	const now = options?.now ?? new Date();
	const ackMinutes = options?.ackMinutes ?? resolveAckMinutes();
	const maxHops = options?.maxHops ?? resolveMaxHops();
	const limit = options?.limit ?? DEFAULT_SWEEP_LIMIT;

	const isLatestHop = sql`${issueEscalations.hop} = (SELECT max(e2.hop) FROM issue_escalations e2 WHERE e2.issue_id = ${issueEscalations.issueId})`;
	const pastDeadline = and(
		isNull(issueEscalations.acknowledgedAt),
		eq(issueEscalations.exhausted, false),
		lte(issueEscalations.ackDeadlineAt, now),
		isLatestHop
	);

	const dueRows = await db
		.select({
			...ESCALATION_SELECTION,
			organizationId: projects.organizationId,
			// §2.2's guard needs both, and decision 7 put `environment` on the issue row itself — so
			// this is two more columns on a join the sweep already makes, not a second query.
			projectId: issues.projectId,
			environment: issues.environment,
		})
		.from(issueEscalations)
		.innerJoin(issues, eq(issues.id, issueEscalations.issueId))
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.where(
			and(
				pastDeadline,
				eq(issues.assigneeType, 'user'),
				sql`${issues.assignedTo} = ${issueEscalations.assigneeId}`
			)
		)
		.orderBy(asc(issueEscalations.ackDeadlineAt))
		.limit(limit);

	// Counted, not acted on: these are the rows a human took over. Reporting the number is the
	// only way an operator can tell "the chain stopped because a person answered" from "the sweep
	// found nothing to do".
	const stoppedRows = await db
		.select({ count: sql<number>`count(*)::int` })
		.from(issueEscalations)
		.innerJoin(issues, eq(issues.id, issueEscalations.issueId))
		.where(
			and(
				pastDeadline,
				sql`(${issues.assignedTo} IS DISTINCT FROM ${issueEscalations.assigneeId} OR ${issues.assigneeType} IS DISTINCT FROM 'user')`
			)
		);

	const result: SweepResult = {
		due: dueRows.length,
		reassigned: 0,
		exhausted: 0,
		stoppedByReassignment: stoppedRows[0]?.count ?? 0,
		skipped: 0,
		unscoped: 0,
		contended: 0,
		notified: [],
	};

	for (const row of dueRows) {
		try {
			const outcome = await sweepOne(row, { now, ackMinutes, maxHops });
			if (outcome.kind === 'reassigned') {
				result.reassigned += 1;
				result.notified.push({ issueId: row.issueId, userId: outcome.assigneeId });
				recordEscalation(ESCALATION_OUTCOME.Reassigned);
				log.info('escalation.reassigned', {
					issueId: row.issueId,
					fromAssigneeId: row.assigneeId,
					assigneeId: outcome.assigneeId,
					hop: outcome.hop,
				});
			} else if (outcome.kind === 'exhausted') {
				result.exhausted += 1;
				recordEscalationExhausted();
				log.warn('escalation.exhausted', {
					issueId: row.issueId,
					assigneeId: row.assigneeId,
					hop: row.hop,
					reason: outcome.reason,
				});
			} else if (outcome.kind === 'unscoped') {
				result.unscoped += 1;
				result.notified.push(...outcome.notified);
				// Its OWN counter, never `recordEscalationExhausted()`: §12's "nobody answered" alert
				// must not fire for a chain that stopped on a grant, at an operator who was never
				// asked anything. Same distinction the `staleSchedule` branch below already draws.
				recordEscalationUnscoped();
				log.warn('escalation.unscoped', {
					issueId: row.issueId,
					assigneeId: row.assigneeId,
					candidateId: outcome.candidateId,
					environment: row.environment,
					hop: row.hop,
					recipients: outcome.notified.length,
					at: 'sweep',
				});
			} else {
				result.skipped += 1;
				// §12's `sentinel_oncall_schedule_unresolved_total`, not the exhausted counter: this
				// chain ended because the org's schedule no longer resolves it, which is the state
				// that counter already names. Counting it as "nobody answered" would fire §4.4's
				// alert at an operator who deliberately deleted a rota.
				recordOncallScheduleUnresolved();
				log.warn('escalation.terminated_stale_schedule', {
					issueId: row.issueId,
					assigneeId: row.assigneeId,
					hop: row.hop,
					reason: outcome.reason,
				});
			}
		} catch (err) {
			if (err instanceof EscalationRaceLost) {
				// A concurrent sweep claimed this row first. Nothing was written by this transaction,
				// which is the whole point of the conditional claim.
				result.contended += 1;
				continue;
			}
			result.contended += 1;
			recordEscalation(ESCALATION_OUTCOME.Failed);
			log.error('escalation.sweep_row_failed', { issueId: row.issueId, hop: row.hop, error: err });
		}
	}

	return result;
}

type SweepOutcome =
	| { kind: 'reassigned'; assigneeId: string; hop: number }
	| { kind: 'exhausted'; reason: 'max_hops' | 'no_untried_member' }
	| { kind: 'staleSchedule'; reason: 'no_active_schedule' | 'schedule_replaced' }
	// sentinel-project-environments §2.2, decisions 38/41 — the FOURTH kind, and it is neither of
	// the other two: the schedule resolves perfectly well, and no human declined.
	| { kind: 'unscoped'; candidateId: string; notified: EscalationEmail[] };

/**
 * One due row, in one transaction.
 *
 * The claim comes first in every branch that writes: a compare-and-set on the deadline this run
 * observed, which a second sweeper cannot also match. Both branches then act only on a row they
 * hold.
 */
async function sweepOne(
	row: EscalationRecord & {
		organizationId: string | null;
		projectId: string;
		environment: string;
	},
	config: { now: Date; ackMinutes: number; maxHops: number }
): Promise<SweepOutcome> {
	// §4.4: at `ESCALATION_MAX_HOPS` the chain stops, `assigned_to` stays on the last engineer
	// tried, and the "escalation exhausted" alert is the thing that notices.
	if (row.hop >= config.maxHops) {
		await markExhausted(row, config.now);
		return { kind: 'exhausted', reason: 'max_hops' };
	}

	// Every branch below is a chain the sweep can never move again, so each one TERMINATES the row
	// instead of leaving it. `markExhausted` is the same claimed write `max_hops` uses, so a
	// concurrent sweep cannot count the same termination twice, and the row stops matching the due
	// predicate — which is what keeps it out of the next minute's bounded budget.
	if (!row.organizationId) {
		await markExhausted(row, config.now);
		return { kind: 'staleSchedule', reason: 'no_active_schedule' };
	}

	const schedule = await getActiveSchedule(row.organizationId);
	if (!schedule) {
		// §4.4a's `DELETE /oncall`: "Escalation then stops running for this org and says so."
		await markExhausted(row, config.now);
		return { kind: 'staleSchedule', reason: 'no_active_schedule' };
	}
	if (schedule.id !== row.scheduleId) {
		// A different schedule is active than the one that opened this chain. Walking a rota the
		// chain was never phased against would move the pager somewhere nobody agreed to — but the
		// chain is still over, so it ends here rather than being re-read every minute forever.
		await markExhausted(row, config.now);
		return { kind: 'staleSchedule', reason: 'schedule_replaced' };
	}

	const tried = await db
		.select({ assigneeId: issueEscalations.assigneeId })
		.from(issueEscalations)
		.where(eq(issueEscalations.issueId, row.issueId));
	const triedIds = new Set(tried.map((t) => t.assigneeId));

	const next = nextUntriedMember(schedule.members, row.assigneeId, triedIds);
	if (!next) {
		// Every eligible member has carried this report. The chain cannot move, so it is over —
		// and it is over in the state §12's "nobody answered" alert already watches.
		await markExhausted(row, config.now);
		return { kind: 'exhausted', reason: 'no_untried_member' };
	}

	// sentinel-project-environments §2.2, decisions 38/41 — the same single-candidate guard intake
	// applies, at the sweep's own call site. There is no walk: `next` is the one candidate, and if
	// they do not hold the issue's environment the fallback fires instead of a hop.
	if (!(await candidateHoldsEnvironment(next, row.projectId, row.environment))) {
		// **TERMINATE THE ROW FIRST, and this is the half that pages a whole rota every minute if it
		// is skipped.** The fallback persists no `issue_escalations` row and no `assigned_to`, so
		// nothing else makes this row stop matching `pastDeadline` — and the cron runs every minute.
		// `markExhausted` is the same claimed conditional write the two `staleSchedule` branches use,
		// so two concurrent sweeps cannot both count one termination, and it throws
		// `EscalationRaceLost` before anything is sent if another sweeper already claimed the row.
		await markExhausted(row, config.now);

		const notified = await writeUnscopedRotaPage({
			issueId: row.issueId,
			scheduleId: row.scheduleId,
			environment: row.environment,
			recipients: unscopedRecipients(schedule.members, null),
		});

		return { kind: 'unscoped', candidateId: next, notified };
	}

	const nextHop = row.hop + 1;
	const nextDeadline = new Date(config.now.getTime() + config.ackMinutes * 60_000);

	await db.transaction(async (tx) => {
		const claimed = await tx
			.update(issueEscalations)
			.set({ ackDeadlineAt: nextDeadline })
			.where(and(eq(issueEscalations.id, row.id), stillDue(config.now)))
			.returning({ id: issueEscalations.id });

		if (claimed.length === 0) {
			// Throw, never return: a `return` here would COMMIT whatever the callback had done
			// (D18). Nothing has been written yet, and this keeps it that way.
			throw new EscalationRaceLost();
		}

		const inserted = await tx
			.insert(issueEscalations)
			.values({
				issueId: row.issueId,
				scheduleId: row.scheduleId,
				assigneeId: next,
				hop: nextHop,
				escalatedAt: config.now,
				ackDeadlineAt: nextDeadline,
			})
			.onConflictDoNothing({ target: [issueEscalations.issueId, issueEscalations.hop] })
			.returning({ id: issueEscalations.id });

		if (inserted.length === 0) {
			throw new EscalationRaceLost();
		}

		// §4.4: "a fresh `assigned` activity row and a fresh `escalated` notification". The previous
		// assignee's subscription stays — they were told, and un-telling them is worse than a stale
		// row — so nothing here unsubscribes anybody.
		await applyAssignment(tx, {
			issueId: row.issueId,
			assigneeId: next,
			scheduleId: row.scheduleId,
			hop: nextHop,
			ackDeadlineAt: nextDeadline,
		});
	});

	return { kind: 'reassigned', assigneeId: next, hop: nextHop };
}

/**
 * The terminal transition, as its own conditional UPDATE so it is claimed exactly like a
 * reassignment: two concurrent sweeps cannot both count an exhaustion.
 */
async function markExhausted(row: EscalationRecord, now: Date): Promise<void> {
	const claimed = await db
		.update(issueEscalations)
		.set({ exhausted: true })
		.where(and(eq(issueEscalations.id, row.id), stillDue(now)))
		.returning({ id: issueEscalations.id });

	if (claimed.length === 0) {
		throw new EscalationRaceLost();
	}
}

/**
 * The claim predicate: "this row is STILL what I read — unacknowledged, not terminated, and past
 * its deadline". Both writing branches set something that makes it false (a deadline in the
 * future, or `exhausted = true`), so the second of two concurrent sweeps matches zero rows and
 * rolls back. Under READ COMMITTED it blocks on the row lock and re-evaluates after the first
 * commits, which is what makes it a compare-and-set rather than a hopeful check.
 *
 * **It is a predicate, not an equality on the deadline this run observed** — and that difference
 * was a live defect, caught by driving the real endpoint against a real database rather than by
 * any test here. `timestamptz` keeps MICROSECONDS; a JS `Date` keeps milliseconds. A row whose
 * `ack_deadline_at` was written by SQL (`now() - interval '5 minutes'`, an operator's backfill,
 * any tool that is not this file) round-trips through postgres-js with its sub-millisecond part
 * truncated, so `ack_deadline_at = <the value we read>` matched NOTHING and every such row was
 * silently counted as lost-the-race, forever. Rows written by this module happened to survive it,
 * because their deadlines start life as JS Dates — which is exactly the shape of bug that passes
 * a test suite that seeds its own fixtures.
 */
function stillDue(now: Date) {
	return and(
		isNull(issueEscalations.acknowledgedAt),
		eq(issueEscalations.exhausted, false),
		lte(issueEscalations.ackDeadlineAt, now)
	);
}

/**
 * §4.4's "the next member of the rotation who has not already been tried on this report".
 *
 * Walks the ORDERED member list forward from the current assignee's position, wrapping, and takes
 * the first eligible member nobody has tried. Walking from the current position rather than from
 * position 0 is what makes the chain follow the rota; skipping ineligible members is §4.4's
 * "skipped, not removed"; and the tried-set is what stops a two-member rota from ping-ponging
 * between the same two people until `max_hops`.
 *
 * Exported for the unit boundary: the selection is the part of the sweep that is a pure function
 * of a member list, and asserting it through three seeded database hops would be indirection.
 */
export function nextUntriedMember(
	members: readonly { userId: string; position: number; eligible: boolean }[],
	currentAssigneeId: string,
	tried: ReadonlySet<string>
): string | null {
	const ordered = [...members].sort((a, b) => a.position - b.position);
	if (ordered.length === 0) return null;

	const currentIndex = ordered.findIndex((m) => m.userId === currentAssigneeId);
	// An assignee who is not on the list at all (an override, or a member removed since) still has
	// a well-defined successor: the list from the top.
	const start = currentIndex === -1 ? 0 : currentIndex + 1;

	for (let step = 0; step < ordered.length; step++) {
		const candidate = ordered[(start + step) % ordered.length];
		if (!candidate.eligible) continue;
		if (tried.has(candidate.userId)) continue;
		return candidate.userId;
	}
	return null;
}
