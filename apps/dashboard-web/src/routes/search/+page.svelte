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

	// §4.1's `Environment` and §4.3a's `UnionEnvironment` in one shape: the union publishes the raw
	// `name` only, so `displayName` is optional and the name is the label when it is absent.
	interface EnvironmentOption {
		name: string;
		displayName?: string;
	}

	// Contract §4.5 change 1 puts an `environments` array on this page's `data` — §4.1's registry
	// when a project is chosen, §4.3a's guarded cross-project union when one is not. The loader that
	// builds it is `+page.server.ts`, which is BE's under contract §6a and does not carry the field
	// yet, so it is read through a widened view of `data` rather than derived here: a list this page
	// assembled for itself would be an unguarded one, and §4.3a's union is a guard, not a
	// convenience. Until BE lands it the select degrades to "All Environments" alone and the form
	// still submits (`S3`).
	let environmentOptions = $derived(
		(data as typeof data & { environments?: EnvironmentOption[] }).environments ?? []
	);

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
					<!--
						sentinel-project-environments task 7, contract §4.5 change 1, flows `S1`-`S3`.
						This select used to hold three hard-coded option literals, so an organisation
						reporting from any other name could never select it — while the ingest edge
						accepted the name and stored the rows. The options now come from the server:
						contract §4.1's registry for a chosen project, and §4.3a's guarded cross-project
						union when no project is chosen. A union entry carries only `name` (two projects'
						same-named environments are two rows with two labels, so §4.3a publishes no
						label at all), which is why the label falls back to the name here.

						`""` still means "every environment", unchanged — the empty registry case simply
						leaves that as the only option and the form still submits (`S3`).
					-->
					<select name="environment">
						<option value="">All Environments</option>
						{#each environmentOptions as environment (environment.name)}
							<option value={environment.name} selected={data.filters.environment === environment.name}>
								{environment.displayName ?? environment.name}
							</option>
						{/each}
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
				<button type="submit" class="btn-search">Search</button>
				<a href="/search" class="clear-btn">Clear</a>
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
		grid-template-columns: 280px 1fr;
		gap: 2rem;
		min-height: calc(100vh - 100px);
	}

	.search-header {
		grid-column: 1 / -1;
		border-bottom: 1px solid var(--border-color);
		padding-bottom: 0.75rem;
		margin-bottom: 0.5rem;
	}

	.search-header h1 {
		font-size: 1.25rem;
		font-weight: 600;
		color: var(--text-primary);
		margin-bottom: 0.25rem;
	}

	.subtitle {
		color: var(--text-muted);
		font-size: 0.8125rem;
	}

	.search-filters {
		border-right: 1px solid var(--border-color);
		padding-right: 1.25rem;
	}

	.filter-section {
		margin-bottom: 1.25rem;
	}

	.filter-section h3 {
		font-size: 0.6875rem;
		font-family: var(--font-mono);
		text-transform: uppercase;
		color: var(--text-subtle);
		margin-bottom: 0.625rem;
		letter-spacing: 0.05em;
	}

	.search-filters label {
		display: block;
		margin-bottom: 0.75rem;
	}

	.search-filters label span {
		display: block;
		font-size: 0.75rem;
		color: var(--text-muted);
		margin-bottom: 0.25rem;
	}

	.search-filters input,
	.search-filters select {
		width: 100%;
		padding: 0.45rem 0.625rem;
		background-color: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		font-size: 0.8125rem;
	}

	.filter-actions {
		display: flex;
		gap: 0.5rem;
		margin-top: 1rem;
	}

	.btn-search {
		flex: 1;
		padding: 0.5rem;
		background: var(--color-primary);
		color: var(--text-primary);
		border: none;
		border-radius: var(--radius-sm);
		font-size: 0.8125rem;
		font-weight: 600;
		cursor: pointer;
		transition: background-color var(--transition-fast);
	}

	.btn-search:hover {
		background: var(--color-primary-hover);
	}

	.clear-btn {
		padding: 0.5rem 0.75rem;
		background: var(--bg-surface);
		color: var(--text-muted);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		text-decoration: none;
		font-size: 0.8125rem;
		text-align: center;
		transition: background-color var(--transition-fast), color var(--transition-fast), border-color var(--transition-fast);
	}

	.clear-btn:hover {
		background: var(--bg-surface-hover);
		color: var(--text-primary);
		border-color: var(--border-color-focus);
	}

	.results-header {
		display: flex;
		align-items: baseline;
		gap: 0.5rem;
		margin-bottom: 1rem;
	}

	.results-count {
		font-weight: 600;
		font-size: 1rem;
		color: var(--text-primary);
	}

	.search-time {
		color: var(--text-subtle);
		font-size: 0.8125rem;
	}

	.results-list {
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
	}

	.result-card {
		display: block;
		padding: 1.125rem;
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		text-decoration: none;
		color: inherit;
		transition: border-color var(--transition-fast), background-color var(--transition-fast), box-shadow var(--transition-fast);
	}

	.result-card:hover {
		border-color: var(--border-color-focus);
		background: var(--bg-surface-hover);
		box-shadow: 0 4px 12px oklch(0 0 0 / 0.2);
	}

	.result-header {
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

	.status {
		font-size: 0.6875rem;
		font-family: var(--font-mono);
		padding: 0.125rem 0.5rem;
		border-radius: var(--radius-sm);
		text-transform: uppercase;
		font-weight: 600;
		letter-spacing: 0.05em;
	}

	.status.open { background: var(--status-open-bg); color: var(--status-open-text); }
	.status.resolved { background: var(--status-resolved-bg); color: var(--status-resolved-text); }
	.status.ignored { background: var(--status-ignored-bg); color: var(--status-ignored-text); }

	.message {
		color: var(--text-muted);
		margin-bottom: 0.75rem;
		line-height: 1.4;
		font-size: 0.875rem;
	}

	.trace-context {
		display: flex;
		flex-wrap: wrap;
		gap: 0.75rem;
		padding: 0.625rem;
		background: var(--bg-root);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		margin-bottom: 0.75rem;
	}

	.trace-item {
		display: flex;
		align-items: center;
		gap: 0.25rem;
		font-size: 0.75rem;
	}

	.trace-label {
		color: var(--text-subtle);
		font-weight: 500;
	}

	.trace-value {
		font-family: var(--font-mono);
		color: var(--text-muted);
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
		font-family: var(--font-mono);
		color: var(--text-subtle);
	}

	.project {
		color: var(--color-primary);
		font-weight: 500;
	}

	.no-results,
	.empty-state {
		text-align: center;
		color: var(--text-subtle);
		padding: 3rem;
		font-size: 0.875rem;
	}

	.hint {
		font-size: 0.75rem;
		margin-top: 0.5rem;
		color: var(--text-subtle);
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
		padding: 0.375rem 0.875rem;
		background: var(--color-primary);
		color: var(--text-primary);
		border-radius: var(--radius-sm);
		text-decoration: none;
		font-weight: 500;
	}
</style>