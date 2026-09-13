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

	// sentinel-project-environments §4.1 — the shape `data.environments` carries. `displayName` is
	// the registry's label; the raw `name` is what the query string and `issues.environment` use,
	// and the two are not interchangeable (§4.2 makes `name` immutable and `displayName` editable).
	interface EnvironmentOption {
		id: string;
		name: string;
		displayName: string;
	}

	let environments = $derived((data.environments ?? []) as EnvironmentOption[]);

	// §4.4: the control renders only inside a selected project. `project=all` returns `environments:
	// []` from the loader, and an environment name is only meaningful inside a project — two
	// projects may both report `production` and they are not the same environment, so a
	// cross-project environment filter here would silently mean "any project's production". Flow
	// `L3`.
	let projectSelected = $derived(data.filters.project !== 'all');
	let environmentActive = $derived(data.filters.environment !== 'all');

	// The label to put in front of a reader: the registry's, when the registry knows this name.
	// Under decisions 8+9 it often does not — an environment lives in the data long before anybody
	// declares it — and the raw name is then the honest label, not a fallback worth flagging.
	let activeEnvironmentLabel = $derived(
		environments.find((env) => env.name === data.filters.environment)?.displayName ??
			data.filters.environment
	);

	/**
	 * The filter strip is a row of ANCHORS on the query string (not a form, not client state), and
	 * every link has to carry the filters it is not itself changing — otherwise clicking "Resolved"
	 * silently drops the environment the reader chose. Flow `L7` is the same requirement on the
	 * pagination links.
	 *
	 * `environment` is deliberately DROPPED when the project changes: environments are per project
	 * (§4.4), so carrying `production` from one project to another would either filter by a name
	 * that project has never reported or, on `project=all`, mean "any project's production".
	 *
	 * Decision 55: the all-projects anchor drops it UNCONDITIONALLY, not only when the project is
	 * changing. On `project=all` the control is not rendered (`data.environments` is `[]`), so that
	 * anchor is the only way out of the filter — it cannot be the link that preserves it. The loader
	 * coerces the echo to `all` there as well, which is what normally keeps this branch unreachable.
	 *
	 * `sort` (sentinel-affected-users §4.1) rides on every anchor the way `environment` does, with
	 * one difference: it SURVIVES a project change (decision 17) — an order is not project-scoped
	 * the way an environment name is, so only `environment` resets. It is OMITTED from the query
	 * string when it is `last_seen`: the default is the absence, so a bookmark made before this
	 * feature keeps meaning what it said, and a page loaded as `/?sort=banana` — which the loader
	 * echoes as `last_seen` — builds anchors that carry nothing (flow `L7`). The sort anchors do not
	 * carry `page`: a change of order starts again from page 1.
	 */
	function href(params: {
		status?: string;
		project?: string;
		environment?: string;
		sort?: IssueSortValue;
		page?: number;
	}) {
		const status = params.status ?? data.filters.status;
		const project = params.project ?? data.filters.project;
		const environment =
			project === 'all' ||
			(params.project !== undefined && params.project !== data.filters.project)
				? 'all'
				: (params.environment ?? data.filters.environment);
		const sort = params.sort ?? data.filters.sort;

		const q = new URLSearchParams();
		q.set('status', status);
		q.set('project', project);
		if (environment && environment !== 'all') q.set('environment', environment);
		if (sort && sort !== 'last_seen') q.set('sort', sort);
		if (params.page && params.page > 1) q.set('page', String(params.page));
		return `?${q.toString()}`;
	}

	// sentinel-affected-users §4.1, flows `L2`/`L3`. The Sort group is a LIST of entries, not two
	// hard-coded anchors, because `sentinel-occurrence-histogram` adds more later and that must be
	// a data change here. The template never validates `sort`: the loader coerces an unknown value
	// to `last_seen` and echoes it on `filters.sort`, which is all the active state reads.
	type IssueSortValue = typeof data.filters.sort;
	const sortOptions: { value: IssueSortValue; label: string }[] = [
		{ value: 'last_seen', label: 'Last seen' },
		{ value: 'affected_users', label: 'Affected users' },
	];
