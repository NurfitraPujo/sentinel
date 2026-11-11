<!--
	sentinel-occurrence-detail-panels §9 (TASK-05): the exception chain, on the selected v2 occurrence,
	only when it has more than one value (a single value IS the top-level stack, which stays above
	this section unchanged: it is the only symbolicated one and the fingerprint input).

	Outermost first. Values at display index ≥ 3 start collapsed (Sentry's
	`initialCollapse={excIdx < values.length - 3}`). Chain frames are never symbolicated (decision 4),
	so on a JavaScript/browser occurrence each stack says so.
-->
<script lang="ts">
	import { SvelteSet } from 'svelte/reactivity';
	import ExpandableText from './ExpandableText.svelte';
	import StackFrames from './StackFrames.svelte';
	import { INLINE_LIMIT, chainDisplayOrder, frameObjects } from './occurrence-display';

	let { exception, platform }: { exception: unknown; platform: string } = $props();

	type Value = Record<string, unknown>;
	const isObject = (v: unknown): v is Value => v !== null && typeof v === 'object' && !Array.isArray(v);
	const text = (v: unknown) => (typeof v === 'string' ? v : '');

	let values = $derived.by((): Value[] => {
		const stored = isObject(exception) ? exception.values : undefined;
		return Array.isArray(stored) ? chainDisplayOrder(stored.filter(isObject)) : [];
	});

	// The processor's default SYMBOLICATE_PLATFORMS (apps/processor-go/main.go): nothing would ever
	// symbolicate a Go or Dart chain, so it gets no label.
	const SYMBOLICATED_PLATFORMS = new Set(['javascript', 'browser']);

	const COLLAPSE_FROM = 3;
	const expanded = new SvelteSet<number>();

	function toggle(index: number) {
		if (expanded.has(index)) expanded.delete(index);
		else expanded.add(index);
	}
</script>

{#if values.length > 1}
	<section class="panel" data-testid="exception-chain">
		<h4>Exception Chain</h4>
		{#each values as value, index (index)}
			{@const type = text(value.type)}
			{@const message = text(value.value)}
			{@const mechanism = isObject(value.mechanism) ? value.mechanism : null}
			{@const mechanismType = mechanism ? text(mechanism.type) : ''}
			{@const handled = mechanism && typeof mechanism.handled === 'boolean' ? mechanism.handled : null}
			{@const frames = frameObjects(value.stacktrace)}
			{@const unsymbolicated =
				SYMBOLICATED_PLATFORMS.has(platform) && frames.length > 0 && !frames.some((f) => f.symbolicated === true)}
			{@const collapsible = index >= COLLAPSE_FROM}
			{@const open = !collapsible || expanded.has(index)}
			<article class="chain-value" data-testid="chain-value">
				<h5>
					{#if type || message}
						{#if type}<ExpandableText text={type} limit={INLINE_LIMIT} />{/if}{#if type && message}{': '}{/if}{#if message}<ExpandableText
								text={message}
								limit={INLINE_LIMIT}
							/>{/if}
					{:else}
						Exception {index + 1} of {values.length}
					{/if}
				</h5>
				{#if collapsible}
					<button
						type="button"
						class="chain-expand"
						aria-expanded={open}
						data-testid="chain-expand"
						onclick={() => toggle(index)}>{open ? 'Hide exception' : 'Show exception'}</button
					>
				{/if}
				{#if open}
					{#if mechanismType || handled !== null}
						<p class="mechanism" data-testid="mechanism">
							{#if mechanismType}{'mechanism: '}<ExpandableText text={mechanismType} limit={INLINE_LIMIT} />{/if}{#if mechanismType && handled !== null}{' · '}{/if}{#if handled !== null}{`handled: ${handled}`}{/if}
						</p>
					{/if}
					{#if unsymbolicated}
						<span
							class="badge unsymbolicated"
							data-testid="chain-unsymbolicated"
							title="Chained exception frames are stored as received; only the top-level stack trace is symbolicated."
							>Not symbolicated</span
						>
					{/if}
					<StackFrames frames={value.stacktrace} showRaw={false} />
				{/if}
			</article>
		{/each}
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

	.chain-value {
		margin-bottom: 0.75rem;
		padding: 0.625rem 0.75rem;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		background: var(--bg-root);
	}

	h5 {
		margin-bottom: 0.25rem;
		color: var(--text-primary);
		font-size: 0.8125rem;
		font-weight: 600;
		overflow-wrap: anywhere;
	}

	.mechanism {
		margin-bottom: 0.375rem;
		color: var(--text-muted);
		font-size: 0.6875rem;
		font-family: var(--font-mono);
	}

	.badge.unsymbolicated {
		display: inline-block;
		margin-bottom: 0.375rem;
		padding: 0.125rem 0.375rem;
		border: 1px solid var(--status-open-border);
		border-radius: var(--radius-sm);
		background: var(--status-open-bg);
		color: var(--status-open-text);
		font-size: 0.6875rem;
		font-family: var(--font-mono);
	}

	.chain-expand {
		padding: 0;
		border: none;
		background: none;
		color: var(--color-primary);
		font-size: 0.6875rem;
		cursor: pointer;
	}

	.chain-expand:hover {
		text-decoration: underline;
	}
</style>
