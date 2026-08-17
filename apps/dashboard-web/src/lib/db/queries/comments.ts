import { db } from '$lib/server/db';
import { issues, issueActivity, issueComments, attachments, users, projects } from '$lib/db/schema';
import { eq, and, desc, gte, inArray } from 'drizzle-orm';
import { claimDraftAttachmentsForComment } from './reports';
import { deleteObject, isStorageConfigured } from '$lib/server/storage';
import { log } from '$lib/server/observability/log';
import { subscribe } from '$lib/db/queries/subscriptions';
import { notifyIssueEvent, type NotifiedUser } from '$lib/server/notify';
import { AGENT_DEDUPE_WINDOW_MS } from '$lib/server/agent-dedupe';

/**
 * Manual Issues M3 (docs/plans/MANUAL_ISSUES_DESIGN.md §5, plus the comment-attachment paths
 * §4, and Q11 groundwork). Threads work on BOTH issue types (§5) -- nothing in this module reads
 * `issues.issue_type`, unlike reports.ts and report-access.ts which are deliberately
 * `user_report`-only. Every write here follows D18 (throw inside db.transaction, never return
 * early to signal failure).
 */

export class CommentValidationError extends Error {}
export class CommentNotFoundError extends Error {}

export type CommentAuthorType = 'user' | 'agent';

export interface CreateCommentInput {
	issueId: string;
	authorType: CommentAuthorType;
	authorId: string;
	bodyMd: string;
	/**
	 * §5: one-level threads. A reply's parent is resolved (not rejected) to the ROOT of whatever
	 * comment it names -- replying to a reply attaches to that reply's own parent, so a thread
	 * never nests more than one level deep, matching Slack.
	 */
	parentId?: string | null;
	/** §4 groundwork: DRAFT attachment ids to claim onto this comment in the same transaction. */
	attachmentIds?: string[];
	/**
	 * M5 §7 step 4 (Q11): set true by the agent `POST /api/agent/issues/[id]/questions` endpoint
	 * ONLY. Marks the comment row `blocking=true`, sets `issues.waiting_on` to `waitingOnAudience`
	 * in the SAME transaction, and fans out `notifications.kind='question_asked'` instead of
	 * 'commented' (bypasses the email throttle per Q11/M4's `BLOCKING_BYPASS_KIND`). Every other
	 * caller (session-authenticated user comments, agent progress/plain comments) omits this and
	 * gets the M3 behavior unchanged.
	 */
	blocking?: boolean;
	/** Required when `blocking` is true: who the question is addressed to (Q11). */
	waitingOnAudience?: 'reporter' | 'team';
}

/**
 * §5/§6/§9/Q11: inserts the comment, resolves+validates `parentId`, claims any draft attachments
 * onto it (reusing/extending the M2 claim helper, B7), writes a `commented` activity row, and --
 * if the author is a USER and the issue is currently `waiting_on` someone -- clears `waiting_on`
 * and writes a `question_answered` activity row. All in one transaction (D18: throw, never
 * return early, so a bad parent or a failed insert rolls back everything together).
 */
