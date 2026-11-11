// Manual Issues M3 (docs/plans/MANUAL_ISSUES_DESIGN.md §5/Q10): pure merge logic for the thread's
// polling loop, extracted from CommentThread.svelte so it is testable without mounting a
// component or faking timers. `GET .../comments?after=<ts>` returns whole root threads that have
// ANY new activity (new root OR new reply on an existing root) -- see comments.ts's listComments
// doc comment -- so merging must REPLACE a root that already exists locally (to pick up its new
// replies) rather than blindly appending, while genuinely new roots get appended in order.

export interface CommentAttachment {
	id: string;
	filename: string;
	contentType: string;
	sizeBytes: number;
}

export interface CommentNode {
	id: string;
	issueId: string;
	parentId: string | null;
	authorType: string;
	authorId: string;
	authorName: string | null;
	authorEmail: string | null;
	blocking: boolean;
	bodyMd: string;
	createdAt: string;
	editedAt: string | null;
	attachments: CommentAttachment[];
	replies: CommentNode[];
}

/**
 * Merges a batch of freshly-polled root threads into the currently-rendered set. Roots are
 * matched by id: an existing root is REPLACED wholesale (its replies array may have grown), a
 * new root is appended. Order is preserved as chronological (existing order first, new roots in
 * the order the server returned them, which is already creation order).
 */
export function mergeNewComments(existing: CommentNode[], incoming: CommentNode[]): CommentNode[] {
	if (incoming.length === 0) return existing;

	const byId = new Map(existing.map((root) => [root.id, root]));
	const result: CommentNode[] = existing.map((root) => byId.get(root.id) ?? root);

	for (const incomingRoot of incoming) {
		if (byId.has(incomingRoot.id)) {
			const idx = result.findIndex((r) => r.id === incomingRoot.id);
			if (idx !== -1) result[idx] = incomingRoot;
		} else {
			result.push(incomingRoot);
			byId.set(incomingRoot.id, incomingRoot);
		}
	}

	return result;
}

/** Latest `createdAt` across every root and reply, as an ISO string -- the next poll's `after`. */
export function latestCommentTimestamp(roots: CommentNode[]): string | null {
	let latest: string | null = null;
	const consider = (value: string) => {
		if (latest === null || new Date(value).getTime() > new Date(latest).getTime()) {
			latest = value;
		}
	};
	for (const root of roots) {
		consider(root.createdAt);
		for (const reply of root.replies) {
			consider(reply.createdAt);
		}
	}
	return latest;
}

/** Total comment count (roots + replies), used for a badge/heading. */
export function countComments(roots: CommentNode[]): number {
	return roots.reduce((sum, root) => sum + 1 + root.replies.length, 0);
}
