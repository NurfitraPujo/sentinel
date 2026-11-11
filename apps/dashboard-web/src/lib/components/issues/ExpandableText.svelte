<!--
	sentinel-occurrence-detail-panels §4.3: every event string an occurrence panel renders goes through
	this component. Over `limit` code points it shows the first `limit`, `…` and a button that reveals
	the rest. The text is a Svelte interpolation, so it is escaped, never parsed as markup.

	The markup is exact, because where it may go depends on it (§4.6):
	- inline: `<span class="expandable-text">{shown}<button/></span>`, phrasing content only;
	- block: `<div class="expandable-text block"><pre>{shown}</pre><button/></div>`, flow content, never
	  inside a `<p>` or any other phrasing-only element.
-->
<script lang="ts">
	let { text, limit, block = false }: { text: string; limit: number; block?: boolean } = $props();

	let expanded = $state(false);
	// Code points, not UTF-16 units, so a surrogate pair is never split.
	let points = $derived(Array.from(text));
	let over = $derived(points.length > limit);
	let shown = $derived(over && !expanded ? points.slice(0, limit).join('') + '…' : text);
</script>

{#snippet toggle()}<button
		type="button"
		class="expand-toggle"
		data-testid="expand-text"
		aria-expanded={expanded}
		onclick={() => (expanded = !expanded)}>{expanded ? 'Show less' : `Show all (${points.length} characters)`}</button
	>{/snippet}

{#if block}<div class="expandable-text block"><pre>{shown}</pre>{#if over}{@render toggle()}{/if}</div>{:else}<span
		class="expandable-text">{shown}{#if over}{@render toggle()}{/if}</span
	>{/if}

<style>
	.expandable-text.block pre {
		margin: 0;
		font-family: var(--font-mono);
		font-size: 0.75rem;
		white-space: pre-wrap;
		word-break: break-word;
		overflow-x: auto;
	}

	.expandable-text {
		overflow-wrap: anywhere;
	}

	.expand-toggle {
		margin-left: 0.375rem;
		padding: 0;
		border: none;
		background: none;
		color: var(--color-primary);
		font-size: 0.6875rem;
		font-family: var(--font-sans);
		cursor: pointer;
	}

	.block .expand-toggle {
		margin: 0.25rem 0 0;
	}

	.expand-toggle:hover {
		text-decoration: underline;
	}
</style>
