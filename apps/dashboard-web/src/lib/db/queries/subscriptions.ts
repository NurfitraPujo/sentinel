import { db } from '$lib/server/db';
import { issueSubscriptions } from '$lib/db/schema';
import { and, eq } from 'drizzle-orm';

/**
 * Manual Issues M4 (docs/plans/MANUAL_ISSUES_DESIGN.md §8): who gets notified about an issue.
 * `subscribe` is an idempotent upsert -- callers (auto-subscribe wiring in reports.ts/issues.ts/
 * comments.ts, and the manual toggle route) never need to check "am I already subscribed" first;
 * calling it twice with the same (issueId, subscriberType, subscriberId) is a no-op on the second
 * call, backed by the DB's UNIQUE(issue_id, subscriber_type, subscriber_id) index
 * (idx_issue_subscriptions_unique). `reason` on a re-subscribe is NOT overwritten -- the first
 * reason recorded (e.g. 'reporter') wins over a later, weaker one (e.g. 'participant' from also
 * commenting); `onConflictDoNothing` leaves the existing row untouched.
 */
export type SubscriberType = 'user' | 'agent';
export type SubscriptionReason = 'reporter' | 'claimant' | 'participant' | 'manual';

export interface SubscribeInput {
	issueId: string;
	subscriberType: SubscriberType;
	subscriberId: string;
	reason: SubscriptionReason;
}

// tx is optional so auto-subscribe wiring can pass the enclosing db.transaction (D18: same
// transaction as the mutation it accompanies) while the manual toggle route can call this
// standalone.
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export async function subscribe(input: SubscribeInput, tx: any = db) {
	await tx
		.insert(issueSubscriptions)
		.values({
			issueId: input.issueId,
			subscriberType: input.subscriberType,
			subscriberId: input.subscriberId,
			reason: input.reason,
		})
		.onConflictDoNothing({
			target: [issueSubscriptions.issueId, issueSubscriptions.subscriberType, issueSubscriptions.subscriberId],
		});
}

export interface UnsubscribeInput {
	issueId: string;
	subscriberType: SubscriberType;
	subscriberId: string;
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
export async function unsubscribe(input: UnsubscribeInput, tx: any = db) {
	await tx
		.delete(issueSubscriptions)
		.where(
			and(
				eq(issueSubscriptions.issueId, input.issueId),
				eq(issueSubscriptions.subscriberType, input.subscriberType),
				eq(issueSubscriptions.subscriberId, input.subscriberId)
			)
		);
}

/** §8 fan-out target list. Returns every subscriber row (user AND agent) for an issue. */
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export async function listSubscribers(issueId: string, tx: any = db) {
	return await tx.select().from(issueSubscriptions).where(eq(issueSubscriptions.issueId, issueId));
}

/** Whether `subscriberId` is currently subscribed to `issueId` -- backs the toggle UI's initial state. */
export async function isSubscribed(
	issueId: string,
	subscriberType: SubscriberType,
	subscriberId: string
): Promise<boolean> {
	const rows = await db
		.select({ id: issueSubscriptions.id })
		.from(issueSubscriptions)
		.where(
			and(
				eq(issueSubscriptions.issueId, issueId),
				eq(issueSubscriptions.subscriberType, subscriberType),
				eq(issueSubscriptions.subscriberId, subscriberId)
			)
		);
	return rows.length > 0;
}
