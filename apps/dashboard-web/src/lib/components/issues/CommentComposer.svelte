<script lang="ts">
	// Manual Issues M3 (docs/plans/MANUAL_ISSUES_DESIGN.md §5/§3/§10): Markdown textarea with a
	// Write/Preview toggle plus a drag-drop upload zone, reused for both the root composer and each
	// reply composer in CommentThread.svelte. Mirrors the report-create form's textarea+preview
	// pattern (§3) and reuses the M2 UploadZone verbatim rather than re-implementing upload
	// machinery -- this component's own job is just "collect bodyMd + attachmentIds and call
	// onsubmit".
	import Markdown from '$lib/components/Markdown.svelte';
	import UploadZone from '$lib/components/attachments/UploadZone.svelte';

	interface Props {
		organizationId: string;
		placeholder?: string;
		submitLabel?: string;
		autofocus?: boolean;
		onsubmit: (input: { bodyMd: string; attachmentIds: string[] }) => Promise<void> | void;
		oncancel?: () => void;
	}

	let {
		organizationId,
		placeholder = 'Write a comment…',
		submitLabel = 'Comment',
		autofocus = false,
		onsubmit,
		oncancel,
	}: Props = $props();

	let mode = $state<'write' | 'preview'>('write');
	let body = $state('');
	let attachmentIds = $state<string[]>([]);
	let submitting = $state(false);
	let submitError = $state<string | null>(null);
	let textareaEl: HTMLTextAreaElement | undefined = $state();

	// a11y: avoid the `autofocus` HTML attribute (svelte-check flags it); focus imperatively
	// instead when this composer is mounted as the active reply target.
	$effect(() => {
		if (autofocus) textareaEl?.focus();
	});

	function handleAttachmentsChange(ids: string[]) {
		attachmentIds = ids;
	}

	function handleInsert(markdown: string) {
		body = body.length > 0 ? `${body}\n${markdown}` : markdown;
	}

	async function handleSubmit(event: Event) {
		event.preventDefault();
		const trimmed = body.trim();
		if (trimmed.length === 0) return;

		submitError = null;
		submitting = true;
		try {
			await onsubmit({ bodyMd: trimmed, attachmentIds });
			body = '';
			attachmentIds = [];
			mode = 'write';
		} catch (err) {
			submitError = err instanceof Error ? err.message : 'Failed to post comment';
		} finally {
			submitting = false;
		}
	}
</script>

<form class="comment-composer" onsubmit={handleSubmit}>
	<div class="composer-tabs" role="tablist" aria-label="Comment editor mode">
		<button
			type="button"
			class="mode-tab"
			class:active={mode === 'write'}
			role="tab"
			aria-selected={mode === 'write'}
			onclick={() => (mode = 'write')}
		>
			Write
		</button>
		<button
			type="button"
			class="mode-tab"
			class:active={mode === 'preview'}
			role="tab"
			aria-selected={mode === 'preview'}
			onclick={() => (mode = 'preview')}
		>
			Preview
		</button>
	</div>

	{#if mode === 'write'}
		<textarea
			bind:this={textareaEl}
			bind:value={body}
			{placeholder}
			class="composer-textarea"
			rows="3"
			aria-label={placeholder}
		></textarea>
		<UploadZone {organizationId} onchange={handleAttachmentsChange} oninsert={handleInsert} />
	{:else}
		<div class="composer-preview" data-testid="composer-preview">
			{#if body.trim().length > 0}
				<Markdown source={body} />
			{:else}
				<p class="preview-empty">Nothing to preview yet.</p>
			{/if}
		</div>
	{/if}

	{#if submitError}
		<p class="composer-error" role="alert">{submitError}</p>
	{/if}

	<div class="composer-actions">
		{#if oncancel}
			<button type="button" class="btn-secondary" onclick={oncancel}>Cancel</button>
		{/if}
		<button type="submit" class="btn-primary" disabled={submitting || body.trim().length === 0}>
			{submitting ? 'Posting…' : submitLabel}
		</button>
	</div>
</form>

<style>
	.comment-composer {
		display: flex;
		flex-direction: column;
		gap: 0.5rem;
	}

	.composer-tabs {
		display: flex;
		gap: 0.25rem;
	}

	.mode-tab {
		background: none;
		border: none;
		border-bottom: 2px solid transparent;
		color: var(--text-muted);
		font-size: 0.75rem;
		font-weight: 500;
		padding: 0.25rem 0.5rem;
		cursor: pointer;
	}

	.mode-tab.active {
		color: var(--text-primary);
		border-bottom-color: var(--color-primary);
	}

	.composer-textarea {
		width: 100%;
		box-sizing: border-box;
		background: var(--bg-root);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.5rem;
		font-size: 0.8125rem;
		font-family: inherit;
		resize: vertical;
	}

	.composer-preview {
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.5rem;
		min-height: 3rem;
		background: var(--bg-root);
	}

	.preview-empty {
		margin: 0;
		color: var(--text-muted);
		font-size: 0.75rem;
	}

	.composer-error {
		color: #ef4444;
		font-size: 0.75rem;
		margin: 0;
	}

	.composer-actions {
		display: flex;
		justify-content: flex-end;
		gap: 0.5rem;
	}

	.btn-primary,
	.btn-secondary {
		border: none;
		border-radius: var(--radius-sm);
		padding: 0.375rem 0.75rem;
		font-size: 0.75rem;
		font-weight: 600;
		cursor: pointer;
	}

	.btn-primary {
		background: var(--color-primary);
		color: #fff;
	}

	.btn-primary:hover {
		background: var(--color-primary-hover);
	}

	.btn-primary:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}

	.btn-secondary {
		background: var(--bg-root);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
	}

	.btn-secondary:hover {
		background: var(--bg-surface-hover);
	}
</style>
