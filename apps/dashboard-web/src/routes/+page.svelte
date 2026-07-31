<script lang="ts">
	import type { PageData } from './$types';

	let { data } = $props();

	function formatDate(date: string | Date | null): string {
		if (!date) return 'Never';
		const d = new Date(date);
		return d.toLocaleDateString('en-US', {
			month: 'short',
			day: 'numeric',
			hour: '2-digit',
			minute: '2-digit',
		});
	}

	function isNew(lastSeen: string | Date | null): boolean {
		if (!lastSeen) return false;
		const diff = Date.now() - new Date(lastSeen).getTime();
		return diff < 24 * 60 * 60 * 1000;
	}
</script>

<div class="dashboard">
	<aside class="filters">
		<h2>Projects</h2>
		<ul>
			<li>
				<a href="?status={data.filters.status}&project=all" 
				   class:active={data.filters.project === 'all'}>
					All Projects
				</a>
			</li>
			{#each data.projects as project}
				<li>
					<a href="?status={data.filters.status}&project={project.id}"
					   class:active={data.filters.project === project.id}>
						{project.name}
					</a>
				</li>
			{/each}
		</ul>

		<h2>Status</h2>
		<ul>
			<li>
				<a href="?status=all&project={data.filters.project}"
				   class:active={data.filters.status === 'all'}>All</a>
			</li>
			<li>
				<a href="?status=open&project={data.filters.project}"
				   class:active={data.filters.status === 'open'}>Open</a>
			</li>
			<li>
				<a href="?status=resolved&project={data.filters.project}"
				   class:active={data.filters.status === 'resolved'}>Resolved</a>
			</li>
			<li>
				<a href="?status=ignored&project={data.filters.project}"
				   class:active={data.filters.status === 'ignored'}>Ignored</a>
			</li>
		</ul>
	</aside>

	<main class="issues">
		<h1>Issues</h1>

		<div class="issue-grid">
			{#each data.issues as issue}
				<a href="/issues/{issue.id}" class="issue-card">
					<div class="card-header">
						<span class="error-class">{issue.errorClass}</span>
						{#if isNew(issue.lastSeen)}
							<span class="badge new">NEW</span>
						{/if}
					</div>
					<p class="message">{issue.message.slice(0, 150)}{issue.message.length > 150 ? '...' : ''}</p>
					<div class="card-footer">
						<span class="count">{Number(issue.count).toLocaleString()} occurrences</span>
						<span class="last-seen">Last seen: {formatDate(issue.lastSeen)}</span>
					</div>
					<div class="meta">
						<span class="project">{issue.projectName}</span>
						<span class="status {issue.status}">{issue.status}</span>
					</div>
				</a>
			{:else}
				<p class="empty">No issues found.</p>
			{/each}
		</div>

		{#if data.pagination.totalPages > 1}
			<nav class="pagination">
				{#if data.pagination.page > 1}
					<a href="?page={data.pagination.page - 1}&status={data.filters.status}&project={data.filters.project}">
						Previous
					</a>
				{/if}
				<span>Page {data.pagination.page} of {data.pagination.totalPages}</span>
				{#if data.pagination.page < data.pagination.totalPages}
					<a href="?page={data.pagination.page + 1}&status={data.filters.status}&project={data.filters.project}">
						Next
					</a>
				{/if}
			</nav>
		{/if}
	</main>
</div>

<style>
	.dashboard {
		display: grid;
		grid-template-columns: 250px 1fr;
		gap: 2rem;
		min-height: calc(100vh - 100px);
	}

	@media (max-width: 768px) {
		.dashboard {
			grid-template-columns: 1fr;
			gap: 1.25rem;
		}

		.filters {
			border-right: none;
			border-bottom: 1px solid var(--border-color);
			padding-right: 0;
			padding-bottom: 1rem;
		}

		.filters ul {
			display: flex;
			flex-wrap: wrap;
			gap: 0.5rem;
		}

		.filters li {
			margin-bottom: 0;
		}

		.filters a {
			min-height: 44px;
			display: inline-flex;
			align-items: center;
			padding: 0.5rem 0.875rem;
		}
	}

	.filters {
		border-right: 1px solid var(--border-color);
		padding-right: 1rem;
	}

	.filters h2 {
		font-size: 0.75rem;
		font-family: var(--font-mono);
		letter-spacing: 0.05em;
		text-transform: uppercase;
		color: var(--text-subtle);
		margin-bottom: 0.5rem;
	}

	.filters ul {
		list-style: none;
		margin-bottom: 1.5rem;
	}

	.filters li {
		margin-bottom: 0.25rem;
	}

	.filters a {
		display: block;
		padding: 0.5rem 0.75rem;
		border-radius: var(--radius-sm);
		color: var(--text-muted);
		text-decoration: none;
		font-size: 0.875rem;
		transition: background 0.15s ease, color 0.15s ease;
	}

	.filters a:hover {
		background: var(--bg-surface-hover);
		color: var(--text-primary);
	}

	.filters a.active {
		background: var(--color-primary);
		color: var(--text-primary);
		font-weight: 500;
	}

	.issues h1 {
		font-size: 1.5rem;
		font-weight: 600;
		margin-bottom: 1.5rem;
		color: var(--text-primary);
	}

	.issue-grid {
		display: grid;
		gap: 0.75rem;
	}

	.issue-card {
		display: block;
		padding: 1rem;
		background-color: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		text-decoration: none;
		color: inherit;
		transition: border-color 0.15s ease, background-color 0.15s ease;
	}

	.issue-card:hover {
		border-color: var(--color-primary);
		background-color: var(--bg-surface-hover);
	}

	.card-header {
		display: flex;
		justify-content: space-between;
		align-items: center;
		margin-bottom: 0.5rem;
	}

	.error-class {
		font-weight: 600;
		font-family: var(--font-mono);
		color: var(--severity-critical-text);
		font-size: 0.875rem;
	}

	.badge.new {
		background: var(--severity-critical-bg);
		color: var(--severity-critical-text);
		border: 1px solid var(--severity-critical-border);
		font-family: var(--font-mono);
		font-size: 0.6875rem;
		font-weight: 700;
		padding: 0.125rem 0.375rem;
		border-radius: var(--radius-sm);
		letter-spacing: 0.05em;
	}

	.message {
		color: var(--text-muted);
		margin-bottom: 0.75rem;
		line-height: 1.4;
		font-size: 0.875rem;
	}

	.card-footer {
		display: flex;
		justify-content: space-between;
		font-size: 0.75rem;
		font-family: var(--font-mono);
		color: var(--text-subtle);
		margin-bottom: 0.5rem;
	}

	.meta {
		display: flex;
		justify-content: space-between;
		align-items: center;
		font-size: 0.75rem;
		color: var(--text-subtle);
	}

	.status {
		padding: 0.125rem 0.5rem;
		border-radius: var(--radius-sm);
		font-family: var(--font-mono);
		font-size: 0.6875rem;
		text-transform: uppercase;
		font-weight: 600;
		letter-spacing: 0.05em;
	}

	.status.open {
		background: var(--status-open-bg);
		color: var(--status-open-text);
	}

	.status.resolved {
		background: var(--status-resolved-bg);
		color: var(--status-resolved-text);
	}

	.status.ignored {
		background: var(--status-ignored-bg);
		color: var(--status-ignored-text);
	}

	.pagination {
		display: flex;
		justify-content: center;
		align-items: center;
		gap: 1rem;
		margin-top: 2rem;
		font-size: 0.875rem;
		color: var(--text-muted);
	}

	.pagination a {
		padding: 0.5rem 1rem;
		min-height: 44px;
		display: inline-flex;
		align-items: center;
		background: var(--color-primary);
		color: var(--text-primary);
		border-radius: var(--radius-sm);
		text-decoration: none;
		font-weight: 500;
		transition: background 0.15s ease;
	}

	.pagination a:hover {
		background: var(--color-primary-hover);
	}

	.empty {
		text-align: center;
		color: var(--text-subtle);
		padding: 3rem;
	}
</style>