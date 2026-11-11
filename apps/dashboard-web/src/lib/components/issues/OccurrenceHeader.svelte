<!--
	sentinel-occurrence-detail-panels §5.1 (TASK-04): a v2 occurrence's own message and header facts,
	on every v2 card, selected or not. The issue header above the page stays the issue's.

	The message container is a `<div>`, never a `<p>`: it holds a block `ExpandableText` (a `<pre>`),
	and the HTML parser would move a `<pre>` out of a `<p>` before hydration (§4.6).
-->
<script lang="ts">
	import ExpandableText from './ExpandableText.svelte';
	import { BLOCK_LIMIT, INLINE_LIMIT, levelTone } from './occurrence-display';

	interface HeaderOccurrence {
		level?: unknown;
		message?: unknown;
		releaseVersion?: unknown;
		dist?: unknown;
		eventId?: unknown;
		serverName?: unknown;
		transaction?: unknown;
		sdk?: unknown;
	}

	let { occurrence, issueMessage }: { occurrence: HeaderOccurrence; issueMessage: string } = $props();

	const text = (v: unknown): string => (typeof v === 'string' && v !== '' ? v : '');

	// The v2 message is NULL when the SDK sent "" (NULLIF), so the issue's message stands in (§5.1).
	let message = $derived(text(occurrence.message) || text(issueMessage));
	let level = $derived(text(occurrence.level));

	let sdkLine = $derived.by(() => {
		const sdk = occurrence.sdk;
		if (sdk === null || typeof sdk !== 'object' || Array.isArray(sdk)) return '';
		const { name, version } = sdk as { name?: unknown; version?: unknown };
		return [text(name), text(version)].filter((part) => part !== '').join(' ');
	});

	// In §5.1's order; a fact renders only when its value is a non-empty string.
	let facts = $derived(
		[
			{ label: 'Release', value: text(occurrence.releaseVersion), mono: false },
			{ label: 'Dist', value: text(occurrence.dist), mono: false },
			{ label: 'Event ID', value: text(occurrence.eventId), mono: true },
			{ label: 'Server', value: text(occurrence.serverName), mono: false },
			{ label: 'Transaction', value: text(occurrence.transaction), mono: false },
			{ label: 'SDK', value: sdkLine, mono: false },
		].filter((fact) => fact.value !== ''),
	);
</script>

{#if message}
	<div class="occurrence-message" data-testid="occurrence-message">
		<ExpandableText text={message} limit={BLOCK_LIMIT} block />
	</div>
{/if}
<dl class="occurrence-facts" data-testid="occurrence-facts">
	{#if level}
		<dt>Level</dt>
		<dd><span class="level-badge {levelTone(level)}" data-testid="level-badge"><ExpandableText text={level} limit={INLINE_LIMIT} /></span></dd>
	{/if}
	{#each facts as fact (fact.label)}
		<dt>{fact.label}</dt>
		<dd class:mono={fact.mono}><ExpandableText text={fact.value} limit={INLINE_LIMIT} /></dd>
	{/each}
</dl>

<style>
	.occurrence-message {
		margin: 0.5rem 0;
		color: var(--text-primary);
		font-size: 0.8125rem;
	}

	.occurrence-facts {
		display: grid;
		grid-template-columns: max-content 1fr;
		gap: 0.25rem 0.75rem;
		margin: 0 0 0.75rem;
		font-size: 0.75rem;
	}

	.occurrence-facts dt {
		color: var(--text-muted);
	}

	.occurrence-facts dd {
		margin: 0;
		color: var(--text-secondary);
		min-width: 0;
	}

	.mono {
		font-family: var(--font-mono);
	}

	.level-badge {
		display: inline-block;
		padding: 0.0625rem 0.375rem;
		border: 1px solid;
		border-radius: var(--radius-sm);
		font-family: var(--font-mono);
		font-size: 0.6875rem;
	}

	/* §4.4: the five levels onto variables app.css defines; colour is never the only carrier. */
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
