import { db, type Tx } from '$lib/server/db';
import { issues, projects, organizations, organizationMembers, notifications, users } from '$lib/db/schema';
import { and, eq, gt, inArray, sql } from 'drizzle-orm';
import { listSubscribers } from '$lib/db/queries/subscriptions';
import { sendIssueNotificationEmail, type IssueNotificationKind } from '$lib/server/email';
import { log } from '$lib/server/observability/log';

/**
 * Manual Issues M4 (docs/plans/MANUAL_ISSUES_DESIGN.md §8, Q7, Q11). The single fan-out entry
 * point every mutation calls, INSIDE its own db.transaction (D18) -- `notifyIssueEvent` writes
 * `notifications` rows for every USER subscriber of the issue except the actor themselves. Agent
 * subscribers get no `notifications` row in M4: they poll (design §8 "Auto-subscribe... agent
 * subscribers get no notifications row -- they poll, skip for M4").
 *
 * Email is a SEPARATE, POST-COMMIT step (`sendIssueNotificationEmails`), mirroring the
 * invitation `delivered`-boolean pattern in email.ts: it must never run inside the mutation's
 * transaction (an SMTP failure or timeout must never roll back the mutation), and it must never
 * run before commit (a subscriber must not be emailed about a change that could still be rolled
 * back by a later statement in the same transaction failing).
 */

export type NotificationKind =
	| 'commented'
	| 'claimed'
	| 'status_changed'
	| 'resolved'
	| 'linked'
	| 'progress_update'
	| 'question_asked';

export interface NotifyIssueEventInput {
	issueId: string;
	kind: NotificationKind;
	actorType: 'user' | 'agent' | 'system';
	actorId: string;
	payload?: Record<string, unknown> | null;
}

export interface NotifiedUser {
	userId: string;
	kind: NotificationKind;
}

/**
 * §8: inserts one `notifications` row per USER subscriber of `input.issueId`, excluding the
 * actor (a user should never be notified about their own action). Returns the list of
 * (userId, kind) it notified -- the caller uses this AFTER its transaction commits to drive
 * `sendIssueNotificationEmails`. Must be called with the enclosing transaction's `tx`, never the
 * module-level `db`, so the insert is atomic with the mutation it describes (D18).
 */
export async function notifyIssueEvent(
	// R15 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): the caller's db.transaction param, typed
	// via `Tx` instead of `any`.
	tx: Tx,
	input: NotifyIssueEventInput
): Promise<NotifiedUser[]> {
	const subscribers = await listSubscribers(input.issueId, tx);

	const userSubscriberIds = subscribers
		.filter((s: { subscriberType: string }) => s.subscriberType === 'user')
		.map((s: { subscriberId: string }) => s.subscriberId);

	// R1 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): a `issue_subscriptions` row can outlive its
	// subscriber's org membership -- nothing today deletes it when the user is removed from the
	// org, so fan-out must re-check CURRENT membership at notify time rather than trusting the
	// subscription row alone (belt-and-suspenders with the removeMember-side cleanup below).
	// Joined through the issue's project -> its org, inside the SAME tx as the mutation (D18):
	// this must see membership as of the mutation, not a stale read from before it started.
	let currentOrgMemberIds = new Set<string>();
	if (userSubscriberIds.length > 0) {
		const memberRows = await tx
			.select({ userId: organizationMembers.userId })
			.from(issues)
			.innerJoin(projects, eq(projects.id, issues.projectId))
			.innerJoin(organizationMembers, eq(organizationMembers.organizationId, projects.organizationId))
			.where(and(eq(issues.id, input.issueId), inArray(organizationMembers.userId, userSubscriberIds)));
		currentOrgMemberIds = new Set(memberRows.map((r: { userId: string }) => r.userId));
	}

	const targets = subscribers.filter((s: { subscriberType: string; subscriberId: string }) => {
		if (s.subscriberType !== 'user') {
			// Agent subscribers are not notified in M4 -- they poll (design §8).
			return false;
		}
		if (input.actorType === 'user' && s.subscriberId === input.actorId) {
			// Never notify the actor about their own action.
			return false;
		}
		if (!currentOrgMemberIds.has(s.subscriberId)) {
			// R1: subscribed but no longer an org member (removed since subscribing) -- never
			// notify. If removeMember's own subscription cleanup ran, this row would already be
			// gone; this is the belt half of belt-and-suspenders for any path that missed it.
			return false;
		}
		return true;
	});

	if (targets.length === 0) {
		return [];
	}

	await tx.insert(notifications).values(
		targets.map((t: { subscriberId: string }) => ({
			userId: t.subscriberId,
			issueId: input.issueId,
			kind: input.kind,
			actorType: input.actorType,
			actorId: input.actorId,
			payload: input.payload ?? null,
		}))
	);

	return targets.map((t: { subscriberId: string }) => ({ userId: t.subscriberId, kind: input.kind }));
}

