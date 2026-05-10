<script lang="ts">
	import type { PageData } from './$types';

	export let data: PageData;

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

	function isNew(lastSeen: string | Date): boolean {
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

	.filters {
		border-right: 1px solid #e5e5e5;
		padding-right: 1rem;
	}

	.filters h2 {
		font-size: 0.875rem;
		text-transform: uppercase;
		color: #666;
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
		border-radius: 6px;
		color: #333;
		text-decoration: none;
	}

	.filters a:hover {
		background: #f5f5f5;
	}

	.filters a.active {
		background: #2563eb;
		color: white;
	}

	.issues h1 {
		margin-bottom: 1.5rem;
	}

	.issue-grid {
		display: grid;
		gap: 1rem;
	}

	.issue-card {
		display: block;
		padding: 1rem;
		border: 1px solid #e5e5e5;
		border-radius: 8px;
		text-decoration: none;
		color: inherit;
		transition: box-shadow 0.2s;
	}

	.issue-card:hover {
		box-shadow: 0 4px 12px rgba(0, 0, 0, 0.1);
	}

	.card-header {
		display: flex;
		justify-content: space-between;
		align-items: center;
		margin-bottom: 0.5rem;
	}

	.error-class {
		font-weight: 600;
		font-family: monospace;
		color: #dc2626;
	}

	.badge.new {
		background: #ef4444;
		color: white;
		font-size: 0.75rem;
		font-weight: 700;
		padding: 0.125rem 0.5rem;
		border-radius: 4px;
	}

	.message {
		color: #666;
		margin-bottom: 0.75rem;
		line-height: 1.4;
	}

	.card-footer {
		display: flex;
		justify-content: space-between;
		font-size: 0.875rem;
		color: #999;
		margin-bottom: 0.5rem;
	}

	.meta {
		display: flex;
		justify-content: space-between;
		font-size: 0.75rem;
		color: #666;
	}

	.status {
		padding: 0.125rem 0.5rem;
		border-radius: 4px;
		text-transform: capitalize;
	}

	.status.open {
		background: #fef3c7;
		color: #92400e;
	}

	.status.resolved {
		background: #d1fae5;
		color: #065f46;
	}

	.status.ignored {
		background: #e5e5e5;
		color: #666;
	}

	.pagination {
		display: flex;
		justify-content: center;
		gap: 1rem;
		margin-top: 2rem;
	}

	.pagination a {
		padding: 0.5rem 1rem;
		background: #2563eb;
		color: white;
		border-radius: 6px;
		text-decoration: none;
	}

	.empty {
		text-align: center;
		color: #999;
		padding: 3rem;
	}
</style>