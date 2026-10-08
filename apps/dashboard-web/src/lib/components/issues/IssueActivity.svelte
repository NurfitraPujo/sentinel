<script lang="ts">
	import { untrack } from 'svelte';
	import {
		absoluteTime,
		activityDetail,
		activityDetailIsMarkdown,
		activityVerb,
		actorLabel,
		timeAgo,
		type ActivityEntry,
		type ActivityPage,
	} from '$lib/issue-activity';
	import Markdown from '$lib/components/Markdown.svelte';

	// sentinel-issue-activity TASK-01: the issue's activity history (claims, status changes, comments, progress,
	// regressions, occurrence bursts) on the issue page. Collapsed by default and read ONLY when opened: the page's own
	// load runs no activity query (page.server.activity.test.ts holds its statement count). Pages come from
	// GET /api/issues/{issueId}/activity -- the issue page's own access rule -- newest first, with runs of
	// `occurrence_burst` already collapsed server-side; "Load older" follows `nextCursor`. All text is interpolated, never
	// raw HTML -- except comment excerpts and progress notes, which are user/agent-authored Markdown and render through the
	// shared `Markdown` component (marked + DOMPurify, the renderer comment threads use), never raw HTML of our own. They are
	// compact: headings are scaled down to the entry's text size, and a long one is clamped with "Show more".
	interface Props {
		issueId: string;
		/** Open (and load) on mount. It also opens when the URL's hash is #activity, so a link can point at it. */
		initiallyOpen?: boolean;
		pageSize?: number;
	}

	let { issueId, initiallyOpen = false, pageSize = 50 }: Props = $props();

	let open = $state(false);
	let entries = $state<ActivityEntry[]>([]);
	let nextCursor = $state<string | null>(null);
	let hasMore = $state(false);
	let loading = $state(false);
	let loaded = $state(false);
	let failure = $state<string | null>(null);
	let now = $state(Date.now());
	// Markdown details the reader has expanded, by entry id.
	let expanded = $state<Record<string, boolean>>({});

	// Clamped (with a "Show more" toggle) past this many lines or characters. Measured on the source, not the layout,
	// so it is the same in every browser (and in jsdom).
	const CLAMP_LINES = 4;
	const CLAMP_CHARS = 240;
	function isLong(md: string): boolean {
		return md.length > CLAMP_CHARS || md.split('\n').length > CLAMP_LINES;
	}

	async function loadPage(before: string | null) {
		loading = true;
		failure = null;
		try {
			const q = new URLSearchParams({ limit: String(pageSize) });
			if (before) q.set('before', before);
			const res = await fetch(`/api/issues/${encodeURIComponent(issueId)}/activity?${q}`);
			if (!res.ok) {
				failure = res.status === 404 ? 'This issue’s activity is not available.' : `Could not load activity (${res.status}).`;
				return;
			}
			const page = (await res.json()) as ActivityPage;
			entries = before ? [...entries, ...page.entries] : page.entries;
			nextCursor = page.nextCursor;
			hasMore = page.hasMore;
			loaded = true;
			now = Date.now();
		} catch {
			failure = 'Could not load activity.';
		} finally {
			loading = false;
		}
	}

	function ontoggle(event: Event) {
		open = (event.currentTarget as HTMLDetailsElement).open;
		if (open && !loaded && !loading) void loadPage(null);
	}

	// Once, in the browser (an effect never runs during SSR). `untrack`: it reads no state, so it never re-runs -- a
	// failed load must not re-trigger itself.
	$effect(() => {
		untrack(() => {
			if (initiallyOpen || location.hash === '#activity') {
				open = true;
				void loadPage(null);
			}
		});
	});

	function icon(entry: ActivityEntry): string {
		if (entry.eventType === 'occurrence_burst') return '📈';
		if (entry.eventType === 'regressed') return '↩';
		if (entry.actorType === 'agent') return '🤖';
		if (entry.actorType === 'system') return '⚙️';
		return '👤';
	}
</script>