// §8/Q7: only these kinds ever produce an email. 'linked' and 'progress_update' are in-app only
// -- deliberately not in this set, even though they DO get a `notifications` row above.
const EMAILABLE_KINDS = new Set<NotificationKind>(['commented', 'claimed', 'status_changed', 'resolved']);

// Q11: a blocking question always bypasses the per-issue-per-user throttle -- it is checked
// separately below rather than folded into EMAILABLE_KINDS, since its bypass behavior is
// different, not just its membership.
const BLOCKING_BYPASS_KIND: NotificationKind = 'question_asked';

const THROTTLE_WINDOW_MS = 15 * 60 * 1000;

/**
 * R5 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): Q7 throttle: "at most one email per (user,
 * issue) per 15 min". Previously implemented as a count over ALL emailable `notifications` rows
 * in the window -- but that counts ATTEMPTS regardless of whether they were actually emailed, so
 * a throttled attempt at t0 poisoned the count for a later, legitimately-emailable attempt at
 * t10 even though t0 never actually sent anything: `count > 1` triggered on the second row ever,
 * so a sub-15-min cadence emailed exactly once, ever, no matter how many more emailable events
 * followed. Fixed to track actual SENDS: `notifications.emailed_at` (1723000000_pr13_remediation.sql)
 * is set by this function itself, post-send, only on rows it actually emailed (see the update at
 * the bottom of the loop below). Throttled = "an emailable notification for this (user, issue)
 * was actually emailed within the last 15 minutes" -- i.e. `max(emailed_at)` in the window, not a
 * row count.
 */
async function isThrottled(userId: string, issueId: string): Promise<boolean> {
	const since = new Date(Date.now() - THROTTLE_WINDOW_MS);
	const rows = await db
		.select({ count: sql<number>`count(*)::int` })
		.from(notifications)
		.where(
			and(
				eq(notifications.userId, userId),
				eq(notifications.issueId, issueId),
				gt(notifications.emailedAt, since),
				inArray(notifications.kind, [...EMAILABLE_KINDS])
			)
		);
	const count = rows[0]?.count ?? 0;
	return count > 0;
}

/**
 * R5: sets `emailed_at = now()` on the most recent not-yet-emailed `notifications` row for
 * (userId, issueId, kind). Scoped by a `LIMIT 1` subquery rather than a bare `UPDATE ... WHERE`
 * so it stamps exactly the row this send was for, not every historical unset row for the same
 * (user, issue, kind) -- an earlier row that was itself throttled (never emailed) must stay
 * unstamped, or a later legitimate send would wrongly "confirm" a delivery that never happened.
 * Best-effort: a failed update here must never stop other users in the fan-out loop.
 */
async function stampEmailedAt(userId: string, issueId: string, kind: NotificationKind): Promise<void> {
	try {
		await db.execute(sql`
			UPDATE ${notifications}
			SET emailed_at = now()
			WHERE id = (
				SELECT id FROM ${notifications}
				WHERE user_id = ${userId} AND issue_id = ${issueId} AND kind = ${kind} AND emailed_at IS NULL
				ORDER BY created_at DESC
				LIMIT 1
			)
		`);
	} catch (err) {
		log.error('notify.emailed_at_stamp_failed', { userId, issueId, kind, error: err });
	}
}

export interface SendNotificationEmailsContext {
	issueId: string;
	/** Request origin (e.g. `url.origin`), used to build the absolute issue link. */
	origin: string;
}

