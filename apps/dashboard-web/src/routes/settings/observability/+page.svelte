<script lang="ts">
	import type { PageData } from './$types';
	import type { DLQItem } from './+page.server';

	let { data }: { data: PageData } = $props();

	let selectedItem = $state<DLQItem | null>(null);
	let searchFilter = $state('');
	let categoryFilter = $state('all');
	let copied = $state(false);

	let observability = $derived(data.observability);
	let processor = $derived(observability.processor);
	let ingestor = $derived(observability.ingestor);
	let dlq = $derived(observability.dlq);

	let filteredItems = $derived(
		dlq.items.filter((item) => {
			const matchesCategory =
				categoryFilter === 'all' ||
				(categoryFilter === 'permanent' && item.error_class === 'permanent') ||
				(categoryFilter === 'transient' && item.error_class === 'transient') ||
				(categoryFilter === 'unclassified' && (!item.error_class || item.error_class === 'unclassified'));

			const matchesSearch =
				!searchFilter ||
				item.event_id.toLowerCase().includes(searchFilter.toLowerCase()) ||
				item.error_message.toLowerCase().includes(searchFilter.toLowerCase()) ||
				item.org_id.toLowerCase().includes(searchFilter.toLowerCase());

			return matchesCategory && matchesSearch;
		})
	);

	let formattedPayload = $derived.by(() => {
		if (!selectedItem) return '';
		try {
			return JSON.stringify(JSON.parse(selectedItem.raw_payload), null, 2);
		} catch (e) {
			return selectedItem.raw_payload;
		}
	});

	function openDrawer(item: DLQItem) {
		selectedItem = item;
		copied = false;
	}

	function closeDrawer() {
		selectedItem = null;
	}

	async function copyPayload() {
		if (!selectedItem) return;
		try {
			await navigator.clipboard.writeText(formattedPayload);
			copied = true;
			setTimeout(() => {
				copied = false;
			}, 2000);
		} catch (err) {
			console.error('Failed to copy', err);
		}
	}

	function getStatusClass(status: string) {
		if (status.includes('healthy') || status === 'ok') return 'status-healthy';
		if (status.includes('attention')) return 'status-attention';
		if (status.includes('critical') || status === 'offline') return 'status-critical';
		return 'status-muted';
	}

	function formatAge(seconds?: number) {
		if (seconds === undefined || seconds === null) return 'N/A';
		if (seconds < 60) return `${Math.round(seconds)}s`;
		if (seconds < 3600) return `${Math.round(seconds / 60)}m`;
		return `${(seconds / 3600).toFixed(1)}h`;
	}
</script>

<svelte:head>
	<title>Observability & DLQ Monitor - Sentinel</title>
</svelte:head>

