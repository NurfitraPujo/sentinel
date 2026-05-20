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

	function buildSearchUrl(params: Record<string, string | undefined>): string {
		const searchParams = new URLSearchParams();
		for (const [key, value] of Object.entries(params)) {
			if (value) {
				searchParams.set(key, value);
			}
		}
		searchParams.set('page', '1');
		return `/search?${searchParams.toString()}`;
	}
</script>

<div class="search-page">
	<header class="search-header">
		<h1>Advanced Error Search</h1>
		<p class="subtitle">Search across user_id, trace_id, span_id, request_id, and full-text message search</p>
	</header>

	<aside class="search-filters">
		<form method="GET" action="/search">
			<div class="filter-section">
				<h3>Full-Text Search</h3>
				
				<label>
					<span>Search Message</span>
					<input type="text" name="search" value={data.filters.search || ''} placeholder="e.g., connection refused" />
				</label>
			</div>

			<div class="filter-section">
				<h3>Trace Identifiers</h3>
				
				<label>
					<span>User ID</span>
					<input type="text" name="user_id" value={data.filters.userId || ''} placeholder="e.g., user-12345" />
				</label>

				<label>
					<span>Tenant ID</span>
					<input type="text" name="tenant_id" value={data.filters.tenantId || ''} placeholder="e.g., tenant-abc" />
				</label>

				<label>
					<span>Trace ID</span>
					<input type="text" name="trace_id" value={data.filters.traceId || ''} placeholder="e.g., 4bf92f3577b34da6a3ce929d0e0e4736" />
				</label>

				<label>
					<span>Span ID</span>
					<input type="text" name="span_id" value={data.filters.spanId || ''} placeholder="e.g., 00f067aa0ba902b7" />
				</label>

				<label>
					<span>Request ID</span>
					<input type="text" name="request_id" value={data.filters.requestId || ''} placeholder="e.g., req-xyz-789" />
				</label>
			</div>

			<div class="filter-section">
				<h3>Context Filters</h3>

				<label>
					<span>Project</span>
					<select name="project">
						<option value="">All Projects</option>
						{#each data.projects as project}
							<option value={project.id} selected={data.filters.projectId === project.id}>
								{project.name}
							</option>
						{/each}
					</select>
				</label>

				<label>
					<span>Environment</span>
					<select name="environment">
						<option value="">All Environments</option>
						<option value="production" selected={data.filters.environment === 'production'}>Production</option>
						<option value="staging" selected={data.filters.environment === 'staging'}>Staging</option>
						<option value="development" selected={data.filters.environment === 'development'}>Development</option>
					</select>
				</label>

				<label>
					<span>Platform</span>
					<select name="platform">
						<option value="">All Platforms</option>
						<option value="go" selected={data.filters.platform === 'go'}>Go</option>
						<option value="ruby" selected={data.filters.platform === 'ruby'}>Ruby/Rails</option>
						<option value="node" selected={data.filters.platform === 'node'}>Node.js</option>
						<option value="python" selected={data.filters.platform === 'python'}>Python</option>
					</select>
				</label>
			</div>

			<div class="filter-actions">
				<button type="submit">Search</button>
				<a href="/search" class="clear-btn">Clear Filters</a>
			</div>
		</form>
	</aside>

	<main class="search-results">
		{#if data.filters.search || data.filters.userId || data.filters.tenantId || data.filters.traceId || data.filters.spanId || data.filters.requestId}
			<div class="results-header">
				<span class="results-count">{data.pagination.total.toLocaleString()} results</span>
				{#if data.pagination.total > 0}
					<span class="search-time">in under 1 second</span>
				{/if}
			</div>

			<div class="results-list">
				{#each data.results as result}
					<a href="/issues/{result.issueId}" class="result-card">
						<div class="result-header">
							<span class="error-class">{result.errorClass}</span>
							<span class="status {result.status}">{result.status}</span>
						</div>
						<p class="message">{result.message.slice(0, 200)}{result.message.length > 200 ? '...' : ''}</p>
						
						<div class="trace-context">
							{#if result.matchingOccurrence.userId}
								<span class="trace-item">
									<span class="trace-label">User:</span>
									<span class="trace-value">{result.matchingOccurrence.userId}</span>
								</span>
							{/if}
							{#if result.matchingOccurrence.tenantId}
								<span class="trace-item">
									<span class="trace-label">Tenant:</span>
									<span class="trace-value">{result.matchingOccurrence.tenantId}</span>
								</span>
							{/if}
							{#if result.matchingOccurrence.traceId}
								<span class="trace-item">
									<span class="trace-label">Trace:</span>
									<span class="trace-value trace-id">{result.matchingOccurrence.traceId}</span>
								</span>
							{/if}
							{#if result.matchingOccurrence.spanId}
								<span class="trace-item">
									<span class="trace-label">Span:</span>
									<span class="trace-value">{result.matchingOccurrence.spanId}</span>
								</span>
							{/if}
							{#if result.matchingOccurrence.requestId}
								<span class="trace-item">
									<span class="trace-label">Request:</span>
									<span class="trace-value">{result.matchingOccurrence.requestId}</span>
								</span>
							{/if}
						</div>

						<div class="result-footer">
							<span class="project">{result.projectName}</span>
							<span class="count">{Number(result.count).toLocaleString()} occurrences</span>
							<span class="occurrence-date">{formatDate(result.matchingOccurrence.createdAt)}</span>
						</div>
					</a>
				{:else}
					<div class="no-results">
						<p>No matching errors found. Try adjusting your search criteria.</p>
					</div>
				{/each}
			</div>

			{#if data.pagination.totalPages > 1}
				<nav class="pagination">
					{#if data.pagination.page > 1}
						<a href={buildSearchUrl({ ...data.filters, page: String(data.pagination.page - 1) })}>
							Previous
						</a>
					{/if}
					<span>Page {data.pagination.page} of {data.pagination.totalPages}</span>
					{#if data.pagination.page < data.pagination.totalPages}
						<a href={buildSearchUrl({ ...data.filters, page: String(data.pagination.page + 1) })}>
							Next
						</a>
					{/if}
				</nav>
			{/if}
		{:else}
			<div class="empty-state">
				<p>Enter search criteria to find errors by trace identifiers, user ID, or tenant ID.</p>
				<p class="hint">Tip: You can search by any combination of fields. All fields support exact matching.</p>
			</div>
		{/if}
	</main>
</div>

<style>
	.search-page {
		display: grid;
		grid-template-columns: 300px 1fr;
		gap: 2rem;
		min-height: calc(100vh - 100px);
	}

	.search-header {
		grid-column: 1 / -1;
		border-bottom: 1px solid #e5e5e5;
		padding-bottom: 1rem;
		margin-bottom: 1rem;
	}

	.search-header h1 {
		font-size: 1.5rem;
		margin-bottom: 0.25rem;
	}

	.subtitle {
		color: #666;
		font-size: 0.875rem;
	}

	.search-filters {
		border-right: 1px solid #e5e5e5;
		padding-right: 1rem;
	}

	.filter-section {
		margin-bottom: 1.5rem;
	}

	.filter-section h3 {
		font-size: 0.75rem;
		text-transform: uppercase;
		color: #666;
		margin-bottom: 0.75rem;
		letter-spacing: 0.05em;
	}

	.search-filters label {
		display: block;
		margin-bottom: 0.75rem;
	}

	.search-filters label span {
		display: block;
		font-size: 0.75rem;
		color: #666;
		margin-bottom: 0.25rem;
	}

	.search-filters input,
	.search-filters select {
		width: 100%;
		padding: 0.5rem;
		border: 1px solid #e5e5e5;
		border-radius: 6px;
		font-size: 0.875rem;
	}

	.search-filters input:focus,
	.search-filters select:focus {
		outline: none;
		border-color: #2563eb;
		box-shadow: 0 0 0 3px rgba(37, 99, 235, 0.1);
	}

	.filter-actions {
		display: flex;
		gap: 0.5rem;
		margin-top: 1rem;
	}

	.filter-actions button {
		flex: 1;
		padding: 0.75rem;
		background: #2563eb;
		color: white;
		border: none;
		border-radius: 6px;
		font-weight: 500;
		cursor: pointer;
	}

	.filter-actions button:hover {
		background: #1d4ed8;
	}

	.clear-btn {
		padding: 0.75rem 1rem;
		background: #f5f5f5;
		color: #666;
		border-radius: 6px;
		text-decoration: none;
		font-size: 0.875rem;
	}

	.clear-btn:hover {
		background: #e5e5e5;
	}

	.results-header {
		display: flex;
		align-items: baseline;
		gap: 0.5rem;
		margin-bottom: 1rem;
	}

	.results-count {
		font-weight: 600;
		font-size: 1.125rem;
	}

	.search-time {
		color: #666;
		font-size: 0.875rem;
	}

	.results-list {
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
	}

	.result-card {
		display: block;
		padding: 1rem;
		border: 1px solid #e5e5e5;
		border-radius: 8px;
		text-decoration: none;
		color: inherit;
		transition: box-shadow 0.2s;
	}

	.result-card:hover {
		box-shadow: 0 4px 12px rgba(0, 0, 0, 0.1);
	}

	.result-header {
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

	.status {
		font-size: 0.75rem;
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

	.message {
		color: #666;
		margin-bottom: 0.75rem;
		line-height: 1.4;
		font-size: 0.875rem;
	}

	.trace-context {
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem;
		padding: 0.75rem;
		background: #f9fafb;
		border-radius: 6px;
		margin-bottom: 0.75rem;
	}

	.trace-item {
		display: flex;
		align-items: center;
		gap: 0.25rem;
		font-size: 0.75rem;
	}

	.trace-label {
		color: #666;
		font-weight: 500;
	}

	.trace-value {
		font-family: monospace;
		color: #374151;
	}

	.trace-id {
		font-size: 0.7rem;
		max-width: 150px;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.result-footer {
		display: flex;
		justify-content: space-between;
		font-size: 0.75rem;
		color: #999;
	}

	.project {
		color: #2563eb;
		font-weight: 500;
	}

	.no-results,
	.empty-state {
		text-align: center;
		color: #999;
		padding: 3rem;
	}

	.hint {
		font-size: 0.875rem;
		margin-top: 0.5rem;
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
</style>