<script lang="ts">
	// Manual Issues M2 (docs/plans/MANUAL_ISSUES_DESIGN.md §4/§10): read-only attachments section
	// for the report detail page. Images render inline as thumbnails via the authorized download
	// URL (/api/attachments/<id> -- GET already enforces read access to the parent issue, see
	// routes/api/attachments/[id]/+server.ts); every other content type renders as a download link
	// showing filename + human size. Purely presentational -- no fetch of its own, the list comes
	// from the page's server load (listIssueAttachments).
	export interface AttachmentSummary {
		id: string;
		filename: string;
		contentType: string;
		sizeBytes: number;
	}

	interface Props {
		attachments: AttachmentSummary[];
	}

	let { attachments }: Props = $props();

	function isImage(contentType: string): boolean {
		return contentType.startsWith('image/');
	}

	function formatBytes(bytes: number): string {
		if (bytes < 1024) return `${bytes} B`;
		if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
		return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
	}
</script>

{#if attachments.length > 0}
	<div class="attachment-list" data-testid="attachment-list">
		{#each attachments as attachment (attachment.id)}
			{#if isImage(attachment.contentType)}
				<a
					class="attachment-thumb"
					href={`/api/attachments/${attachment.id}`}
					target="_blank"
					rel="noopener noreferrer"
					title={attachment.filename}
				>
					<img src={`/api/attachments/${attachment.id}`} alt={attachment.filename} loading="lazy" />
					<span class="thumb-caption">{attachment.filename}</span>
				</a>
			{:else}
				<a class="attachment-file" href={`/api/attachments/${attachment.id}`} download={attachment.filename}>
					<span class="file-icon" aria-hidden="true">&#128206;</span>
					<span class="file-name">{attachment.filename}</span>
					<span class="file-size">{formatBytes(attachment.sizeBytes)}</span>
				</a>
			{/if}
		{/each}
	</div>
{:else}
	<p class="attachment-empty">No attachments.</p>
{/if}

<style>
	.attachment-list {
		display: flex;
		flex-wrap: wrap;
		gap: 0.75rem;
	}

	.attachment-empty {
		font-size: 0.8125rem;
		color: var(--text-muted);
		margin: 0;
	}

	.attachment-thumb {
		display: flex;
		flex-direction: column;
		gap: 0.25rem;
		width: 8rem;
		text-decoration: none;
	}

	.attachment-thumb img {
		width: 8rem;
		height: 6rem;
		object-fit: cover;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		background: var(--bg-root);
	}

	.thumb-caption {
		font-size: 0.6875rem;
		color: var(--text-muted);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.attachment-file {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.5rem 0.75rem;
		text-decoration: none;
		color: var(--text-primary);
		font-size: 0.8125rem;
		background: var(--bg-root);
	}

	.attachment-file:hover {
		background: var(--bg-surface-hover);
	}

	.file-icon {
		font-size: 1rem;
	}

	.file-name {
		font-weight: 500;
	}

	.file-size {
		color: var(--text-muted);
		font-size: 0.75rem;
	}
</style>
