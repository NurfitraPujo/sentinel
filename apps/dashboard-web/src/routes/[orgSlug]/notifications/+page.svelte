<script lang="ts">
	// Manual Issues M4 (docs/plans/MANUAL_ISSUES_DESIGN.md §8/§10): full, paginated, newest-first
	// notification list. Unread rows are highlighted the same way NotificationBell's dropdown
	// highlights them; "mark all read" mirrors the dropdown's action against the same
	// `PATCH /api/notifications` endpoint.
	import type { PageData } from './$types';

	let { data }: { data: PageData } = $props();

	// svelte-ignore state_referenced_locally -- deliberate: seeded once, then kept in sync with
	// `data` (which changes across `?page=` navigations) by the `$effect` below.
	let items = $state(data.notifications);
	let markingAll = $state(false);
	let markAllError = $state<string | null>(null);

	$effect(() => {
		items = data.notifications;
	});

	const totalPages = $derived(Math.max(1, Math.ceil(data.total / data.pageSize)));

	const KIND_ICONS: Record<string, string> = {
		commented: '\u{1F4AC}',
		claimed: '✋',
		status_changed: '\u{1F504}',
		resolved: '✅',
		linked: '\u{1F517}',
		progress_update: '⏳',
		question_asked: '❓',
	};

	function iconFor(kind: string): string {
		return KIND_ICONS[kind] ?? '\u{1F514}';
	}

	function linkFor(item: (typeof items)[number]): string {
		if (item.issueType === 'user_report') {
			return `/${item.orgSlug}/reports/${item.issueId}`;
		}
		return `/${item.orgSlug}/projects/${item.projectId}/issues/${item.issueId}`;
	}

	function actorLabel(item: (typeof items)[number]): string {
		if (item.actorType === 'agent') return item.actorName ?? item.actorId;
		if (item.actorType === 'system') return 'System';
		return item.actorName ?? item.actorId;
	}

	function formatTime(value: string | Date): string {
		const date = value instanceof Date ? value : new Date(value);
		if (Number.isNaN(date.getTime())) return 'unknown time';
		return date.toLocaleString();
	}

	async function markRead(item: (typeof items)[number]) {
		if (item.readAt) return;
		items = items.map((n) => (n.id === item.id ? { ...n, readAt: new Date() } : n));
		try {
			await fetch('/api/notifications', {
				method: 'PATCH',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ id: item.id }),
			});
		} catch {
			// best-effort, same as NotificationBell
		}
	}

	async function markAllRead() {
		markAllError = null;
		markingAll = true;
		try {
			const res = await fetch('/api/notifications', {
				method: 'PATCH',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ all: true }),
			});
			if (!res.ok) {
				throw new Error(`Failed to mark all read (${res.status})`);
			}
			items = items.map((n) => (n.readAt ? n : { ...n, readAt: new Date() }));
		} catch (err) {
			markAllError = err instanceof Error ? err.message : 'Failed to mark all read';
		} finally {
			markingAll = false;
		}
	}
</script>

<div class="notifications-page">
	<div class="page-header">
		<h1 class="page-title">Notifications</h1>
		<button type="button" class="btn-secondary" onclick={markAllRead} disabled={markingAll}>
			{markingAll ? 'Marking…' : 'Mark all read'}
		</button>
	</div>

	{#if markAllError}
		<div class="error-banner" role="alert">{markAllError}</div>
	{/if}

	{#if items.length === 0}
		<p class="empty-state">No notifications yet.</p>
	{:else}
		<ul class="notification-list" data-testid="notification-list">
			{#each items as item (item.id)}
				<li class="notification-row" class:unread={!item.readAt}>
					<a href={linkFor(item)} class="notification-link" onclick={() => markRead(item)}>
						<span class="kind-icon" aria-hidden="true">{iconFor(item.kind)}</span>
						<div class="notification-body">
							<span class="notification-title">{item.issueTitle}</span>
							<span class="notification-meta">
								{actorLabel(item)} · {item.kind.replace('_', ' ')} · {formatTime(item.createdAt)}
							</span>
						</div>
						{#if !item.readAt}<span class="unread-dot" aria-hidden="true"></span>{/if}
					</a>
				</li>
			{/each}
		</ul>

		{#if totalPages > 1}
			<nav class="pagination" aria-label="Notification pages">
				<a
					class="page-link"
					class:disabled={data.page <= 1}
					href="?page={Math.max(1, data.page - 1)}"
				>
					Previous
				</a>
				<span class="page-status">Page {data.page} of {totalPages}</span>
				<a
					class="page-link"
					class:disabled={data.page >= totalPages}
					href="?page={Math.min(totalPages, data.page + 1)}"
				>
					Next
				</a>
			</nav>
		{/if}
	{/if}
</div>

<style>
	.notifications-page {
		max-width: 42rem;
		margin: 0 auto;
		display: flex;
		flex-direction: column;
		gap: 1rem;
	}

	.page-header {
		display: flex;
		justify-content: space-between;
		align-items: center;
	}

	.page-title {
		font-size: 1.125rem;
		font-weight: 700;
		color: var(--text-primary);
		margin: 0;
	}

	.btn-secondary {
		background: var(--bg-root);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.375rem 0.75rem;
		font-size: 0.75rem;
		font-weight: 600;
		cursor: pointer;
	}

	.btn-secondary:hover {
		background: var(--bg-surface-hover);
	}

	.btn-secondary:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}

	.error-banner {
		background: rgba(239, 68, 68, 0.15);
		border: 1px solid rgba(239, 68, 68, 0.3);
		color: #ef4444;
		padding: 0.5rem 0.75rem;
		border-radius: var(--radius-sm);
		font-size: 0.8125rem;
	}

	.empty-state {
		color: var(--text-muted);
		font-size: 0.875rem;
	}

	.notification-list {
		list-style: none;
		margin: 0;
		padding: 0;
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		overflow: hidden;
	}

	.notification-row + .notification-row {
		border-top: 1px solid var(--border-color-muted);
	}

	.notification-row.unread {
		background: rgba(59, 130, 246, 0.08);
	}

	.notification-link {
		display: flex;
		gap: 0.625rem;
		align-items: flex-start;
		padding: 0.75rem 1rem;
		text-decoration: none;
		color: inherit;
	}

	.notification-link:hover {
		background: var(--bg-surface-hover);
	}

	.kind-icon {
		font-size: 1rem;
		flex-shrink: 0;
	}

	.notification-body {
		flex: 1;
		min-width: 0;
		display: flex;
		flex-direction: column;
		gap: 0.125rem;
	}

	.notification-title {
		font-size: 0.875rem;
		font-weight: 500;
		color: var(--text-primary);
	}

	.notification-meta {
		font-size: 0.75rem;
		color: var(--text-subtle);
		text-transform: capitalize;
	}

	.unread-dot {
		flex-shrink: 0;
		width: 0.5rem;
		height: 0.5rem;
		border-radius: 50%;
		background: var(--color-primary);
		margin-top: 0.3rem;
	}

	.pagination {
		display: flex;
		justify-content: center;
		align-items: center;
		gap: 1rem;
	}

	.page-link {
		color: var(--color-primary);
		font-size: 0.8125rem;
		text-decoration: none;
	}

	.page-link:hover {
		text-decoration: underline;
	}

	.page-link.disabled {
		color: var(--text-subtle);
		pointer-events: none;
	}

	.page-status {
		font-size: 0.8125rem;
		color: var(--text-muted);
	}
</style>
