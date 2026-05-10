<script lang="ts">
	import { enhance } from '$app/forms';
	import type { PageData } from './$types';

	export let data: PageData;

	interface AlertConfig {
		id: string;
		projectId: string;
		channel: string;
		channelTarget: string;
		frequencyThreshold: number;
		windowSeconds: number;
		enabled: boolean;
		createdAt: Date;
	}

	let selectedProjectId = '';
	let channel = 'email';
	let channelTarget = '';
	let frequencyThreshold = 50;
	let windowSeconds = 60;
	let enabled = true;
	let isSubmitting = false;
	let editMode = false;
	let editingConfig: AlertConfig | null = null;

	$: editableAlertConfigIds = new Set(data.editableAlertConfigs.map((c: AlertConfig) => c.id));

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
	<h1>Alert Configuration</h1>

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
						<label for="frequencyThreshold">Frequency Threshold (errors per window)</label>
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
						Enabled
					</label>
				</div>

				<div class="form-actions">
					{#if editMode}
						<button type="button" on:click={cancelEdit}>Cancel</button>
					{/if}
					<button type="submit" disabled={isSubmitting}>
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
								<td>{getProjectName(config.projectId)}</td>
								<td>{config.channel}</td>
								<td class="channel-target">{config.channelTarget}</td>
								<td>{config.frequencyThreshold} errors</td>
								<td>{config.windowSeconds}s</td>
								<td>
									<span class="status-badge" class:enabled={config.enabled} class:disabled={!config.enabled}>
										{config.enabled ? 'Enabled' : 'Disabled'}
									</span>
								</td>
								<td>
									{#if hasWritePermission(config.projectId)}
										<button type="button" on:click={() => startEdit(config)}>Edit</button>
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
		padding: 2rem;
		max-width: 1200px;
		margin: 0 auto;
	}

	h1 {
		font-size: 1.75rem;
		margin-bottom: 1.5rem;
	}

	h2 {
		font-size: 1.25rem;
		margin-bottom: 1rem;
	}

	.form-section {
		background: #f8f9fa;
		padding: 1.5rem;
		border-radius: 8px;
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
		margin-bottom: 0.5rem;
		font-weight: 500;
	}

	select,
	input[type='text'],
	input[type='number'] {
		width: 100%;
		padding: 0.5rem;
		border: 1px solid #ddd;
		border-radius: 4px;
		font-size: 1rem;
	}

	.checkbox-group label {
		display: flex;
		align-items: center;
		gap: 0.5rem;
	}

	.form-actions {
		display: flex;
		gap: 1rem;
		margin-top: 1rem;
	}

	button {
		padding: 0.5rem 1rem;
		border: none;
		border-radius: 4px;
		font-size: 1rem;
		cursor: pointer;
	}

	button[type='submit'] {
		background: #2563eb;
		color: white;
	}

	button[type='submit']:disabled {
		background: #93c5fd;
		cursor: not-allowed;
	}

	button[type='button'] {
		background: #e5e7eb;
		color: #374151;
	}

	.configs-section {
		margin-top: 2rem;
	}

	.configs-table {
		width: 100%;
		border-collapse: collapse;
	}

	.configs-table th,
	.configs-table td {
		padding: 0.75rem;
		text-align: left;
		border-bottom: 1px solid #e5e7eb;
	}

	.configs-table th {
		background: #f8f9fa;
		font-weight: 600;
	}

	.channel-target {
		max-width: 200px;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.status-badge {
		display: inline-block;
		padding: 0.25rem 0.5rem;
		border-radius: 4px;
		font-size: 0.875rem;
	}

	.status-badge.enabled {
		background: #d1fae5;
		color: #065f46;
	}

	.status-badge.disabled {
		background: #fee2e2;
		color: #991b1b;
	}

	.no-projects,
	.no-configs {
		color: #6b7280;
		margin-top: 2rem;
	}

	.no-permission {
		color: #9ca3af;
		font-size: 0.875rem;
	}
</style>