<script lang="ts">
	// Manual Issues M4 (docs/plans/MANUAL_ISSUES_DESIGN.md §8/§10, Q10): the header bell. Polls the
	// unread count on the same ~10s visibility-aware cadence CommentThread (M3) uses for thread
	// refresh, via the shared `startVisiblePolling` helper. Dropdown lists the latest ~10
	// notifications (`GET /api/notifications?limit=10`); clicking a row marks it read and follows
	// its link (built per `issue_type`, mirroring notify.ts's `buildIssueUrl` split -- `user_report`
	// goes to `/reports/[issueId]`, everything else to `/projects/[projectId]/issues/[issueId]`).
	import { startVisiblePolling } from '$lib/utils/visible-poll';
	import type { NotificationListItem } from '$lib/notifications/types';

	interface Props {
		/** Polling interval in ms -- overridable so tests don't have to wait 10s. */
		pollIntervalMs?: number;
	}

	let { pollIntervalMs = 10_000 }: Props = $props();

	let unreadCount = $state(0);
	let open = $state(false);
	let items = $state<NotificationListItem[]>([]);
	let loading = $state(false);
	let loadError = $state<string | null>(null);

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

	function linkFor(item: NotificationListItem): string {
		if (item.issueType === 'user_report') {
			return `/${item.orgSlug}/reports/${item.issueId}`;
		}
		return `/${item.orgSlug}/projects/${item.projectId}/issues/${item.issueId}`;
	}

	function actorLabel(item: NotificationListItem): string {
		if (item.actorType === 'agent') return item.actorName ?? item.actorId;
		if (item.actorType === 'system') return 'System';
		return item.actorName ?? item.actorId;
	}

	function formatRelative(value: string | Date): string {
		const date = value instanceof Date ? value : new Date(value);
		if (Number.isNaN(date.getTime())) return '';
		const diffMs = Date.now() - date.getTime();
		const diffMin = Math.floor(diffMs / 60_000);
		if (diffMin < 1) return 'just now';
		if (diffMin < 60) return `${diffMin}m ago`;
		const diffHr = Math.floor(diffMin / 60);
		if (diffHr < 24) return `${diffHr}h ago`;
		const diffDay = Math.floor(diffHr / 24);
		return `${diffDay}d ago`;
	}

	async function refreshUnreadCount() {
		try {
			const res = await fetch('/api/notifications?count=unread');
			if (!res.ok) return; // best-effort, same rationale as CommentThread's poll()
			const body = await res.json();
			unreadCount = typeof body.count === 'number' ? body.count : 0;
		} catch {
			// best-effort
		}
	}

	async function loadDropdown() {
		loading = true;
		loadError = null;
		try {
			const res = await fetch('/api/notifications?limit=10');
			if (!res.ok) {
				throw new Error(`Failed to load notifications (${res.status})`);
			}
			const body = await res.json();
			items = body.notifications ?? [];
		} catch (err) {
			loadError = err instanceof Error ? err.message : 'Failed to load notifications';
		} finally {
			loading = false;
		}
	}

	async function toggleOpen() {
		open = !open;
		if (open) {
			await loadDropdown();
		}
	}

	async function markRead(item: NotificationListItem) {
		if (item.readAt) return;
		// Optimistic: flip locally so the row stops looking unread immediately, and drop the badge
		// count -- a failed PATCH just leaves a stale-but-harmless read state, same trade-off
		// CommentThread's best-effort poll makes.
		items = items.map((n) => (n.id === item.id ? { ...n, readAt: new Date() } : n));
		unreadCount = Math.max(0, unreadCount - 1);
		try {
			await fetch('/api/notifications', {
				method: 'PATCH',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ id: item.id }),
			});
		} catch {
			// best-effort
		}
	}

	async function markAllRead() {
		const hadUnread = items.some((n) => !n.readAt);
		items = items.map((n) => (n.readAt ? n : { ...n, readAt: new Date() }));
		unreadCount = 0;
		try {
			await fetch('/api/notifications', {
				method: 'PATCH',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ all: true }),
			});
		} catch {
			if (hadUnread) await refreshUnreadCount();
		}
	}

	$effect(() => {
		void refreshUnreadCount();
	});

	$effect(() => {
		return startVisiblePolling(refreshUnreadCount, pollIntervalMs);
	});
</script>

