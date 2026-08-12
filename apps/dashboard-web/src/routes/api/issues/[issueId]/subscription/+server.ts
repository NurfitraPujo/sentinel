import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { subscribe, unsubscribe, isSubscribed } from '$lib/db/queries/subscriptions';
import { requireCommentReadAccess } from '$lib/server/comment-access';

// Manual Issues M4 (docs/plans/MANUAL_ISSUES_DESIGN.md §8): the manual subscribe/unsubscribe
// toggle. Works on BOTH issue types (comment-access.ts's per-issue-type dispatcher, same as the
// comments/attachments routes) -- anyone who can READ an issue may opt in/out of its
// notifications, mirroring §9's "comment" permission tier, not the stricter "write" tier.
//
// Manual-validation style (allowlists + throw error(status)), matching the invitations/reports
// endpoints -- no schema library.

export const GET: RequestHandler = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}
	const userId = session.user.id;

	const { issueId } = params;
	if (!issueId) {
		throw error(400, 'Missing issueId');
	}

	await requireCommentReadAccess(userId, issueId);

	const subscribed = await isSubscribed(issueId, 'user', userId);
	return json({ subscribed });
};

export const PUT: RequestHandler = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}
	const userId = session.user.id;

	const { issueId } = params;
	if (!issueId) {
		throw error(400, 'Missing issueId');
	}

	await requireCommentReadAccess(userId, issueId);

	// §8: the manual toggle always records reason 'manual', regardless of any EARLIER auto-subscribe
	// reason -- subscribe() is an idempotent upsert that leaves an existing row's reason untouched,
	// so a user who is already subscribed as 'reporter'/'claimant'/'participant' and then explicitly
	// opts in via this toggle keeps their original reason (which is the more informative one to
	// keep, and matches "leave the existing row's reason untouched" documented on subscribe()).
	await subscribe({ issueId, subscriberType: 'user', subscriberId: userId, reason: 'manual' });

	return json({ subscribed: true });
};

export const DELETE: RequestHandler = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}
	const userId = session.user.id;

	const { issueId } = params;
	if (!issueId) {
		throw error(400, 'Missing issueId');
	}

	await requireCommentReadAccess(userId, issueId);

	await unsubscribe({ issueId, subscriberType: 'user', subscriberId: userId });

	return json({ subscribed: false });
};
