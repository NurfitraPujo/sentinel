<script lang="ts">
	// sentinel-reporting-and-dashboard D12/D13: `/` is the dashboard. Each section is its own streamed promise (D14), so a
	// slow or failing section shows "unavailable" without touching the others, the nav or /issues. A viewer with no
	// accessible project gets the empty state and nothing else (D11).
	import type { PageData } from './$types';
	import FilterBar from '$lib/components/dashboard/FilterBar.svelte';
	import FreshnessLine from '$lib/components/dashboard/FreshnessLine.svelte';
	import HealthRow from '$lib/components/dashboard/HealthRow.svelte';
	import EventsTimeline from '$lib/components/dashboard/EventsTimeline.svelte';
	import BudgetBurndown from '$lib/components/dashboard/BudgetBurndown.svelte';
	import SloTable from '$lib/components/dashboard/SloTable.svelte';
	import ReleaseTable from '$lib/components/dashboard/ReleaseTable.svelte';
	import BacklogSection from '$lib/components/dashboard/BacklogSection.svelte';
	import ResponseSection from '$lib/components/dashboard/ResponseSection.svelte';
	import SectionState from '$lib/components/dashboard/SectionState.svelte';

	let { data }: { data: PageData } = $props();
	let apiService = $derived(data.empty ? undefined : (data.apiServices?.[0] as string | undefined));
</script>

<svelte:head><title>Dashboard · Sentinel</title></svelte:head>

<div class="dashboard" data-testid="dashboard">
	<header class="page-head">
		<h1>Dashboard</h1>
		<a class="to-issues" href="/issues" data-testid="to-issues">All issues →</a>
	</header>

	{#if data.empty}
		<div class="empty" data-testid="dashboard-empty">
			<h2>No projects yet</h2>
			<p>There is nothing to show until you belong to a project. Create an organization and a project to start collecting errors.</p>
			<a href="/onboarding">Set up a project</a>
		</div>
	{:else}
		<FilterBar filters={data.filters} projects={data.projects} environments={data.environments} />
		{#await data.sections.freshness then f}
			{#if f.status === 'ok'}<FreshnessLine data={f.data} />{/if}
		{/await}

		<section aria-labelledby="h-slo" class="section">
			<h2 id="h-slo">Service health (SLO)</h2>
			{#await data.sections.slo}
				<SectionState state="loading" name="slo" />
			{:then s}
				{#if s.status !== 'ok'}
					<SectionState state="unavailable" name="slo" />
				{:else if s.data.rows.length === 0}
					<SectionState state="empty" name="slo" message="No service levels are tracked for the projects you can see." />
				{:else}
					<SloTable rows={s.data.rows} range={data.filters.range} natsBreakdown={data.sections.natsBreakdown} />
				{/if}
			{/await}
		</section>

		<section aria-labelledby="h-budget" class="section">
			<h2 id="h-budget">Error budget</h2>
			{#await data.sections.budget}
				<SectionState state="loading" name="budget" />
			{:then s}
				{#if s.status !== 'ok'}
					<SectionState state="unavailable" name="budget" />
				{:else if s.data.charts.length === 0}
					<SectionState state="empty" name="budget" message="No objective with a target to burn down." />
				{:else}
					<BudgetBurndown charts={s.data.charts} />
				{/if}
			{/await}
		</section>

		<section aria-labelledby="h-errors" class="section">
			<h2 id="h-errors">Error tracking</h2>
			<p class="lede" data-testid="errors-lede">
				Exceptions the SDKs sent to Sentinel. These are not the SLO's failures: a 5xx with no exception, or an exception the service recovered from, counts in only one of them.
			</p>
			{#await data.sections.health}
				<SectionState state="loading" name="health" />
			{:then s}
				{#if s.status === 'ok'}
					<HealthRow data={s.data} filters={data.filters} {apiService} />
				{:else}
					<SectionState state="unavailable" name="health" />
				{/if}
			{/await}
			<div class="subsection">
				{#await data.sections.timeline}
					<SectionState state="loading" name="timeline" />
				{:then s}
					{#if s.status === 'ok'}
						<EventsTimeline data={s.data} filters={data.filters} {apiService} />
					{:else}
						<SectionState state="unavailable" name="timeline" />
					{/if}
				{/await}
			</div>
		</section>

		<section aria-labelledby="h-releases" class="section">
			<h2 id="h-releases">Release health</h2>
			{#await data.sections.releases}
				<SectionState state="loading" name="releases" />
			{:then s}
				{#if s.status !== 'ok'}
					<SectionState state="unavailable" name="releases" />
				{:else if s.data.needsProject}
					<SectionState state="empty" name="releases" message="Pick a project to see its releases." />
				{:else if s.data.rows.length === 0}
					<SectionState state="empty" name="releases" message="No releases have reported events for this selection." />
				{:else}
					<ReleaseTable rows={s.data.rows} showEnvironment={data.filters.environment === 'all'} showProject={s.data.allProjects} />
					{#if s.data.allProjects}
						<p class="hint" data-testid="releases-hint">Pick a project to see all its releases.</p>
					{/if}
				{/if}
			{/await}
		</section>

		<section aria-labelledby="h-backlog" class="section">
			<h2 id="h-backlog">Backlog</h2>
			{#await data.sections.backlog}
				<SectionState state="loading" name="backlog" />
			{:then s}
				{#if s.status === 'ok'}
					<BacklogSection data={s.data} filters={data.filters} />
				{:else}
					<SectionState state="unavailable" name="backlog" />
				{/if}
			{/await}
		</section>

		<section aria-labelledby="h-response" class="section">
			<h2 id="h-response">Response</h2>
			{#await data.sections.response}
				<SectionState state="loading" name="response" />
			{:then s}
				{#if s.status === 'ok'}
					<ResponseSection data={s.data} filters={data.filters} />
				{:else}
					<SectionState state="unavailable" name="response" />
				{/if}
			{/await}
		</section>
	{/if}
</div>

<style>
	.dashboard { display: flex; flex-direction: column; gap: 1rem; max-width: 80rem; margin: 0 auto; padding: 1rem; }
	.page-head { display: flex; align-items: baseline; justify-content: space-between; gap: 1rem; }
	h1 { margin: 0; font-size: 1.25rem; }
	h2 { margin: 0 0 0.5rem; font-size: 0.8125rem; text-transform: uppercase; letter-spacing: 0.05em; color: var(--text-muted); font-weight: 600; }
	.to-issues { font-size: 0.8125rem; color: var(--color-primary); text-decoration: none; }
	.to-issues:hover { text-decoration: underline; }
	.section { padding-top: 0.5rem; border-top: 1px solid var(--border-color-muted); }
	.hint { margin: 0.5rem 0 0; font-size: 0.75rem; color: var(--text-muted); }
	.lede { margin: 0 0 0.625rem; font-size: 0.75rem; color: var(--text-muted); }
	.subsection { margin-top: 0.75rem; }
	.empty { padding: 2rem 1rem; text-align: center; color: var(--text-secondary); }
	.empty h2 { text-transform: none; font-size: 1rem; color: var(--text-primary); letter-spacing: 0; }
</style>
