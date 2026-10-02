<script lang="ts">
	// sentinel-cross-repo-trace-view TASK-01 (contract D3/D4): every error occurrence sharing one
	// trace id across the org's projects, oldest first. Error occurrences only — a span/transaction
	// view is sentinel-span-ingest's, not this page's.
	import { goto } from '$app/navigation';
	import type { PageData } from './$types';

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
						{#if r.level}<span class="badge level level-{r.level}">{r.level}</span>{/if}
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
	.trace-list { list-style: none; padding: 0; margin: 1rem 0; }
	.trace-row { border: 1px solid var(--border, #ddd); border-radius: 6px; padding: 0.75rem; margin-bottom: 0.5rem; }
	.trace-row-head { display: flex; flex-wrap: wrap; gap: 0.5rem; align-items: center; font-size: 0.85rem; }
	.trace-row-body { margin-top: 0.4rem; }
	.trace-row-body .message { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; display: inline-block; max-width: 100%; vertical-align: bottom; }
	.trace-row-meta { margin-top: 0.3rem; font-size: 0.8rem; opacity: 0.7; display: flex; gap: 0.75rem; }
	.badge { padding: 0.1rem 0.5rem; border-radius: 999px; background: var(--badge-bg, #eee); font-size: 0.75rem; }
	.skew { font-size: 0.75rem; opacity: 0.7; }
	.retention-note { font-size: 0.8rem; opacity: 0.7; }
	.load-more { margin: 0.5rem 0 1rem; }
</style>
