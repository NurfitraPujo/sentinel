<script lang="ts">
	// D13 section 3: totals and errors per SLI with the good-ratio line (flow H2's chart half).
	import SliChart from './SliChart.svelte';
	import { count, pct } from '$lib/dashboard-format';
	import type { SliSeriesData } from '$lib/server/analytics/service';

	interface Props {
		data: SliSeriesData[];
	}
	let { data }: Props = $props();
	const fmt = (bucket: string, s: string) => (bucket === 'hour' ? s.slice(5, 16) : s.slice(0, 10));
</script>

<div class="slis" data-testid="sli-section">
	{#each data as s (s.id)}
		<article class="sli" data-testid="sli-{s.id}">
			<header>
				<h3>{s.id}</h3>
				<span class="totals">{count(s.total)} total · {count(s.errors)} errors · {s.total === 0 ? 'no data' : pct(1 - s.errors / s.total)} good</span>
			</header>
			<SliChart id={s.id} points={s.points} goal={s.availabilityTarget} formatBucket={(b) => fmt(s.bucket, b)} testId="sli-chart-{s.id}" />
		</article>
	{/each}
</div>

<style>
	.slis { display: grid; grid-template-columns: repeat(auto-fit, minmax(18rem, 1fr)); gap: 0.75rem; }
	.sli { background: var(--bg-surface); border: 1px solid var(--border-color); border-radius: var(--radius-md); padding: 0.625rem 0.75rem; }
	header { display: flex; justify-content: space-between; align-items: baseline; gap: 0.5rem; flex-wrap: wrap; margin-bottom: 0.375rem; }
	h3 { margin: 0; font-size: 0.875rem; }
	.totals { font-size: 0.75rem; color: var(--text-muted); font-variant-numeric: tabular-nums; }
</style>