<div class="observability-page">
	<header class="page-header">
		<div>
			<h1 class="page-title">System Observability & DLQ Monitor</h1>
			<p class="page-subtitle">Real-time operational health metrics and dead-lettered event inspector</p>
		</div>
		<div class="refresh-badge">
			<span class="pulse-dot"></span>
			<span>Updated {new Date(observability.fetchedAt).toLocaleTimeString()}</span>
		</div>
	</header>

	<!-- Operational Health Metrics -->
	<section class="metrics-grid">
		<div class="metric-card">
			<div class="metric-header">
				<span class="metric-label">Processor Status</span>
				<span class="status-indicator {getStatusClass(processor.status)}">{processor.status}</span>
			</div>
			<div class="metric-value">{processor.status === 'healthy' ? 'Operational' : processor.status}</div>
			<div class="metric-footer">
				<span>Database: <strong class={processor.database === 'ok' ? 'text-healthy' : 'text-critical'}>{processor.database}</strong></span>
			</div>
		</div>

		<div class="metric-card">
			<div class="metric-header">
				<span class="metric-label">Ingestor Status</span>
				<span class="status-indicator {getStatusClass(ingestor.status)}">{ingestor.status}</span>
			</div>
			<div class="metric-value">{ingestor.status === 'healthy' ? 'Operational' : ingestor.status}</div>
			<div class="metric-footer">
				<span>Event ingestion active</span>
			</div>
		</div>

		<div class="metric-card">
			<div class="metric-header">
				<span class="metric-label">DLQ Backlog Depth</span>
				<span class="status-indicator {dlq.total_depth > processor.dlq_threshold ? 'status-critical' : dlq.total_depth > 0 ? 'status-attention' : 'status-healthy'}">
					{dlq.total_depth > 0 ? `${dlq.total_depth} parked` : 'Clean'}
				</span>
			</div>
			<div class="metric-value">{dlq.total_depth}</div>
			<div class="metric-footer">
				<span>Alert threshold: <strong>{processor.dlq_threshold} events</strong></span>
			</div>
		</div>

		<div class="metric-card">
			<div class="metric-header">
				<span class="metric-label">Oldest Message Age</span>
				<span class="status-indicator {processor.dlq_oldest_class === 'permanent' ? 'status-critical' : 'status-muted'}">
					{processor.dlq_oldest_class || 'None'}
				</span>
			</div>
			<div class="metric-value">{formatAge(dlq.oldest_age_seconds)}</div>
			<div class="metric-footer">
				<span>Stale threshold: <strong>{formatAge(processor.dlq_stale_after_seconds)}</strong></span>
			</div>
		</div>
	</section>

	<!-- DLQ Event Inspector Section -->
	<section class="dlq-section">
		<div class="section-header">
			<div>
				<h2 class="section-title">Dead-Letter Queue Inspector</h2>
				<p class="section-subtitle">Inspect unprocessable parked events and failure causes</p>
			</div>
			<div class="filter-controls">
				<div class="pill-group">
					<button class="pill {categoryFilter === 'all' ? 'active' : ''}" onclick={() => categoryFilter = 'all'}>All</button>
					<button class="pill {categoryFilter === 'permanent' ? 'active' : ''}" onclick={() => categoryFilter = 'permanent'}>Permanent</button>
					<button class="pill {categoryFilter === 'transient' ? 'active' : ''}" onclick={() => categoryFilter = 'transient'}>Transient</button>
				</div>
				<input
					type="text"
					placeholder="Filter by Event ID, Org, or Error..."
					class="search-input"
					bind:value={searchFilter}
				/>
			</div>
		</div>

		{#if filteredItems.length === 0}
			<div class="empty-state">
				<div class="empty-icon">✓</div>
				<h3>No Dead-Lettered Events</h3>
				<p>The queue is empty or no events match your current filter criteria.</p>
			</div>
		{:else}
			<div class="table-wrapper">
				<table class="dlq-table">
					<thead>
						<tr>
							<th>Event ID</th>
							<th>Org / Project</th>
							<th>Error Classification</th>
							<th>Failure Message</th>
							<th>Retries</th>
							<th>Timestamp</th>
							<th>Action</th>
						</tr>
					</thead>
					<tbody>
						{#each filteredItems as item}
							<tr class="table-row" onclick={() => openDrawer(item)}>
								<td class="font-mono">{item.event_id}</td>
								<td>{item.org_id} / {item.project_id}</td>
								<td>
									<span class="class-tag tag-{item.error_class || 'unclassified'}">
										{item.error_class || 'unclassified'}
									</span>
								</td>
								<td class="error-msg-cell">{item.error_message}</td>
								<td class="text-center">{item.retry_attempts}</td>
								<td class="font-mono text-muted">{new Date(item.failed_at).toLocaleTimeString()}</td>
								<td>
									<button class="btn-inspect" onclick={(e) => { e.stopPropagation(); openDrawer(item); }}>Inspect</button>
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}
	</section>

	<!-- Slide-out Side Drawer for JSON Payload Inspection -->
	{#if selectedItem}
		<div class="drawer-backdrop" onclick={closeDrawer}>
			<div class="drawer-panel" onclick={(e) => e.stopPropagation()}>
				<header class="drawer-header">
					<div>
						<h3 class="drawer-title">DLQ Event Detail</h3>
						<span class="font-mono text-subtle">{selectedItem.event_id}</span>
					</div>
					<button class="btn-close" onclick={closeDrawer}>×</button>
				</header>

				<div class="drawer-body">
					<div class="detail-group">
						<label>Error Classification</label>
						<span class="class-tag tag-{selectedItem.error_class || 'unclassified'}">
							{selectedItem.error_class || 'unclassified'}
						</span>
					</div>

					<div class="detail-group">
						<label>Failure Context</label>
						<p class="error-box">{selectedItem.error_message}</p>
					</div>

					<div class="detail-group">
						<div class="payload-header">
							<label>Raw Event Payload</label>
							<button class="btn-copy" onclick={copyPayload}>
								{copied ? 'Copied!' : 'Copy JSON'}
							</button>
						</div>
						<pre class="json-code"><code>{formattedPayload}</code></pre>
					</div>
				</div>
			</div>
		</div>
	{/if}
</div>

<style>
	.observability-page {
		max-width: 1200px;
		margin: 0 auto;
		display: flex;
		flex-direction: column;
		gap: 2rem;
	}

	.page-header {
		display: flex;
		justify-content: space-between;
		align-items: flex-start;
	}

	.page-title {
		font-size: 1.5rem;
		font-weight: 700;
		color: var(--text-primary);
	}

	.page-subtitle {
		font-size: 0.875rem;
		color: var(--text-muted);
		margin-top: 0.25rem;
	}

	.refresh-badge {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		font-size: 0.75rem;
		color: var(--text-subtle);
		background-color: var(--bg-surface);
		padding: 0.375rem 0.75rem;
		border-radius: var(--radius-md);
		border: 1px solid var(--border-color);
	}

	.pulse-dot {
		width: 8px;
		height: 8px;
		border-radius: 50%;
		background-color: #10b981;
		box-shadow: 0 0 8px #10b981;
	}

	.metrics-grid {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
		gap: 1rem;
	}

	.metric-card {
		background-color: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		padding: 1.25rem;
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
	}

	.metric-header {
		display: flex;
		justify-content: space-between;
		align-items: center;
	}

	.metric-label {
		font-size: 0.8125rem;
		font-weight: 500;
		color: var(--text-muted);
	}

	.metric-value {
		font-size: 1.75rem;
		font-weight: 700;
		font-family: var(--font-mono);
		color: var(--text-primary);
	}

	.metric-footer {
		font-size: 0.75rem;
		color: var(--text-subtle);
	}

	.status-indicator {
		font-size: 0.75rem;
		font-weight: 600;
		padding: 0.125rem 0.5rem;
		border-radius: var(--radius-sm);
		text-transform: capitalize;
	}

	.status-healthy { background: rgba(16, 185, 129, 0.15); color: #10b981; }
	.status-attention { background: rgba(245, 158, 11, 0.15); color: #f59e0b; }
	.status-critical { background: rgba(239, 68, 68, 0.15); color: #ef4444; }
	.status-muted { background: rgba(148, 163, 184, 0.15); color: #94a3b8; }

	.text-healthy { color: #10b981; }
	.text-critical { color: #ef4444; }

	/* DLQ Section */
	.dlq-section {
		background-color: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		padding: 1.5rem;
		display: flex;
		flex-direction: column;
		gap: 1.25rem;
	}

	.section-header {
		display: flex;
		justify-content: space-between;
		align-items: center;
		flex-wrap: wrap;
		gap: 1rem;
	}

	.section-title {
		font-size: 1.125rem;
		font-weight: 600;
	}

	.section-subtitle {
		font-size: 0.8125rem;
		color: var(--text-muted);
	}

	.filter-controls {
		display: flex;
		gap: 1rem;
		align-items: center;
	}

	.pill-group {
		display: flex;
		background: var(--bg-root);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 2px;
	}

	.pill {
		background: transparent;
		border: none;
		color: var(--text-muted);
		padding: 0.25rem 0.625rem;
		font-size: 0.75rem;
		border-radius: var(--radius-sm);
		cursor: pointer;

		&.active {
			background: var(--bg-surface-hover);
			color: var(--text-primary);
			font-weight: 600;
		}
	}

	.search-input {
		padding: 0.375rem 0.75rem;
		font-size: 0.8125rem;
		width: 260px;
	}

	.table-wrapper {
		overflow-x: auto;
	}

	.dlq-table {
		width: 100%;
		border-collapse: collapse;
		text-align: left;
		font-size: 0.8125rem;
	}

	.dlq-table th {
		padding: 0.625rem 1rem;
		border-bottom: 1px solid var(--border-color);
		color: var(--text-muted);
		font-weight: 600;
	}

	.table-row {
		border-bottom: 1px solid var(--border-color-muted);
		cursor: pointer;
		transition: background 0.15s ease;

		&:hover {
			background-color: var(--bg-surface-hover);
		}
	}

	.dlq-table td {
		padding: 0.75rem 1rem;
	}

	.error-msg-cell {
		max-width: 320px;
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
		color: #f87171;
	}

	.class-tag {
		display: inline-block;
		padding: 0.125rem 0.5rem;
		border-radius: var(--radius-sm);
		font-size: 0.75rem;
		font-family: var(--font-mono);

		&.tag-permanent { background: rgba(239, 68, 68, 0.2); color: #ef4444; }
		&.tag-transient { background: rgba(245, 158, 11, 0.2); color: #f59e0b; }
		&.tag-unclassified { background: rgba(148, 163, 184, 0.2); color: #94a3b8; }
	}

	.btn-inspect {
		background: transparent;
		border: 1px solid var(--border-color);
		color: var(--color-primary);
		padding: 0.25rem 0.5rem;
		border-radius: var(--radius-sm);
		font-size: 0.75rem;
		cursor: pointer;

		&:hover {
			background: var(--color-primary);
			color: #fff;
		}
	}

	.empty-state {
		text-align: center;
		padding: 3rem 1rem;
		color: var(--text-muted);
	}

	.empty-icon {
		font-size: 2rem;
		color: #10b981;
		margin-bottom: 0.5rem;
	}

	/* Side Drawer */
	.drawer-backdrop {
		position: fixed;
		top: 0;
		left: 0;
		right: 0;
		bottom: 0;
		background: rgba(0, 0, 0, 0.6);
		backdrop-filter: blur(2px);
		z-index: 100;
		display: flex;
		justify-content: flex-end;
	}

	.drawer-panel {
		width: 500px;
		max-width: 90vw;
		height: 100%;
		background: var(--bg-surface);
		border-left: 1px solid var(--border-color);
		display: flex;
		flex-direction: column;
		padding: 1.5rem;
		gap: 1.5rem;
		overflow-y: auto;
	}

	.drawer-header {
		display: flex;
		justify-content: space-between;
		align-items: center;
		border-bottom: 1px solid var(--border-color);
		padding-bottom: 1rem;
	}

	.drawer-title {
		font-size: 1.125rem;
		font-weight: 600;
	}

	.btn-close {
		background: transparent;
		border: none;
		color: var(--text-muted);
		font-size: 1.5rem;
		cursor: pointer;
	}

	.drawer-body {
		display: flex;
		flex-direction: column;
		gap: 1.25rem;
	}

	.detail-group {
		display: flex;
		flex-direction: column;
		gap: 0.375rem;

		label {
			font-size: 0.75rem;
			font-weight: 600;
			color: var(--text-muted);

		}
	}

	.error-box {
		background: rgba(239, 68, 68, 0.1);
		border: 1px solid rgba(239, 68, 68, 0.3);
		color: #ef4444;
		padding: 0.75rem;
		border-radius: var(--radius-sm);
		font-family: var(--font-mono);
		font-size: 0.8125rem;
	}

	.payload-header {
		display: flex;
		justify-content: space-between;
		align-items: center;
	}

	.btn-copy {
		background: var(--bg-root);
		border: 1px solid var(--border-color);
		color: var(--text-primary);
		padding: 0.25rem 0.5rem;
		font-size: 0.75rem;
		border-radius: var(--radius-sm);
		cursor: pointer;
	}

	.json-code {
		background: var(--bg-root);
		border: 1px solid var(--border-color);
		padding: 1rem;
		border-radius: var(--radius-sm);
		font-family: var(--font-mono);
		font-size: 0.75rem;
		color: #38bdf8;
		white-space: pre-wrap;
		word-break: break-all;
		max-height: 400px;
		overflow-y: auto;
	}

	.font-mono { font-family: var(--font-mono); }
	.text-muted { color: var(--text-muted); }
	.text-subtle { color: var(--text-subtle); }
	.text-center { text-align: center; }
</style>
