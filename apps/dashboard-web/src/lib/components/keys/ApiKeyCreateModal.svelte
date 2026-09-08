<script lang="ts">
	import { createEventDispatcher } from 'svelte';
	
	export let isOpen = false;
	export let projects: Array<{ id: string, name: string }> = [];
	// D27: the project-scoped page (settings/keys/+page.server.ts under projects/[projectId]) ALWAYS
	// mints a key scoped to `data.projectId` and ignores whatever `targetProject` this modal
	// dispatches — so rendering an "All Projects [Org-Wide]" option there let a user believe they
	// were creating an org-wide key while a project-scoped one was silently minted instead. The
	// project-scoped page passes `allowOrgWide={false}` to remove the misleading option entirely;
	// the org-level page (which DOES honour targetProject) leaves it at the default `true`.
	export let allowOrgWide = true;
	// D26: bound by the parent (via bind:isSubmitting) so a create failure that the parent handles
	// (toast + early return, modal stays open) can reset this back to false. Previously this was a
	// local-only variable that only handleClose ever reset — on any 400/403/network error the parent
	// toasts and returns without closing the modal, so the button stayed disabled reading "Creating..."
	// forever, with no way to retry short of a full page reload.
	export let isSubmitting = false;

	const ORG_WIDE_SENTINEL = 'All Projects [Org-Wide]';

	let name = '';
	let targetProject = allowOrgWide ? ORG_WIDE_SENTINEL : (projects[0]?.id ?? ORG_WIDE_SENTINEL);
	let scope = 'ingest';
	let rateLimitRpm = '';
	// N9: optional key lifetime in days. Empty = non-expiring (historical default).
	let expiresInDays = '';
	
	const dispatch = createEventDispatcher();
	
	const availableScopes = [
		{ id: 'ingest', label: 'Ingest (Ingest metrics & logs)', desc: 'Allows submitting telemetry events' },
		{ id: 'read', label: 'Read (Query telemetry data)', desc: 'Allows querying dashboards & logs' },
		{ id: 'admin', label: 'Admin (Full privileges)', desc: 'Full administrative access' }
	];

	function handleSubmit() {
		if (!name.trim()) return;
		isSubmitting = true;
		dispatch('create', {
			name: name.trim(),
			targetProject,
			scope,
			rateLimitRpm: rateLimitRpm ? parseInt(rateLimitRpm, 10) : undefined,
			expiresInDays: expiresInDays ? parseInt(expiresInDays, 10) : undefined
		});
	}

	function resetForm() {
		name = '';
		targetProject = allowOrgWide ? ORG_WIDE_SENTINEL : (projects[0]?.id ?? ORG_WIDE_SENTINEL);
		scope = 'ingest';
		rateLimitRpm = '';
		expiresInDays = '';
		isSubmitting = false;
	}

	// D26: `isSubmitting` latches on until something resets it, and only `handleClose` ever did.
	// Both failure and success left it stuck: on an error the parent toasts and returns WITHOUT
	// closing, so the button stayed disabled reading "Creating…" forever; on success the parent
	// sets `isOpen = false` directly rather than calling `handleClose`, so the NEXT open showed a
	// permanently disabled form. Resetting on the closed→open transition makes every open start
	// clean regardless of how the previous one ended, which is the only place that holds for both.
	let wasOpen = false;
	$: if (isOpen !== wasOpen) {
		wasOpen = isOpen;
		if (isOpen) resetForm();
	}

	function handleClose() {
		isOpen = false;
		resetForm();
		dispatch('close');
	}
</script>