export async function createComment(
	input: CreateCommentInput
): Promise<{ comment: typeof issueComments.$inferSelect; notified: NotifiedUser[] }> {
	const bodyMd = input.bodyMd.trim();
	if (bodyMd.length === 0) {
		throw new CommentValidationError('bodyMd must not be empty');
	}
	if (input.blocking && input.waitingOnAudience !== 'reporter' && input.waitingOnAudience !== 'team') {
		throw new CommentValidationError('waitingOnAudience is required and must be "reporter" or "team" when blocking');
	}

	return await db.transaction(async (tx) => {
		const issueRows = await tx
			.select({ id: issues.id, projectId: issues.projectId, waitingOn: issues.waitingOn })
			.from(issues)
			.where(eq(issues.id, input.issueId));

		const issueRow = issueRows[0];
		if (!issueRow) {
			throw new CommentNotFoundError(`Issue ${input.issueId} not found`);
		}

		const [projectRow] = await tx
			.select({ organizationId: projects.organizationId })
			.from(projects)
			.where(eq(projects.id, issueRow.projectId));

		if (!projectRow?.organizationId) {
			throw new Error('Issue project does not belong to an organization');
		}

		// §5: replying to a reply resolves to the SAME parent as the comment it replies to, rather
		// than rejecting the request -- the client shouldn't need to know whether the comment it's
		// replying to is itself a reply.
		let resolvedParentId: string | null = null;
		if (input.parentId) {
			const parentRows = await tx
				.select({
					id: issueComments.id,
					issueId: issueComments.issueId,
					parentId: issueComments.parentId,
				})
				.from(issueComments)
				.where(eq(issueComments.id, input.parentId));

			const parent = parentRows[0];
			if (!parent || parent.issueId !== input.issueId) {
				throw new CommentValidationError('parentId does not belong to this issue');
			}

			resolvedParentId = parent.parentId ?? parent.id;
		}

		// A05-comment (N7d): dedupe a plain-comment retry (dropped response, agent resends the
		// identical request) by natural key -- same issue+author+body within AGENT_DEDUPE_WINDOW_MS.
		// Deliberately NOT applied to blocking questions: a question's `waiting_on` side effect must
		// be predictable (a caller always gets a definite "this question is now live" signal), and
		// silently reusing an older row here could mask whether the CURRENT `waiting_on` state
		// actually reflects this call. Plain comments have no such side effect to protect, so
		// swallowing a rare identical-repost is the safer trade there (documented risk, N7d plan).
		if (!input.blocking) {
			const recentDuplicates = await tx
				.select()
				.from(issueComments)
				.where(
					and(
						eq(issueComments.issueId, input.issueId),
						eq(issueComments.authorType, input.authorType),
						eq(issueComments.authorId, input.authorId),
						eq(issueComments.bodyMd, bodyMd),
						gte(issueComments.createdAt, new Date(Date.now() - AGENT_DEDUPE_WINDOW_MS))
					)
				)
				.orderBy(desc(issueComments.createdAt));

			const existing = recentDuplicates[0];
			if (existing) {
				return { comment: existing, notified: [] };
			}
		}

		const [comment] = await tx
			.insert(issueComments)
			.values({
				issueId: input.issueId,
				parentId: resolvedParentId,
				authorType: input.authorType,
				authorId: input.authorId,
				bodyMd,
				blocking: Boolean(input.blocking),
			})
			.returning();

		if (!comment) {
			throw new Error('Failed to create comment');
		}

		let claimedAttachmentIds: string[] = [];
		if (input.attachmentIds && input.attachmentIds.length > 0) {
			const claimed = await claimDraftAttachmentsForComment(
				tx,
				input.attachmentIds,
				comment.id,
				input.authorId,
				projectRow.organizationId,
				input.authorType
			);
			claimedAttachmentIds = claimed.map((a) => a.id);
		}

		// M5 §7 step 4 (Q11): a blocking question sets issues.waiting_on in the SAME transaction as
		// the comment insert and its activity row (D18) -- 'question_asked' rather than 'commented'.
		if (input.blocking) {
			// N9 (AGENT_WORKER_PLAN C12): stamp waiting_since in the SAME transaction as waiting_on so
			// the list's `waitingSince` reflects when THIS question started blocking.
			await tx
				.update(issues)
				.set({ waitingOn: input.waitingOnAudience, waitingSince: new Date() })
				.where(eq(issues.id, input.issueId));
		}

		await tx.insert(issueActivity).values({
			issueId: input.issueId,
			eventType: input.blocking ? 'question_asked' : 'commented',
			actorType: input.authorType,
			actorId: input.authorId,
			newValue: {
				commentId: comment.id,
				parentId: resolvedParentId,
				...(input.blocking ? { waitingOn: input.waitingOnAudience } : {}),
				...(claimedAttachmentIds.length > 0 ? { attachmentIds: claimedAttachmentIds } : {}),
			},
		});

		// Q11 step 5: any USER reply clears a pending `waiting_on`, regardless of who set it or
		// which audience it targeted -- the agent work-loop (M5) polls comments to notice the answer.
		// A blocking question is itself authored by an agent (never a user), so this branch and the
		// one above are mutually exclusive for a single call.
		if (input.authorType === 'user' && issueRow.waitingOn !== null) {
			// N9 (AGENT_WORKER_PLAN C12): clear waiting_since alongside waiting_on.
			await tx.update(issues).set({ waitingOn: null, waitingSince: null }).where(eq(issues.id, input.issueId));

			await tx.insert(issueActivity).values({
				issueId: input.issueId,
				eventType: 'question_answered',
				actorType: 'user',
				actorId: input.authorId,
				oldValue: { waitingOn: issueRow.waitingOn },
				newValue: { waitingOn: null },
			});
		}

		// §8 auto-subscribe: any USER commenter is subscribed (reason 'participant') -- including a
		// commenter who is already subscribed for another reason (reporter/claimant), since
		// subscribe() is an idempotent upsert that leaves an existing row's reason untouched. Agent
		// commenters are not subscribed -- they poll (design §8), same as every other agent path in
		// M4.
		if (input.authorType === 'user') {
			await subscribe(
				{ issueId: input.issueId, subscriberType: 'user', subscriberId: input.authorId, reason: 'participant' },
				tx
			);
		}

		// M5 §7 step 4/Q11: a blocking question fans out kind 'question_asked' -- notify.ts's
		// BLOCKING_BYPASS_KIND makes this bypass the 15-min email throttle unconditionally, unlike
		// plain 'commented'.
		const notified = await notifyIssueEvent(tx, {
			issueId: input.issueId,
			kind: input.blocking ? 'question_asked' : 'commented',
			actorType: input.authorType,
			actorId: input.authorId,
			payload: { commentId: comment.id, parentId: resolvedParentId },
		});

		return { comment, notified };
	});
}

