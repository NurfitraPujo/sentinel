<script lang="ts">
	// sentinel-cross-repo-trace-view TASK-01 (contract D3/D4): every error occurrence sharing one
	// trace id across the org's projects, oldest first, under a span waterfall (sentinel-span-ingest D6)
	// that pins each error to its span.
	import { goto } from '$app/navigation';
	import type { PageData } from './$types';
	import { levelTone } from '$lib/components/issues/occurrence-display';
	import SpanWaterfall from '$lib/components/traces/SpanWaterfall.svelte';
	import SpanPullControl from '$lib/components/traces/SpanPullControl.svelte';

	let { data }: { data: PageData } = $props();

	// "Load more" APPENDS: each page arrives as `data` for `?cursor=…`, and is merged into what is
	// already shown. A different trace id resets the list.
	// svelte-ignore state_referenced_locally -- seeded once, kept in sync by the effect below.
	let rows = $state(data.occurrences);
	// svelte-ignore state_referenced_locally -- same: the effect tracks changes.
	let shownTrace = data.traceId;
	$effect(() => {
		if (data.traceId !== shownTrace) {
			shownTrace = data.traceId;
			rows = data.occurrences;
			return;
		}
		const have = new Set(rows.map((r) => r.id));
		const fresh = data.occurrences.filter((r) => !have.has(r.id));
		if (fresh.length > 0) rows = [...rows, ...fresh];
	});

	// D3: `received_at` is only worth showing when it differs from the event time by more than a
	// minute — below that it is clock noise, above it the device clock is skewed or the event queued.
	const SKEW_MS = 60_000;

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

	// Computed by Postgres over the two timestamptz values (`skewSeconds`), not by subtracting two
	// JS Dates: the Drizzle mirror types created_at without a zone, so a non-UTC session skewed it.
	function skewed(r: { skewSeconds: number | null }): boolean {
		return r.skewSeconds !== null && Math.abs(r.skewSeconds) * 1000 > SKEW_MS;
	}

	const totalLabel = $derived(
		data.totalCapped ? '1,000+' : Number(data.total).toLocaleString()
	);

	function loadMore() {
		if (!data.nextCursor) return;
		goto(`?cursor=${encodeURIComponent(data.nextCursor)}`, {
			noScroll: true,
			keepFocus: true,
		});
	}
</script>

