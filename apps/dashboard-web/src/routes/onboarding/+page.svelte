<script lang="ts">
	import { enhance } from '$app/forms';
	import type { PageData, ActionData } from './$types';

	export let data: PageData;
	export let form: ActionData;

	let loading = false;
	let name = '';
	let slug = '';
	let slugTouched = false;

	// Derive the slug from the name until the user edits it themselves — the slug is a URL segment
	// most people should never have to think about, but it stays editable because it is permanent.
	$: if (!slugTouched) {
		slug = name
			.toLowerCase()
			.replace(/[^a-z0-9]+/g, '-')
			.replace(/^-+|-+$/g, '')
			.slice(0, 255);
	}
</script>

<div class="onboarding">
	<div class="panel">
		<div class="panel-header">
			<div class="step-label">{data.organization ? 'Step 2 of 2' : 'Step 1 of 2'}</div>
			<h1 class="panel-title">
				{data.organization ? 'Create your first project' : 'Create your organization'}
			</h1>
			<p class="panel-subtitle">
				{#if data.organization}
					A project is what errors are reported against. You can add more later, and issue an SDK
					key from the project's settings once it exists.
				{:else}
					Everything in Sentinel belongs to an organization — projects, members and API keys.
				{/if}
			</p>
		</div>

		{#if form?.error}
			<p class="form-error">{form.error}</p>
		{/if}

		{#if data.organization}
			<form
				method="POST"
				action="?/createProject"
				use:enhance={() => {
					loading = true;
					return async ({ update }) => {
						await update();
						loading = false;
					};
				}}
			>
				<label class="field">
					<span class="field-label">Project name</span>
					<input name="name" placeholder="Web app" required maxlength="255" class="input" />
				</label>
				<button type="submit" disabled={loading} class="btn btn-primary btn-full">
					{loading ? 'Creating…' : 'Create project'}
				</button>
			</form>
			<p class="org-note">Organization: <strong>{data.organization.name}</strong></p>
		{:else}
			<form
				method="POST"
				action="?/createOrg"
				use:enhance={() => {
					loading = true;
					return async ({ update }) => {
						await update();
						loading = false;
					};
				}}
			>
				<label class="field">
					<span class="field-label">Organization name</span>
					<input
						name="name"
						bind:value={name}
						placeholder="Acme Engineering"
						required
						maxlength="255"
						class="input"
					/>
				</label>

				<label class="field">
					<span class="field-label">URL slug</span>
					<input
						name="slug"
						bind:value={slug}
						on:input={() => (slugTouched = true)}
						placeholder="acme-engineering"
						required
						maxlength="255"
						pattern="[a-z0-9][a-z0-9\-]*"
						class="input"
					/>
					<span class="field-hint">Lowercase letters, numbers and hyphens. Used in your URLs.</span>
				</label>

				<button type="submit" disabled={loading} class="btn btn-primary btn-full">
					{loading ? 'Creating…' : 'Create organization'}
				</button>
			</form>
		{/if}
	</div>
</div>

<style>
	.onboarding {
		min-height: calc(100vh - 120px);
		display: flex;
		align-items: center;
		justify-content: center;
		padding: 2rem 1rem;
	}

	.panel {
		width: 100%;
		max-width: 460px;
		background-color: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-lg);
		padding: 2rem;
		box-shadow: 0 10px 30px rgba(0, 0, 0, 0.35);
	}

	.panel-header {
		margin-bottom: 1.5rem;
	}

	.step-label {
		font-family: var(--font-mono);
		font-size: 0.6875rem;
		letter-spacing: 0.1em;
		color: var(--color-primary);
		margin-bottom: 0.5rem;
	}

	.panel-title {
		font-size: 1.25rem;
		font-weight: 600;
		color: var(--text-primary);
		margin-bottom: 0.5rem;
	}

	.panel-subtitle {
		font-size: 0.8125rem;
		color: var(--text-muted);
		line-height: 1.5;
	}

	.field {
		display: block;
		margin-bottom: 1rem;
	}

	.field-label {
		display: block;
		font-size: 0.8125rem;
		font-weight: 500;
		color: var(--text-primary);
		margin-bottom: 0.375rem;
	}

	.field-hint {
		display: block;
		font-size: 0.75rem;
		color: var(--text-subtle);
		margin-top: 0.375rem;
	}

	.input {
		width: 100%;
		padding: 0.625rem 0.875rem;
		font-size: 0.875rem;
	}

	.form-error {
		color: var(--severity-critical-text);
		font-size: 0.8125rem;
		margin-bottom: 1rem;
	}

	.org-note {
		font-size: 0.75rem;
		color: var(--text-muted);
		margin-top: 1rem;
		text-align: center;
	}

	.btn {
		padding: 0.625rem 1rem;
		border-radius: var(--radius-sm);
		font-size: 0.875rem;
		font-weight: 500;
		cursor: pointer;
		border: none;
	}

	.btn-full {
		width: 100%;
	}

	.btn-primary {
		background-color: var(--color-primary);
		color: var(--text-primary);
	}

	.btn-primary:hover:not(:disabled) {
		background-color: var(--color-primary-hover);
	}

	.btn:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
</style>
