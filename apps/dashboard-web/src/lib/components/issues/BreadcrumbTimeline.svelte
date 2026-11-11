<!--
	sentinel-occurrence-detail-panels §8 (TASK-02): the breadcrumbs, on the selected v2 occurrence, in
	STORED order (oldest first, the SDK's), with time relative to the occurrence and category/level
	filters.

	Every crumb renders (ingest caps an event at 100), and the expensive part, `data`, is not rendered,
	stringified or measured until its toggle is opened. A row written outside the ingest bound renders
	its last 100 crumbs, the ones nearest the error, with a guard line.

	Filter values are collected in a `Set` and ordered through a `Map`, so a category `constructor` or a
	level `toString` is an ordinary option (§4.2 rule 5).
-->
<script lang="ts">
	import { SvelteSet } from 'svelte/reactivity';
	import ExpandableText from './ExpandableText.svelte';
	import {
		BLOCK_LIMIT,
		INLINE_LIMIT,
		anchorMs,
		clip,
		formatRelative,
		levelTone,
		parseCrumbTime,
	} from './occurrence-display';

	let { breadcrumbs, occurredAt }: { breadcrumbs: unknown; occurredAt: string | Date | null } = $props();

	type Crumb = Record<string, unknown>;
	const isObject = (v: unknown): v is Crumb => v !== null && typeof v === 'object' && !Array.isArray(v);
	const text = (v: unknown) => (typeof v === 'string' ? v : '');
	const byCodeUnit = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0);

	const RENDER_BOUND = 100;
	const LEVEL_RANK = new Map([
		['fatal', 0],
		['error', 1],
		['warning', 2],
		['info', 3],
		['debug', 4],
	]);

	let all = $derived(Array.isArray(breadcrumbs) ? breadcrumbs.filter(isObject) : []);
	let crumbs = $derived(all.length > RENDER_BOUND ? all.slice(-RENDER_BOUND) : all);

	let categories = $derived(
		[...new Set(crumbs.map((c) => text(c.category)).filter((c) => c !== ''))].sort(byCodeUnit),
	);
	let levels = $derived(
		[...new Set(crumbs.map((c) => text(c.level)).filter((l) => l !== ''))].sort((a, b) => {
			const ra = LEVEL_RANK.get(a) ?? LEVEL_RANK.size;
			const rb = LEVEL_RANK.get(b) ?? LEVEL_RANK.size;
			return ra - rb || byCodeUnit(a, b);
		}),
	);

	// '' is "all": only non-empty values are options.
	let categoryFilter = $state('');
	let levelFilter = $state('');

	let shown = $derived(
		crumbs
			.map((crumb, index) => ({ crumb, index }))
			.filter(
				({ crumb }) =>
					(categoryFilter === '' || text(crumb.category) === categoryFilter) &&
					(levelFilter === '' || text(crumb.level) === levelFilter),
			),
	);

	let anchor = $derived(anchorMs(occurredAt));

	// Per crumb (its index among the rendered crumbs); local, one timeline per occurrence.
	const openData = new SvelteSet<number>();

	function toggleData(index: number) {
		if (openData.has(index)) openData.delete(index);
		else openData.add(index);
	}

	const hasData = (crumb: Crumb) => isObject(crumb.data) && Object.keys(crumb.data).length > 0;
</script>

