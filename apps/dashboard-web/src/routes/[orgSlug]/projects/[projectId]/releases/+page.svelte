<script lang="ts">
	import { SvelteMap, SvelteSet } from 'svelte/reactivity';

	let { data } = $props();

	// sentinel-sourcemaps §4.2 — one row of the list the expand control fetches, camelCase as the
	// route sends it. No `storageKey`, no `sourcesContent`, no map byte (contract §1): the response
	// does not carry them and nothing here asks for them.
	interface SourcemapRow {
		id: string;
		minifiedPath: string;
		sizeBytes: number;
		sha256: string;
		uploadedAt: string;
	}

	type SourcemapsState =
		| { status: 'loading' }
		| { status: 'loaded'; files: SourcemapRow[] }
		| { status: 'error' };

	// §4.6, flows `R2`/`R3`: which rows are open, and what each version's first expand fetched. The
	// two are separate on purpose — collapsing removes the version from `expandedVersions` and leaves
	// its entry in `sourcemapsByVersion`, so re-expanding renders the kept data and never refetches
	// (fe.md task 2). `SvelteSet`/`SvelteMap`, not `$state(new Set())`: `$state` proxies plain
	// objects and arrays only, so `.add`/`.delete` on a raw `Set` re-renders nothing.
	const expandedVersions = new SvelteSet<string>();
	const sourcemapsByVersion = new SvelteMap<string, SourcemapsState>();

	async function toggleSourcemaps(version: string) {
		if (expandedVersions.has(version)) {
			expandedVersions.delete(version);
			return;
		}
		expandedVersions.add(version);
		if (sourcemapsByVersion.has(version)) return;

		sourcemapsByVersion.set(version, { status: 'loading' });
		try {
			// §4.2 — `orgId` is `data.org.id` (the loader's, never the slug); the version goes through
			// `encodeURIComponent` because it is whatever string CI sent (§4.1, decision 25).
			const response = await fetch(
				`/api/organizations/${data.org.id}/projects/${data.project.id}/sourcemaps?release_version=${encodeURIComponent(version)}`
			);
			if (!response.ok) {
				throw new Error(`HTTP ${response.status}`);
			}
			const body = (await response.json()) as { files: SourcemapRow[] };
			sourcemapsByVersion.set(version, { status: 'loaded', files: body.files ?? [] });
		} catch {
			sourcemapsByVersion.set(version, { status: 'error' });
		}
	}

	// `sizeBytes` in a human unit, one decimal (fe.md task 2): KB below a mebibyte, MB from there.
	function formatSize(bytes: number): string {
		if (bytes >= 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
		return `${(bytes / 1024).toFixed(1)} KB`;
	}

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

	sentinel-sourcemaps §4.6, flows `R1`–`R3`: a sixth column, Maps, with `sourcemapCount` on every
	row (`0` renders `0`, never a dash — a release with no maps is a fact, fe.md task 1) and a control
	in that cell that opens a second `<tr>` under the row: the §4.2 list (path · size · uploaded), or
	*No sourcemaps*, or the error line. Map-only versions are not rows (decision 16).
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
						<th class="align-right">Maps</th>
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
							<td class="align-right cell-maps">
								<span data-testid="release-sourcemap-count">{Number(release.sourcemapCount).toLocaleString()}</span>
								<button
									type="button"
									class="maps-toggle"
									data-testid="release-sourcemaps-toggle"
									aria-expanded={expandedVersions.has(release.version)}
									onclick={() => toggleSourcemaps(release.version)}
								>
									{expandedVersions.has(release.version) ? 'Hide' : 'Show'}
								</button>
							</td>
						</tr>
						{#if expandedVersions.has(release.version)}
							{@const maps = sourcemapsByVersion.get(release.version)}
							<tr class="sourcemaps-row" data-testid="release-sourcemaps">
								<td colspan="6">
									{#if !maps || maps.status === 'loading'}
										<p class="sourcemaps-note" data-testid="release-sourcemaps-loading">Loading sourcemaps…</p>
									{:else if maps.status === 'error'}
										<p class="sourcemaps-note sourcemaps-error" data-testid="release-sourcemaps-error">
											Could not load sourcemaps for this release
										</p>
									{:else if maps.files.length === 0}
										<p class="sourcemaps-note" data-testid="release-sourcemaps-empty">No sourcemaps</p>
									{:else}
										<table class="sourcemaps-table" data-testid="release-sourcemaps-list">
											<thead>
												<tr>
													<th>Path</th>
													<th class="align-right">Size</th>
													<th>Uploaded</th>
												</tr>
											</thead>
											<tbody>
												{#each maps.files as file (file.id)}
													<tr>
														<td class="cell-path">{file.minifiedPath}</td>
														<td class="align-right cell-muted">{formatSize(file.sizeBytes)}</td>
														<td class="cell-muted">{formatDate(file.uploadedAt)}</td>
													</tr>
												{/each}
											</tbody>
										</table>
									{/if}
								</td>
							</tr>
						{/if}
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

	/* sentinel-sourcemaps §4.6 — the Maps cell's control and the expanded region under the row. */
	.cell-maps {
		white-space: nowrap;
	}

	.maps-toggle {
		margin-left: 0.5rem;
		padding: 0.125rem 0.5rem;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		background: var(--bg-root);
		color: var(--text-muted);
		font-size: 0.6875rem;
		font-family: var(--font-mono);
		cursor: pointer;
		transition: background 0.15s ease, color 0.15s ease;
	}

	.maps-toggle:hover {
		background: var(--bg-surface-hover);
		color: var(--text-primary);
	}

	.release-table tbody tr.sourcemaps-row:hover {
		background-color: transparent;
	}

	.sourcemaps-row td {
		padding: 0.5rem 1rem 0.875rem 2rem;
		white-space: normal;
		background-color: var(--bg-root);
	}

	.sourcemaps-note {
		font-size: 0.8125rem;
		color: var(--text-muted);
	}

	.sourcemaps-error {
		color: var(--severity-critical-text);
	}

	.sourcemaps-table {
		width: 100%;
		border-collapse: collapse;
		font-size: 0.8125rem;
	}

	.sourcemaps-table th {
		padding: 0.375rem 0.75rem;
		font-size: 0.625rem;
		background: none;
	}

	.sourcemaps-table td {
		padding: 0.375rem 0.75rem;
		border-top: 1px solid var(--border-color-muted);
		background: none;
	}

	.cell-path {
		font-family: var(--font-mono);
		color: var(--text-primary);
		white-space: normal;
		word-break: break-all;
	}
</style>
