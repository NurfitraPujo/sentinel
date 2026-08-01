<script lang="ts">
	import type { PageData } from './$types';

	let { data } = $props();

	let expandedMetadata = $state(new Set<string>());

	function formatDate(date: string | Date | null): string {
		if (!date) return 'Unknown';
		return new Date(date).toLocaleString('en-US', {
			year: 'numeric',
			month: 'short',
			day: 'numeric',
			hour: '2-digit',
			minute: '2-digit',
			second: '2-digit',
		});
	}

	function toggleMetadata(id: string) {
		if (expandedMetadata.has(id)) {
			expandedMetadata.delete(id);
		} else {
			expandedMetadata.add(id);
		}
	}

	function formatStackFrame(frame: { file: string; line: number; function: string }): string {
		let file = frame.file;
		if (file.length > 50) {
			const start = file.slice(0, 20);
			const end = file.slice(-25);
			file = start + '...' + end;
		}
		return `at ${frame.function} (${file}:${frame.line})`;
	}

	function getDateGroup(dateStr: string | Date | null): string {
		if (!dateStr) return 'Unknown';
		const date = new Date(dateStr);
		const today = new Date();
		const yesterday = new Date(today);
		yesterday.setDate(yesterday.getDate() - 1);

		if (date.toDateString() === today.toDateString()) return 'Today';
		if (date.toDateString() === yesterday.toDateString()) return 'Yesterday';
		return date.toLocaleDateString('en-US', { month: 'long', day: 'numeric', year: 'numeric' });
	}

	import IssueRelations from '$lib/components/issues/IssueRelations.svelte';
	import { filterKnownRelationTypes } from '$lib/types/relation-type';

	interface StackFrame {
		file: string;
		line: number;
		function: string;
	}

	interface Occurrence {
		id: string;
		issueId: string;
		environment: string;
		platform: string;
		stacktrace: StackFrame[];
		metadata: Record<string, unknown>;
		traceId: string | null;
		spanId: string | null;
		createdAt: string | Date | null;
	}

	interface OccurrenceGroup {
		date: string;
		occurrences: Occurrence[];
	}

	let occurrenceGroups = $derived((data.occurrences as Occurrence[]).reduce((groups: OccurrenceGroup[], occ) => {
		const groupLabel = getDateGroup(occ.createdAt as string | Date | null);
		const existing = groups.find(g => g.date === groupLabel);
		if (existing) {
			existing.occurrences.push(occ);
		} else {
			groups.push({ date: groupLabel, occurrences: [occ] });
		}
		return groups;
	}, []));

	// data.relations comes straight off the issue_relations DB column, which is a plain varchar
	// with no DB-level enum — TypeScript only ever knows it as `relationType: string`. Narrow to
	// the three values the migration's CHECK constraint (and the relations API) actually permit
	// here, at the boundary where DB data enters IssueRelations' typed props, dropping (and
	// logging) anything unrecognized rather than casting it through.
	let knownRelations = $derived(filterKnownRelationTypes(data.relations || []));

	async function handleStatusChangeRequest(newStatus: 'resolved') {
		try {
			await fetch(`/api/issues/${data.issue.id}/status`, {
				method: 'PATCH',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ status: newStatus }),
			});
			window.location.reload();
		} catch (err) {
			console.error('Failed to update status:', err);
		}
	}
</script>