{#if crumbs.length > 0}
	<section class="panel" data-testid="breadcrumbs">
		<h4>Breadcrumbs</h4>
		<div class="filters">
			<select aria-label="Filter by category" data-testid="crumb-category-filter" bind:value={categoryFilter}>
				<option value="">All categories</option>
				{#each categories as category (category)}
					<option value={category}>{clip(category, INLINE_LIMIT)}</option>
				{/each}
			</select>
			<select aria-label="Filter by level" data-testid="crumb-level-filter" bind:value={levelFilter}>
				<option value="">All levels</option>
				{#each levels as level (level)}
					<option value={level}>{clip(level, INLINE_LIMIT)}</option>
				{/each}
			</select>
		</div>
		{#if all.length > RENDER_BOUND}
			<p class="crumb-note" data-testid="crumb-guard">Showing the {RENDER_BOUND} most recent of {all.length} breadcrumbs.</p>
		{/if}
		{#if categoryFilter !== '' || levelFilter !== ''}
			<p class="crumb-note" data-testid="crumb-count">Showing {shown.length} of {crumbs.length}</p>
		{/if}
		<ol class="crumbs">
			{#each shown as { crumb, index } (index)}
				{@const timestamp = text(crumb.timestamp)}
				{@const at = parseCrumbTime(timestamp)}
				{@const level = text(crumb.level)}
				{@const category = text(crumb.category)}
				{@const type = text(crumb.type)}
				{@const message = text(crumb.message)}
				{@const open = openData.has(index)}
				<li class="crumb" data-testid="crumb">
					{#if at !== null && anchor !== null}
						<time class="crumb-time" data-testid="crumb-time" title={timestamp} datetime={timestamp}
							>{formatRelative(at - anchor)}</time
						>
					{:else if at !== null}
						<time class="crumb-time" data-testid="crumb-time" datetime={timestamp}
							><ExpandableText text={timestamp} limit={INLINE_LIMIT} /></time
						>
					{:else if timestamp !== ''}
						<time class="crumb-time" data-testid="crumb-time"><ExpandableText text={timestamp} limit={INLINE_LIMIT} /></time>
					{:else}
						<time class="crumb-time" data-testid="crumb-time">—</time>
					{/if}
					{#if level}<span class="crumb-level {levelTone(level)}"><ExpandableText text={level} limit={INLINE_LIMIT} /></span>{/if}
					{#if category}<span class="crumb-category"><ExpandableText text={category} limit={INLINE_LIMIT} /></span>{/if}
					{#if type}<span class="crumb-type"><ExpandableText text={type} limit={INLINE_LIMIT} /></span>{/if}
					{#if message}<span class="crumb-message"><ExpandableText text={message} limit={INLINE_LIMIT} /></span>{/if}
					{#if hasData(crumb)}
						<button
							type="button"
							class="crumb-data-toggle"
							aria-expanded={open}
							data-testid="crumb-data-toggle"
							onclick={() => toggleData(index)}>{open ? 'Hide data' : 'Show data'}</button
						>
						{#if open}
							<div class="crumb-data" data-testid="crumb-data">
								<ExpandableText text={JSON.stringify(crumb.data, null, 2)} limit={BLOCK_LIMIT} block />
							</div>
						{/if}
					{/if}
				</li>
			{/each}
		</ol>
	</section>
{/if}

<style>
	.panel {
		margin: 0.75rem 0;
		font-size: 0.75rem;
	}

	h4 {
		margin-bottom: 0.5rem;
		color: var(--text-subtle);
		font-size: 0.6875rem;
		font-family: var(--font-mono);
		text-transform: uppercase;
		letter-spacing: 0.05em;
	}

	.filters {
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem;
		margin-bottom: 0.5rem;
	}

	select {
		max-width: 16rem;
		padding: 0.1875rem 0.375rem;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		background: var(--bg-root);
		color: var(--text-primary);
		font-size: 0.75rem;
	}

	.crumb-note {
		margin-bottom: 0.375rem;
		color: var(--text-muted);
	}

	.crumbs {
		list-style: none;
		margin: 0;
		padding: 0;
	}

	.crumb {
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		gap: 0.25rem 0.625rem;
		padding: 0.25rem 0;
		border-bottom: 1px solid var(--border-color-muted);
		overflow-wrap: anywhere;
	}

	.crumb-time {
		min-width: 4rem;
		color: var(--text-muted);
		font-family: var(--font-mono);
	}

	.crumb-level {
		padding: 0 0.3125rem;
		border: 1px solid;
		border-radius: var(--radius-sm);
		font-family: var(--font-mono);
		font-size: 0.6875rem;
	}

	.crumb-category {
		color: var(--text-secondary);
		font-weight: 500;
	}

	.crumb-type {
		color: var(--text-subtle);
	}

	.crumb-message {
		flex: 1 1 16rem;
		min-width: 0;
		color: var(--text-primary);
	}

	.crumb-data-toggle {
		padding: 0;
		border: none;
		background: none;
		color: var(--color-primary);
		font-size: 0.6875rem;
		cursor: pointer;
	}

	.crumb-data {
		flex-basis: 100%;
		padding: 0.375rem 0.5rem;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		background: var(--bg-root);
		color: var(--text-primary);
	}

	/* §4.4: the level tones, from variables app.css defines. */
	.tone-critical {
		color: var(--severity-critical-text);
		background: var(--severity-critical-bg);
		border-color: var(--severity-critical-border);
	}

	.tone-warning {
		color: var(--status-open-text);
		background: var(--status-open-bg);
		border-color: var(--status-open-border);
	}

	.tone-info {
		color: var(--color-primary);
		background: var(--color-primary-subtle);
		border-color: var(--border-color);
	}

	.tone-debug {
		color: var(--status-ignored-text);
		background: var(--status-ignored-bg);
		border-color: var(--status-ignored-border);
	}

	.tone-neutral {
		color: var(--text-muted);
		background: none;
		border-color: var(--border-color);
	}
</style>
