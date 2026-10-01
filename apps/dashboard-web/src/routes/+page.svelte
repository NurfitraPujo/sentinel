<script lang="ts">
	import type { PageData } from './$types';
	import { goto } from '$app/navigation';
	import { untrack } from 'svelte';
	import TrendSparkline from '$lib/components/issues/TrendSparkline.svelte';

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

	// sentinel-issue-trend §4.1, flow `L9`, decisions 12 and 22 — ONE 24-hour rule for both badges,
	// against `Date.now()` as the card always did, client-side from page data.
	//
	// `NEW` fires on `firstSeen` (decision 12; it used to read `lastSeen`), so the badged cards and
	// the `new=24h` set are the same set — `first_seen >= now − 24h` is the filter's predicate.
	//
	// `REGRESSED` (decision 22) exists because `first_seen` is never reset on regression: an issue
	// resolved months ago that re-fired an hour ago — the card the dispatcher's alert sends the
	// triager to — would lose the `NEW` badge it wore under the old rule and get nothing in its
	// place. `regressionStatus` and `lastRegressedAt` are two existing `issues` columns the loader's
	// projection now carries; nothing is computed from anything else. Both may show on one card.
	function isRecent(timestamp: string | Date | null): boolean {
		if (!timestamp) return false;
		const diff = Date.now() - new Date(timestamp).getTime();
		return diff < 24 * 60 * 60 * 1000;
	}

	function isNew(firstSeen: string | Date | null): boolean {
		return isRecent(firstSeen);
	}

	function isRegressed(issue: { regressionStatus: string; lastRegressedAt: string | Date | null }): boolean {
		return issue.regressionStatus === 'regressed' && isRecent(issue.lastRegressedAt);
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
	 *
	 * `new` (sentinel-issue-trend §4.1, decision 6) rides exactly the way `sort` does: it SURVIVES a
	 * project change (age is not project-scoped — the same decision-17 reasoning), it is OMITTED
	 * when it is `all` (the default is the absence, so `/?new=banana` — echoed by the loader as
	 * `all` — builds anchors that carry nothing, flow `L7`), and the `new` anchors do not carry
	 * `page`: a change of filter starts again from page 1. Unlike `sort` it IS a filter, so the
	 * loader narrows `pagination.total` with it; nothing here needs to know that.
	 */
	function href(params: {
		status?: string;
		project?: string;
		environment?: string;
		sort?: IssueSortValue;
		new?: IssueNewValue;
		claim?: string;
		q?: string;
		volume?: string;
		seen?: string;
		seen_from?: string;
		seen_to?: string;
		page?: number;
		limit?: number;
	}) {
		const status = params.status ?? data.filters.status;
		const project = params.project ?? data.filters.project;
		const environment =
			project === 'all' ||
			(params.project !== undefined && params.project !== data.filters.project)
				? 'all'
				: (params.environment ?? data.filters.environment);
		const sort = params.sort ?? data.filters.sort;
		const newFilter = params.new ?? data.filters.new;
		const claim = params.claim ?? data.filters.claim ?? 'all';
		const searchQueryParam = params.q !== undefined ? params.q : (data.filters.q ?? '');
		const limit = params.limit ?? data.pagination.limit;
		// fix-sentinel-issues-dashboard D3: `volume`, `seen` and the custom range are server filters
		// like `new` — carried across every anchor, omitted at their default, and (because `page` is
		// only set by the pagination anchors) reset to page 1 when changed. A preset wins over a
		// custom range, so the range is only emitted beside `seen=all`.
		const volume = params.volume ?? data.filters.volume;
		const seen = params.seen ?? data.filters.seen;
		const seenFrom = params.seen_from ?? data.filters.seen_from;
		const seenTo = params.seen_to ?? data.filters.seen_to;

		const q = new URLSearchParams();
		q.set('status', status);
		q.set('project', project);
		if (environment && environment !== 'all') q.set('environment', environment);
		if (sort && sort !== 'last_seen') q.set('sort', sort);
		if (newFilter && newFilter !== 'all') q.set('new', newFilter);
		if (claim && claim !== 'all') q.set('claim', claim);
		if (volume && volume !== 'all') q.set('volume', volume);
		if (seen && seen !== 'all') q.set('seen', seen);
		if (seen === 'all' && seenFrom) q.set('seen_from', seenFrom);
		if (seen === 'all' && seenTo) q.set('seen_to', seenTo);
		if (searchQueryParam && searchQueryParam.trim()) q.set('q', searchQueryParam.trim());
		if (limit && limit !== 20) q.set('limit', String(limit));
		if (params.page && params.page > 1) q.set('page', String(params.page));
		return `?${q.toString()}`;
	}

	// sentinel-affected-users §4.1, flows `L2`/`L3`. The Sort group is a LIST of entries, not two
	// hard-coded anchors, because `sentinel-occurrence-histogram` adds more later and that must be
	// a data change here. The template never validates `sort`: the loader coerces an unknown value
	// to `last_seen` and echoes it on `filters.sort`, which is all the active state reads.
	//
	// sentinel-issue-trend §4.1, flows `L2`/`L3`, decision 5 — the extension that comment reserved:
	// two more entries after Affected users. `href()` already carries `sort`; nothing else changes.
	type IssueSortValue = typeof data.filters.sort;
	const sortOptions: { value: IssueSortValue; label: string }[] = [
		{ value: 'last_seen', label: 'Last seen' },
		{ value: 'affected_users', label: 'Affected users' },
		{ value: 'count', label: 'Occurrences' },
		{ value: 'first_seen', label: 'First seen' },
	];

	// sentinel-issue-trend §4.1, flows `L4`–`L7`, decision 6. Same idiom as `sortOptions`: a data
	// list, the template never validates the value — the loader coerces an unknown `new` to `all`
	// and echoes it on `filters.new`, which is all the active state and `href()` read.
	type IssueNewValue = typeof data.filters.new;
	const newOptions: { value: IssueNewValue; label: string }[] = [
		{ value: 'all', label: 'All' },
		{ value: '24h', label: 'New in the last 24 hours' },
	];

	// D2: the status group is the vocabulary `issues.check_status` allows; counts are `data.facets.status`.
	const statusOptions = [
		{ value: 'all', label: 'All' },
		{ value: 'unresolved', label: 'Unresolved' },
		{ value: 'resolved', label: 'Resolved' },
		{ value: 'ignored', label: 'Ignored' },
	] as const;

	let searchQuery = $state(untrack(() => data.filters.q ?? ''));
	$effect(() => {
		searchQuery = data.filters.q ?? '';
	});

	function handleSearchCommit(term: string) {
		const targetHref = href({ q: term.trim(), page: 1 });
		goto(targetHref, { keepFocus: true, noScroll: true });
	}

	// Compact/Expanded is a view, not a filter, so it stays client-side.
	let displayMode = $state<'expanded' | 'compact'>('expanded');

	// Date range (D3). Every value here is DERIVED from `data.filters` — the server applies the
	// filter, the URL carries it, and the trigger's label reads the echo so a reload shows it. The
	// only local state is the popover's open flag and the two custom inputs' draft values.
	const seenPresets: { key: '1h' | '24h' | '7d' | '14d' | '30d'; label: string; button: string }[] = [
		{ key: '1h', label: 'Last 1 Hour', button: '1 Hour' },
		{ key: '24h', label: 'Last 24 Hours', button: '24 Hours' },
		{ key: '7d', label: 'Last 7 Days', button: '7 Days' },
		{ key: '14d', label: 'Last 14 Days', button: '14 Days' },
		{ key: '30d', label: 'Last 30 Days', button: '30 Days' },
	];

	// An ISO instant -> the value a `datetime-local` input takes (local time, minute precision).
	function toLocalInput(iso: string): string {
		if (!iso) return '';
		const d = new Date(iso);
		if (Number.isNaN(d.getTime())) return '';
		const pad = (n: number) => String(n).padStart(2, '0');
		return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
	}

	let dateModalOpen = $state(false);
	let customDateStart = $state(untrack(() => toLocalInput(data.filters.seen_from)));
	let customDateEnd = $state(untrack(() => toLocalInput(data.filters.seen_to)));
	$effect(() => {
		customDateStart = toLocalInput(data.filters.seen_from);
		customDateEnd = toLocalInput(data.filters.seen_to);
	});

	let dateRangeActive = $derived(
		data.filters.seen !== 'all' || Boolean(data.filters.seen_from || data.filters.seen_to)
	);
	let selectedDateLabel = $derived.by(() => {
		const preset = seenPresets.find((p) => p.key === data.filters.seen);
		if (preset) return preset.label;
		if (data.filters.seen_from || data.filters.seen_to) {
			const s = data.filters.seen_from ? toLocalInput(data.filters.seen_from).replace('T', ' ') : 'Any';
			const e = data.filters.seen_to ? toLocalInput(data.filters.seen_to).replace('T', ' ') : 'Now';
			return `${s} → ${e}`;
		}
		return 'Date Range';
	});

	function toggleDateModal() {
		dateModalOpen = !dateModalOpen;
	}

	// *Cancel* closes WITHOUT navigating and drops the draft.
	function cancelDateModal() {
		customDateStart = toLocalInput(data.filters.seen_from);
		customDateEnd = toLocalInput(data.filters.seen_to);
		dateModalOpen = false;
	}

	function pickDatePreset(key: string) {
		dateModalOpen = false;
		goto(href({ seen: key, seen_from: '', seen_to: '' }));
	}

	function clearDateRange() {
		dateModalOpen = false;
		goto(href({ seen: 'all', seen_from: '', seen_to: '' }));
	}

	function applyCustomDateRange() {
		const from = customDateStart ? new Date(customDateStart) : null;
		const to = customDateEnd ? new Date(customDateEnd) : null;
		const iso = (d: Date | null) => (d && !Number.isNaN(d.getTime()) ? d.toISOString() : '');
		dateModalOpen = false;
		goto(href({ seen: 'all', seen_from: iso(from), seen_to: iso(to) }));
	}

	// "Reset all filters": keeps `project`, `sort` and `limit`, drops every other filter.
	let anyFilterActive = $derived(
		data.filters.status !== 'all' ||
			data.filters.environment !== 'all' ||
			data.filters.new !== 'all' ||
			data.filters.claim !== 'all' ||
			Boolean(data.filters.q) ||
			data.filters.volume !== 'all' ||
			dateRangeActive
	);
	let resetHref = $derived(
		href({
			status: 'all',
			environment: 'all',
			new: 'all',
			claim: 'all',
			q: '',
			volume: 'all',
			seen: 'all',
			seen_from: '',
			seen_to: '',
		})
	);

	// Claim facet: 'all', 'unclaimed', 'claimed-me', 'claimed-agent', or the exact `assigned_to` value.

	// True database claimant detection helper
	function getIssueClaimInfo(issue: (typeof data.issues)[number]) {
		const assignedTo = issue.assignedTo;
		const assigneeType = issue.assigneeType;

		if (!assignedTo) {
			return { type: 'unclaimed' as const, label: 'Unclaimed', assignee: null, assigneeType: null };
		}

		// D7: the SAME rule as the server's `claimed-me`: a user-typed claim whose `assigned_to` is
		// the current user's id. An email is not the user.
		const isMe = assigneeType === 'user' && Boolean(data.currentUser) && assignedTo === data.currentUser?.id;

		if (assigneeType === 'agent') {
			const agentName = data.agentsById?.[assignedTo]?.name || assignedTo;
			return {
				type: 'claimed-agent' as const,
				label: `🤖 ${agentName}`,
				assignee: assignedTo,
				assigneeType: 'agent' as const,
			};
		}

		const matchedUser = data.usersById?.[assignedTo];
		const displayIdentifier = matchedUser ? (matchedUser.name || matchedUser.email) : assignedTo;

		return {
			type: isMe ? ('claimed-me' as const) : ('claimed-other' as const),
			label: isMe ? 'Claimed by Me' : `Claimed: ${displayIdentifier}`,
			assignee: assignedTo,
			assigneeType: 'user' as const,
		};
	}

	// Derived distinct claimants and counts from all issues in database across organization
	let distinctClaimants = $derived.by(() => {
		const list: Array<{ id: string; label: string; count: number; isAgent: boolean }> = [];
		const addedIds = new Set<string>();

		// 1. All users in the organizations (shows 0 if no claimed issues)
		if (data.orgUsers && data.orgUsers.length > 0) {
			for (const u of data.orgUsers) {
				// D7: counted by the EXACT `assigned_to` value — the user's id only. A legacy row
				// keyed by the user's email is its own row below, and filters by that exact value.
				const count = data.facets.claim.byClaimant[u.id] ?? 0;
				list.push({
					id: u.id,
					label: u.name || u.email,
					count,
					isAgent: false,
				});
				addedIds.add(u.id);
			}
		}

		// 2. All agents in the organizations (shows 0 if no claimed issues)
		if (data.orgAgents && data.orgAgents.length > 0) {
			for (const a of data.orgAgents) {
				const count = data.facets.claim.byClaimant[a.id] ?? 0;
				list.push({
					id: a.id,
					label: a.name,
					count,
					isAgent: true,
				});
				addedIds.add(a.id);
			}
		}

		// 3. Any additional claimants in database (e.g. claimants not in orgUsers/orgAgents list)
		{
			for (const [assigneeId, count] of Object.entries(data.facets.claim.byClaimant)) {
				if (!addedIds.has(assigneeId)) {
					const isAgent = data.facets.claim.claimantTypes[assigneeId] === 'agent';
					const matchedUser = data.usersById?.[assigneeId];
					const matchedAgent = data.agentsById?.[assigneeId];
					const label = isAgent
						? (matchedAgent ? matchedAgent.name : assigneeId)
						: (matchedUser ? (matchedUser.name || matchedUser.email) : assigneeId);
					list.push({
						id: assigneeId,
						label,
						count,
						isAgent,
					});
					addedIds.add(assigneeId);
				}
			}
		}

		return list.sort((a, b) => b.count - a.count || a.label.localeCompare(b.label));
	});
</script>

<!-- Decision 16's zero shape, stated once: the dash replaces the number only, on its own span. -->
{#snippet affectedFigure(count: number, kind: 'user' | 'tenant')}{#if count === 0}<span class="zero" title="no {kind} id reported in the last {data.retentionDays} days">—</span>{:else}{Number(count).toLocaleString()}{/if}{/snippet}

<div class="dashboard">
	<aside class="filters">
		<h2>Projects</h2>
		<ul>
			<li>
				<a href={href({ project: 'all' })}
				   aria-label="All Projects"
				   class:active={data.filters.project === 'all'}>
					<span>All Projects</span>
					<strong class="count-badge">{data.facets.project.all}</strong>
				</a>
			</li>
			{#each data.projects as project}
				<li>
					<a href={href({ project: project.id })}
				   aria-label={project.name}
					   class:active={data.filters.project === project.id}>
						<span>{project.name}</span>
						<strong class="count-badge">{data.facets.project.byProject[project.id] ?? 0}</strong>
					</a>
				</li>
			{/each}
		</ul>

		<h2>Status</h2>
		<ul>
			{#each statusOptions as option (option.value)}
				<li>
					<a href={href({ status: option.value })}
					   aria-label={option.label}
					   class:active={data.filters.status === option.value}>
						<span>{option.label}</span>
						<strong class="count-badge">{data.facets.status[option.value]}</strong>
					</a>
				</li>
			{/each}
		</ul>

		<!--
			sentinel-issue-trend §4.1, flows `L4`–`L7` (decision 6). Its OWN group after Status and
			before Sort, not a third row of Status: `new` is a filter on `first_seen`, orthogonal to
			`status` — a reader can want "new AND resolved" — and a row inside the Status group would
			read as a fourth status. Anchors on the query string like every group here; `href()`
			carries `new` across paging, status, sort and project changes and drops it on `all`.
		-->
		<h2>Age</h2>
		<ul data-testid="new-control">
			{#each newOptions as option (option.value)}
				<li>
					<a href={href({ new: option.value })}
					   aria-label={option.label}
					   class:active={data.filters.new === option.value}
					   data-testid="new-{option.value}">
						<span>{option.label}</span>
						<strong class="count-badge">{option.value === '24h' ? data.facets.age.new24h : data.facets.age.all}</strong>
					</a>
				</li>
			{/each}
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
						   aria-label="All Environments"
						   class:active={data.filters.environment === 'all'}>
							<span>All Environments</span>
							<strong class="count-badge">{data.facets.environment.all}</strong>
						</a>
					</li>
					{#each environments as environment (environment.name)}
						<li>
							<a href={href({ environment: environment.name })}
							   aria-label={environment.displayName}
							   class:active={data.filters.environment === environment.name}>
								<span>{environment.displayName}</span>
								<strong class="count-badge">{data.facets.environment.byEnvironment[environment.name] ?? 0}</strong>
							</a>
						</li>
					{/each}
				</ul>
			{:else}
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

		<!-- Claim & Triage Status Facet -->
		<h2>Claim & Triage Status</h2>
		<ul class="facet-list">
			<li>
				<a
					href={href({ claim: 'all', page: 1 })}
					class="facet-btn"
					class:active={data.filters.claim === 'all'}
				>
					<span>All Issues</span>
					<strong class="count-badge">{data.facets.claim.total}</strong>
				</a>
			</li>
			<li>
				<a
					href={href({ claim: 'unclaimed', page: 1 })}
					class="facet-btn"
					class:active={data.filters.claim === 'unclaimed'}
				>
					<span class="facet-label-with-icon"><span class="icon-open">⚪</span> Unclaimed</span>
					<strong class="count-badge">{data.facets.claim.unclaimed}</strong>
				</a>
			</li>
			<li>
				<a
					href={href({ claim: 'claimed-me', page: 1 })}
					class="facet-btn"
					class:active={data.filters.claim === 'claimed-me'}
				>
					<span class="facet-label-with-icon"><span class="icon-primary">🔵</span> Claimed by Me</span>
					<strong class="count-badge">{data.facets.claim.claimedMe}</strong>
				</a>
			</li>
			<li>
				<a
					href={href({ claim: 'claimed-agent', page: 1 })}
					class="facet-btn"
					class:active={data.filters.claim === 'claimed-agent'}
				>
					<span class="facet-label-with-icon"><span class="icon-agent">🤖</span> Claimed by Agent</span>
					<strong class="count-badge">{data.facets.claim.claimedAgent}</strong>
				</a>
			</li>
		</ul>

		<!-- Specific Claimers (derived across all issues in database and organization users) -->
		{#if distinctClaimants.length > 0}
			<h2>Claimed By</h2>
			<ul class="facet-list">
				{#each distinctClaimants as claimant (claimant.id)}
					<li>
						<a
							href={href({ claim: claimant.id, page: 1 })}
							class="facet-btn"
							class:active={data.filters.claim === claimant.id}
						>
							<span>{claimant.isAgent ? '🤖 ' : ''}{claimant.label}</span>
							<strong class="count-badge">{claimant.count}</strong>
						</a>
					</li>
				{/each}
			</ul>
		{/if}
	</aside>

	<main class="issues">
		<div class="issues-top-bar">
			<div class="top-title-row">
				<div class="title-with-badge">
					<h1>Issues</h1>
				</div>
				<div class="display-toggle-group">
					<span class="display-label">Display:</span>
					<button
						type="button"
						class="btn-display"
						class:active={displayMode === 'compact'}
						onclick={() => (displayMode = 'compact')}
					>
						Compact
					</button>
					<button
						type="button"
						class="btn-display"
						class:active={displayMode === 'expanded'}
						onclick={() => (displayMode = 'expanded')}
					>
						Expanded
					</button>
				</div>
			</div>
			
			<div class="search-and-chips">
				<div class="live-search-container">
					<svg class="search-icon" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
						<circle cx="11" cy="11" r="8"></circle>
						<line x1="21" y1="21" x2="16.65" y2="16.65"></line>
					</svg>
					<input
						type="text"
						class="live-search-input"
						placeholder="Search issues by error class, message, or fingerprint... (e.g., timeout, SyntaxError)"
						bind:value={searchQuery}
						onkeydown={(e) => {
							if (e.key === 'Enter') {
								e.preventDefault();
								handleSearchCommit(searchQuery);
							}
						}}
						aria-label="Filter issues"
					/>
					{#if searchQuery}
						<button
							type="button"
							class="btn-clear-search"
							onclick={() => {
								searchQuery = '';
								handleSearchCommit('');
							}}
							aria-label="Clear search"
						>✕</button>
					{/if}
				</div>

				<div class="quick-chips-and-date-row">
					<div class="quick-chips-row">
						<span class="quick-filter-label">Quick Filters:</span>
						<a
							href={href({ volume: 'all' })}
							class="chip-btn"
							class:active={data.filters.volume === 'all'}
						>
							All
						</a>
						<a
							href={href({ volume: 'high' })}
							class="chip-btn"
							class:active={data.filters.volume === 'high'}
							data-testid="volume-high"
						>
							100+ events
						</a>
					</div>

					<!-- Date range: a server filter on `last_seen` (`seen`, `seen_from`, `seen_to`). -->
					<div class="date-picker-wrapper">
						<button
							type="button"
							class="btn-date-trigger"
							onclick={toggleDateModal}
							aria-expanded={dateModalOpen}
						>
							<span class="date-icon">📅</span>
							<span class="date-text">{selectedDateLabel}</span>
							<span class="date-caret">▼</span>
						</button>

						{#if dateModalOpen}
							<div class="date-picker-popover" role="dialog" aria-modal="true" tabindex="-1">
								<div class="popover-header">
									<strong class="popover-title">Select Date & Time Range</strong>
									<button type="button" class="btn-popover-close" onclick={cancelDateModal}>✕</button>
								</div>

								<div class="presets-section">
									<span class="presets-label">Presets</span>
									<div class="presets-grid">
										{#each seenPresets as preset (preset.key)}
											<button type="button" class="btn-preset" class:active={data.filters.seen === preset.key} onclick={() => pickDatePreset(preset.key)}>{preset.button}</button>
										{/each}
									</div>
								</div>

								<div class="custom-range-inputs">
									<div class="range-field">
										<label for="date-range-start">Start Date & Time (Local)</label>
										<input
											id="date-range-start"
											type="datetime-local"
											class="date-input"
											bind:value={customDateStart}
										/>
									</div>
									<div class="range-field">
										<label for="date-range-end">End Date & Time (Local)</label>
										<input
											id="date-range-end"
											type="datetime-local"
											class="date-input"
											bind:value={customDateEnd}
										/>
									</div>
								</div>

								<div class="popover-actions">
									{#if dateRangeActive}
										<button type="button" class="btn-cancel" onclick={clearDateRange}>Clear</button>
									{/if}
									<button type="button" class="btn-cancel" onclick={cancelDateModal}>Cancel</button>
									<button type="button" class="btn-apply" onclick={applyCustomDateRange}>Apply Range</button>
								</div>
							</div>
						{/if}
					</div>
				</div>

				<div class="sub-filter-status-row">
					<span class="filter-count-readout">Showing {data.pagination.total.toLocaleString()} matching issue{data.pagination.total === 1 ? '' : 's'}</span>
					{#if anyFilterActive}
						<a class="btn-reset-filters" href={resetHref} data-testid="reset-filters">Reset all filters</a>
					{/if}
				</div>
			</div>
		</div>

		<div class="issue-grid" class:compact={displayMode === 'compact'}>
			{#each data.issues as issue}
				{@const claimInfo = getIssueClaimInfo(issue)}
				<a href="/issues/{issue.id}" class="issue-card" class:compact={displayMode === 'compact'}>
					<div class="card-header">
						<span class="error-class">{issue.errorClass}</span>
						<div class="card-header-right">
							<!-- Flow `L9`: `NEW` on `firstSeen`, `REGRESSED` after it when both apply (see `isRecent`). -->
							<span class="badges">
								{#if isNew(issue.firstSeen)}
									<span class="badge new" data-testid="badge-new">NEW</span>
								{/if}
								{#if isRegressed(issue)}
									<span class="badge regressed" data-testid="badge-regressed">REGRESSED</span>
								{/if}
							</span>
							<span class="badge-pill status-pill {issue.status}">{issue.status.toUpperCase()}</span>
							<span
								class="badge-pill claim-badge"
								class:unclaimed={claimInfo.type === 'unclaimed'}
								class:claimed-me={claimInfo.type === 'claimed-me'}
								class:claimed-agent={claimInfo.type === 'claimed-agent'}
							>
								{claimInfo.label}
							</span>
						</div>
					</div>
					<p class="message">{issue.message.slice(0, 150)}{issue.message.length > 150 ? '...' : ''}</p>
					<!--
						sentinel-issue-trend §4.1, flows `L1`/`L10` (decisions 7, 10, 13). Between the
						message and the footer, full card width: the shape reads as a row of its own
						rather than crowding the footer's three figures, and the footer's `{count}
						occurrences` stays the lifetime number beside it — the sparkline's own total is
						its `aria-label`, and it names its window. `trend24h` is dense from the loader (§3),
						so the drawing renders on every card; all zeros is a flat baseline, not an absence.
					-->
					<TrendSparkline values={issue.trend24h} />
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
						</div>
				</a>
			{:else}
				<!--
					Flow `L5` (environments): the empty state names THIS environment rather than saying
					nothing found. sentinel-issue-trend flow `L8`: with `new=24h` active it says WHICH
					filter came up empty — `No issues found.` under a filter looks like a broken page —
					composed with the same environment clause.
				-->
				{#if data.filters.new === '24h'}
					{#if environmentActive}
						<p class="empty">No new issues in the last 24 hours in {activeEnvironmentLabel}.</p>
					{:else}
						<p class="empty">No new issues in the last 24 hours.</p>
					{/if}
				{:else if environmentActive}
					<p class="empty">No issues found in {activeEnvironmentLabel}.</p>
				{:else}
					<p class="empty">No issues found.</p>
				{/if}
			{/each}
		</div>

		{#if data.pagination.total > 0}
			<nav class="pagination" aria-label="Pagination Navigation">
				<div class="pagination-info">
					Showing <span class="highlight">{Math.min((data.pagination.page - 1) * data.pagination.limit + 1, data.pagination.total)}–{Math.min(data.pagination.page * data.pagination.limit, data.pagination.total)}</span> of <span class="highlight">{data.pagination.total.toLocaleString()}</span> issues
				</div>

				<div class="pagination-actions">
					{#if data.pagination.totalPages > 1}
						<div class="pagination-stepper">
							{#if data.pagination.page > 1}
								<a href={href({ page: data.pagination.page - 1 })} class="page-nav" data-testid="pagination-prev">
									Previous
								</a>
							{:else}
								<span class="page-nav disabled" aria-disabled="true">Previous</span>
							{/if}

							<span class="page-current">Page {data.pagination.page} of {data.pagination.totalPages}</span>

							{#if data.pagination.page < data.pagination.totalPages}
								<a href={href({ page: data.pagination.page + 1 })} class="page-nav" data-testid="pagination-next">
									Next
								</a>
							{:else}
								<span class="page-nav disabled" aria-disabled="true">Next</span>
							{/if}
						</div>
					{/if}

					<div class="per-page-selector">
						<span class="per-page-label">Per page:</span>
						{#each [20, 50, 100] as limitOption}
							<a
								href={href({ limit: limitOption, page: 1 })}
								class="limit-option"
								class:active={data.pagination.limit === limitOption}
								data-testid="limit-{limitOption}"
							>
								{limitOption}
							</a>
						{/each}
					</div>
				</div>
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
		background: var(--color-primary-subtle);
		color: var(--color-primary);
		border: 1px solid var(--border-color-focus);
		font-weight: 600;
	}

	.issues h1 {
		font-size: 1.5rem;
		font-weight: 600;
		letter-spacing: -0.01em;
		margin-bottom: 1.5rem;
		color: var(--text-primary);
	}

	.issue-grid {
		display: grid;
		gap: 0.75rem;
	}

	.issue-card {
		display: block;
		padding: 1.125rem;
		background-color: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		text-decoration: none;
		color: inherit;
		transition: border-color var(--transition-fast), background-color var(--transition-fast), box-shadow var(--transition-fast);
	}

	.issue-card:hover {
		border-color: var(--border-color-focus);
		background-color: var(--bg-surface-hover);
		box-shadow: 0 4px 12px oklch(0 0 0 / 0.2);
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
		letter-spacing: -0.01em;
	}

	.badges {
		display: inline-flex;
		gap: 0.375rem;
	}

	.badge {
		font-family: var(--font-mono);
		font-size: 0.6875rem;
		font-weight: 700;
		padding: 0.125rem 0.375rem;
		border-radius: var(--radius-sm);
		letter-spacing: 0.05em;
	}

	.badge.new {
		background: var(--severity-critical-bg);
		color: var(--severity-critical-text);
		border: 1px solid var(--severity-critical-border);
	}

	/* Same family as `NEW` (decision 22); the open-status trio so the two read as different words. */
	.badge.regressed {
		background: var(--status-open-bg);
		color: var(--status-open-text);
		border: 1px solid var(--status-open-border);
	}

	.message {
		color: var(--text-secondary);
		margin-bottom: 0.75rem;
		line-height: 1.45;
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

	.pagination {
		display: flex;
		flex-wrap: wrap;
		justify-content: space-between;
		align-items: center;
		gap: 1.25rem;
		margin-top: 2.5rem;
		padding: 1rem 1.25rem;
		background: var(--bg-card);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		font-size: 0.875rem;
		color: var(--text-muted);
	}

	.pagination-info {
		font-size: 0.8125rem;
		color: var(--text-muted);
	}

	.pagination-info .highlight {
		color: var(--text-primary);
		font-weight: 600;
	}

	.pagination-actions {
		display: flex;
		align-items: center;
		flex-wrap: wrap;
		gap: 1.5rem;
	}

	.pagination-stepper {
		display: flex;
		align-items: center;
		gap: 0.75rem;
	}

	.page-nav {
		padding: 0.375rem 0.875rem;
		min-height: 36px;
		display: inline-flex;
		align-items: center;
		background: var(--bg-card);
		border: 1px solid var(--border-color);
		color: var(--text-primary);
		border-radius: var(--radius-sm);
		text-decoration: none;
		font-weight: 500;
		font-size: 0.8125rem;
		transition: all 0.15s ease;
	}

	.page-nav:hover:not(.disabled) {
		background: var(--bg-card-hover);
		border-color: var(--border-color-hover);
	}

	.page-nav.disabled {
		opacity: 0.4;
		cursor: not-allowed;
		border-color: var(--border-color);
		color: var(--text-muted);
	}

	.page-current {
		font-size: 0.8125rem;
		color: var(--text-muted);
		padding: 0 0.25rem;
	}

	.per-page-selector {
		display: flex;
		align-items: center;
		gap: 0.375rem;
		font-size: 0.8125rem;
	}

	.per-page-label {
		color: var(--text-muted);
		margin-right: 0.25rem;
	}

	.limit-option {
		padding: 0.25rem 0.625rem;
		min-height: 28px;
		display: inline-flex;
		align-items: center;
		border-radius: var(--radius-sm);
		text-decoration: none;
		color: var(--text-muted);
		font-weight: 500;
		font-size: 0.8125rem;
		border: 1px solid transparent;
		transition: all 0.15s ease;
	}

	.limit-option:hover {
		color: var(--text-primary);
		background: var(--bg-card-hover);
	}

	.limit-option.active {
		background: var(--color-primary);
		color: var(--text-primary);
		font-weight: 600;
	}

	.empty {
		text-align: center;
		color: var(--text-subtle);
		padding: 3rem;
	}

	/* Search & Quick Chips Bar */
	.issues-top-bar {
		display: flex;
		flex-direction: column;
		gap: 1rem;
		margin-bottom: 1.5rem;
	}

	.search-and-chips {
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
	}

	.live-search-container {
		position: relative;
		display: flex;
		align-items: center;
		width: 100%;
	}

	.search-icon {
		position: absolute;
		left: 0.875rem;
		color: var(--text-subtle);
		pointer-events: none;
	}

	.live-search-input {
		width: 100%;
		padding: 0.625rem 2.25rem 0.625rem 2.5rem;
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		color: var(--text-primary);
		font-size: 0.875rem;
		transition: border-color var(--transition-fast), box-shadow var(--transition-fast);
	}

	.live-search-input:focus {
		outline: none;
		border-color: var(--border-color-focus);
		box-shadow: 0 0 0 1px var(--color-primary);
	}

	.btn-clear-search {
		position: absolute;
		right: 0.75rem;
		background: none;
		border: none;
		color: var(--text-muted);
		cursor: pointer;
		font-size: 0.875rem;
		padding: 0.25rem;
	}

	.btn-clear-search:hover {
		color: var(--text-primary);
	}

	.quick-chips-row {
		display: flex;
		align-items: center;
		flex-wrap: wrap;
		gap: 0.5rem;
	}

	.chip-btn {
		padding: 0.25rem 0.75rem;
		text-decoration: none;
		border-radius: var(--radius-full);
		font-size: 0.75rem;
		font-weight: 500;
		background: var(--bg-surface);
		color: var(--text-muted);
		border: 1px solid var(--border-color);
		cursor: pointer;
		transition: all var(--transition-fast);
	}

	.chip-btn:hover {
		background: var(--bg-surface-hover);
		color: var(--text-primary);
	}

	.chip-btn.active {
		background: var(--color-primary-subtle);
		color: var(--color-primary);
		border-color: var(--border-color-focus);
		font-weight: 600;
	}

	.match-readout {
		font-size: 0.75rem;
		font-family: var(--font-mono);
		color: var(--text-subtle);
		margin-left: 0.5rem;
	}

	/* Top Bar Layout Enhancements */
	.top-title-row {
		display: flex;
		justify-content: space-between;
		align-items: center;
		flex-wrap: wrap;
		gap: 1rem;
	}

	.title-with-badge {
		display: flex;
		align-items: center;
		gap: 1rem;
	}

	.top-title-row h1 {
		margin-bottom: 0;
	}

	.display-toggle-group {
		display: flex;
		align-items: center;
		gap: 0.35rem;
	}

	.display-label {
		font-size: 0.75rem;
		font-family: var(--font-mono);
		color: var(--text-subtle);
		margin-right: 0.25rem;
	}

	.btn-display {
		padding: 0.2rem 0.55rem;
		border-radius: var(--radius-sm);
		border: 1px solid var(--border-color);
		background: var(--bg-surface);
		color: var(--text-muted);
		font-size: 0.75rem;
		font-family: var(--font-mono);
		cursor: pointer;
		transition: all var(--transition-fast);
	}

	.btn-display:hover {
		background: var(--bg-surface-hover);
		color: var(--text-primary);
	}

	.btn-display.active {
		background: var(--color-primary-subtle);
		color: var(--color-primary);
		border-color: var(--border-color-focus);
		font-weight: 600;
	}

	.quick-chips-and-date-row {
		display: flex;
		justify-content: space-between;
		align-items: center;
		flex-wrap: wrap;
		gap: 0.75rem;
	}

	.quick-filter-label {
		font-size: 0.7rem;
		font-family: var(--font-mono);
		text-transform: uppercase;
		color: var(--text-subtle);
		letter-spacing: 0.05em;
		margin-right: 0.25rem;
	}

	/* Immersive Date Picker Popover */
	.date-picker-wrapper {
		position: relative;
	}

	.btn-date-trigger {
		display: inline-flex;
		align-items: center;
		gap: 0.5rem;
		padding: 0.35rem 0.75rem;
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		font-family: var(--font-mono);
		font-size: 0.75rem;
		cursor: pointer;
		transition: all var(--transition-fast);
	}

	.btn-date-trigger:hover {
		border-color: var(--border-color-focus);
		background: var(--bg-surface-hover);
	}

	.date-caret {
		font-size: 0.625rem;
		color: var(--text-subtle);
	}

	.date-picker-popover {
		position: absolute;
		right: 0;
		top: calc(100% + 8px);
		width: 360px;
		background: var(--bg-surface-elevated, var(--bg-surface));
		border: 1px solid var(--border-color-focus);
		border-radius: var(--radius-md);
		box-shadow: 0 16px 36px oklch(0 0 0 / 0.5);
		padding: 1.25rem;
		z-index: 50;
		animation: popoverFadeIn 150ms var(--ease-out-expo, ease-out);
	}

	@keyframes popoverFadeIn {
		from { opacity: 0; transform: translateY(-4px); }
		to { opacity: 1; transform: translateY(0); }
	}

	.popover-header {
		display: flex;
		justify-content: space-between;
		align-items: center;
		margin-bottom: 1rem;
		padding-bottom: 0.5rem;
		border-bottom: 1px solid var(--border-color);
	}

	.popover-title {
		font-size: 0.8125rem;
		font-family: var(--font-mono);
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-primary);
	}

	.btn-popover-close {
		background: none;
		border: none;
		color: var(--text-subtle);
		cursor: pointer;
		font-size: 1rem;
		line-height: 1;
	}

	.btn-popover-close:hover {
		color: var(--text-primary);
	}

	.presets-section {
		margin-bottom: 1rem;
	}

	.presets-label {
		display: block;
		font-size: 0.7rem;
		font-family: var(--font-mono);
		color: var(--text-subtle);
		text-transform: uppercase;
		margin-bottom: 0.4rem;
	}

	.presets-grid {
		display: grid;
		grid-template-columns: repeat(3, 1fr);
		gap: 0.35rem;
	}

	.btn-preset {
		padding: 0.3rem 0.4rem;
		border-radius: var(--radius-sm);
		border: 1px solid var(--border-color);
		background: var(--bg-surface);
		color: var(--text-muted);
		font-size: 0.75rem;
		font-family: var(--font-mono);
		cursor: pointer;
		transition: all var(--transition-fast);
	}

	.btn-preset:hover {
		background: var(--bg-surface-hover);
		color: var(--text-primary);
	}

	.btn-preset.active {
		background: var(--color-primary-subtle);
		color: var(--color-primary);
		border-color: var(--border-color-focus);
		font-weight: 600;
	}

	.custom-range-inputs {
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
		margin-bottom: 1.25rem;
	}

	.range-field label {
		display: block;
		font-size: 0.7rem;
		font-family: var(--font-mono);
		color: var(--text-subtle);
		text-transform: uppercase;
		margin-bottom: 0.25rem;
	}

	.date-input {
		width: 100%;
		padding: 0.4rem 0.6rem;
		background: var(--bg-root);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		font-size: 0.75rem;
		font-family: var(--font-mono);
		outline: none;
	}

	.date-input:focus {
		border-color: var(--color-primary);
	}

	.popover-actions {
		display: flex;
		justify-content: flex-end;
		gap: 0.5rem;
	}

	.btn-cancel {
		padding: 0.35rem 0.75rem;
		border-radius: var(--radius-sm);
		border: 1px solid var(--border-color);
		background: var(--bg-surface);
		color: var(--text-muted);
		font-size: 0.75rem;
		cursor: pointer;
	}

	.btn-cancel:hover {
		background: var(--bg-surface-hover);
		color: var(--text-primary);
	}

	.btn-apply {
		padding: 0.35rem 0.75rem;
		border-radius: var(--radius-sm);
		border: none;
		background: var(--color-primary);
		color: var(--text-primary);
		font-size: 0.75rem;
		font-weight: 600;
		cursor: pointer;
		transition: background var(--transition-fast);
	}

	.btn-apply:hover {
		background: var(--color-primary-hover);
	}

	.sub-filter-status-row {
		display: flex;
		justify-content: space-between;
		align-items: center;
		padding-top: 0.25rem;
	}

	.filter-count-readout {
		font-size: 0.8125rem;
		color: var(--text-muted);
		font-family: var(--font-mono);
	}

	.btn-reset-filters {
		background: none;
		border: none;
		color: var(--color-primary);
		font-size: 0.75rem;
		font-family: var(--font-mono);
		text-decoration: underline;
		cursor: pointer;
		padding: 0;
	}

	.btn-reset-filters:hover {
		color: var(--color-primary-hover);
	}

	/* Sidebar Facet Buttons & Count Badges */
	.filters a,
	.facet-btn {
		display: flex;
		justify-content: space-between;
		align-items: center;
		width: 100%;
		padding: 0.45rem 0.75rem;
		border-radius: var(--radius-sm);
		color: var(--text-muted);
		text-decoration: none;
		font-size: 0.875rem;
		background: none;
		border: 1px solid transparent;
		cursor: pointer;
		font-family: inherit;
		text-align: left;
		transition: background var(--transition-fast), color var(--transition-fast), border-color var(--transition-fast);
	}

	.facet-list {
		list-style: none;
		margin-bottom: 1.5rem;
	}

	.facet-list li {
		margin-bottom: 0.25rem;
	}

	.facet-btn:hover {
		background: var(--bg-surface-hover);
		color: var(--text-primary);
	}

	.facet-btn.active {
		background: var(--color-primary-subtle);
		color: var(--color-primary);
		border-color: var(--border-color-focus);
		font-weight: 600;
	}

	.count-badge {
		font-size: 0.75rem;
		font-family: var(--font-mono);
		color: var(--text-subtle);
		background: var(--bg-root);
		padding: 0.1rem 0.4rem;
		border-radius: var(--radius-sm);
		border: 1px solid var(--border-color);
		font-weight: 600;
	}

	.facet-btn.active .count-badge {
		color: var(--color-primary);
		border-color: var(--border-color-focus);
	}

	.facet-label-with-icon {
		display: inline-flex;
		align-items: center;
		gap: 0.35rem;
	}

	.icon-open { color: var(--status-open-text); }
	.icon-primary { color: var(--color-primary); }
	.icon-agent { color: var(--status-resolved-text); }

	.card-header-right {
		display: inline-flex;
		align-items: center;
		gap: 0.5rem;
	}

	/* Card Status & Claim Badges */
	.badge-pill {
		font-family: var(--font-mono);
		font-size: 0.6875rem;
		font-weight: 700;
		padding: 0.125rem 0.45rem;
		border-radius: var(--radius-sm);
		letter-spacing: 0.05em;
	}

	.badge-pill.status-pill.unresolved {
		background: var(--status-open-bg);
		color: var(--status-open-text);
		border: 1px solid var(--status-open-border);
	}

	.badge-pill.status-pill.resolved {
		background: var(--status-resolved-bg);
		color: var(--status-resolved-text);
		border: 1px solid var(--status-resolved-border);
	}

	.badge-pill.status-pill.ignored {
		background: var(--status-ignored-bg);
		color: var(--status-ignored-text);
		border: 1px solid var(--status-ignored-border);
	}

	.badge-pill.claim-badge.unclaimed {
		background: var(--bg-root);
		border: 1px dashed var(--border-color);
		color: var(--text-subtle);
	}

	.badge-pill.claim-badge.claimed-me {
		background: var(--color-primary-subtle);
		color: var(--color-primary);
		border: 1px solid var(--border-color-focus);
	}

	.badge-pill.claim-badge.claimed-agent {
		background: var(--status-resolved-bg);
		color: var(--status-resolved-text);
		border: 1px solid var(--status-resolved-border);
	}

	/* Compact View Layout */
	.issue-grid.compact {
		gap: 0.4rem;
	}

	.issue-card.compact {
		padding: 0.65rem 0.85rem;
	}

	.issue-card.compact .message {
		display: none;
	}

	.issue-card.compact .card-header {
		margin-bottom: 0.25rem;
	}
</style>