</script>

<!-- Decision 16's zero shape, stated once: the dash replaces the number only, on its own span. -->
{#snippet affectedFigure(count: number, kind: 'user' | 'tenant')}{#if count === 0}<span class="zero" title="no {kind} id reported in the last {data.retentionDays} days">—</span>{:else}{Number(count).toLocaleString()}{/if}{/snippet}

<div class="dashboard">
	<aside class="filters">
		<h2>Projects</h2>
		<ul>
			<li>
				<a href={href({ project: 'all' })}
				   class:active={data.filters.project === 'all'}>
					All Projects
				</a>
			</li>
			{#each data.projects as project}
				<li>
					<a href={href({ project: project.id })}
					   class:active={data.filters.project === project.id}>
						{project.name}
					</a>
				</li>
			{/each}
		</ul>

		<h2>Status</h2>
		<ul>
			<li>
				<a href={href({ status: 'all' })}
				   class:active={data.filters.status === 'all'}>All</a>
			</li>
			<li>
				<a href={href({ status: 'open' })}
				   class:active={data.filters.status === 'open'}>Open</a>
			</li>
			<li>
				<a href={href({ status: 'resolved' })}
				   class:active={data.filters.status === 'resolved'}>Resolved</a>
			</li>
			<li>
				<a href={href({ status: 'ignored' })}
				   class:active={data.filters.status === 'ignored'}>Ignored</a>
			</li>
		</ul>

		<!--
			sentinel-affected-users §4.1, flows `L2`-`L7`. After Status, before Environment: status and
			order are the two things a triager sets first, and the environment group below is absent
			on `project=all`, so this keeps the top of the aside stable across that change. Anchors on
			the query string in the same idiom as the groups above — the order is the server's.
		-->
		<h2>Sort</h2>
		<ul data-testid="sort-control">
			{#each sortOptions as option (option.value)}
				<li>
					<a href={href({ sort: option.value })}
					   class:active={data.filters.sort === option.value}
					   data-testid="sort-{option.value}">
						{option.label}
					</a>
				</li>
			{/each}
		</ul>

		<!--
			sentinel-project-environments tasks 3 and 4, flows `L1`-`L3`, `L6`, `L7`. The control is
			anchors in this same strip, and it is absent entirely on `project=all` — not disabled, not
			empty. See `href()` for why the project links drop `environment`.
		-->
		{#if projectSelected}
			<h2>Environment</h2>
			{#if environments.length > 0}
				<ul data-testid="environment-filter">
					<li>
						<a href={href({ environment: 'all' })}
						   class:active={data.filters.environment === 'all'}>All</a>
					</li>
					{#each environments as environment (environment.name)}
						<li>
							<a href={href({ environment: environment.name })}
							   class:active={data.filters.environment === environment.name}>
								{environment.displayName}
							</a>
						</li>
					{/each}
				</ul>
			{:else}
				<!--
					Flow `L6`. `data.environments` is `[]`, and under decision 8 that is the state of
					EVERY project on the day this ships — no registry row is ever created by traffic,
					only by somebody declaring one in project settings. So this is a designed
					explanation, not an edge case and not a bare empty select: it says what is missing,
					that nothing is broken, and where to go. The link needs the org slug the root layout
					already puts on `data`; without it the sentence still stands on its own.
				-->
				<div class="environment-empty" data-testid="environment-registry-empty">
					<p>No environments have been set up for this project yet.</p>
					<p class="hint">
						Errors are still arriving and being stored with the environment they were reported
						from — nothing is lost. An environment has to be declared before it can be
						filtered on.
					</p>
					{#if data.orgSlug}
						<a class="settings-link" href="/{data.orgSlug}/projects/{data.filters.project}/settings">
							Set up environments in project settings
						</a>
					{/if}
				</div>
			{/if}
		{/if}
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
						<!--
							Flow `L4`, and it is the INVERSE of what the first draft of this brief asked
							for. Under decision 7 an issue belongs to exactly one environment
							(`UNIQUE (project_id, environment, fingerprint)`), so `count`, `firstSeen` and
							`lastSeen` on this row ARE that environment's — they are not a total that has
							been narrowed and they need no hedge. Saying so out loud is only honest with a
							filter active, because that is when a reader might otherwise take the number
							for a project-wide one.
						-->
						<span class="impact">
							<span class="count">
								{Number(issue.count).toLocaleString()} occurrences{#if environmentActive}
									<span class="count-scope">in {activeEnvironmentLabel}</span>
								{/if}
							</span>
							<!--
								sentinel-affected-users §4.1, flow `L1` (decisions 12, 15, 16, 21). Two more
								impact numbers beside the occurrence count, ONE window suffix after the
								tenant figure, the window being page data (`retentionDays`) and never a
								literal 30. The scope note above applies to these equally — an `issues` row
								IS one environment (§3) — and is kept on the occurrence span (flow `L8`, the
								second of the two allowed compositions) so nothing here moves. A `0` means
								"no usable id inside the window", not "nobody was affected", so the dash
								replaces ONLY the number and says why on its `title`.
							-->
							·
							<span class="affected" data-testid="issue-affected">
								<span data-testid="issue-affected-users">{@render affectedFigure(issue.affectedUserCount, 'user')} users</span>
								·
								<span data-testid="issue-affected-tenants">{@render affectedFigure(issue.affectedTenantCount, 'tenant')} tenants</span>
								in the last {data.retentionDays} days
							</span>
						</span>
						<span class="last-seen">Last seen: {formatDate(issue.lastSeen)}</span>
					</div>
					<div class="meta">
						<span class="project">{issue.projectName}</span>
						<!--
							Flow `L8`. `issues.environment` is a column on the row the loader already
							selects — no extra query, no aggregate — and the chip is on every row whether
							or not a filter is active, so a reader scanning an unfiltered list can still
							see which environment each issue is in.
						-->
						<span class="environment-chip" data-testid="issue-environment">{issue.environment}</span>
						<span class="status {issue.status}">{issue.status}</span>
					</div>
				</a>
			{:else}
				<!-- Flow `L5`: the empty state names THIS environment rather than saying nothing found. -->
				{#if environmentActive}
					<p class="empty">No issues found in {activeEnvironmentLabel}.</p>
				{:else}
					<p class="empty">No issues found.</p>
				{/if}
			{/each}
		</div>

		{#if data.pagination.totalPages > 1}
			<nav class="pagination">
				{#if data.pagination.page > 1}
					<!-- Flow `L7`: the environment survives a page change; `href()` carries it. -->
					<a href={href({ page: data.pagination.page - 1 })}>
						Previous
					</a>
				{/if}
				<span>Page {data.pagination.page} of {data.pagination.totalPages}</span>
				{#if data.pagination.page < data.pagination.totalPages}
					<a href={href({ page: data.pagination.page + 1 })}>
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
		gap: 0.5rem;
		font-size: 0.75rem;
		color: var(--text-subtle);
	}

	.meta .project {
		flex: 1;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.environment-chip {
		font-family: var(--font-mono);
		font-size: 0.6875rem;
		padding: 0.125rem 0.5rem;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		color: var(--text-muted);
		white-space: nowrap;
	}

	.count-scope {
		margin-left: 0.375rem;
		color: var(--text-muted);
	}

	.impact {
		display: inline-flex;
		flex-wrap: wrap;
		column-gap: 0.375rem;
	}

	.affected .zero {
		color: var(--text-muted);
		cursor: help;
	}

	.environment-empty {
		margin-bottom: 1.5rem;
		padding: 0.75rem;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		font-size: 0.8125rem;
		color: var(--text-muted);
		line-height: 1.45;
	}

	.environment-empty p {
		margin: 0 0 0.5rem;
	}

	.environment-empty .hint {
		color: var(--text-subtle);
		font-size: 0.75rem;
	}

	.environment-empty .settings-link {
		display: inline;
		padding: 0;
		color: var(--color-primary);
		font-size: 0.75rem;
		text-decoration: underline;
	}

	.environment-empty .settings-link:hover {
		background: none;
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