export interface CommentWithThread {
	id: string;
	issueId: string;
	parentId: string | null;
	authorType: string;
	authorId: string;
	authorName: string | null;
	authorEmail: string | null;
	blocking: boolean;
	bodyMd: string;
	createdAt: Date;
	editedAt: Date | null;
	attachments: (typeof attachments.$inferSelect)[];
	replies: CommentWithThread[];
}

/**
 * §5/§10: root comments chronological, each with its nested replies and their attachments +
 * author display info (left-joined against `users` -- agent authors simply get null name/email,
 * per design "join users for user authors").
 *
 * `options.after`, when given, serves the polling endpoint (Q10): only roots that are themselves
 * new, OR whose thread has a new reply, are returned -- and the WHOLE thread comes back (not just
 * the new reply) so a poller always has enough context to render it, never a orphaned reply with
 * no visible parent.
 */
export async function listComments(
	issueId: string,
	options: { after?: Date } = {}
): Promise<CommentWithThread[]> {
	const commentRows = await db
		.select({
			id: issueComments.id,
			issueId: issueComments.issueId,
			parentId: issueComments.parentId,
			authorType: issueComments.authorType,
			authorId: issueComments.authorId,
			blocking: issueComments.blocking,
			bodyMd: issueComments.bodyMd,
			createdAt: issueComments.createdAt,
			editedAt: issueComments.editedAt,
			authorName: users.name,
			authorEmail: users.email,
		})
		.from(issueComments)
		.leftJoin(users, eq(users.id, issueComments.authorId))
		.where(eq(issueComments.issueId, issueId))
		.orderBy(issueComments.createdAt);

	const commentIds = commentRows.map((c) => c.id);
	const attachmentRows =
		commentIds.length > 0
			? await db.select().from(attachments).where(inArray(attachments.commentId, commentIds))
			: [];

	const attachmentsByComment = new Map<string, (typeof attachments.$inferSelect)[]>();
	for (const row of attachmentRows) {
		if (!row.commentId) continue;
		const list = attachmentsByComment.get(row.commentId) ?? [];
		list.push(row);
		attachmentsByComment.set(row.commentId, list);
	}

	const nodesById = new Map<string, CommentWithThread>();
	for (const row of commentRows) {
		nodesById.set(row.id, {
			...row,
			attachments: attachmentsByComment.get(row.id) ?? [],
			replies: [],
		});
	}

	const roots: CommentWithThread[] = [];
	for (const row of commentRows) {
		const node = nodesById.get(row.id)!;
		if (row.parentId) {
			const parent = nodesById.get(row.parentId);
			// A parent could theoretically be missing from this fetch only if the data is corrupt
			// (FK guarantees it exists) -- fail closed by dropping the orphaned reply rather than
			// throwing and breaking the whole listing.
			if (parent) {
				parent.replies.push(node);
			}
		} else {
			roots.push(node);
		}
	}

	if (!options.after) {
		return roots;
	}

	const after = options.after;
	// R9 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): this filtered on `createdAt` only, so an
	// EDIT to an existing comment (root or reply) never made it back through the poll -- editing
	// doesn't insert a new row or touch `createdAt`, only `editedAt`. Checked on every node in the
	// thread (root + each reply), matching the "whole affected thread" contract this function's
	// own doc comment already establishes for new replies.
	//
	// Deletes: a REPLY delete is covered by `deleteComment` touching its root's `editedAt` (see
	// that function's doc comment) -- the edited-root check below then re-sends the (now-shorter)
	// thread. A ROOT delete has nothing left in this result set to touch or filter on -- the
	// deliberately simplest correct contract here is that a deleted root's disappearance is NOT
	// live-propagated through polling; it surfaces on the next full `listComments()` (unfiltered)
	// load, same as CommentThread.svelte's initial mount.
	const wasEdited = (node: CommentWithThread) => node.editedAt !== null && node.editedAt > after;
	return roots.filter(
		(root) =>
			root.createdAt > after ||
			wasEdited(root) ||
			root.replies.some((reply) => reply.createdAt > after || wasEdited(reply))
	);
}