<details class="issue-activity" id="activity" data-testid="issue-activity" {open} {ontoggle}>
	<summary class="section-title">Activity</summary>

	{#if open}
		{#if loading && !loaded}
			<p class="muted" data-testid="activity-loading">Loading activity…</p>
		{:else if failure && !loaded}
			<p class="error" role="alert" data-testid="activity-error">{failure}</p>
		{:else if loaded && entries.length === 0}
			<p class="muted" data-testid="activity-empty">No activity yet.</p>
		{:else}
			<ol class="entries">
				{#each entries as entry (entry.id)}
					{@const detail = activityDetail(entry)}
					<li class="entry" data-testid="activity-entry" data-event-type={entry.eventType}>
						<span class="icon" aria-hidden="true" title={entry.actorType}>{icon(entry)}</span>
						<div class="body">
							<div class="line">
								<span class="actor" data-testid="activity-actor">{actorLabel(entry.actor)}</span>
								<span class="verb">{activityVerb(entry)}</span>
								{#if entry.burst && entry.burst.rows > 1}
									<span class="when">
										<time datetime={entry.burst.firstAt} title={absoluteTime(entry.burst.firstAt)}>{timeAgo(entry.burst.firstAt, now)}</time>
										–
										<time datetime={entry.burst.lastAt} title={absoluteTime(entry.burst.lastAt)}>{timeAgo(entry.burst.lastAt, now)}</time>
									</span>
								{:else}
									<time class="when" datetime={entry.createdAt} title={absoluteTime(entry.createdAt)}>{timeAgo(entry.createdAt, now)}</time>
								{/if}
							</div>
							{#if detail && activityDetailIsMarkdown(entry)}
								{@const long = isLong(detail)}
								{@const isOpen = !!expanded[entry.id]}
								<div class="detail detail-md" class:clamped={long && !isOpen} data-testid="activity-detail">
									<Markdown source={detail} />
								</div>
								{#if long}
									<button
										type="button"
										class="more"
										data-testid="activity-detail-toggle"
										aria-expanded={isOpen}
										onclick={() => (expanded[entry.id] = !isOpen)}>{isOpen ? 'Show less' : 'Show more'}</button
									>
								{/if}
							{:else if detail}
								<div class="detail" data-testid="activity-detail">{detail}</div>
							{/if}
						</div>
					</li>
				{/each}
			</ol>
			{#if failure}
				<p class="error" role="alert" data-testid="activity-error">{failure}</p>
			{/if}
			{#if hasMore}
				<button type="button" class="load-older" data-testid="activity-load-older" disabled={loading} onclick={() => loadPage(nextCursor)}>
					{loading ? 'Loading…' : 'Load older'}
				</button>
			{/if}
		{/if}
	{/if}
</details>

<style>
	.issue-activity {
		background: var(--bg-surface, #1e293b);
		border: 1px solid var(--border-color, #334155);
		border-radius: var(--radius-md, 6px);
		padding: 1rem;
		margin-bottom: 1.5rem;
		color: var(--text-primary, #f8fafc);
	}

	summary.section-title {
		font-size: 1.125rem;
		font-weight: 600;
		cursor: pointer;
	}

	.issue-activity[open] summary.section-title {
		margin-bottom: 1rem;
	}

	.muted {
		color: var(--text-muted);
		font-size: 0.8125rem;
	}

	.error {
		color: var(--color-danger, #f87171);
		font-size: 0.8125rem;
	}

	.entries {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
	}

	.entry {
		display: flex;
		gap: 0.625rem;
		align-items: flex-start;
	}

	.icon {
		flex-shrink: 0;
		width: 1.75rem;
		height: 1.75rem;
		display: flex;
		align-items: center;
		justify-content: center;
		background: var(--bg-root);
		border: 1px solid var(--border-color);
		border-radius: 50%;
		font-size: 0.875rem;
	}

	.body {
		flex: 1;
		min-width: 0;
	}

	.line {
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		gap: 0.375rem;
		font-size: 0.8125rem;
	}

	.actor {
		font-weight: 600;
	}

	.verb {
		color: var(--text-muted);
	}

	.when {
		margin-left: auto;
		font-size: 0.6875rem;
		color: var(--text-subtle);
		font-family: var(--font-mono);
	}

	.detail {
		margin-top: 0.25rem;
		font-size: 0.75rem;
		color: var(--text-muted);
		white-space: pre-wrap;
		overflow-wrap: anywhere;
		display: -webkit-box;
		-webkit-line-clamp: 4;
		line-clamp: 4;
		-webkit-box-orient: vertical;
		overflow: hidden;
	}

	/* Markdown in an entry: the entry's own scale, not a document's. Selectors go through .markdown-body so they outrank
	   Markdown.svelte's own rules. */
	.detail-md {
		white-space: normal;
		display: block;
		-webkit-line-clamp: none;
		line-clamp: none;
		overflow: visible;
	}

	.detail-md.clamped {
		max-height: 5.25rem;
		overflow: hidden;
		mask-image: linear-gradient(to bottom, #000 60%, transparent);
		-webkit-mask-image: linear-gradient(to bottom, #000 60%, transparent);
	}

	.detail-md :global(.markdown-body) {
		font-size: 0.75rem;
		line-height: 1.5;
		color: var(--text-muted);
	}

	.detail-md :global(.markdown-body h1),
	.detail-md :global(.markdown-body h2),
	.detail-md :global(.markdown-body h3),
	.detail-md :global(.markdown-body h4),
	.detail-md :global(.markdown-body h5),
	.detail-md :global(.markdown-body h6) {
		font-size: inherit;
		line-height: inherit;
		font-weight: 600;
		margin: 0.375rem 0 0.125rem 0;
		color: var(--text-primary);
	}

	.detail-md :global(.markdown-body > :first-child) {
		margin-top: 0;
	}

	.detail-md :global(.markdown-body p),
	.detail-md :global(.markdown-body pre),
	.detail-md :global(.markdown-body blockquote) {
		margin: 0 0 0.25rem 0;
	}

	.detail-md :global(.markdown-body ul),
	.detail-md :global(.markdown-body ol) {
		margin: 0 0 0.25rem 1rem;
		padding: 0;
	}

	.detail-md :global(.markdown-body code) {
		font-size: 0.6875rem;
	}

	.detail-md :global(.markdown-body pre) {
		padding: 0.375rem 0.5rem;
	}

	.detail-md :global(.markdown-body img) {
		max-height: 6rem;
	}

	.more {
		margin-top: 0.125rem;
		padding: 0;
		border: none;
		background: none;
		color: var(--color-primary);
		font-size: 0.6875rem;
		font-family: var(--font-sans);
		cursor: pointer;
	}

	.more:hover {
		text-decoration: underline;
	}

	.load-older {
		margin-top: 1rem;
		padding: 0.375rem 0.875rem;
		font-size: 0.8125rem;
		background: var(--bg-root);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm, 4px);
		cursor: pointer;
	}

	.load-older:disabled {
		opacity: 0.6;
		cursor: default;
	}
</style>
