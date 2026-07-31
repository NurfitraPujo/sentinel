<script lang="ts">
	import { enhance } from '$app/forms';
	import type { PageData } from './$types';

	let { data } = $props();

	interface AlertConfig {
		id: string;
		projectId: string;
		channel: string;
		channelTarget: string;
		frequencyThreshold: number;
		windowSeconds: number;
		enabled: boolean;
		createdAt: Date | null;
	}

	let selectedProjectId = $state('');
	let channel = $state('email');
	let channelTarget = $state('');
	let frequencyThreshold = $state(50);
	let windowSeconds = $state(60);
	let enabled = $state(true);
	let isSubmitting = $state(false);
	let editMode = $state(false);
	let editingConfig = $state<AlertConfig | null>(null);

	let editableAlertConfigIds = $derived(new Set(data.editableAlertConfigs.map((c: AlertConfig) => c.id)));

	function getProjectName(projectId: string): string {
		const project = data.projects.find((p: { id: string; name: string }) => p.id === projectId);
		return project?.name ?? projectId;
	}

	function startEdit(config: AlertConfig) {
		editMode = true;
		editingConfig = config;
		selectedProjectId = config.projectId;
		channel = config.channel;
		channelTarget = config.channelTarget;
		frequencyThreshold = config.frequencyThreshold;
		windowSeconds = config.windowSeconds;
		enabled = config.enabled;
	}

	function cancelEdit() {
		editMode = false;
		editingConfig = null;
		resetForm();
	}

	function resetForm() {
		selectedProjectId = '';
		channel = 'email';
		channelTarget = '';
		frequencyThreshold = 50;
		windowSeconds = 60;
		enabled = true;
	}

	function hasWritePermission(projectId: string): boolean {
		const role = data.projectRoles[projectId];
		if (!role) return false;
		const permissions: Record<string, string[]> = {
			admin: ['read', 'write', 'delete', 'manage_members'],
			developer: ['read', 'write'],
			viewer: ['read'],
		};
		return permissions[role]?.includes('write') ?? false;
	}
</script>

