<script lang="ts">
	import type { TagFacetBlock } from '$lib/tag-facets';
	import { tagSearchHref } from '$lib/tag-filter';

	// sentinel-issue-tag-facets spec §7 (TASK-02): how this issue is distributed across its tags —
	// release, environment, OS, device, browser, user and every custom tag — from the issue_tag_values
	// rollup, which the loader reads once per page load. Each block shows the key's top 5 values with
	// their share of the key's total and an "Other" line for the rest (never a link). Each value links
	// to /search for that key:value through the one shared encoder (tagSearchHref). Keys and values are
	// caller-supplied: text interpolation only, never raw HTML. No blocks, no panel (F6).
	interface Props {
		facets: TagFacetBlock[];
	}

	let { facets }: Props = $props();

	const formatCount = (n: number) => n.toLocaleString();
	const formatPercent = (p: number) => `${p.toFixed(1)}%`;
</script>

{#if facets.length > 0}
	<section class="tag-facets" data-testid="issue-tag-facets" aria-label="Tag distribution">
		<h2 class="section-title">Tags</h2>
		<div class="blocks">
			{#each facets as block (block.key)}
				<div class="block" data-testid="tag-facet-block" data-key={block.key}>
					<h3 class="block-key" title={block.key}>{block.key}</h3>
					<ul class="lines">
						{#each block.values as line (line.value)}
							<li class="line" data-testid="tag-facet-value">
								<a class="line-label" href={tagSearchHref(block.key, line.value)} title={line.value}>{line.value}</a>
								<span class="line-count">{formatCount(line.count)}</span>
								<span class="line-percent">{formatPercent(line.percent)}</span>
								<span class="bar" aria-hidden="true"><span class="bar-fill" style:width="{line.percent}%"></span></span>
							</li>
						{/each}
						{#if block.other}
							<li class="line other" data-testid="tag-facet-other">
								<span class="line-label">Other</span>
								<span class="line-count">{formatCount(block.other.count)}</span>
								<span class="line-percent">{formatPercent(block.other.percent)}</span>
								<span class="bar" aria-hidden="true"><span class="bar-fill" style:width="{block.other.percent}%"></span></span>
							</li>
						{/if}
					</ul>
				</div>
			{/each}
		</div>
	</section>
{/if}

<style>
	.tag-facets {
		background: var(--bg-surface, #1e293b);
		border: 1px solid var(--border-color, #334155);
		border-radius: var(--radius-md, 6px);
		padding: 1rem;
		margin-bottom: 1.5rem;
		color: var(--text-primary, #f8fafc);
	}

	.section-title {
		font-size: 0.875rem;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted, #94a3b8);
		font-family: var(--font-mono, 'JetBrains Mono', monospace);
		margin: 0 0 0.875rem;
	}

	.blocks {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(16rem, 1fr));
		gap: 1rem 1.5rem;
	}

	.block-key {
		font-size: 0.8125rem;
		font-weight: 600;
		font-family: var(--font-mono, monospace);
		color: var(--text-secondary, #cbd5e1);
		margin: 0 0 0.375rem;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.lines {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.25rem;
	}

	.line {
		display: grid;
		grid-template-columns: minmax(0, 1fr) auto 3.5rem;
		grid-template-areas:
			'label count percent'
			'bar bar bar';
		column-gap: 0.5rem;
		row-gap: 0.125rem;
		font-size: 0.8125rem;
	}

	.line-label {
		grid-area: label;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		color: var(--text-primary, #f8fafc);
		text-decoration: none;
	}

	a.line-label:hover,
	a.line-label:focus-visible {
		color: var(--color-primary-hover, #60a5fa);
		text-decoration: underline;
	}

	.other .line-label {
		color: var(--text-muted, #94a3b8);
		font-style: italic;
	}

	.line-count {
		grid-area: count;
		font-family: var(--font-mono, monospace);
		color: var(--text-secondary, #cbd5e1);
	}

	.line-percent {
		grid-area: percent;
		text-align: right;
		font-family: var(--font-mono, monospace);
		color: var(--text-muted, #94a3b8);
	}

	.bar {
		grid-area: bar;
		display: block;
		height: 3px;
		background: var(--bg-root, #0f172a);
		border-radius: var(--radius-sm, 3px);
		overflow: hidden;
	}

	.bar-fill {
		display: block;
		height: 100%;
		background: var(--color-primary, #3b82f6);
	}

	.other .bar-fill {
		background: var(--border-color, #334155);
	}
</style>
