import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import {
	editComment,
	deleteComment,
	getCommentById,
	CommentValidationError,
	CommentNotFoundError,
} from '$lib/db/queries/comments';
import { requireCommentReadAccess, isCommentModeratorRole } from '$lib/server/comment-access';

// Manual Issues M3 (design §5, §9): author-only edit; author OR owner/admin delete, enforced
// HERE at the route layer (per the design's own phrasing) rather than in the query functions,
// which trust their caller the same way claimIssue/releaseClaim already do.

async function loadOwnedComment(issueId: string, commentId: string) {
	const comment = await getCommentById(commentId);
	if (!comment || comment.issueId !== issueId) {
		throw error(404, 'Comment not found');
	}
	return comment;
}

export const PATCH: RequestHandler = async ({ params, request, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}
	const userId = session.user.id;

	const { issueId, commentId } = params;
	if (!issueId || !commentId) {
		throw error(400, 'Missing issueId or commentId');
	}

	const access = await requireCommentReadAccess(userId, issueId);
	const comment = await loadOwnedComment(issueId, commentId);

	const isAuthor = comment.authorType === 'user' && comment.authorId === userId;
	if (!isAuthor) {
		throw error(403, 'Forbidden: only the comment author may edit it');
	}
	if (access.issueStatus === 'resolved') {
		throw error(409, 'Cannot edit a comment on a resolved issue');
	}

	const body = await request.json().catch(() => null);
	if (!body || typeof body.bodyMd !== 'string') {
		throw error(400, 'bodyMd is required');
	}

	try {
		const updated = await editComment(commentId, body.bodyMd);
		return json({ comment: updated });
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

export const DELETE: RequestHandler = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}
	const userId = session.user.id;

	const { issueId, commentId } = params;
	if (!issueId || !commentId) {
		throw error(400, 'Missing issueId or commentId');
	}

	const access = await requireCommentReadAccess(userId, issueId);
	const comment = await loadOwnedComment(issueId, commentId);

	const isAuthor = comment.authorType === 'user' && comment.authorId === userId;
	if (!isAuthor) {
		// §9: "Force-release a claim, delete others' comments: owner, admin".
		if (!isCommentModeratorRole(access.role)) {
			throw error(403, 'Forbidden: only the comment author or an org owner/admin may delete it');
		}
	} else if (access.issueStatus === 'resolved') {
		throw error(409, 'Cannot delete a comment on a resolved issue');
	}

	try {
		const result = await deleteComment(commentId);
		return json({ success: true, issueId: result.issueId });
	} catch (err) {
		if (err instanceof CommentNotFoundError) {
			throw error(404, err.message);
		}
		throw err;
	}
};
