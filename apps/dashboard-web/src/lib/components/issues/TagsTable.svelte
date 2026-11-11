<!--
	sentinel-occurrence-detail-panels §6.3 (TASK-03): the event's tags, on the selected v2 occurrence.
	Each value links to `/search?tag=…` through the ONE shared encoder, `tagSearchHref` in
	$lib/tag-filter (contract decisions 6 and 7, shared with sentinel-issue-tag-facets): it is never
	redefined here. The link text is clipped rather than expandable, because an `<a>` may not contain a
	button; the href always carries the full value.
-->
<script lang="ts">
	import { tagSearchHref } from '$lib/tag-filter';
	import ExpandableText from './ExpandableText.svelte';
	import { INLINE_LIMIT, clip } from './occurrence-display';

	let { tags }: { tags: unknown } = $props();

	// Sorted by key, comparing code units: keys are ASCII by the ingest pattern, so this is byte order.
	let rows = $derived.by(() => {
		if (tags === null || typeof tags !== 'object' || Array.isArray(tags)) return [];
		return Object.entries(tags as Record<string, unknown>)
			.filter((entry): entry is [string, string] => typeof entry[1] === 'string')
			.sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
	});
</script>

{#if rows.length > 0}
	<section class="panel">
		<h4>Tags</h4>
		<table data-testid="tags-table">
			<tbody>
				{#each rows as [key, value] (key)}
					<tr>
						<th scope="row"><ExpandableText text={key} limit={INLINE_LIMIT} /></th>
						<td><a href={tagSearchHref(key, value)} data-testid="tag-link">{clip(value, INLINE_LIMIT)}</a></td>
					</tr>
				{/each}
			</tbody>
		</table>
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

	table {
		width: 100%;
		border-collapse: collapse;
		table-layout: fixed;
	}

	th,
	td {
		padding: 0.1875rem 0.5rem 0.1875rem 0;
		border-bottom: 1px solid var(--border-color-muted);
		text-align: left;
		vertical-align: top;
		overflow-wrap: anywhere;
	}

	th {
		width: 30%;
		color: var(--text-muted);
		font-weight: 500;
		font-family: var(--font-mono);
	}

	a {
		color: var(--color-primary);
		text-decoration: none;
		font-family: var(--font-mono);
	}

	a:hover {
		text-decoration: underline;
	}
</style>
