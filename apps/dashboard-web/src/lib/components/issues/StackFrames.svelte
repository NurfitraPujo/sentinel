<!--
	sentinel-occurrence-detail-panels §5.2: a v2 stack (TASK-04), and each exception-chain stack
	(TASK-05, with showRaw false). In stored order, crashing frame first. It marks in-app frames,
	collapses runs of system frames the way Sentry does, and shows `module` and numbered source context.

	A stored frame is never handed to formatStackFrame directly: `frameObjects` skips non-objects and
	`frameLine` coerces the rest, so a hand-written row cannot throw here (a 500 under SSR).
-->
<script lang="ts">
	import { SvelteSet } from 'svelte/reactivity';
	import ExpandableText from './ExpandableText.svelte';
	import {
		BLOCK_LIMIT,
		INLINE_LIMIT,
		frameLine,
		frameObjects,
		frameVisibility,
		sourceContextLines,
	} from './occurrence-display';

	let { frames, showRaw }: { frames: unknown; showRaw: boolean } = $props();

	let objects = $derived(frameObjects(frames));

	type Segment = { kind: 'frame'; index: number } | { kind: 'run'; start: number; indices: number[] };

	// Visible frames in order, each maximal run of hidden ones folded into one toggle.
	let segments = $derived.by(() => {
		const visible = frameVisibility(objects);
		const out: Segment[] = [];
		visible.forEach((on, index) => {
			const last = out.at(-1);
			if (on) out.push({ kind: 'frame', index });
			else if (last?.kind === 'run') last.indices.push(index);
			else out.push({ kind: 'run', start: index, indices: [index] });
		});
		return out;
	});

	// Per run, keyed by the run's first index; local state, so it resets when the stack remounts.
	const expandedRuns = new SvelteSet<number>();

	function toggleRun(start: number) {
		if (expandedRuns.has(start)) expandedRuns.delete(start);
		else expandedRuns.add(start);
	}

	const text = (v: unknown): string => (typeof v === 'string' ? v : '');
</script>

{#snippet frameItem(index: number)}
	{@const frame = objects[index]}
	{@const inApp = frame.in_app === true}
	{@const module = text(frame.module)}
	{@const context = sourceContextLines(frame)}
	<li class="frame" class:system={!inApp} data-testid="frame" data-in-app={inApp ? 'true' : 'false'}>
		<div class="frame-head">
			<code class="frame-line"><ExpandableText text={frameLine(frame, showRaw)} limit={BLOCK_LIMIT} /></code>
			{#if inApp}<span class="in-app" data-testid="in-app">in app</span>{/if}
			{#if module}<span class="frame-module" data-testid="frame-module"><ExpandableText text={module} limit={INLINE_LIMIT} /></span>{/if}
		</div>
		{#if context}
			<ol class="frame-context" data-testid="frame-context">
				{#each context as line, i (i)}
					<li class:current={line.current} aria-current={line.current ? 'true' : undefined}>
						{#if line.lineno !== null}<span class="lineno">{line.lineno}</span>{/if}<code><ExpandableText text={line.text} limit={BLOCK_LIMIT} /></code>
					</li>
				{/each}
			</ol>
		{/if}
	</li>
{/snippet}

{#if objects.length > 0}
	<ol class="frames" data-testid="frames">
		{#each segments as segment (segment.kind === 'frame' ? `f${segment.index}` : `r${segment.start}`)}
			{#if segment.kind === 'frame'}
				{@render frameItem(segment.index)}
			{:else}
				{@const n = segment.indices.length}
				{@const open = expandedRuns.has(segment.start)}
				<li class="frames-toggle">
					<button type="button" aria-expanded={open} data-testid="show-frames" onclick={() => toggleRun(segment.start)}
						>{open ? 'Hide' : 'Show'} {n} more frame{n === 1 ? '' : 's'}</button
					>
				</li>
				{#if open}
					{#each segment.indices as index (index)}
						{@render frameItem(index)}
					{/each}
				{/if}
			{/if}
		{/each}
	</ol>
{/if}

<style>
	.frames {
		list-style: none;
		margin: 0;
		padding: 0;
		font-family: var(--font-mono);
		font-size: 0.75rem;
	}

	.frame {
		padding: 0.1875rem 0;
		border-bottom: 1px solid var(--border-color-muted);
	}

	.frame:last-child {
		border-bottom: none;
	}

	.frame-head {
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		gap: 0.5rem;
	}

	.frame-line {
		color: var(--text-primary);
		overflow-wrap: anywhere;
	}

	.frame.system .frame-line {
		color: var(--text-muted);
	}

	.in-app {
		padding: 0 0.25rem;
		border-radius: var(--radius-sm);
		background: var(--color-primary-subtle);
		color: var(--color-primary);
		font-size: 0.625rem;
		font-family: var(--font-sans);
	}

	.frame-module {
		color: var(--text-subtle);
	}

	.frame-context {
		list-style: none;
		margin: 0.25rem 0 0.25rem 1rem;
		padding: 0;
		overflow-x: auto;
	}

	.frame-context li {
		display: flex;
		gap: 0.75rem;
		white-space: pre;
		color: var(--text-secondary);
	}

	.frame-context li.current {
		background: var(--color-primary-subtle);
		color: var(--text-primary);
	}

	.lineno {
		min-width: 2.5rem;
		text-align: right;
		color: var(--text-subtle);
		user-select: none;
	}

	.frames-toggle button {
		margin: 0.25rem 0;
		padding: 0;
		border: none;
		background: none;
		color: var(--color-primary);
		font-family: var(--font-sans);
		font-size: 0.6875rem;
		cursor: pointer;
	}

	.frames-toggle button:hover {
		text-decoration: underline;
	}
</style>
