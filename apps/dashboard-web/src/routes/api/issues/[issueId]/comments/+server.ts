import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import {
	createComment,
	listComments,
	CommentValidationError,
	CommentNotFoundError,
} from '$lib/db/queries/comments';
import { requireCommentReadAccess, requireCommentWriteAccess } from '$lib/server/comment-access';

// Manual Issues M3 (design §5, §9, §10). Manual-validation style (allowlists + throw
// error(status)), matching uploads/reports. Works on BOTH issue types -- see comment-access.ts's
// header for why this dispatches per issue_type instead of using either `report-access.ts` or
// `issue-access.ts` alone.

export const GET: RequestHandler = async ({ params, url, locals }) => {
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

	// §5/Q10: `?after=<ISO timestamp>` serves the polling endpoint.
	const afterParam = url.searchParams.get('after');
	let after: Date | undefined;
	if (afterParam !== null) {
		const parsed = new Date(afterParam);
		if (Number.isNaN(parsed.getTime())) {
			throw error(400, 'after must be a valid ISO 8601 timestamp');
		}
		after = parsed;
	}

	const comments = await listComments(issueId, { after });

	return json({ comments });
};

export const POST: RequestHandler = async ({ params, request, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}
	const userId = session.user.id;

	const { issueId } = params;
	if (!issueId) {
		throw error(400, 'Missing issueId');
	}

	// §9: any member who can read the issue, including a report's viewer -- comment permission,
	// not write.
	await requireCommentWriteAccess(userId, issueId);

	const body = await request.json().catch(() => null);
	if (!body || typeof body !== 'object') {
		throw error(400, 'Expected a JSON body');
	}

	if (typeof body.bodyMd !== 'string' || body.bodyMd.trim().length === 0) {
		throw error(400, 'bodyMd is required');
	}

	let parentId: string | undefined;
	if (body.parentId !== undefined && body.parentId !== null) {
		if (typeof body.parentId !== 'string' || body.parentId.length === 0) {
			throw error(400, 'parentId must be a non-empty string');
		}
		parentId = body.parentId;
	}

	let attachmentIds: string[] | undefined;
	if (body.attachmentIds !== undefined) {
		if (
			!Array.isArray(body.attachmentIds) ||
			!body.attachmentIds.every((id: unknown) => typeof id === 'string')
		) {
			throw error(400, 'attachmentIds must be an array of strings');
		}
		attachmentIds = body.attachmentIds;
	}

	try {
		// This route is session-authenticated only -- every comment created here is authored by a
		// USER. Agent-authored comments arrive through the key-authenticated `/api/agent/*`
		// work-loop (M5), which will call the SAME `createComment` with `authorType: 'agent'`.
		const comment = await createComment({
			issueId,
			authorType: 'user',
			authorId: userId,
			bodyMd: body.bodyMd,
			parentId,
			attachmentIds,
		});

		return json({ comment }, { status: 201 });
	} catch (err) {
		if (err instanceof CommentValidationError) {
			throw error(400, err.message);
		}
		if (err instanceof CommentNotFoundError) {
			throw error(404, err.message);
		}
		throw err;
	}
};