{#if isOpen}
<div class="modal-backdrop">
	<div class="modal">
		<div class="modal-head">
			<h2 class="modal-title">Create API Key</h2>
			<button on:click={handleClose} class="modal-close" aria-label="Close">✕</button>
		</div>

		<form on:submit|preventDefault={handleSubmit} class="modal-form">
			<div class="field">
				<label for="key-name" class="field-label">Key Name</label>
				<input type="text" id="key-name" bind:value={name} class="field-input" required placeholder="e.g. Production Ingestion Service" />
			</div>

			<div class="field">
				<label for="target-project" class="field-label">Target Project</label>
				<select id="target-project" bind:value={targetProject} class="field-input">
					{#if allowOrgWide}
						<option value={ORG_WIDE_SENTINEL}>{ORG_WIDE_SENTINEL}</option>
					{/if}
					{#each projects as project}
						<option value={project.id}>{project.name}</option>
					{/each}
				</select>
			</div>

			<fieldset class="scope-set">
				<!--
					A `<label>` here (not associated with any single control -- it groups a set of radio
					inputs, not one) was flagged by a11y_label_has_associated_control. `<fieldset>` +
					`<legend>` is the correct HTML semantics for a labeled group of radio inputs, and gives
					the group an accessible name the way a bare label never could.
				-->
				<legend class="field-label">Scope</legend>
				<div class="scope-list">
					{#each availableScopes as sc}
						<label class="scope-option">
							<input type="radio" name="scope-group" value={sc.id} bind:group={scope} class="scope-radio" />
							<div>
								<div class="scope-label">{sc.label}</div>
								<div class="scope-desc">{sc.desc}</div>
							</div>
						</label>
					{/each}
				</div>
			</fieldset>

			<div class="field">
				<label for="rate-limit" class="field-label">Rate Limit Override (RPM)</label>
				<input type="number" id="rate-limit" bind:value={rateLimitRpm} class="field-input" placeholder="Default (6000 rpm)" />
			</div>

			<div class="field">
				<label for="expires-in-days" class="field-label">Expires In (Days)</label>
				<input type="number" id="expires-in-days" min="1" bind:value={expiresInDays} class="field-input" placeholder="Never expires" />
			</div>

			<div class="modal-actions">
				<button type="button" on:click={handleClose} class="btn btn-cancel">
					Cancel
				</button>
				<button type="submit" disabled={isSubmitting || !name.trim()} class="btn btn-primary">
					{isSubmitting ? 'Creating...' : 'Create Key'}
				</button>
			</div>
		</form>
	</div>
</div>
{/if}

<style>
	/* Rewritten from Tailwind utility classes, which never resolved — this project has no Tailwind. */
	.modal-backdrop {
		position: fixed;
		inset: 0;
		z-index: 50;
		display: flex;
		align-items: center;
		justify-content: center;
		padding: 1rem;
		background-color: rgba(0, 0, 0, 0.75);
		overflow-y: auto;
	}

	.modal {
		width: 100%;
		max-width: 28rem;
		padding: 1.5rem;
		background-color: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-lg);
		box-shadow: 0 20px 40px rgba(0, 0, 0, 0.5);
		color: var(--text-primary);
	}

	.modal-head {
		display: flex;
		justify-content: space-between;
		align-items: center;
		margin-bottom: 1.25rem;
	}

	.modal-title {
		font-size: 1.0625rem;
		font-weight: 600;
	}

	.modal-close {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 0.875rem;
		font-weight: 600;
		cursor: pointer;
	}

	.modal-close:hover {
		color: var(--text-primary);
	}

	.modal-form {
		display: flex;
		flex-direction: column;
		gap: 1rem;
	}

	.field-label {
		display: block;
		font-size: 0.75rem;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
		margin-bottom: 0.25rem;
		padding: 0;
	}

	.field-input {
		width: 100%;
		padding: 0.5rem 0.75rem;
		font-size: 0.875rem;
	}

	.scope-set {
		border: 0;
		padding: 0;
		margin: 0;
	}

	.scope-list {
		display: flex;
		flex-direction: column;
		gap: 0.5rem;
	}

	.scope-option {
		display: flex;
		align-items: flex-start;
		gap: 0.625rem;
		padding: 0.5rem;
		border-radius: var(--radius-md);
		background-color: var(--bg-root);
		border: 1px solid var(--border-color);
		cursor: pointer;
	}

	.scope-option:hover {
		border-color: var(--color-primary);
	}

	.scope-radio {
		margin-top: 0.25rem;
		accent-color: var(--color-primary);
	}

	.scope-label {
		font-size: 0.75rem;
		font-weight: 500;
		color: var(--text-primary);
	}

	.scope-desc {
		font-size: 0.6875rem;
		color: var(--text-subtle);
	}

	.modal-actions {
		display: flex;
		justify-content: flex-end;
		gap: 0.75rem;
		padding-top: 0.75rem;
		border-top: 1px solid var(--border-color);
	}

	.btn {
		padding: 0.5rem 1rem;
		border-radius: var(--radius-md);
		font-size: 0.75rem;
		font-weight: 600;
		border: none;
		cursor: pointer;
	}

	.btn:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	.btn-cancel {
		background-color: var(--bg-surface-hover);
		color: var(--text-primary);
		font-weight: 500;
	}

	.btn-primary {
		background-color: var(--status-resolved-text);
		color: var(--bg-root);
	}

	.btn-primary:hover:not(:disabled) {
		opacity: 0.9;
	}
</style>
