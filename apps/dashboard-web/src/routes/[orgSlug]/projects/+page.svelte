<script lang="ts">
	import { enhance } from '$app/forms';
	import type { PageData, ActionData } from './$types';

	export let data: PageData;
	export let form: ActionData;

	let showForm = false;
	let loading = false;

	// Only the validation failure echoes the submitted name back; the permission failure does not.
	$: draftName = (form as { values?: { name?: string } } | null)?.values?.name ?? '';
</script>

<div class="projects-index">
	<header class="page-header">
		<div>
			<h1 class="page-title">Projects</h1>
			<p class="page-subtitle">{data.org.name}</p>
		</div>
		{#if data.canCreate && !showForm}
			<button type="button" class="btn btn-primary" on:click={() => (showForm = true)}>New project</button>
		{/if}
	</header>

	{#if data.canCreate && showForm}
		<form
			method="POST"
			action="?/create"
			class="create-form"
			use:enhance={() => {
				loading = true;
				return async ({ result, update }) => {
					await update();
					loading = false;
					// Keep the form open on a failure so the error sits next to the field it is about;
					// close it once the project exists and is in the list below.
					if (result.type === 'success') showForm = false;
				};
			}}
		>
			<label class="field">
				<span class="field-label">Project name</span>
				<input
					type="text"
					name="name"
					placeholder="Web app"
					required
					maxlength="255"
					class="input"
					value={draftName}
				/>
			</label>
			{#if form?.error}
				<p class="form-error" role="alert">{form.error}</p>
			{/if}
			<div class="form-actions">
				<button type="submit" disabled={loading} class="btn btn-primary">
					{loading ? 'Creating…' : 'Create project'}
				</button>
				<button type="button" class="btn btn-secondary" disabled={loading} on:click={() => (showForm = false)}>
					Cancel
				</button>
			</div>
		</form>
	{/if}

	{#if form?.created}
		<p class="form-success" role="status">Created <strong>{form.created.name}</strong>.</p>
	{/if}

	{#if data.projects.length === 0}
		{#if data.canCreate}
			<p class="empty">No projects yet. Use <strong>New project</strong> to create the first one.</p>
		{:else}
			<p class="empty">No projects yet. Ask an owner, admin or engineer of {data.org.name} to create one.</p>
		{/if}
	{:else}
		<ul class="project-list">
			{#each data.projects as project}
				<li class="project-row">
					<span class="project-name">{project.name}</span>
					<span class="project-links">
						<a href="/{data.org.slug}/projects/{project.id}/settings/keys">API keys</a>
						<a href="/{data.org.slug}/projects/{project.id}/settings">Settings</a>
						<!--
							sentinel-releases-snooze §4.4, flow `R1` (decision 25). Unconditional, for every row
							and every member: there is no `releasesVisible` on page data and none is coming
							(decision 28) — an environment-scoped member sees the link and the page answers
							`404 Project not found` (decision 20). The loader decides who gets the page.
						-->
						<a href="/{data.org.slug}/projects/{project.id}/releases">Releases</a>
					</span>
				</li>
			{/each}
		</ul>
	{/if}
</div>

<style>
	.projects-index {
		max-width: 960px;
		margin: 0 auto;
		padding: 2rem 1rem;
	}

	.page-header {
		display: flex;
		align-items: flex-start;
		justify-content: space-between;
		gap: 1rem;
		margin-bottom: 1.25rem;
	}

	.create-form {
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		background-color: var(--bg-surface);
		padding: 1rem;
		margin-bottom: 1.25rem;
	}

	.field {
		display: block;
		margin-bottom: 0.75rem;
	}

	.field-label {
		display: block;
		font-size: 0.8125rem;
		font-weight: 500;
		color: var(--text-primary);
		margin-bottom: 0.375rem;
	}

	.input {
		width: 100%;
		padding: 0.625rem 0.875rem;
		font-size: 0.875rem;
	}

	.form-error {
		color: var(--severity-critical-text);
		font-size: 0.8125rem;
		margin-bottom: 0.75rem;
	}

	.form-success {
		font-size: 0.8125rem;
		color: var(--text-muted);
		margin-bottom: 0.75rem;
	}

	.form-actions {
		display: flex;
		gap: 0.5rem;
	}

	.btn {
		padding: 0.625rem 1rem;
		border-radius: var(--radius-sm);
		font-size: 0.875rem;
		font-weight: 500;
		cursor: pointer;
		border: none;
	}

	.btn-primary {
		background-color: var(--color-primary);
		color: var(--text-primary);
	}

	.btn-primary:hover:not(:disabled) {
		background-color: var(--color-primary-hover);
	}

	.btn-secondary {
		background-color: transparent;
		color: var(--text-muted);
		border: 1px solid var(--border-color);
	}

	.btn:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	.page-title {
		font-size: 1.375rem;
		font-weight: 600;
		color: var(--text-primary);
		margin-bottom: 0.25rem;
	}

	.page-subtitle {
		font-size: 0.8125rem;
		color: var(--text-muted);
	}

	.project-list {
		list-style: none;
		padding: 0;
		margin: 0;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		overflow: hidden;
	}

	.project-row {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 1rem;
		padding: 0.875rem 1rem;
		background-color: var(--bg-surface);
	}

	.project-row + .project-row {
		border-top: 1px solid var(--border-color);
	}

	.project-name {
		font-size: 0.875rem;
		font-weight: 500;
		color: var(--text-primary);
	}

	.project-links {
		display: flex;
		gap: 1rem;
	}

	.project-links a {
		font-size: 0.8125rem;
		color: var(--color-primary);
		text-decoration: none;
	}

	.project-links a:hover {
		text-decoration: underline;
	}

	.empty {
		font-size: 0.8125rem;
		color: var(--text-muted);
	}
</style>
