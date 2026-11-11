<!--
	sentinel-occurrence-detail-panels §6.2 (TASK-03): the event's request, on the selected v2 occurrence.
	Values are rendered as stored, masked ones included (`***REDACTED***`): nothing is unmasked, parsed or
	re-derived, and the URL is TEXT, never an `<a>` (§4.2). `request.env` is never rendered (§2).
-->
<script lang="ts">
	import ExpandableText from './ExpandableText.svelte';
	import { BLOCK_LIMIT, INLINE_LIMIT, formatValue } from './occurrence-display';

	let { request }: { request: unknown } = $props();

	let r = $derived(
		request !== null && typeof request === 'object' && !Array.isArray(request)
			? (request as Record<string, unknown>)
			: null,
	);

	const text = (v: unknown) => (typeof v === 'string' ? v : '');
	const byCodeUnit = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0);

	let method = $derived(text(r?.method));
	let url = $derived(text(r?.url));
	let query = $derived(text(r?.query_string));

	// jsonb keeps no client order (it orders keys by length, then bytes), so a stable one is chosen:
	// case-insensitively, then by name, comparing code units.
	let headers = $derived.by(() => {
		const h = r?.headers;
		if (h === null || typeof h !== 'object' || Array.isArray(h)) return [];
		return Object.entries(h as Record<string, unknown>)
			.filter((entry): entry is [string, string] => typeof entry[1] === 'string')
			.sort(([a], [b]) => byCodeUnit(a.toLowerCase(), b.toLowerCase()) || byCodeUnit(a, b));
	});

	let body = $derived(
		r === null || r.data === undefined || r.data === null
			? null
			: typeof r.data === 'string'
				? r.data
				: formatValue(r.data, true),
	);

	let empty = $derived(!method && !url && !query && headers.length === 0 && body === null);
</script>

{#if r && !empty}
	<section class="panel" data-testid="request-panel">
		<h4>Request</h4>
		{#if method || url}
			<p class="request-line">
				{#if method}<span class="method"><ExpandableText text={method} limit={INLINE_LIMIT} /></span>{/if}
				{#if url}<span class="request-url"><ExpandableText text={url} limit={INLINE_LIMIT} /></span>{/if}
			</p>
		{/if}
		{#if query}
			<dl class="request-query">
				<dt>Query</dt>
				<dd><ExpandableText text={query} limit={INLINE_LIMIT} /></dd>
			</dl>
		{/if}
		{#if headers.length > 0}
			<table class="headers" data-testid="request-headers">
				<tbody>
					{#each headers as [name, value], i (i)}
						<tr>
							<th scope="row"><ExpandableText text={name} limit={INLINE_LIMIT} /></th>
							<td><ExpandableText text={value} limit={INLINE_LIMIT} /></td>
						</tr>
					{/each}
				</tbody>
			</table>
		{/if}
		{#if body !== null}
			<div class="request-body" data-testid="request-body">
				<ExpandableText text={body} limit={BLOCK_LIMIT} block />
			</div>
		{/if}
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

	.request-line {
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem;
		margin-bottom: 0.5rem;
		font-family: var(--font-mono);
		color: var(--text-primary);
	}

	.method {
		font-weight: 600;
	}

	.request-url {
		min-width: 0;
		overflow-wrap: anywhere;
	}

	.request-query {
		display: grid;
		grid-template-columns: max-content 1fr;
		gap: 0.25rem 0.75rem;
		margin-bottom: 0.5rem;
	}

	.request-query dt {
		color: var(--text-muted);
	}

	.request-query dd {
		margin: 0;
		font-family: var(--font-mono);
		color: var(--text-secondary);
		overflow-wrap: anywhere;
	}

	.headers {
		width: 100%;
		border-collapse: collapse;
		margin-bottom: 0.5rem;
		table-layout: fixed;
	}

	.headers th,
	.headers td {
		padding: 0.1875rem 0.5rem 0.1875rem 0;
		border-bottom: 1px solid var(--border-color-muted);
		text-align: left;
		vertical-align: top;
		overflow-wrap: anywhere;
	}

	.headers th {
		width: 30%;
		color: var(--text-muted);
		font-weight: 500;
	}

	.headers td {
		font-family: var(--font-mono);
		color: var(--text-secondary);
	}

	.request-body {
		padding: 0.5rem;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		background: var(--bg-root);
		color: var(--text-primary);
	}
</style>