/**
 * §8 post-commit email step. For each `(userId, kind)` `notifyIssueEvent` returned:
 *   - `question_asked` ALWAYS emails, bypassing the throttle (Q11 -- a direct question deserves
 *     immediate email).
 *   - `commented` / `claimed` / `status_changed` / `resolved` email, subject to the 15-minute
 *     per-(user, issue) throttle (Q7).
 *   - everything else (`linked`, `progress_update`) never emails.
 *
 * Best-effort, like the invitation pattern (email.ts) -- a failed lookup or send for one user
 * never stops the others, and this function never throws; it is meant to be called AFTER the
 * mutation's transaction has committed, so nothing here can affect that transaction's outcome.
 */
export async function sendIssueNotificationEmails(
	notified: NotifiedUser[],
	ctx: SendNotificationEmailsContext
): Promise<void> {
	if (!notified || notified.length === 0) {
		return;
	}
	const toSend = notified.filter((n) => n.kind === BLOCKING_BYPASS_KIND || EMAILABLE_KINDS.has(n.kind));
	if (toSend.length === 0) {
		return;
	}

	let issueInfo: { title: string; issueType: string; projectId: string; orgSlug: string } | null = null;
	try {
		issueInfo = await getIssueLinkInfo(ctx.issueId);
	} catch (err) {
		log.error('notify.issue_link_lookup_failed', { issueId: ctx.issueId, error: err });
		return;
	}
	if (!issueInfo) {
		return;
	}

	const issueUrl = buildIssueUrl(ctx.origin, ctx.issueId, issueInfo);

	for (const { userId, kind } of toSend) {
		try {
			if (kind !== BLOCKING_BYPASS_KIND) {
				const throttled = await isThrottled(userId, ctx.issueId);
				if (throttled) {
					log.info('notify.email_throttled', { userId, issueId: ctx.issueId, kind });
					continue;
				}
			}

			const [userRow] = await db.select({ email: users.email }).from(users).where(eq(users.id, userId));
			if (!userRow?.email) {
				continue;
			}

			await sendIssueNotificationEmail(
				userRow.email,
				issueUrl,
				kind as IssueNotificationKind,
				issueInfo.title
			);

			// R5: stamp the notification row this email was actually for, so isThrottled's
			// `emailed_at`-based window reflects a real send, not just an attempt. Targets the
			// latest not-yet-emailed row for this (user, issue, kind) rather than the NotifiedUser
			// entry directly (it carries no row id) -- best-effort, like the rest of this function:
			// a failed update here never stops other users' emails from sending.
			await stampEmailedAt(userId, ctx.issueId, kind);
		} catch (err) {
			log.error('notify.email_send_failed', { userId, issueId: ctx.issueId, kind, error: err });
		}
	}
}

async function getIssueLinkInfo(
	issueId: string
): Promise<{ title: string; issueType: string; projectId: string; orgSlug: string } | null> {
	const [row] = await db
		.select({
			title: issues.message,
			issueType: issues.issueType,
			projectId: issues.projectId,
			orgSlug: organizations.slug,
		})
		.from(issues)
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.innerJoin(organizations, eq(organizations.id, projects.organizationId))
		.where(eq(issues.id, issueId));

	return row ?? null;
}

/**
 * §8/§10: reports vs service-issue path, by `issue_type` -- `/[orgSlug]/reports/[issueId]` for
 * `user_report` (routes/[orgSlug]/reports/[issueId]), `/[orgSlug]/projects/[projectId]/issues/
 * [issueId]` for `system_error` (routes/[orgSlug]/projects/[projectId]/issues/[issueId]).
 */
function buildIssueUrl(
	origin: string,
	issueId: string,
	info: { issueType: string; projectId: string; orgSlug: string }
): string {
	const trimmedOrigin = origin.replace(/\/$/, '');
	if (info.issueType === 'user_report') {
		return `${trimmedOrigin}/${info.orgSlug}/reports/${issueId}`;
	}
	return `${trimmedOrigin}/${info.orgSlug}/projects/${info.projectId}/issues/${issueId}`;
}