/** Fetches a single comment row (no thread/attachment expansion) for route-layer author checks. */
export async function getCommentById(commentId: string) {
	const rows = await db
		.select({
			id: issueComments.id,
			issueId: issueComments.issueId,
			parentId: issueComments.parentId,
			authorType: issueComments.authorType,
			authorId: issueComments.authorId,
			blocking: issueComments.blocking,
			bodyMd: issueComments.bodyMd,
			createdAt: issueComments.createdAt,
			editedAt: issueComments.editedAt,
		})
		.from(issueComments)
		.where(eq(issueComments.id, commentId));

	return rows[0] ?? null;
}

/**
 * §5/§9: author-only editing (enforced at the route layer, not here -- this function trusts its
 * caller). D18: throw, don't return, when the target row doesn't exist.
 */
export async function editComment(commentId: string, bodyMd: string) {
	const trimmed = bodyMd.trim();
	if (trimmed.length === 0) {
		throw new CommentValidationError('bodyMd must not be empty');
	}

	const [updated] = await db
		.update(issueComments)
		.set({ bodyMd: trimmed, editedAt: new Date() })
		.where(eq(issueComments.id, commentId))
		.returning();

	if (!updated) {
		throw new CommentNotFoundError(`Comment ${commentId} not found`);
	}

	return updated;
}

/**
 * §5/§9: deletes a comment. A ROOT comment's replies cascade via the `parent_id` FK
 * (`ON DELETE CASCADE`, schema.ts), and any attachments linked to the comment (or its replies)
 * cascade via `attachments.comment_id`'s FK the same way -- so the DB rows are handled by one
 * `DELETE` inside the transaction. What the FK cascade does NOT do is clean up the underlying
 * MinIO objects, so this collects every affected attachment's `storageKey` *before* deleting, and
 * removes them from storage best-effort AFTER the transaction commits (mirrors the ordering
 * rationale in attachments/[id]/+server.ts's DELETE: a crash between DB commit and storage
 * cleanup leaves an orphaned object, not a dangling DB reference).
 */
export async function deleteComment(commentId: string) {
	const { issueId, storageKeys } = await db.transaction(async (tx) => {
		const [comment] = await tx
			.select({ id: issueComments.id, issueId: issueComments.issueId, parentId: issueComments.parentId })
			.from(issueComments)
			.where(eq(issueComments.id, commentId));

		if (!comment) {
			throw new CommentNotFoundError(`Comment ${commentId} not found`);
		}

		const replyRows = await tx
			.select({ id: issueComments.id })
			.from(issueComments)
			.where(eq(issueComments.parentId, commentId));

		const allCommentIds = [commentId, ...replyRows.map((r) => r.id)];

		const attachmentRows = await tx
			.select({ storageKey: attachments.storageKey })
			.from(attachments)
			.where(inArray(attachments.commentId, allCommentIds));

		await tx.delete(issueComments).where(eq(issueComments.id, commentId));

		// R9 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): deleting a REPLY leaves its root
		// untouched by the delete above, so a poller's `after` filter (createdAt/editedAt only)
		// would never notice the reply is gone. Bumping the root's `editedAt` here makes the
		// thread look "changed" to the next poll, which then re-fetches and replaces it with the
		// (now shorter) thread -- the same mechanism an edit already relies on. A ROOT delete has
		// no row left to bump; see listComments's doc comment for that half of the contract.
		if (comment.parentId) {
			await tx.update(issueComments).set({ editedAt: new Date() }).where(eq(issueComments.id, comment.parentId));
		}

		return { issueId: comment.issueId, storageKeys: attachmentRows.map((a) => a.storageKey) };
	});

	if (storageKeys.length > 0 && isStorageConfigured()) {
		for (const key of storageKeys) {
			try {
				await deleteObject(key);
			} catch (err) {
				log.error('comments.delete_attachment_storage_failed', { commentId, key, error: err });
			}
		}
	}

	return { issueId };
}