<div class="alerts-page">
	<header class="page-header">
		<h1>Alert Configuration</h1>
		<p class="subtitle">Set up real-time notifications for error spikes and threshold breaches</p>
	</header>

	{#if data.projects.length === 0}
		<p class="no-projects">You don't have access to any projects yet.</p>
	{:else}
		<form
			method="POST"
			action="/api/alerts"
			use:enhance={() => {
				return async ({ result, update }) => {
					isSubmitting = false;
					if (result.type === 'success') {
						update();
					}
				};
			}}
		>
			<input type="hidden" name="intent" value={editMode ? 'update' : 'create'} />
			{#if editMode && editingConfig}
				<input type="hidden" name="id" value={editingConfig.id} />
			{/if}

			<div class="form-section">
				<h2>{editMode ? 'Edit Alert Configuration' : 'Create New Alert'}</h2>

				<div class="form-group">
					<label for="projectId">Project</label>
					<select
						id="projectId"
						name="projectId"
						bind:value={selectedProjectId}
						required
						disabled={editMode}
					>
						<option value="">Select a project</option>
						{#each data.projects as project}
							<option value={project.id}>{project.name}</option>
						{/each}
					</select>
				</div>

				<div class="form-group">
					<label for="channel">Notification Channel</label>
					<select id="channel" name="channel" bind:value={channel} required>
						<option value="email">Email</option>
						<option value="telegram">Telegram</option>
					</select>
				</div>

				<div class="form-group">
					<label for="channelTarget">
						{channel === 'email' ? 'Email Address' : 'Telegram Bot Token / Chat ID'}
					</label>
					<input
						id="channelTarget"
						name="channelTarget"
						type="text"
						bind:value={channelTarget}
						placeholder={channel === 'email' ? 'user@example.com' : 'bot_token:chat_id'}
						required
					/>
				</div>

				<div class="form-row">
					<div class="form-group">
						<label for="frequencyThreshold">Threshold (errors per window)</label>
						<input
							id="frequencyThreshold"
							name="frequencyThreshold"
							type="number"
							min="1"
							bind:value={frequencyThreshold}
							required
						/>
					</div>

					<div class="form-group">
						<label for="windowSeconds">Time Window (seconds)</label>
						<input
							id="windowSeconds"
							name="windowSeconds"
							type="number"
							min="1"
							bind:value={windowSeconds}
							required
						/>
					</div>
				</div>

				<div class="form-group checkbox-group">
					<label>
						<input type="checkbox" bind:checked={enabled} />
						<span>Enabled</span>
					</label>
				</div>

				<div class="form-actions">
					{#if editMode}
						<button type="button" class="btn btn-cancel" onclick={cancelEdit}>Cancel</button>
					{/if}
					<button type="submit" class="btn btn-submit" disabled={isSubmitting}>
						{editMode ? 'Update Alert' : 'Create Alert'}
					</button>
				</div>
			</div>
		</form>

		{#if data.alertConfigs.length > 0}
			<div class="configs-section">
				<h2>Existing Alert Configurations</h2>
				<table class="configs-table">
					<thead>
						<tr>
							<th>Project</th>
							<th>Channel</th>
							<th>Target</th>
							<th>Threshold</th>
							<th>Window</th>
							<th>Status</th>
							<th>Actions</th>
						</tr>
					</thead>
					<tbody>
						{#each data.alertConfigs as config}
							<tr>
								<td class="project-cell">{getProjectName(config.projectId)}</td>
								<td class="channel-cell">{config.channel}</td>
								<td class="channel-target">{config.channelTarget}</td>
								<td class="mono-cell">{config.frequencyThreshold} errors</td>
								<td class="mono-cell">{config.windowSeconds}s</td>
								<td>
									<span class="status-badge" class:enabled={config.enabled} class:disabled={!config.enabled}>
										{config.enabled ? 'Enabled' : 'Disabled'}
									</span>
								</td>
								<td>
									{#if hasWritePermission(config.projectId)}
										<button type="button" class="btn-action" onclick={() => startEdit(config)}>Edit</button>
									{:else}
										<span class="no-permission">View only</span>
									{/if}
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{:else}
			<p class="no-configs">No alert configurations found.</p>
		{/if}
	{/if}
</div>

<style>
	.alerts-page {
		max-width: 1100px;
		margin: 0 auto;
	}

	.page-header {
		border-bottom: 1px solid var(--border-color);
		padding-bottom: 0.75rem;
		margin-bottom: 1.5rem;
	}

	h1 {
		font-size: 1.25rem;
		font-weight: 600;
		color: var(--text-primary);
		margin-bottom: 0.25rem;
	}

	.subtitle {
		color: var(--text-muted);
		font-size: 0.8125rem;
	}

	h2 {
		font-size: 0.9375rem;
		font-weight: 600;
		color: var(--text-primary);
		margin-bottom: 1rem;
	}

	.form-section {
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		padding: 1.25rem;
		border-radius: var(--radius-md);
		margin-bottom: 2rem;
	}

	.form-group {
		margin-bottom: 1rem;
	}

	.form-row {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: 1rem;
	}

	label {
		display: block;
		margin-bottom: 0.375rem;
		font-size: 0.75rem;
		color: var(--text-muted);
		font-weight: 500;
	}

	select,
	input[type='text'],
	input[type='number'] {
		width: 100%;
		padding: 0.45rem 0.625rem;
		background-color: var(--bg-root);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		font-size: 0.8125rem;
	}

	.checkbox-group label {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		cursor: pointer;
		font-size: 0.8125rem;
		color: var(--text-primary);
	}

	.form-actions {
		display: flex;
		gap: 0.75rem;
		margin-top: 1.25rem;
	}

	.btn {
		padding: 0.45rem 1rem;
		border-radius: var(--radius-sm);
		font-size: 0.8125rem;
		font-weight: 500;
		cursor: pointer;
		border: none;
		transition: background 0.15s ease;
	}

	.btn-submit {
		background: var(--color-primary);
		color: var(--text-primary);
	}

	.btn-submit:hover:not(:disabled) {
		background: var(--color-primary-hover);
	}

	.btn-submit:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	.btn-cancel {
		background: var(--bg-root);
		color: var(--text-muted);
		border: 1px solid var(--border-color);
	}

	.btn-cancel:hover {
		background: var(--bg-surface-hover);
		color: var(--text-primary);
	}

	.configs-section {
		margin-top: 2rem;
	}

	.configs-table {
		width: 100%;
		border-collapse: collapse;
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		overflow: hidden;
	}

	.configs-table th,
	.configs-table td {
		padding: 0.625rem 0.875rem;
		text-align: left;
		font-size: 0.8125rem;
		border-bottom: 1px solid var(--border-color);
	}

	.configs-table th {
		background: var(--bg-root);
		font-family: var(--font-mono);
		font-size: 0.6875rem;
		text-transform: uppercase;
		color: var(--text-subtle);
		letter-spacing: 0.05em;
	}

	.channel-target, .mono-cell {
		font-family: var(--font-mono);
		font-size: 0.75rem;
		color: var(--text-muted);
	}

	.channel-target {
		max-width: 180px;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.status-badge {
		display: inline-block;
		padding: 0.125rem 0.5rem;
		border-radius: var(--radius-sm);
		font-family: var(--font-mono);
		font-size: 0.6875rem;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
	}

	.status-badge.enabled {
		background: var(--status-resolved-bg);
		color: var(--status-resolved-text);
	}

	.status-badge.disabled {
		background: var(--severity-critical-bg);
		color: var(--severity-critical-text);
	}

	.btn-action {
		background: transparent;
		border: 1px solid var(--border-color);
		color: var(--text-muted);
		padding: 0.25rem 0.5rem;
		border-radius: var(--radius-sm);
		font-size: 0.75rem;
		cursor: pointer;
	}

	.btn-action:hover {
		background: var(--bg-surface-hover);
		color: var(--text-primary);
	}

	.no-projects,
	.no-configs {
		color: var(--text-subtle);
		margin-top: 2rem;
		font-size: 0.875rem;
	}

	.no-permission {
		color: var(--text-subtle);
		font-size: 0.75rem;
	}
</style>