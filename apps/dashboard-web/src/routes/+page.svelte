<script lang="ts">
	// sentinel-reporting-and-dashboard D12/D13: `/` is the dashboard. Each section is its own streamed promise (D14), so a
	// slow or failing section shows "unavailable" without touching the others, the nav or /issues. A viewer with no
	// accessible project gets the empty state and nothing else (D11).
	import type { PageData } from './$types';
	import FilterBar from '$lib/components/dashboard/FilterBar.svelte';
	import FreshnessLine from '$lib/components/dashboard/FreshnessLine.svelte';
	import HealthRow from '$lib/components/dashboard/HealthRow.svelte';
	import EventsTimeline from '$lib/components/dashboard/EventsTimeline.svelte';
	import SliSection from '$lib/components/dashboard/SliSection.svelte';
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

		<section aria-labelledby="h-health" class="section">
			<h2 id="h-health">Health</h2>
			{#await data.sections.health}
				<SectionState state="loading" name="health" />
			{:then s}
				{#if s.status === 'ok'}
					<HealthRow data={s.data} filters={data.filters} {apiService} />
				{:else}
					<SectionState state="unavailable" name="health" />
				{/if}
			{/await}
		</section>

		<section aria-labelledby="h-timeline" class="section">
			<h2 id="h-timeline">Error events over time</h2>
			{#await data.sections.timeline}
				<SectionState state="loading" name="timeline" />
			{:then s}
				{#if s.status === 'ok'}
					<EventsTimeline data={s.data} />
				{:else}
					<SectionState state="unavailable" name="timeline" />
				{/if}
			{/await}
		</section>

		<section aria-labelledby="h-slis" class="section">
			<h2 id="h-slis">Service levels</h2>
			{#await data.sections.slis}
				<SectionState state="loading" name="slis" />
			{:then s}
				{#if s.status !== 'ok'}
					<SectionState state="unavailable" name="slis" />
				{:else if s.data.length === 0}
					<SectionState state="empty" name="slis" message="No service levels are tracked for the projects you can see." />
				{:else}
					<SliSection data={s.data} />
				{/if}
			{/await}
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
					<ReleaseTable rows={s.data.rows} showEnvironment={data.filters.environment === 'all'} />
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
	.empty { padding: 2rem 1rem; text-align: center; color: var(--text-secondary); }
	.empty h2 { text-transform: none; font-size: 1rem; color: var(--text-primary); letter-spacing: 0; }
</style>
