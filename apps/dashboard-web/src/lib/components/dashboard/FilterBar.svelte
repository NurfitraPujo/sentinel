<script lang="ts">
	// D12, flow H1: the dashboard's three filters. `project` (all or one id), `environment` (only inside one project) and
	// `range` (24h 7d 28d 90d, default 7d). Plain anchors, so the page works without script and every selection is a URL.
	import { RANGES } from '$lib/dashboard-format';

	interface Props {
		filters: { project: string; environment: string; range: string };
		projects: { id: string; name: string }[];
		environments: { name: string; displayName: string }[];
	}
	let { filters, projects, environments }: Props = $props();

	function href(p: Partial<{ project: string; environment: string; range: string }>): string {
		const project = p.project ?? filters.project;
		// a project change drops the environment (it is per project)
		const environment = p.project !== undefined && p.project !== filters.project ? 'all' : (p.environment ?? filters.environment);
		const range = p.range ?? filters.range;
		const q = new URLSearchParams();
		if (project !== 'all') q.set('project', project);
		if (project !== 'all' && environment !== 'all') q.set('environment', environment);
		if (range !== '7d') q.set('range', range);
		const s = q.toString();
		return s ? `/?${s}` : '/';
	}
</script>

<nav class="filters" aria-label="Dashboard filters" data-testid="dashboard-filters">
	<div class="group" role="group" aria-label="Range" data-testid="range-filter">
		{#each RANGES as r (r)}
			<a href={href({ range: r })} class:active={filters.range === r} aria-current={filters.range === r ? 'true' : undefined}>{r}</a>
		{/each}
	</div>
	<div class="group" role="group" aria-label="Project" data-testid="project-filter">
		<a href={href({ project: 'all' })} class:active={filters.project === 'all'} aria-current={filters.project === 'all' ? 'true' : undefined}>All projects</a>
		{#each projects as p (p.id)}
			<a href={href({ project: p.id })} class:active={filters.project === p.id} aria-current={filters.project === p.id ? 'true' : undefined}>{p.name}</a>
		{/each}
	</div>
	{#if filters.project !== 'all' && environments.length > 0}
		<div class="group" role="group" aria-label="Environment" data-testid="environment-filter">
			<a href={href({ environment: 'all' })} class:active={filters.environment === 'all'} aria-current={filters.environment === 'all' ? 'true' : undefined}>All environments</a>
			{#each environments as e (e.name)}
				<a href={href({ environment: e.name })} class:active={filters.environment === e.name} aria-current={filters.environment === e.name ? 'true' : undefined}>{e.displayName}</a>
			{/each}
		</div>
	{/if}
</nav>

<style>
	.filters { display: flex; flex-wrap: wrap; gap: 0.5rem 1rem; align-items: center; }
	.group { display: inline-flex; flex-wrap: wrap; border: 1px solid var(--border-color); border-radius: var(--radius-md); overflow: hidden; }
	a { padding: 0.25rem 0.625rem; font-size: 0.8125rem; color: var(--text-secondary); text-decoration: none; border-right: 1px solid var(--border-color); }
	a:last-child { border-right: none; }
	a:hover { background: var(--bg-surface-hover); }
	a.active { background: var(--color-primary-subtle); color: var(--text-primary); font-weight: 600; }
</style>
