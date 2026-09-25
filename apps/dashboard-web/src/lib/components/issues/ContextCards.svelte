<!--
	sentinel-occurrence-detail-panels §7 (TASK-01): the event's contexts, on the selected v2 occurrence,
	one card per context: the well-known ones first in the contract's order, with titles, a headline and
	labelled rows; every other context rendered generically under its raw key.

	A non-object context (such as `***REDACTED***`) is shown as stored in a block `<div>`, never a `<p>`
	(§4.6). With no card, the whole section, heading included, is absent.
-->
<script lang="ts">
	import ExpandableText from './ExpandableText.svelte';
	import { BLOCK_LIMIT, INLINE_LIMIT, contextCards } from './occurrence-display';

	let { contexts }: { contexts: unknown } = $props();

	let cards = $derived(contextCards(contexts));
</script>

{#if cards.length > 0}
	<section class="panel" data-testid="contexts">
		<h4>Contexts</h4>
		<div class="context-cards">
			{#each cards as card (card.key)}
				<section class="context-card" data-testid="context-card" data-context-key={card.key}>
					<h5>
						{#if card.title !== null}{card.title}{:else}<ExpandableText text={card.key} limit={INLINE_LIMIT} />{/if}
					</h5>
					{#if card.rows}
						{#if card.headline !== null}
							<p class="context-headline"><ExpandableText text={card.headline} limit={INLINE_LIMIT} /></p>
						{/if}
						<dl>
							{#each card.rows as row (row.key)}
								<dt>{#if row.label !== null}{row.label}{:else}<ExpandableText text={row.key} limit={INLINE_LIMIT} />{/if}</dt>
								<dd><ExpandableText text={row.value} limit={INLINE_LIMIT} /></dd>
							{/each}
						</dl>
					{:else if card.scalar !== null}
						<div class="context-scalar"><ExpandableText text={card.scalar} limit={BLOCK_LIMIT} block /></div>
					{/if}
				</section>
			{/each}
		</div>
	</section>
{/if}

<style>
	.panel {
		margin: 0.75rem 0;
	}

	h4 {
		margin-bottom: 0.5rem;
		color: var(--text-subtle);
		font-size: 0.6875rem;
		font-family: var(--font-mono);
		text-transform: uppercase;
		letter-spacing: 0.05em;
	}

	.context-cards {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(16rem, 1fr));
		gap: 0.5rem;
	}

	.context-card {
		min-width: 0;
		padding: 0.5rem 0.625rem;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		background: var(--bg-root);
		font-size: 0.75rem;
		overflow-wrap: anywhere;
	}

	h5 {
		margin: 0 0 0.25rem;
		color: var(--text-primary);
		font-size: 0.75rem;
		font-weight: 600;
	}

	.context-headline {
		margin: 0 0 0.375rem;
		color: var(--text-secondary);
	}

	dl {
		display: grid;
		grid-template-columns: max-content 1fr;
		gap: 0.125rem 0.625rem;
		margin: 0;
	}

	dt {
		color: var(--text-muted);
	}

	dd {
		margin: 0;
		min-width: 0;
		color: var(--text-secondary);
	}

	.context-scalar {
		color: var(--text-secondary);
	}
</style>
