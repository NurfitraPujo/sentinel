<script lang="ts">
	import { goto } from '$app/navigation';
	import Markdown from '$lib/components/Markdown.svelte';
	import type { PageData } from './$types';

	let { data }: { data: PageData } = $props();

	const SEVERITIES = ['low', 'medium', 'high', 'critical'] as const;

	let projectId = $state('');
	let title = $state('');
	let severity = $state<(typeof SEVERITIES)[number]>('medium');
	let bodyMd = $state('');
	let showPreview = $state(false);
	let submitting = $state(false);
	let errorMessage = $state<string | null>(null);

	async function handleSubmit(event: SubmitEvent) {
		event.preventDefault();
		errorMessage = null;

		if (title.trim().length === 0) {
			errorMessage = 'Title is required';
			return;
		}
		if (bodyMd.trim().length === 0) {
			errorMessage = 'Report body is required';
			return;
		}

		submitting = true;
		try {
			const res = await fetch(`/api/organizations/${data.orgId}/reports`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({
					title,
					bodyMd,
					severity,
					projectId: projectId || null,
				}),
			});

			if (!res.ok) {
				const body = await res.json().catch(() => ({}));
				throw new Error(body.message || `Failed to create report (${res.status})`);
			}

			const created = await res.json();
			await goto(`/${data.orgSlug}/reports/${created.issue.id}`);
		} catch (err) {
			errorMessage = err instanceof Error ? err.message : 'Failed to create report';
		} finally {
			submitting = false;
		}
	}
</script>

<div class="new-report-page">
	<h1 class="page-title">New report</h1>

	{#if errorMessage}
		<div class="error-banner" role="alert">{errorMessage}</div>
	{/if}

	<form class="report-form" onsubmit={handleSubmit}>
		<label class="field">
			<span class="field-label">Project</span>
			<select bind:value={projectId} class="field-input">
				<option value="">Not sure? Leave blank → Triage</option>
				{#each data.projects as project}
					<option value={project.id}>{project.name}</option>
				{/each}
			</select>
		</label>

		<label class="field">
			<span class="field-label">Title</span>
			<input type="text" bind:value={title} class="field-input" placeholder="Short summary of the issue" required />
		</label>

		<label class="field">
			<span class="field-label">Severity</span>
			<select bind:value={severity} class="field-input">
				{#each SEVERITIES as s}
					<option value={s}>{s}</option>
				{/each}
			</select>
		</label>

		<div class="field">
			<div class="body-header">
				<span class="field-label">Description (Markdown)</span>
				<div class="preview-toggle" role="tablist" aria-label="Markdown edit or preview">
					<button type="button" class="toggle-btn" class:active={!showPreview} onclick={() => (showPreview = false)}>
						Write
					</button>
					<button type="button" class="toggle-btn" class:active={showPreview} onclick={() => (showPreview = true)}>
						Preview
					</button>
				</div>
			</div>

			{#if showPreview}
				<div class="preview-box">
					<Markdown source={bodyMd} />
				</div>
			{:else}
				<textarea
					bind:value={bodyMd}
					class="field-input body-textarea"
					rows="10"
					placeholder="Describe what happened, steps to reproduce, expected vs actual behavior…"
					required
				></textarea>
			{/if}
		</div>

		<div class="form-actions">
			<button type="submit" class="btn-submit" disabled={submitting}>
				{submitting ? 'Submitting…' : 'Submit report'}
			</button>
			<a href="/{data.orgSlug}/reports" class="btn-cancel">Cancel</a>
		</div>
	</form>
</div>

<style>
	.new-report-page {
		max-width: 42rem;
		margin: 0 auto;
	}

	.page-title {
		font-size: 1.25rem;
		font-weight: 700;
		color: var(--text-primary);
		margin: 0 0 1rem 0;
	}

	.error-banner {
		background: rgba(239, 68, 68, 0.15);
		border: 1px solid rgba(239, 68, 68, 0.3);
		color: #ef4444;
		padding: 0.625rem 0.875rem;
		border-radius: var(--radius-sm);
		font-size: 0.8125rem;
		margin-bottom: 1rem;
	}

	.report-form {
		display: flex;
		flex-direction: column;
		gap: 1rem;
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		padding: 1.25rem;
	}

	.field {
		display: flex;
		flex-direction: column;
		gap: 0.375rem;
	}

	.field-label {
		font-size: 0.75rem;
		font-weight: 600;
		color: var(--text-muted);
	}

	.field-input {
		background: var(--bg-root);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.5rem 0.625rem;
		font-size: 0.875rem;
		font-family: inherit;
	}

	.field-input:focus {
		outline: none;
		border-color: var(--color-primary);
	}

	.body-header {
		display: flex;
		justify-content: space-between;
		align-items: center;
	}

	.preview-toggle {
		display: flex;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		overflow: hidden;
	}

	.toggle-btn {
		background: var(--bg-root);
		color: var(--text-muted);
		border: none;
		padding: 0.25rem 0.625rem;
		font-size: 0.75rem;
		cursor: pointer;
	}

	.toggle-btn.active {
		background: var(--color-primary);
		color: #fff;
	}

	.body-textarea {
		resize: vertical;
		font-family: var(--font-mono);
		font-size: 0.8125rem;
	}

	.preview-box {
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.75rem;
		min-height: 12rem;
		background: var(--bg-root);
	}

	.form-actions {
		display: flex;
		gap: 0.75rem;
		align-items: center;
	}

	.btn-submit {
		background: var(--color-primary);
		color: #fff;
		border: none;
		padding: 0.5rem 1rem;
		border-radius: var(--radius-sm);
		font-size: 0.8125rem;
		font-weight: 600;
		cursor: pointer;
	}

	.btn-submit:hover {
		background: var(--color-primary-hover);
	}

	.btn-submit:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}

	.btn-cancel {
		color: var(--text-muted);
		text-decoration: none;
		font-size: 0.8125rem;
	}

	.btn-cancel:hover {
		color: var(--text-primary);
	}
</style>
