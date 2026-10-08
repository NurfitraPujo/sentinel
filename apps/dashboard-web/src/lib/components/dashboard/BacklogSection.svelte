<script lang="ts">
	// D13 section 5, flow H7: opened (new + regressed) vs resolved per week for the last 12 weeks, the age buckets of the
	// open backlog, the unassigned share, and the top issues by distinct users in range.
	import StackedBars from './StackedBars.svelte';
	import { count, listHref } from '$lib/dashboard-format';
	import type { BacklogData } from '$lib/server/analytics/service';

	interface Props {
		data: BacklogData;
		filters: { project: string; environment: string };
	}
	let { data, filters }: Props = $props();

	const AGES = ['0-7d', '7-30d', '30-90d', '90d+'] as const;
	let peak = $derived(Math.max(1, ...AGES.map((a) => data.age[a])));
	let unassignedShare = $derived(data.open === 0 ? null : data.unassigned / data.open);
	let weeks = $derived(data.weekly.map((w) => ({ start: w.week, values: { opened: w.opened, resolved: w.resolved } })));
</script>

<div class="backlog" data-testid="backlog-section">
	<StackedBars
		heading="Opened vs resolved per week"
		series={[{ key: 'opened', label: 'Opened (new + regressed)' }, { key: 'resolved', label: 'Resolved' }]}
		buckets={weeks}
		mode="grouped"
		formatBucket={(s) => s.slice(0, 10)}
		labelEvery={2}
		testId="weekly"
	/>

	<section aria-labelledby="age-h">
		<h3 id="age-h">Open backlog by age</h3>
		<ul class="ages" data-testid="age-buckets">
			{#each AGES as a (a)}
				<li data-testid="age-{a}">
					<span class="age-label">{a}</span>
					<span class="bar" style="width: {(data.age[a] / peak) * 100}%"></span>
					<span class="age-count">{count(data.age[a])}</span>
				</li>
			{/each}
		</ul>
		<p class="share" data-testid="unassigned-share">
			{#if unassignedShare === null}Nothing open.{:else}
				<a href={listHref('unassigned', filters)}>{count(data.unassigned)} unassigned</a> of {count(data.open)} open ({(unassignedShare * 100).toFixed(0)}%)
			{/if}
		</p>
	</section>

	<section aria-labelledby="top-h">
		<h3 id="top-h">Top issues by users affected</h3>
		{#if data.top.length === 0}
			<p class="muted" data-testid="top-empty">No issues with events in this range.</p>
		{:else}
			<ol class="top" data-testid="top-issues">
				{#each data.top as t (t.id)}
					<li>
						<a href="/issues/{t.id}"><span class="cls">{t.errorClass || 'Error'}</span> {t.message}</a>
						<span class="meta">{count(t.users)} users · {count(t.events)} events</span>
					</li>
				{/each}
			</ol>
		{/if}
	</section>
</div>

<style>
	.backlog { display: grid; grid-template-columns: repeat(auto-fit, minmax(18rem, 1fr)); gap: 1rem; align-items: start; }
	h3 { margin: 0 0 0.375rem; font-size: 0.8125rem; color: var(--text-secondary); }
	.ages { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 0.25rem; }
	.ages li { display: grid; grid-template-columns: 4rem 1fr 3rem; align-items: center; gap: 0.5rem; font-size: 0.8125rem; font-variant-numeric: tabular-nums; }
	.age-label { color: var(--text-muted); font-family: var(--font-mono); }
	.bar { display: block; height: 0.625rem; background: var(--color-primary); border-radius: 2px; min-width: 1px; }
	.age-count { text-align: right; }
	.share, .muted { font-size: 0.8125rem; color: var(--text-muted); margin: 0.5rem 0 0; }
	.top { margin: 0; padding-left: 1.25rem; display: flex; flex-direction: column; gap: 0.375rem; font-size: 0.8125rem; }
	.top a { color: var(--text-primary); text-decoration: none; overflow-wrap: anywhere; }
	.top a:hover { text-decoration: underline; }
	.cls { font-weight: 600; margin-right: 0.25rem; }
	.meta { display: block; color: var(--text-muted); font-size: 0.75rem; }
</style>
