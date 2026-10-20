<script lang="ts">
	// D13 section 2 as the Error tracking half's chart (sentinel-dashboard-metrics-improvement D12): captured exceptions over
	// time, split by origin (the five D19 groups) or by service (top 8, the rest "Other services"), on every project
	// selection. Hourly buckets for 24h and 7d, daily for 28d and 90d. A bar opens the issues seen in its bucket.
	import StackedBars from './StackedBars.svelte';
	import { SPLITS, SPLIT_LABELS, dashboardHref, eventsBarHref, stackEvents, type DashboardFilters, type Split } from '$lib/dashboard-format';
	import type { TimelineData } from '$lib/server/analytics/service';

	interface Props {
		data: TimelineData;
		filters?: DashboardFilters;
		apiService?: string;
	}
	let { data, filters = { project: 'all', environment: 'all', range: '7d' }, apiService }: Props = $props();

	let split = $derived((data.split ?? 'group') as Split);
	let stacking = $derived(stackEvents(data.byGroup, split));
	let bucketMs = $derived(data.bucket === 'hour' ? 3_600_000 : 86_400_000);
	let buckets = $derived.by(() => {
		const by = new Map<string, Record<string, number>>();
		for (const r of data.byGroup) {
			const v = by.get(r.bucket) ?? {};
			const k = stacking.keyOf(r);
			v[k] = (v[k] ?? 0) + r.n;
			by.set(r.bucket, v);
		}
		return [...by.entries()].sort((a, b) => a[0].localeCompare(b[0])).map(([start, values]) => ({ start, values }));
	});
	const fmt = (s: string) => (data.bucket === 'hour' ? s.slice(5, 16).replace(' ', ' ') : s.slice(0, 10));
	// a marker sits on the bucket that contains the release's first-seen instant
	let markers = $derived(
		data.releaseMarkers
			.map((m) => {
				const iso = m.at.slice(0, data.bucket === 'hour' ? 13 : 10);
				const start = buckets.find((b) => b.start.replace(' ', 'T').startsWith(iso))?.start;
				return start ? { start, label: m.version } : null;
			})
			.filter((m): m is { start: string; label: string } => m !== null)
	);
	const scope = $derived({ project: filters.project, environment: filters.environment });
</script>

<div class="timeline">
	<div class="split" role="group" aria-label="Split the chart" data-testid="split-toggle">
		{#each SPLITS as s (s)}
			<a href={dashboardHref(filters, { split: s })} class:active={split === s} aria-current={split === s ? 'true' : undefined}>{SPLIT_LABELS[s]}</a>
		{/each}
	</div>
	<StackedBars
		heading={split === 'service' ? 'Captured exceptions by service' : 'Captured exceptions by origin'}
		series={stacking.series}
		{buckets}
		formatBucket={fmt}
		{markers}
		labelEvery={data.bucket === 'hour' ? 12 : 4}
		href={(start, s) => {
			const series = stacking.series.find((x) => x.key === s.key);
			return series ? eventsBarHref({ scope, series, bucketStart: start, bucketMs, apiService }) : null;
		}}
		testId="timeline"
	/>
</div>

<style>
	.timeline { display: flex; flex-direction: column; gap: 0.375rem; }
	.split { align-self: flex-start; display: inline-flex; border: 1px solid var(--border-color); border-radius: var(--radius-md); overflow: hidden; }
	a { padding: 0.25rem 0.625rem; font-size: 0.8125rem; color: var(--text-secondary); text-decoration: none; border-right: 1px solid var(--border-color); }
	a:last-child { border-right: none; }
	a:hover { background: var(--bg-surface-hover); }
	a.active { background: var(--color-primary-subtle); color: var(--text-primary); font-weight: 600; }
</style>
