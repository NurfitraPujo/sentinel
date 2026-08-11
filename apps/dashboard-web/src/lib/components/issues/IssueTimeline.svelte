<script lang="ts">
	// Manual Issues M1 (docs/plans/MANUAL_ISSUES_DESIGN.md §6): the single user-visible timeline
	// for BOTH issue types, rendering `issue_activity` rows. §6 explicitly starts as a separate
	// "Activity" tab (not interleaved with thread comments, which are M3), so this component only
	// ever renders activity entries.
	interface ActivityEntry {
		id: string;
		issueId: string;
		eventType: string;
		actorType: string; // 'user' | 'agent' | 'system'
		actorId: string;
		oldValue: unknown;
		newValue: unknown;
		createdAt: string | Date | null;
	}

	interface Props {
		activity?: ActivityEntry[];
	}

	let { activity = [] }: Props = $props();

	const EVENT_LABELS: Record<string, string> = {
		status_changed: 'changed status',
		assigned: 'assigned this issue',
		unassigned: 'unassigned this issue',
		regressed: 'flagged a regression',
		ai_analysis: 'ran AI analysis',
		linked: 'linked an issue',
		commented: 'commented',
		claimed: 'claimed this issue',
		claim_released: 'released their claim',
		progress_update: 'posted a progress update',
		question_asked: 'asked a question',
		question_answered: 'answered a question',
		moved: 'moved this issue',
		attachment_added: 'added an attachment',
		report_edited: 'edited the report',
	};

	function eventLabel(eventType: string): string {
		return EVENT_LABELS[eventType] ?? eventType.replace(/_/g, ' ');
	}

	function actorIcon(actorType: string): string {
		if (actorType === 'agent') return '\u{1F916}'; // robot
		if (actorType === 'system') return '⚙️'; // gear
		return '\u{1F464}'; // person
	}

	function formatTime(value: string | Date | null): string {
		if (!value) return 'unknown time';
		const date = value instanceof Date ? value : new Date(value);
		if (Number.isNaN(date.getTime())) return 'unknown time';
		return date.toLocaleString();
	}

	// old/new value jsonb columns are free-form per event type -- render a compact, generically
	// safe key: value summary rather than trying to special-case every event shape.
	function summarize(value: unknown): string | null {
		if (!value || typeof value !== 'object' || Object.keys(value).length === 0) return null;
		return Object.entries(value as Record<string, unknown>)
			.map(([key, val]) => `${key}: ${val === null || val === undefined ? '—' : String(val)}`)
			.join(', ');
	}

	let sorted = $derived(
		[...activity].sort((a, b) => {
			const at = a.createdAt ? new Date(a.createdAt).getTime() : 0;
			const bt = b.createdAt ? new Date(b.createdAt).getTime() : 0;
			return bt - at;
		})
	);
</script>

<div class="issue-timeline">
	{#if sorted.length === 0}
		<div class="empty-state">
			<p>No activity yet.</p>
		</div>
	{:else}
		<ul class="timeline-list">
			{#each sorted as entry (entry.id)}
				<li class="timeline-entry">
					<span class="actor-icon" title={entry.actorType} aria-hidden="true">{actorIcon(entry.actorType)}</span>
					<div class="entry-body">
						<div class="entry-header">
							<span class="actor-id">{entry.actorId}</span>
							<span class="event-label">{eventLabel(entry.eventType)}</span>
							<span class="event-time">{formatTime(entry.createdAt)}</span>
						</div>
						{#if summarize(entry.oldValue) || summarize(entry.newValue)}
							<div class="entry-detail">
								{#if summarize(entry.oldValue)}
									<span class="detail-old">from {summarize(entry.oldValue)}</span>
								{/if}
								{#if summarize(entry.newValue)}
									<span class="detail-new">{summarize(entry.oldValue) ? '→ ' : ''}{summarize(entry.newValue)}</span>
								{/if}
							</div>
						{/if}
					</div>
				</li>
			{/each}
		</ul>
	{/if}
</div>

<style>
	.issue-timeline {
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		padding: 1rem;
		color: var(--text-primary);
	}

	.empty-state {
		text-align: center;
		padding: 0.75rem 0;
		color: var(--text-muted);
		font-size: 0.75rem;
	}

	.timeline-list {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
	}

	.timeline-entry {
		display: flex;
		gap: 0.625rem;
		align-items: flex-start;
	}

	.actor-icon {
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

	.entry-body {
		flex: 1;
		min-width: 0;
	}

	.entry-header {
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		gap: 0.375rem;
		font-size: 0.8125rem;
	}

	.actor-id {
		font-weight: 600;
		color: var(--text-primary);
		font-family: var(--font-mono);
	}

	.event-label {
		color: var(--text-muted);
	}

	.event-time {
		margin-left: auto;
		font-size: 0.6875rem;
		color: var(--text-subtle);
		font-family: var(--font-mono);
	}

	.entry-detail {
		margin-top: 0.25rem;
		font-size: 0.75rem;
		color: var(--text-muted);
		font-family: var(--font-mono);
		display: flex;
		gap: 0.375rem;
		flex-wrap: wrap;
	}

	.detail-old {
		text-decoration: line-through;
		opacity: 0.7;
	}
</style>