<div class="issue-detail">
	<header class="detail-nav">
		<a href="/" class="back-link">&larr; Back to Issues</a>
	</header>

	<div class="issue-header">
		<h1 class="error-title">{data.issue.errorClass}</h1>
		<span class="status {data.issue.status}">{data.issue.status}</span>
	</div>

	<div class="issue-info">
		<p class="message">{data.issue.message}</p>
		<div class="meta-info">
			<span class="meta-item"><strong class="meta-label">Project:</strong> {data.project?.name ?? 'Unknown'}</span>
			<span class="meta-item"><strong class="meta-label">First seen:</strong> {formatDate(data.issue.firstSeen)}</span>
			<span class="meta-item"><strong class="meta-label">Last seen:</strong> {formatDate(data.issue.lastSeen)}</span>
			<span class="meta-item"><strong class="meta-label">Occurrences:</strong> {Number(data.issue.count).toLocaleString()}</span>
		</div>
	</div>

	<IssueRelations
		currentIssueId={data.issue.id}
		initialRelations={knownRelations}
		onStatusChangeRequest={handleStatusChangeRequest}
	/>

	<section class="occurrences">
		<h2 class="section-title">Occurrence History ({data.occurrences.length})</h2>

		{#each occurrenceGroups as group}
			<div class="date-group">
				<h3 class="date-header">{group.date}</h3>
				{#each group.occurrences as occurrence}
					<div class="occurrence-card">
						<div class="occurrence-header">
							<span class="timestamp">{formatDate(occurrence.createdAt)}</span>
							<span class="badge env">{occurrence.environment}</span>
							<span class="badge platform">{occurrence.platform}</span>
						</div>

						{#if occurrence.traceId}
							<div class="trace-info">
								<span class="trace-id">Trace: {occurrence.traceId}</span>
								{#if occurrence.spanId}
									<span class="span-id">Span: {occurrence.spanId}</span>
								{/if}
							</div>
						{/if}

						{#if occurrence.stacktrace && occurrence.stacktrace.length > 0}
							<div class="stacktrace">
								<h4>Stack Trace</h4>
								<pre><code>{#each occurrence.stacktrace as frame}{formatStackFrame(frame)}
{/each}</code></pre>
							</div>
						{/if}

						<button class="metadata-toggle" onclick={() => toggleMetadata(occurrence.id)}>
							{expandedMetadata.has(occurrence.id) ? 'Hide' : 'Show'} Metadata
						</button>

						{#if expandedMetadata.has(occurrence.id) && occurrence.metadata}
							<div class="metadata">
								<h4>Metadata</h4>
								<pre><code>{JSON.stringify(occurrence.metadata, null, 2)}</code></pre>
							</div>
						{/if}
					</div>
				{/each}
			</div>
		{/each}
	</section>
</div>

<style>
	.issue-detail {
		max-width: 1200px;
		margin: 0 auto;
	}

	.detail-nav {
		margin-bottom: 1.5rem;
	}

	.back-link {
		color: var(--color-primary);
		text-decoration: none;
		font-size: 0.875rem;
		font-weight: 500;
	}

	.back-link:hover {
		text-decoration: underline;
	}

	.issue-header {
		display: flex;
		justify-content: space-between;
		align-items: center;
		margin-bottom: 1rem;
	}

	.error-title {
		font-family: var(--font-mono);
		color: var(--severity-critical-text);
		font-size: 1.5rem;
		font-weight: 600;
	}

	.status {
		padding: 0.125rem 0.5rem;
		border-radius: var(--radius-sm);
		font-family: var(--font-mono);
		font-size: 0.6875rem;
		text-transform: uppercase;
		font-weight: 600;
		letter-spacing: 0.05em;
	}

	.status.open { background: var(--status-open-bg); color: var(--status-open-text); }
	.status.resolved { background: var(--status-resolved-bg); color: var(--status-resolved-text); }
	.status.ignored { background: var(--status-ignored-bg); color: var(--status-ignored-text); }

	.issue-info {
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		padding: 1.25rem;
		border-radius: var(--radius-md);
		margin-bottom: 2rem;
	}

	.message {
		font-size: 1rem;
		line-height: 1.5;
		margin-bottom: 1rem;
		color: var(--text-primary);
	}

	.meta-info {
		display: flex;
		flex-wrap: wrap;
		gap: 1.5rem;
		font-size: 0.8125rem;
		color: var(--text-muted);
	}

	.meta-label {
		color: var(--text-subtle);
		font-weight: 500;
	}

	.section-title {
		font-size: 1.125rem;
		font-weight: 600;
		color: var(--text-primary);
		margin-bottom: 1rem;
	}

	.date-group {
		margin-bottom: 1.5rem;
	}

	.date-header {
		font-size: 0.75rem;
		font-family: var(--font-mono);
		color: var(--text-subtle);
		text-transform: uppercase;
		letter-spacing: 0.05em;
		border-bottom: 1px solid var(--border-color);
		padding-bottom: 0.375rem;
		margin-bottom: 0.75rem;
	}

	.occurrence-card {
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		padding: 1rem;
		margin-bottom: 0.75rem;
	}

	.occurrence-header {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		margin-bottom: 0.75rem;
		font-size: 0.75rem;
	}

	.timestamp {
		font-family: var(--font-mono);
		color: var(--text-muted);
	}

	.badge {
		padding: 0.125rem 0.375rem;
		border-radius: var(--radius-sm);
		font-family: var(--font-mono);
		font-size: 0.6875rem;
		background: var(--bg-root);
		color: var(--text-muted);
		border: 1px solid var(--border-color);
	}

	.trace-info {
		display: flex;
		gap: 1rem;
		font-size: 0.75rem;
		font-family: var(--font-mono);
		color: var(--text-subtle);
		margin-bottom: 0.75rem;
	}

	.stacktrace {
		background: var(--bg-root);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
		padding: 0.875rem;
		border-radius: var(--radius-sm);
		overflow-x: auto;
	}

	.stacktrace h4 {
		color: var(--text-subtle);
		margin-bottom: 0.5rem;
		font-size: 0.6875rem;
		font-family: var(--font-mono);
		text-transform: uppercase;
		letter-spacing: 0.05em;
	}

	.stacktrace pre {
		margin: 0;
		font-family: var(--font-mono);
		font-size: 0.75rem;
		line-height: 1.4;
	}

	.metadata-toggle {
		margin-top: 0.75rem;
		padding: 0.25rem 0.625rem;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		background: var(--bg-root);
		color: var(--text-muted);
		font-size: 0.75rem;
		font-family: var(--font-mono);
		cursor: pointer;
		transition: background 0.15s ease, color 0.15s ease;
	}

	.metadata-toggle:hover {
		background: var(--bg-surface-hover);
		color: var(--text-primary);
	}

	.metadata {
		margin-top: 0.75rem;
		background: var(--bg-root);
		border: 1px solid var(--border-color);
		padding: 0.875rem;
		border-radius: var(--radius-sm);
	}

	.metadata h4 {
		font-size: 0.6875rem;
		font-family: var(--font-mono);
		text-transform: uppercase;
		color: var(--text-subtle);
		margin-bottom: 0.5rem;
	}

	.metadata pre {
		font-family: var(--font-mono);
		font-size: 0.75rem;
		color: var(--text-muted);
		overflow-x: auto;
	}
</style>