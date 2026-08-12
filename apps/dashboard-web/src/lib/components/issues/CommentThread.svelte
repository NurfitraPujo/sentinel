<script lang="ts">
	// Manual Issues M3 (docs/plans/MANUAL_ISSUES_DESIGN.md §5/§3/§10): Slack-like discussion thread.
	// Works on BOTH issue types (mounted on /reports/[issueId] and
	// /projects/[projectId]/issues/[issueId]) -- this component itself doesn't know or care which
	// kind of issue it's attached to, matching comment-access.ts's per-issue-type dispatch on the
	// server. Root comments render chronologically; each root's replies start collapsed behind a
	// "N replies" expander (one-level threads, per createComment's parentId resolution). Freshness
	// is polling (Q10): while the document is visible, GET .../comments?after=<latest seen
	// createdAt> every ~10s and merge the result in; the merge/timestamp logic lives in the
	// testable `comment-poll.ts` module rather than inline here.
	import Markdown from '$lib/components/Markdown.svelte';
	import AttachmentList from '$lib/components/attachments/AttachmentList.svelte';
	import CommentComposer from '$lib/components/issues/CommentComposer.svelte';
	import {
		mergeNewComments,
		latestCommentTimestamp,
		countComments,
		type CommentNode,
	} from '$lib/comments/comment-poll';

	interface Props {
		issueId: string;
		organizationId: string;
		currentUserId: string;
		currentUserRole?: string | null;
		/** Polling interval in ms -- overridable so tests don't have to wait 10s. */
		pollIntervalMs?: number;
	}

	let { issueId, organizationId, currentUserId, currentUserRole = null, pollIntervalMs = 10_000 }: Props = $props();

	let roots = $state<CommentNode[]>([]);
	let loading = $state(true);
	let loadError = $state<string | null>(null);
	let lastSeen = $state<string | null>(null);
	let expanded = $state<Set<string>>(new Set());
	let replyTarget = $state<string | null>(null);
	let editingId = $state<string | null>(null);
	let editBody = $state('');
	let editError = $state<string | null>(null);
	let rowError = $state<Record<string, string>>({});

	const isModerator = $derived(currentUserRole === 'owner' || currentUserRole === 'admin');

	function isOwn(node: CommentNode): boolean {
		return node.authorType === 'user' && node.authorId === currentUserId;
	}

	function canDelete(node: CommentNode): boolean {
		return isOwn(node) || isModerator;
	}

	function displayName(node: CommentNode): string {
		if (node.authorType === 'agent') return node.authorName ?? node.authorId;
		return node.authorName ?? node.authorEmail ?? node.authorId;
	}

	function formatTime(value: string): string {
		const date = new Date(value);
		if (Number.isNaN(date.getTime())) return 'unknown time';
		return date.toLocaleString();
	}

	async function loadInitial() {
		loading = true;
		loadError = null;
		try {
			const res = await fetch(`/api/issues/${issueId}/comments`);
			if (!res.ok) {
				throw new Error(`Failed to load comments (${res.status})`);
			}
			const body = await res.json();
			roots = body.comments ?? [];
			lastSeen = latestCommentTimestamp(roots);
		} catch (err) {
			loadError = err instanceof Error ? err.message : 'Failed to load comments';
		} finally {
			loading = false;
		}
	}

	async function poll() {
		if (typeof document !== 'undefined' && document.hidden) return;
		try {
			const url = lastSeen
				? `/api/issues/${issueId}/comments?after=${encodeURIComponent(lastSeen)}`
				: `/api/issues/${issueId}/comments`;
			const res = await fetch(url);
			if (!res.ok) return; // best-effort -- a transient poll failure shouldn't surface an error banner
			const body = await res.json();
			const incoming: CommentNode[] = body.comments ?? [];
			if (incoming.length > 0) {
				roots = mergeNewComments(roots, incoming);
				const newest = latestCommentTimestamp(incoming);
				if (newest && (!lastSeen || new Date(newest).getTime() > new Date(lastSeen).getTime())) {
					lastSeen = newest;
				}
			}
		} catch {
			// best-effort, same rationale as above
		}
	}

	$effect(() => {
		void loadInitial();
	});

	$effect(() => {
		const interval = setInterval(() => void poll(), pollIntervalMs);
		return () => clearInterval(interval);
	});

	function toggleExpanded(rootId: string) {
		const next = new Set(expanded);
		if (next.has(rootId)) next.delete(rootId);
		else next.add(rootId);
		expanded = next;
	}

	async function postComment(bodyMd: string, attachmentIds: string[], parentId: string | null) {
		const res = await fetch(`/api/issues/${issueId}/comments`, {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ bodyMd, attachmentIds, parentId: parentId ?? undefined }),
		});
		if (!res.ok) {
			const body = await res.json().catch(() => ({}));
			throw new Error(body.message || `Failed to post comment (${res.status})`);
		}
		// A freshly posted comment is itself "new" activity for its root -- refetch just this
		// comment's thread via the normal poll path rather than hand-rolling a second merge shape.
		await poll();
		if (parentId) {
			const next = new Set(expanded);
			next.add(parentId);
			expanded = next;
		}
		replyTarget = null;
	}

	function startEdit(node: CommentNode) {
		editingId = node.id;
		editBody = node.bodyMd;
		editError = null;
	}

	function cancelEdit() {
		editingId = null;
		editBody = '';
		editError = null;
	}

	async function saveEdit(node: CommentNode) {
		const trimmed = editBody.trim();
		if (trimmed.length === 0) return;
		editError = null;
		try {
			const res = await fetch(`/api/issues/${issueId}/comments/${node.id}`, {
				method: 'PATCH',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ bodyMd: trimmed }),
			});
			if (!res.ok) {
				const body = await res.json().catch(() => ({}));
				throw new Error(body.message || `Failed to edit comment (${res.status})`);
			}
			node.bodyMd = trimmed;
			node.editedAt = new Date().toISOString();
			editingId = null;
		} catch (err) {
			editError = err instanceof Error ? err.message : 'Failed to edit comment';
		}
	}

	async function deleteComment(node: CommentNode) {
		rowError = { ...rowError, [node.id]: '' };
		try {
			const res = await fetch(`/api/issues/${issueId}/comments/${node.id}`, { method: 'DELETE' });
			if (!res.ok) {
				const body = await res.json().catch(() => ({}));
				throw new Error(body.message || `Failed to delete comment (${res.status})`);
			}
			roots = roots
				.filter((r) => r.id !== node.id)
				.map((r) => ({ ...r, replies: r.replies.filter((reply) => reply.id !== node.id) }));
		} catch (err) {
			rowError = { ...rowError, [node.id]: err instanceof Error ? err.message : 'Failed to delete' };
		}
	}
