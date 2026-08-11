<script lang="ts">
	import { marked } from 'marked';
	import DOMPurify from 'dompurify';

	// Manual Issues M1 (docs/plans/MANUAL_ISSUES_DESIGN.md §3): the single shared renderer for
	// report bodies, thread messages, and agent posts. Sanitize AT RENDER, never trust what is
	// stored -- `body_md` is plain user/agent-authored text with no server-side sanitization on
	// write, so every render path must go through this component rather than `{@html}`ing
	// `marked.parse()` output directly.
	//
	// DOMPurify's default export needs a DOM `window` to become a working sanitizer -- under plain
	// Node (SvelteKit's SSR render) `DOMPurify.sanitize` is not a function at all, it stays an
	// unconfigured factory until called with one. Rather than pull `jsdom` (a Node-only package
	// that uses `fs`/`vm`) into this universal component -- which would either break the browser
	// build or need SSR/client code-splitting well beyond v1's scope -- sanitization runs
	// client-side only, in an effect. SSR paints an inert plain-text fallback (HTML-escaped, no
	// `{@html}`) so there is never a moment where unsanitized markup reaches the DOM.
	interface Props {
		source: string;
	}

	let { source }: Props = $props();

	let html = $state<string | null>(null);

	$effect(() => {
		const raw = marked.parse(source ?? '', { async: false, breaks: true, gfm: true }) as string;
		html = DOMPurify.sanitize(raw);
	});
</script>

<div class="markdown-body">
	{#if html !== null}
		{@html html}
	{:else}
		<p class="markdown-fallback">{source}</p>
	{/if}
</div>

<style>
	.markdown-body {
		color: var(--text-primary);
		font-size: 0.875rem;
		line-height: 1.6;
		word-break: break-word;
	}

	.markdown-fallback {
		white-space: pre-wrap;
	}

	.markdown-body :global(p) {
		margin: 0 0 0.75rem 0;
	}

	.markdown-body :global(p:last-child) {
		margin-bottom: 0;
	}

	.markdown-body :global(h1),
	.markdown-body :global(h2),
	.markdown-body :global(h3) {
		font-weight: 600;
		margin: 1rem 0 0.5rem 0;
		color: var(--text-primary);
	}

	.markdown-body :global(a) {
		color: var(--color-primary);
	}

	.markdown-body :global(code) {
		font-family: var(--font-mono);
		background: var(--bg-root);
		border: 1px solid var(--border-color);
		border-radius: 3px;
		padding: 0.05rem 0.3rem;
		font-size: 0.8125rem;
	}

	.markdown-body :global(pre) {
		background: var(--bg-root);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.75rem;
		overflow-x: auto;
		margin: 0 0 0.75rem 0;
	}

	.markdown-body :global(pre code) {
		background: none;
		border: none;
		padding: 0;
	}

	.markdown-body :global(ul),
	.markdown-body :global(ol) {
		margin: 0 0 0.75rem 1.25rem;
	}

	.markdown-body :global(blockquote) {
		border-left: 3px solid var(--border-color);
		margin: 0 0 0.75rem 0;
		padding-left: 0.75rem;
		color: var(--text-muted);
	}

	.markdown-body :global(img) {
		max-width: 100%;
		border-radius: var(--radius-sm);
	}
</style>
