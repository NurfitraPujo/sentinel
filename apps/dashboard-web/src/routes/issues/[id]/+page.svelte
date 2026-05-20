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
</script>

<div class="issue-detail">
	<header>
		<a href="/">&larr; Back to Issues</a>
	</header>

	<div class="issue-header">
		<h1>{data.issue.errorClass}</h1>
		<span class="status {data.issue.status}">{data.issue.status}</span>
	</div>

	<div class="issue-info">
		<p class="message">{data.issue.message}</p>
		<div class="meta-info">
			<span>Project: {data.project?.name ?? 'Unknown'}</span>
			<span>First seen: {formatDate(data.issue.firstSeen)}</span>
			<span>Last seen: {formatDate(data.issue.lastSeen)}</span>
			<span>Total occurrences: {Number(data.issue.count).toLocaleString()}</span>
		</div>
	</div>

	<section class="occurrences">
		<h2>Occurrence History ({data.occurrences.length})</h2>

		{#each occurrenceGroups as group}
			<div class="date-group">
				<h3 class="date-header">{group.date}</h3>
				{#each group.occurrences as occurrence}
					<div class="occurrence-card">
						<div class="occurrence-header">
							<span class="timestamp">{formatDate(occurrence.createdAt)}</span>
							<span class="environment">{occurrence.environment}</span>
							<span class="platform">{occurrence.platform}</span>
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

	header {
		margin-bottom: 2rem;
	}

	header a {
		color: #2563eb;
		text-decoration: none;
	}

	header a:hover {
		text-decoration: underline;
	}

	.issue-header {
		display: flex;
		justify-content: space-between;
		align-items: center;
		margin-bottom: 1rem;
	}

	.issue-header h1 {
		font-family: monospace;
		color: #dc2626;
	}

	.status {
		padding: 0.25rem 0.75rem;
		border-radius: 4px;
		text-transform: capitalize;
		font-size: 0.875rem;
		font-weight: 600;
	}

	.status.open { background: #fef3c7; color: #92400e; }
	.status.resolved { background: #d1fae5; color: #065f46; }
	.status.ignored { background: #e5e5e5; color: #666; }

	.issue-info {
		background: #f9fafb;
		padding: 1.5rem;
		border-radius: 8px;
		margin-bottom: 2rem;
	}

	.message {
		font-size: 1.125rem;
		line-height: 1.6;
		margin-bottom: 1rem;
	}

	.meta-info {
		display: flex;
		gap: 2rem;
		font-size: 0.875rem;
		color: #666;
	}

	.occurrences h2 {
		margin-bottom: 1rem;
	}

	.date-group {
		margin-bottom: 1.5rem;
	}

	.date-header {
		font-size: 0.875rem;
		color: #666;
		border-bottom: 1px solid #e5e5e5;
		padding-bottom: 0.5rem;
		margin-bottom: 0.75rem;
	}

	.occurrence-card {
		border: 1px solid #e5e5e5;
		border-radius: 8px;
		padding: 1rem;
		margin-bottom: 1rem;
	}

	.occurrence-header {
		display: flex;
		gap: 1rem;
		margin-bottom: 0.75rem;
		font-size: 0.875rem;
	}

	.occurrence-header span {
		padding: 0.25rem 0.5rem;
		border-radius: 4px;
		background: #f3f4f6;
	}

	.trace-info {
		display: flex;
		gap: 1rem;
		font-size: 0.875rem;
		color: #666;
		margin-bottom: 0.75rem;
	}

	.stacktrace {
		background: #1f2937;
		color: #e5e7eb;
		padding: 1rem;
		border-radius: 6px;
		overflow-x: auto;
	}

	.stacktrace h4 {
		color: #9ca3af;
		margin-bottom: 0.5rem;
		font-size: 0.75rem;
		text-transform: uppercase;
	}

	.stacktrace pre {
		margin: 0;
		font-family: monospace;
		font-size: 0.75rem;
	}

	.metadata-toggle {
		margin-top: 0.75rem;
		padding: 0.5rem 1rem;
		border: 1px solid #d1d5db;
		border-radius: 6px;
		background: white;
		cursor: pointer;
	}

	.metadata-toggle:hover {
		background: #f3f4f6;
	}

	.metadata {
		margin-top: 0.75rem;
		background: #f9fafb;
		padding: 1rem;
		border-radius: 6px;
	}

	.metadata h4 {
		font-size: 0.75rem;
		text-transform: uppercase;
		color: #666;
		margin-bottom: 0.5rem;
	}

	.metadata pre {
		font-family: monospace;
		font-size: 0.75rem;
		overflow-x: auto;
	}
</style>