</script>

{#snippet commentRow(node: CommentNode, isReply: boolean)}
	<li class="comment-row" class:is-reply={isReply} data-testid="comment-{node.id}">
		<span class="avatar" class:agent-avatar={node.authorType === 'agent'} aria-hidden="true">
			{node.authorType === 'agent' ? '\u{1F916}' : '\u{1F464}'}
		</span>
		<div class="comment-body-wrap">
			<div class="comment-meta">
				<span class="author-name">{displayName(node)}</span>
				{#if node.authorType === 'agent'}
					<span class="agent-badge">Agent</span>
				{/if}
				<span class="comment-time">{formatTime(node.createdAt)}</span>
				{#if node.editedAt}
					<span class="edited-marker">(edited)</span>
				{/if}
			</div>

			{#if editingId === node.id}
				<div class="edit-box">
					<textarea class="edit-textarea" bind:value={editBody} rows="3" aria-label="Edit comment"></textarea>
					{#if editError}<p class="row-error" role="alert">{editError}</p>{/if}
					<div class="edit-actions">
						<button type="button" class="btn-link" onclick={cancelEdit}>Cancel</button>
						<button type="button" class="btn-primary-sm" onclick={() => saveEdit(node)}>Save</button>
					</div>
				</div>
			{:else}
				<Markdown source={node.bodyMd} />
				{#if node.attachments.length > 0}
					<div class="comment-attachments">
						<AttachmentList attachments={node.attachments} />
					</div>
				{/if}
			{/if}

			{#if rowError[node.id]}
				<p class="row-error" role="alert">{rowError[node.id]}</p>
			{/if}

			{#if editingId !== node.id}
				<div class="comment-actions">
					{#if !isReply}
						<button type="button" class="btn-link" onclick={() => (replyTarget = replyTarget === node.id ? null : node.id)}>
							Reply
						</button>
					{/if}
					{#if isOwn(node)}
						<button type="button" class="btn-link" onclick={() => startEdit(node)}>Edit</button>
					{/if}
					{#if canDelete(node)}
						<button type="button" class="btn-link btn-link-danger" onclick={() => deleteComment(node)}>Delete</button>
					{/if}
				</div>
			{/if}
		</div>
	</li>
{/snippet}

<div class="comment-thread" data-testid="comment-thread">
	<h2 class="thread-heading">Discussion {#if !loading}<span class="thread-count">({countComments(roots)})</span>{/if}</h2>

	{#if loading}
		<p class="thread-status">Loading discussion…</p>
	{:else if loadError}
		<p class="thread-status thread-error" role="alert">{loadError}</p>
	{:else if roots.length === 0}
		<p class="thread-status">No comments yet. Start the discussion below.</p>
	{:else}
		<ul class="root-list">
			{#each roots as root (root.id)}
				<li class="thread-block">
					<ul class="comment-list">
						{@render commentRow(root, false)}
					</ul>

					{#if root.replies.length > 0}
						{#if expanded.has(root.id)}
							<ul class="reply-list">
								{#each root.replies as reply (reply.id)}
									{@render commentRow(reply, true)}
								{/each}
							</ul>
							<button type="button" class="btn-link replies-toggle" onclick={() => toggleExpanded(root.id)}>
								Hide replies
							</button>
						{:else}
							<button type="button" class="btn-link replies-toggle" onclick={() => toggleExpanded(root.id)}>
								{root.replies.length} {root.replies.length === 1 ? 'reply' : 'replies'}
							</button>
						{/if}
					{/if}

					{#if replyTarget === root.id}
						<div class="reply-composer">
							<CommentComposer
								{organizationId}
								placeholder="Write a reply…"
								submitLabel="Reply"
								autofocus
								onsubmit={({ bodyMd, attachmentIds }) => postComment(bodyMd, attachmentIds, root.id)}
								oncancel={() => (replyTarget = null)}
							/>
						</div>
					{/if}
				</li>
			{/each}
		</ul>
	{/if}

	<div class="root-composer">
		<CommentComposer
			{organizationId}
			placeholder="Write a comment…"
			submitLabel="Comment"
			onsubmit={({ bodyMd, attachmentIds }) => postComment(bodyMd, attachmentIds, null)}
		/>
	</div>
</div>

<style>
	.comment-thread {
		display: flex;
		flex-direction: column;
		gap: 1rem;
	}

	.thread-heading {
		font-size: 0.9375rem;
		font-weight: 600;
		color: var(--text-primary);
		margin: 0;
	}

	.thread-count {
		color: var(--text-muted);
		font-weight: 400;
	}

	.thread-status {
		font-size: 0.8125rem;
		color: var(--text-muted);
		margin: 0;
	}

	.thread-error {
		color: #ef4444;
	}

	.root-list,
	.comment-list,
	.reply-list {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
	}

	.root-list {
		gap: 1.25rem;
	}

	.thread-block {
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		padding: 0.75rem;
		background: var(--bg-surface);
	}

	.reply-list {
		margin-top: 0.5rem;
		margin-left: 2.25rem;
		padding-left: 0.75rem;
		border-left: 2px solid var(--border-color);
	}

	.comment-row {
		display: flex;
		gap: 0.625rem;
		align-items: flex-start;
	}

	.avatar {
		flex-shrink: 0;
		width: 1.75rem;
		height: 1.75rem;
		display: flex;
		align-items: center;
		justify-content: center;
		background: var(--bg-root);
		border: 1px solid var(--border-color);
		border-radius: 50%;
		font-size: 0.875rem;
	}

	.agent-avatar {
		border-color: var(--color-primary);
	}

	.comment-body-wrap {
		flex: 1;
		min-width: 0;
	}

	.comment-meta {
		display: flex;
		align-items: baseline;
		gap: 0.375rem;
		flex-wrap: wrap;
		font-size: 0.75rem;
		margin-bottom: 0.25rem;
	}

	.author-name {
		font-weight: 600;
		color: var(--text-primary);
	}

	.agent-badge {
		font-size: 0.625rem;
		text-transform: uppercase;
		letter-spacing: 0.02em;
		background: var(--status-open-bg);
		color: var(--status-open-text);
		border-radius: 3px;
		padding: 0.0625rem 0.3rem;
	}

	.comment-time {
		color: var(--text-subtle);
		font-family: var(--font-mono);
	}

	.edited-marker {
		color: var(--text-muted);
		font-style: italic;
	}

	.comment-attachments {
		margin-top: 0.5rem;
	}

	.comment-actions {
		display: flex;
		gap: 0.75rem;
		margin-top: 0.25rem;
	}

	.btn-link {
		background: none;
		border: none;
		color: var(--color-primary);
		font-size: 0.75rem;
		cursor: pointer;
		padding: 0;
	}

	.btn-link:hover {
		text-decoration: underline;
	}

	.btn-link-danger {
		color: #ef4444;
	}

	.replies-toggle {
		margin-top: 0.5rem;
		font-weight: 500;
	}

	.reply-composer {
		margin-top: 0.75rem;
		margin-left: 2.25rem;
	}

	.row-error {
		color: #ef4444;
		font-size: 0.75rem;
		margin: 0.25rem 0 0 0;
	}

	.edit-box {
		display: flex;
		flex-direction: column;
		gap: 0.375rem;
	}

	.edit-textarea {
		width: 100%;
		box-sizing: border-box;
		background: var(--bg-root);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.5rem;
		font-size: 0.8125rem;
		font-family: inherit;
		resize: vertical;
	}

	.edit-actions {
		display: flex;
		justify-content: flex-end;
		gap: 0.5rem;
	}

	.btn-primary-sm {
		background: var(--color-primary);
		color: #fff;
		border: none;
		border-radius: var(--radius-sm);
		padding: 0.25rem 0.625rem;
		font-size: 0.75rem;
		font-weight: 600;
		cursor: pointer;
	}

	.root-composer {
		border-top: 1px solid var(--border-color);
		padding-top: 1rem;
	}
</style>
