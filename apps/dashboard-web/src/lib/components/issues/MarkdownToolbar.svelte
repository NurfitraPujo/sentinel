<script lang="ts">
	// M6 Feature B (docs/plans/M6_PRESIGNED_UPLOADS_AND_TOOLBAR_PLAN.md §Feature B): a
	// dependency-free row of Markdown-syntax buttons that sits above a Write-mode textarea. Reads
	// the textarea's current selection, delegates the actual text transform to the pure
	// applyMarkdownAction helper (fully unit-tested separately), calls onchange with the new value,
	// then restores focus + selection on the textarea once Svelte has re-rendered it (tick()).
	//
	// No upload logic here -- this composes alongside the existing UploadZone, it does not replace
	// or wrap it.
	import { tick } from 'svelte';
	import { applyMarkdownAction, type MarkdownToolbarAction } from '$lib/markdown-toolbar';

	interface Props {
		textarea: HTMLTextAreaElement | undefined;
		value: string;
		onchange: (value: string) => void;
	}

	let { textarea, value, onchange }: Props = $props();

	interface ToolbarButton {
		action: MarkdownToolbarAction;
		label: string;
		glyph: string;
	}

	const buttons: ToolbarButton[] = [
		{ action: 'bold', label: 'Bold', glyph: 'B' },
		{ action: 'italic', label: 'Italic', glyph: 'I' },
		{ action: 'strikethrough', label: 'Strikethrough', glyph: 'S' },
		{ action: 'code', label: 'Code', glyph: '</>' },
		{ action: 'heading', label: 'Heading', glyph: 'H' },
		{ action: 'quote', label: 'Quote', glyph: '“' },
		{ action: 'ul', label: 'Bulleted list', glyph: '•' },
		{ action: 'ol', label: 'Numbered list', glyph: '1.' },
		{ action: 'link', label: 'Link', glyph: '🔗' },
	];

	async function handleClick(action: MarkdownToolbarAction) {
		const el = textarea;
		const selStart = el?.selectionStart ?? value.length;
		const selEnd = el?.selectionEnd ?? value.length;

		const next = applyMarkdownAction(value, selStart, selEnd, action);
		onchange(next.text);

		await tick();
		if (el) {
			el.focus();
			el.setSelectionRange(next.selStart, next.selEnd);
		}
	}
</script>

<div class="markdown-toolbar" role="toolbar" aria-label="Markdown formatting">
	{#each buttons as button (button.action)}
		<button
			type="button"
			class="toolbar-btn"
			aria-label={button.label}
			title={button.label}
			onclick={() => handleClick(button.action)}
		>
			{button.glyph}
		</button>
	{/each}
</div>

<style>
	.markdown-toolbar {
		display: flex;
		flex-wrap: wrap;
		gap: 0.25rem;
	}

	.toolbar-btn {
		background: var(--bg-root);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		font-size: 0.75rem;
		font-weight: 600;
		line-height: 1;
		padding: 0.3rem 0.5rem;
		cursor: pointer;
		min-width: 1.75rem;
	}

	.toolbar-btn:hover {
		background: var(--bg-surface-hover);
	}

	.toolbar-btn:focus-visible {
		outline: 2px solid var(--color-primary);
		outline-offset: 1px;
	}
</style>
