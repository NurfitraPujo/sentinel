<script lang="ts">
	import type { ObservabilityData } from '$lib/server/observability';

	// Typed against what this component actually READS, not against either route's `PageData`.
	// It is rendered by two routes whose PageData differ (`/settings/observability` sits under
	// `settings/+layout.server.ts` so its `session` is non-null; `/[orgSlug]/settings/observability`
	// gets the root layout's `Session | null`). Pinning to this route's own PageData made the org
	// route's data un-assignable for a field this component never touches.
	let { data }: { data: { observability: ObservabilityData } } = $props();

	let observability = $derived(data.observability);
	let processor = $derived(observability.processor);
	let ingestor = $derived(observability.ingestor);

	function getStatusClass(status: string) {
		if (status.includes('healthy') || status === 'ok') return 'status-healthy';
		if (status.includes('attention')) return 'status-attention';
		if (status.includes('critical') || status === 'offline') return 'status-critical';
		return 'status-muted';
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

	</section>

	<!-- DLQ Event Inspector: withheld here on purpose. The DLQ depth/backlog
	     and per-event payloads aggregate every tenant's failures, and this
	     codebase has no platform-operator role to gate that view on (D05). -->
	<section class="dlq-section">
		<div class="section-header">
			<div>
				<h2 class="section-title">Dead-Letter Queue Inspector</h2>
				<p class="section-subtitle">Cross-tenant DLQ data requires a platform-operator role</p>
			</div>
		</div>
		<div class="empty-state">
			<div class="empty-icon">🔒</div>
			<h3>Restricted</h3>
			<p>
				DLQ backlog depth and parked-event payloads span every organization on this instance.
				This build has no platform-operator role to gate that view on, so it is withheld from all
				authenticated users rather than shown to anyone with a session.
			</p>
		</div>
	</section>
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

	.text-muted { color: var(--text-muted); }
</style>
