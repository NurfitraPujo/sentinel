<script lang="ts">
	import type { PageData } from './$types';

	export let data: PageData;
</script>

<div class="projects-index">
	<header class="page-header">
		<h1 class="page-title">Projects</h1>
		<p class="page-subtitle">{data.org.name}</p>
	</header>

	{#if data.projects.length === 0}
		<p class="empty">No projects yet. Create one from onboarding.</p>
	{:else}
		<ul class="project-list">
			{#each data.projects as project}
				<li class="project-row">
					<span class="project-name">{project.name}</span>
					<span class="project-links">
						<a href="/{data.org.slug}/projects/{project.id}/settings/keys">API keys</a>
						<a href="/{data.org.slug}/projects/{project.id}/settings">Settings</a>
					</span>
				</li>
			{/each}
		</ul>
	{/if}
</div>

<style>
	.projects-index {
		max-width: 960px;
		margin: 0 auto;
		padding: 2rem 1rem;
	}

	.page-header {
		margin-bottom: 1.25rem;
	}

	.page-title {
		font-size: 1.375rem;
		font-weight: 600;
		color: var(--text-primary);
		margin-bottom: 0.25rem;
	}

	.page-subtitle {
		font-size: 0.8125rem;
		color: var(--text-muted);
	}

	.project-list {
		list-style: none;
		padding: 0;
		margin: 0;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		overflow: hidden;
	}

	.project-row {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 1rem;
		padding: 0.875rem 1rem;
		background-color: var(--bg-surface);
	}

	.project-row + .project-row {
		border-top: 1px solid var(--border-color);
	}

	.project-name {
		font-size: 0.875rem;
		font-weight: 500;
		color: var(--text-primary);
	}

	.project-links {
		display: flex;
		gap: 1rem;
	}

	.project-links a {
		font-size: 0.8125rem;
		color: var(--color-primary);
		text-decoration: none;
	}

	.project-links a:hover {
		text-decoration: underline;
	}

	.empty {
		font-size: 0.8125rem;
		color: var(--text-muted);
	}
</style>
