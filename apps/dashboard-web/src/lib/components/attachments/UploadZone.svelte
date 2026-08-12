<script lang="ts">
	// Manual Issues M2 (docs/plans/MANUAL_ISSUES_DESIGN.md §4/§10): drag-drop + file-picker upload
	// zone for /reports/new. Each dropped/picked file is POSTed individually to /api/uploads as
	// soon as it is added -- the report create call only ever sends attachmentIds[] afterward
	// (createManualIssue's claimDraftAttachments does the actual linking), so this component's job
	// ends at "get the file into a DRAFT attachment row and track its id".
	//
	// Progress is tracked per-file as a status machine (pending -> uploading -> done|error) rather
	// than a byte-level percentage: the Fetch API gives no upload-progress signal without piping
	// through a ReadableStream, which would add real complexity for a v1 cap of 25 MB files where
	// "uploading…" is enough feedback. XHR would give byte progress but this component is tested
	// by mocking `fetch`, matching every other component test in this codebase (see
	// IssueRelations.test.ts) -- consistency with that convention outweighs the finer-grained bar.
	interface UploadedAttachment {
		id: string;
		url: string;
		filename: string;
		contentType: string;
		sizeBytes: number;
	}

	type FileStatus = 'uploading' | 'done' | 'error';

	interface TrackedFile {
		key: string;
		file: File;
		status: FileStatus;
		errorMessage?: string;
		attachment?: UploadedAttachment;
	}

	interface Props {
		organizationId: string;
		/** Called whenever the set of successfully uploaded attachment ids changes. */
		onchange?: (attachmentIds: string[]) => void;
		/** Called when the user asks to insert an uploaded image into the Markdown body. */
		oninsert?: (markdown: string) => void;
	}

	let { organizationId, onchange, oninsert }: Props = $props();

	let files = $state<TrackedFile[]>([]);
	let dragActive = $state(false);
	let fileInput: HTMLInputElement | undefined = $state();

	function isImage(contentType: string | undefined): boolean {
		return !!contentType && contentType.startsWith('image/');
	}

	function formatBytes(bytes: number): string {
		if (bytes < 1024) return `${bytes} B`;
		if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
		return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
	}

	function notifyChange() {
		onchange?.(files.filter((f) => f.status === 'done' && f.attachment).map((f) => f.attachment!.id));
	}

	async function uploadOne(tracked: TrackedFile) {
		try {
			const formData = new FormData();
			formData.append('organizationId', organizationId);
			formData.append('file', tracked.file);
			formData.append('filename', tracked.file.name);

			const res = await fetch('/api/uploads', { method: 'POST', body: formData });

			if (!res.ok) {
				const body = await res.json().catch(() => ({}));
				throw new Error(body.message || `Upload failed (${res.status})`);
			}

			const attachment: UploadedAttachment = await res.json();

			files = files.map((f) => (f.key === tracked.key ? { ...f, status: 'done', attachment } : f));
		} catch (err) {
			files = files.map((f) =>
				f.key === tracked.key
					? { ...f, status: 'error', errorMessage: err instanceof Error ? err.message : 'Upload failed' }
					: f
			);
		} finally {
			notifyChange();
		}
	}

	function addFiles(fileList: FileList | File[]) {
		const newTracked: TrackedFile[] = Array.from(fileList).map((file) => ({
			key: `${file.name}-${file.size}-${Date.now()}-${Math.random().toString(36).slice(2)}`,
			file,
			status: 'uploading' as FileStatus,
		}));

		files = [...files, ...newTracked];
		for (const tracked of newTracked) {
			void uploadOne(tracked);
		}
	}

	function handleInputChange(event: Event) {
		const target = event.target as HTMLInputElement;
		if (target.files && target.files.length > 0) {
			addFiles(target.files);
		}
		target.value = '';
	}

	function handleDrop(event: DragEvent) {
		event.preventDefault();
		dragActive = false;
		if (event.dataTransfer?.files && event.dataTransfer.files.length > 0) {
			addFiles(event.dataTransfer.files);
		}
	}

	function handleDragOver(event: DragEvent) {
		event.preventDefault();
		dragActive = true;
	}

	function handleDragLeave() {
		dragActive = false;
	}

	async function removeFile(tracked: TrackedFile) {
		if (tracked.status === 'done' && tracked.attachment) {
			try {
				await fetch(`/api/attachments/${tracked.attachment.id}`, { method: 'DELETE' });
			} catch {
				// Best-effort -- the reaper will clean up an orphaned draft even if this call fails
				// (24h sweep, attachment-reaper.ts). The user's intent ("get this out of my list") is
				// still honored locally below.
			}
		}
		files = files.filter((f) => f.key !== tracked.key);
		notifyChange();
	}

	function insertIntoBody(tracked: TrackedFile) {
		if (!tracked.attachment) return;
		oninsert?.(`![${tracked.attachment.filename}](${tracked.attachment.url})`);
	}
</script>

