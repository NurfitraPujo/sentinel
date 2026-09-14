<script lang="ts">
	let { data } = $props();

	// The detail page's `formatDate` idiom (issues/[id]/+page.svelte): viewer-local, `en-US`.
	// `firstSeen`/`lastSeen` are the occurrence's event timestamp (§3 *Clocks*), rendered the way
	// `First seen:` and `Last seen:` are on the issue.
	function formatDate(date: string | Date | null): string {
		if (!date) return 'Unknown';
		return new Date(date).toLocaleString('en-US', {
			year: 'numeric',
			month: 'short',
			day: 'numeric',
			hour: '2-digit',
			minute: '2-digit',
			second: '2-digit',
		});
	}
</script>

<!--
	sentinel-releases-snooze §4.4, flows `R2`–`R6`. The project's releases as the loader hands them:
	`first_seen DESC` (decision 1 — never re-sorted here, and never by parsing the version), at most
	200 (decision 24, no paging), with a one-line note above the table when `truncated`. `[]` is a
	real state, shown with the empty-state copy — the loader never sends `null`. Nothing on a row
	links out (`R6`): the issues-introduced LIST is not built (§1). The five columns and the two copy
	strings are acceptance; the layout is the projects page's own style.
-->
<div class="releases-page">
	<header class="page-header">
		<div>
			<a href="/{data.org.slug}/projects" class="back-link">&larr; Back to Projects</a>
			<h1 class="page-title">Releases</h1>
			<p class="page-subtitle">{data.project.name} · {data.org.name}</p>
		</div>
	</header>

	{#if data.truncated}
		<p class="truncation-note" data-testid="releases-truncated">Showing the 200 most recent</p>
	{/if}

	{#if data.releases.length === 0}
		<p class="empty" data-testid="releases-empty">
			No releases yet — send <code>release_version</code> with your events and they appear here
		</p>
	{:else}
		<div class="table-card">
			<table class="release-table" data-testid="releases-table">
				<thead>
					<tr>
						<th>Version</th>
						<th>First seen</th>
						<th>Last seen</th>
						<th class="align-right">Occurrences</th>
						<th class="align-right">Issues introduced</th>
					</tr>
				</thead>
				<tbody>
					{#each data.releases as release (release.version)}
						<tr>
							<td class="cell-version">{release.version}</td>
							<td class="cell-muted">{formatDate(release.firstSeen)}</td>
							<td class="cell-muted">{formatDate(release.lastSeen)}</td>
							<td class="align-right">{Number(release.occurrenceCount).toLocaleString()}</td>
							<td class="align-right">{Number(release.issuesIntroduced).toLocaleString()}</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
</div>

<style>
	.releases-page {
		max-width: 960px;
		margin: 0 auto;
		padding: 2rem 1rem;
	}

	.page-header {
		display: flex;
		align-items: flex-start;
		justify-content: space-between;
		gap: 1rem;
		margin-bottom: 1.25rem;
	}

	.back-link {
		display: inline-block;
		color: var(--color-primary);
		text-decoration: none;
		font-size: 0.875rem;
		font-weight: 500;
		margin-bottom: 0.75rem;
	}

	.back-link:hover {
		text-decoration: underline;
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

	.truncation-note {
		font-size: 0.8125rem;
		color: var(--text-subtle);
		margin-bottom: 0.75rem;
	}

	.empty {
		font-size: 0.8125rem;
		color: var(--text-muted);
	}

	.empty code {
		font-family: var(--font-mono);
		color: var(--text-primary);
	}

	.table-card {
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		background-color: var(--bg-surface);
		overflow-x: auto;
	}

	.release-table {
		width: 100%;
		border-collapse: collapse;
		font-size: 0.875rem;
	}

	.release-table thead {
		background-color: var(--bg-root);
	}

	.release-table th {
		padding: 0.75rem 1rem;
		text-align: left;
		font-size: 0.6875rem;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
		white-space: nowrap;
	}

	.release-table td {
		padding: 0.875rem 1rem;
		white-space: nowrap;
		border-top: 1px solid var(--border-color-muted);
	}

	.release-table tbody tr:hover {
		background-color: var(--bg-surface-hover);
	}

	.align-right {
		text-align: right;
	}

	.cell-version {
		font-family: var(--font-mono);
		color: var(--text-primary);
	}

	.cell-muted {
		color: var(--text-muted);
	}
</style>