<div class="notification-bell">
	<button
		type="button"
		class="bell-btn"
		aria-label="Notifications"
		aria-expanded={open}
		onclick={toggleOpen}
	>
		<span aria-hidden="true">🔔</span>
		{#if unreadCount > 0}
			<span class="unread-badge" data-testid="unread-badge">{unreadCount > 99 ? '99+' : unreadCount}</span>
		{/if}
	</button>

	{#if open}
		<div class="dropdown" data-testid="notification-dropdown">
			<div class="dropdown-header">
				<span class="dropdown-title">Notifications</span>
				<button type="button" class="btn-link" onclick={markAllRead}>Mark all read</button>
			</div>

			{#if loading}
				<p class="dropdown-status">Loading…</p>
			{:else if loadError}
				<p class="dropdown-status dropdown-error" role="alert">{loadError}</p>
			{:else if items.length === 0}
				<p class="dropdown-status">No notifications yet.</p>
			{:else}
				<ul class="notification-list">
					{#each items as item (item.id)}
						<li class="notification-row" class:unread={!item.readAt}>
							<a
								href={linkFor(item)}
								class="notification-link"
								data-testid="notification-{item.id}"
								onclick={() => markRead(item)}
							>
								<span class="kind-icon" aria-hidden="true">{iconFor(item.kind)}</span>
								<div class="notification-body">
									<span class="notification-title">{item.issueTitle}</span>
									<span class="notification-meta">
										{actorLabel(item)} · {item.kind.replace('_', ' ')} · {formatRelative(item.createdAt)}
									</span>
								</div>
							</a>
						</li>
					{/each}
				</ul>
			{/if}

			<a href="/{items[0]?.orgSlug ?? ''}/notifications" class="view-all-link">View all</a>
		</div>
	{/if}
</div>

<style>
	.notification-bell {
		position: relative;
	}

	.bell-btn {
		position: relative;
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 1rem;
		cursor: pointer;
		padding: 0.25rem 0.375rem;
		border-radius: var(--radius-sm);
	}

	.bell-btn:hover {
		color: var(--text-primary);
		background: var(--bg-surface-hover);
	}

	.unread-badge {
		position: absolute;
		top: -0.125rem;
		right: -0.125rem;
		background: var(--severity-critical-text, #ef4444);
		color: #fff;
		font-size: 0.625rem;
		font-weight: 700;
		line-height: 1;
		border-radius: 999px;
		padding: 0.125rem 0.3rem;
		min-width: 1rem;
		text-align: center;
	}

	.dropdown {
		position: absolute;
		right: 0;
		top: calc(100% + 0.375rem);
		width: 22rem;
		max-width: 90vw;
		max-height: 24rem;
		overflow-y: auto;
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		box-shadow: 0 8px 24px rgba(0, 0, 0, 0.3);
		z-index: 50;
		display: flex;
		flex-direction: column;
	}

	.dropdown-header {
		display: flex;
		justify-content: space-between;
		align-items: center;
		padding: 0.625rem 0.75rem;
		border-bottom: 1px solid var(--border-color);
	}

	.dropdown-title {
		font-size: 0.8125rem;
		font-weight: 600;
		color: var(--text-primary);
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

	.dropdown-status {
		padding: 0.75rem;
		font-size: 0.8125rem;
		color: var(--text-muted);
		margin: 0;
	}

	.dropdown-error {
		color: #ef4444;
	}

	.notification-list {
		list-style: none;
		margin: 0;
		padding: 0;
	}

	.notification-row + .notification-row {
		border-top: 1px solid var(--border-color-muted);
	}

	.notification-row.unread {
		background: rgba(59, 130, 246, 0.08);
	}

	.notification-link {
		display: flex;
		gap: 0.5rem;
		align-items: flex-start;
		padding: 0.625rem 0.75rem;
		text-decoration: none;
		color: inherit;
	}

	.notification-link:hover {
		background: var(--bg-surface-hover);
	}

	.kind-icon {
		font-size: 0.9375rem;
		flex-shrink: 0;
	}

	.notification-body {
		display: flex;
		flex-direction: column;
		gap: 0.125rem;
		min-width: 0;
	}

	.notification-title {
		font-size: 0.8125rem;
		font-weight: 500;
		color: var(--text-primary);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.notification-meta {
		font-size: 0.6875rem;
		color: var(--text-subtle);
		text-transform: capitalize;
	}

	.view-all-link {
		display: block;
		text-align: center;
		padding: 0.5rem;
		font-size: 0.75rem;
		color: var(--color-primary);
		text-decoration: none;
		border-top: 1px solid var(--border-color);
	}

	.view-all-link:hover {
		text-decoration: underline;
	}
</style>