<div class="upload-zone-wrapper">
	<div
		class="dropzone"
		class:active={dragActive}
		role="button"
		tabindex="0"
		aria-label="Upload attachments"
		ondrop={handleDrop}
		ondragover={handleDragOver}
		ondragleave={handleDragLeave}
		onclick={() => fileInput?.click()}
		onkeydown={(e) => {
			if (e.key === 'Enter' || e.key === ' ') {
				e.preventDefault();
				fileInput?.click();
			}
		}}
	>
		<p class="dropzone-text">Drag and drop files here, or click to browse</p>
		<p class="dropzone-hint">Images, video, PDF, text, doc, zip — up to 25 MB each</p>
	</div>
	<input
		bind:this={fileInput}
		type="file"
		multiple
		class="file-input-hidden"
		onchange={handleInputChange}
		aria-label="Choose files"
	/>

	{#if files.length > 0}
		<ul class="file-list" data-testid="upload-file-list">
			{#each files as tracked (tracked.key)}
				<li class="file-row">
					{#if tracked.status === 'done' && tracked.attachment && isImage(tracked.attachment.contentType)}
						<img class="file-thumb" src={tracked.attachment.url} alt={tracked.attachment.filename} />
					{:else}
						<span class="file-thumb file-thumb-placeholder" aria-hidden="true">&#128206;</span>
					{/if}

					<div class="file-info">
						<span class="file-name">{tracked.file.name}</span>
						<span class="file-meta">
							{formatBytes(tracked.file.size)}
							{#if tracked.status === 'uploading'}
								<span class="status status-uploading">Uploading…</span>
							{:else if tracked.status === 'done'}
								<span class="status status-done">Uploaded</span>
							{:else if tracked.status === 'error'}
								<span class="status status-error">{tracked.errorMessage ?? 'Failed'}</span>
							{/if}
						</span>
					</div>

					<div class="file-actions">
						{#if tracked.status === 'done' && tracked.attachment && isImage(tracked.attachment.contentType)}
							<button type="button" class="btn-link" onclick={() => insertIntoBody(tracked)}>
								Insert into body
							</button>
						{/if}
						<button type="button" class="btn-remove" onclick={() => removeFile(tracked)} aria-label={`Remove ${tracked.file.name}`}>
							Remove
						</button>
					</div>
				</li>
			{/each}
		</ul>
	{/if}
</div>

<style>
	.upload-zone-wrapper {
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
	}

	.dropzone {
		border: 2px dashed var(--border-color);
		border-radius: var(--radius-md);
		padding: 1.25rem;
		text-align: center;
		cursor: pointer;
		background: var(--bg-root);
	}

	.dropzone.active {
		border-color: var(--color-primary);
		background: var(--bg-surface-hover);
	}

	.dropzone-text {
		margin: 0 0 0.25rem 0;
		font-size: 0.8125rem;
		color: var(--text-primary);
		font-weight: 500;
	}

	.dropzone-hint {
		margin: 0;
		font-size: 0.6875rem;
		color: var(--text-muted);
	}

	.file-input-hidden {
		position: absolute;
		width: 1px;
		height: 1px;
		opacity: 0;
		overflow: hidden;
		pointer-events: none;
	}

	.file-list {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.5rem;
	}

	.file-row {
		display: flex;
		align-items: center;
		gap: 0.625rem;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.5rem;
		background: var(--bg-surface);
	}

	.file-thumb {
		width: 2.5rem;
		height: 2.5rem;
		object-fit: cover;
		border-radius: var(--radius-sm);
		flex-shrink: 0;
	}

	.file-thumb-placeholder {
		display: flex;
		align-items: center;
		justify-content: center;
		background: var(--bg-root);
		border: 1px solid var(--border-color);
		font-size: 1.125rem;
	}

	.file-info {
		display: flex;
		flex-direction: column;
		gap: 0.125rem;
		flex: 1;
		min-width: 0;
	}

	.file-name {
		font-size: 0.8125rem;
		color: var(--text-primary);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.file-meta {
		font-size: 0.6875rem;
		color: var(--text-muted);
		display: flex;
		gap: 0.5rem;
		align-items: center;
	}

	.status-uploading {
		color: var(--text-muted);
	}

	.status-done {
		color: var(--status-resolved-text);
	}

	.status-error {
		color: #ef4444;
	}

	.file-actions {
		display: flex;
		gap: 0.5rem;
		align-items: center;
		flex-shrink: 0;
	}

	.btn-link {
		background: none;
		border: none;
		color: var(--color-primary);
		font-size: 0.75rem;
		cursor: pointer;
		padding: 0;
	}

	.btn-link:hover {
		text-decoration: underline;
	}

	.btn-remove {
		background: none;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		color: var(--text-muted);
		font-size: 0.75rem;
		padding: 0.25rem 0.5rem;
		cursor: pointer;
	}

	.btn-remove:hover {
		color: #ef4444;
		border-color: #ef4444;
	}
</style>