<div class="container" data-testid="trace-page">
	<h1>Trace <code data-testid="trace-id">{data.traceId}</code></h1>

	<!-- D6: above the occurrence list; the row list and its paging are unchanged. Errors pinned are the rows
	     loaded so far, so "Load more" pins the next page's errors too. -->
	{#if data.spans.length === 0 && rows.length > 0 && data.spansState && data.spansState.status !== 'available'}
		<!-- sentinel-occurrence-spans: no spans to draw — say why, and offer the pull instead of an empty waterfall. -->
		<SpanPullControl
			variant="panel"
			organizationId={data.organizationId}
			traceId={data.traceId}
			status={data.spansState.status}
			reason={data.spansState.reason}
			enabled={!!data.spanPullEnabled}
		/>
	{:else if data.spans.length > 0 || rows.length > 0}
		<SpanWaterfall
			spans={data.spans}
			errors={rows}
			truncated={data.spansTruncated}
			retentionDays={data.spanRetentionDays}
		/>
	{/if}

	{#if rows.length === 0}
		<!-- D2: one message for "never existed", "another org's" and "hidden by an environment grant". -->
		<p class="empty" data-testid="trace-empty">No errors in this trace.</p>
	{:else}
		<p class="trace-summary" data-testid="trace-summary">
			<span data-testid="trace-total">{totalLabel}</span>
			{data.total === 1 && !data.totalCapped ? 'error' : 'errors'} across
			{data.projectCount}
			{data.projectCount === 1 ? 'project' : 'projects'}:
			<span data-testid="trace-projects">{data.projects.map((p) => p.name).join(', ')}{data.projectCount > data.projects.length ? ', …' : ''}</span>
		</p>

		<ol class="trace-list">
			{#each rows as r (r.id)}
				<li class="trace-row" data-testid="trace-row">
					<div class="trace-row-head">
						<span class="timestamp">{formatDate(r.createdAt)}</span>
						{#if skewed(r)}
							<span class="skew" data-testid="trace-skew" title="Server received this event at a different time than the client recorded"
								>received {formatDate(r.receivedAt)}</span
							>
						{/if}
						<span class="badge project" data-testid="trace-project">{r.projectName}</span>
						<span class="badge env">{r.environment}</span>
						{#if r.level}<span class="badge level level-{r.level} {levelTone(r.level)}">{r.level}</span>{/if}
					</div>
					<div class="trace-row-body">
						<a
							class="trace-issue-link"
							data-testid="trace-issue-link"
							href="/issues/{r.issueId}?occurrence={r.id}"
						>
							{#if r.errorClass}<strong>{r.errorClass}</strong>: {/if}<span class="message"
								>{r.message ?? 'No message'}</span
							>
						</a>
					</div>
					{#if r.serverName || r.releaseVersion}
						<div class="trace-row-meta">
							{#if r.serverName}<span>{r.serverName}</span>{/if}
							{#if r.releaseVersion}<span>release {r.releaseVersion}</span>{/if}
						</div>
					{/if}
				</li>
			{/each}
		</ol>

		{#if data.nextCursor}
			<button type="button" class="load-more" data-testid="trace-load-more" onclick={loadMore}>
				Load more
			</button>
		{/if}
	{/if}

	<!-- The issue page says the same: the sweep deletes occurrences older than the window. -->
	<p class="retention-note" data-testid="trace-retention">
		Occurrences older than {data.retentionDays} days have been removed and are not shown.
	</p>
</div>

<style>
	.container { max-width: 1000px; margin: 0 auto; padding: 1.5rem; }
	h1 { font-size: 1.25rem; }
	code { word-break: break-all; }
	.trace-summary { color: var(--text-secondary); }
	.empty { color: var(--text-muted); }
	.trace-list { list-style: none; padding: 0; margin: 1rem 0; }
	.trace-row { background: var(--bg-surface); border: 1px solid var(--border-color); border-radius: var(--radius-md); padding: 0.75rem; margin-bottom: 0.5rem; }
	.trace-row-head { display: flex; flex-wrap: wrap; gap: 0.5rem; align-items: center; font-size: 0.85rem; }
	.trace-row-body { margin-top: 0.4rem; }
	.trace-row-body a { color: var(--color-primary); text-decoration: none; }
	.trace-row-body a:hover { color: var(--color-primary-hover); text-decoration: underline; }
	.trace-row-body .message { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; display: inline-block; max-width: 100%; vertical-align: bottom; }
	.trace-row-meta { margin-top: 0.3rem; font-size: 0.8rem; color: var(--text-muted); display: flex; gap: 0.75rem; }
	.timestamp { font-family: var(--font-mono); color: var(--text-muted); }
	.badge { padding: 0.125rem 0.375rem; border-radius: var(--radius-sm); font-family: var(--font-mono); font-size: 0.6875rem; background: var(--bg-root); color: var(--text-muted); border: 1px solid var(--border-color); }
	/* Level badges reuse the issue page's tones (occurrence-display levelTone), same variables as OccurrenceHeader. */
	.badge.tone-critical { color: var(--severity-critical-text); background: var(--severity-critical-bg); border-color: var(--severity-critical-border); }
	.badge.tone-warning { color: var(--status-open-text); background: var(--status-open-bg); border-color: var(--status-open-border); }
	.badge.tone-info { color: var(--color-primary); background: var(--color-primary-subtle); border-color: var(--border-color); }
	.badge.tone-debug { color: var(--status-ignored-text); background: var(--status-ignored-bg); border-color: var(--status-ignored-border); }
	.skew { font-size: 0.75rem; color: var(--text-muted); }
	.retention-note { font-size: 0.8rem; color: var(--text-muted); }
	.load-more { margin: 0.5rem 0 1rem; }
</style>
