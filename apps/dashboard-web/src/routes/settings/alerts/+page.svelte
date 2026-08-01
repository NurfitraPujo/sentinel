<script lang="ts">
	import { invalidateAll } from '$app/navigation';
	import type { PageData } from './$types';

	let { data } = $props();

	interface AlertConfig {
		id: string;
		scope: 'organization' | 'project';
		organizationId: string;
		projectId: string | null;
		channel: string;
		channelTarget: string;
		frequencyThreshold: number;
		windowSeconds: number;
		enabled: boolean;
		createdAt: Date | null;
	}

	let scope = $state<'project' | 'organization'>('project');
	let selectedProjectId = $state('');
	let channel = $state('email');
	let channelTarget = $state('');
	let frequencyThreshold = $state(50);
	let windowSeconds = $state(60);
	let enabled = $state(true);
	let isSubmitting = $state(false);
	let editMode = $state(false);
	let editingConfig = $state<AlertConfig | null>(null);

	// Inline validation errors (HTTP 400 / 422) mapped to field names
	let fieldErrors = $state<Record<string, string>>({});
	// System / Permission errors (HTTP 403 / 500) shown via Toast / Banner
	let toastMessage = $state<{ text: string; type: 'error' | 'success' } | null>(null);

	// Inline delete confirmation tracking
	let deletingConfigId = $state<string | null>(null);
	let isDeleting = $state(false);

	let selectedOrgId = $state('');

	let editableAlertConfigIds = $derived(
		new Set(data.editableAlertConfigs.map((c: AlertConfig) => c.id))
	);

	let activeOrgId = $derived(
		selectedOrgId || data.userOrganizations[0]?.id || data.projects[0]?.organizationId || ''
	);

	function getProjectName(projectId: string | null): string {
		if (!projectId) return 'Organization-Wide';
		const project = data.projects.find((p: { id: string; name: string }) => p.id === projectId);
		return project?.name ?? projectId;
	}

	function startEdit(config: AlertConfig) {
		editMode = true;
		editingConfig = config;
		scope = config.scope;
		selectedProjectId = config.projectId ?? '';
		selectedOrgId = config.organizationId ?? '';
		channel = config.channel;
		channelTarget = config.channelTarget;
		frequencyThreshold = config.frequencyThreshold;
		windowSeconds = config.windowSeconds;
		enabled = config.enabled;
		fieldErrors = {};
		toastMessage = null;
	}

	function cancelEdit() {
		editMode = false;
		editingConfig = null;
		resetForm();
	}

	function resetForm() {
		scope = 'project';
		selectedProjectId = '';
		selectedOrgId = '';
		channel = 'email';
		channelTarget = '';
		frequencyThreshold = 50;
		windowSeconds = 60;
		enabled = true;
		fieldErrors = {};
		toastMessage = null;
	}

	function clearToast() {
		toastMessage = null;
	}

	async function handleSubmit(event: SubmitEvent) {
		event.preventDefault();
		isSubmitting = true;
		fieldErrors = {};
		toastMessage = null;

		// Client-side validation checks
		if (scope === 'project' && !selectedProjectId) {
			fieldErrors.projectId = 'Please select a project';
			isSubmitting = false;
			return;
		}
		if (scope === 'organization' && data.userOrganizations.length > 1 && !activeOrgId) {
			fieldErrors.organizationId = 'Please select an organization';
			isSubmitting = false;
			return;
		}
		if (!channelTarget.trim()) {
			fieldErrors.channelTarget = 'Notification destination target is required';
			isSubmitting = false;
			return;
		}

		const payload: Record<string, unknown> = {
			channel,
			channelTarget,
			frequencyThreshold,
			windowSeconds,
			enabled,
		};

		let url = '/api/alerts';
		let method = 'POST';

		if (editMode && editingConfig) {
			// Scope (project vs organization, and which one) is immutable via PUT — the toggle is disabled
			// while editing, and the payload intentionally omits projectId/organizationId so there is nothing
			// for the API's scope-change guard to reject. See api/alerts/+server.ts PUT.
			payload.id = editingConfig.id;
			method = 'PUT';
		} else if (scope === 'project') {
			payload.projectId = selectedProjectId;
		} else {
			payload.projectId = null;
			payload.organizationId = activeOrgId;
		}

		try {
			const response = await fetch(url, {
				method,
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(payload),
			});

			const result = await response.json();

			if (!response.ok) {
				// HTTP 400 / 422: Render inline field errors if specific, else toast
				if (response.status === 400 || response.status === 422) {
					const errMsg = typeof result.error === 'string' ? result.error : '';
					if (errMsg.toLowerCase().includes('project') || errMsg.toLowerCase().includes('projectid')) {
						fieldErrors.projectId = errMsg;
					} else if (errMsg.toLowerCase().includes('org') || errMsg.toLowerCase().includes('organizationid')) {
						fieldErrors.organizationId = errMsg;
					} else if (errMsg.toLowerCase().includes('target') || errMsg.toLowerCase().includes('channel')) {
						fieldErrors.channelTarget = errMsg;
					} else {
						toastMessage = { text: errMsg || 'Validation error occurred', type: 'error' };
					}
				} else if (response.status === 403) {
					toastMessage = {
						text: result.error ?? 'Insufficient permissions to perform this action',
						type: 'error',
					};
				} else {
					toastMessage = {
						text: result.error ?? 'An unexpected error occurred. Please try again.',
						type: 'error',
					};
				}
			} else {
				toastMessage = {
					text: editMode ? 'Alert rule updated successfully' : 'Alert rule created successfully',
					type: 'success',
				};
				resetForm();
				editMode = false;
				editingConfig = null;
				await invalidateAll();
			}
		} catch (err) {
			toastMessage = { text: 'Network error occurred', type: 'error' };
		} finally {
			isSubmitting = false;
		}
	}

	async function handleDelete(id: string) {
		isDeleting = true;
		try {
			const response = await fetch('/api/alerts', {
				method: 'DELETE',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ id }),
			});

			const result = await response.json();

			if (!response.ok) {
				toastMessage = {
					text: result.error ?? 'Failed to delete alert rule',
					type: 'error',
				};
			} else {
				toastMessage = { text: 'Alert rule deleted', type: 'success' };
				deletingConfigId = null;
				await invalidateAll();
			}
		} catch (err) {
			toastMessage = { text: 'Network error occurred during deletion', type: 'error' };
		} finally {
			isDeleting = false;
		}
	}
</script>

<div class="alerts-page">
	<header class="page-header">
		<h1>Alert Configuration</h1>
		<p class="subtitle">Set up real-time notifications for error spikes and threshold breaches</p>
	</header>

	{#if toastMessage}
		<div class="toast-banner" class:toast-error={toastMessage.type === 'error'} class:toast-success={toastMessage.type === 'success'}>
			<span>{toastMessage.text}</span>
			<button type="button" class="toast-dismiss" onclick={clearToast}>&times;</button>
		</div>
	{/if}

	{#if data.projects.length === 0 && data.userOrganizations.length === 0}
		<p class="no-projects">You don't have access to any projects or organizations yet.</p>
	{:else}
		<form onsubmit={handleSubmit}>
			<div class="form-section">
				<h2>{editMode ? 'Edit Alert Configuration' : 'Create New Alert'}</h2>

				<!-- Form Scope Switcher -->
				<div class="form-group">
					<!--
						A bare `<label>` above a group of buttons (not one control it can associate with)
						was flagged by a11y_label_has_associated_control. This is a two-way toggle between
						buttons, not a set of radio inputs, so fieldset/legend doesn't fit either -- a
						labeled ARIA group is the correct semantics here.
					-->
					<span id="alert-rule-scope-label" class="form-group-label">Alert Rule Scope</span>
					<div class="segmented-control" role="group" aria-labelledby="alert-rule-scope-label">
						<button
							type="button"
							class="segmented-btn"
							class:active={scope === 'project'}
							disabled={editMode}
							title={editMode ? 'Scope cannot be changed when editing an existing rule' : ''}
							onclick={() => (scope = 'project')}
						>
							Project Alert
						</button>
						<button
							type="button"
							class="segmented-btn"
							class:active={scope === 'organization'}
							disabled={editMode || !data.canManageOrgAlerts}
							title={editMode
								? 'Scope cannot be changed when editing an existing rule'
								: !data.canManageOrgAlerts
									? 'Requires manage_keys org permission'
									: ''}
							onclick={() => (scope = 'organization')}
						>
							Organization-Wide Alert
							{#if !data.canManageOrgAlerts}
								<span class="lock-icon">🔒</span>
							{/if}
						</button>
					</div>
					{#if editMode}
						<p class="scope-hint">Scope cannot be changed when editing an existing rule.</p>
					{:else if !data.canManageOrgAlerts}
						<p class="scope-hint">Organization-wide rules require owner, admin, or engineer org role.</p>
					{/if}
				</div>

				{#if scope === 'project'}
					<div class="form-group">
						<label for="projectId">Target Project</label>
						<select
							id="projectId"
							name="projectId"
							bind:value={selectedProjectId}
							required
							class:input-error={!!fieldErrors.projectId}
						>
							<option value="">Select a project</option>
							{#each data.projects as project}
								<option value={project.id}>{project.name}</option>
							{/each}
						</select>
						{#if fieldErrors.projectId}
							<p class="field-error-text">{fieldErrors.projectId}</p>
						{/if}
					</div>
				{:else}
					<div class="form-group">
						<!--
							`for="organizationId"` associates this with the real select below when it renders
							(userOrganizations.length > 1). When it doesn't (the org-badge branch), there is no
							form control to associate with at all -- nothing here needs a label then either.
						-->
						<label for="organizationId">Target Scope</label>
						{#if data.userOrganizations.length > 1}
							<select
								id="organizationId"
								name="organizationId"
								bind:value={selectedOrgId}
								required
								class:input-error={!!fieldErrors.organizationId}
							>
								<option value="">Select an organization</option>
								{#each data.userOrganizations as org}
									<option value={org.id}>{org.name}</option>
								{/each}
							</select>
							{#if fieldErrors.organizationId}
								<p class="field-error-text">{fieldErrors.organizationId}</p>
							{/if}
						{:else}
							<div class="org-scope-box">
								<span class="org-badge-large">[ORG-WIDE]</span>
								<span class="org-scope-desc">Applies automatically to all current and future projects in your organization</span>
							</div>
						{/if}
					</div>
				{/if}


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
						class:input-error={!!fieldErrors.channelTarget}
					/>
					{#if fieldErrors.channelTarget}
						<p class="field-error-text">{fieldErrors.channelTarget}</p>
					{/if}
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
							<th>Scope / Target</th>
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
								<td class="project-cell">
									{#if config.scope === 'organization'}
										<span class="org-badge">[ORG-WIDE]</span>
									{:else}
										<span class="project-name">{getProjectName(config.projectId)}</span>
									{/if}
								</td>
								<td class="channel-cell">{config.channel}</td>
								<td class="channel-target">{config.channelTarget}</td>
								<td class="mono-cell">{config.frequencyThreshold} errors</td>
								<td class="mono-cell">{config.windowSeconds}s</td>
								<td>
									<span class="status-badge" class:enabled={config.enabled} class:disabled={!config.enabled}>
										{config.enabled ? 'Enabled' : 'Disabled'}
									</span>
								</td>
								<td class="actions-cell">
									{#if editableAlertConfigIds.has(config.id)}
										{#if deletingConfigId === config.id}
											<div class="delete-confirm-box">
												<span class="confirm-text">Confirm delete?</span>
												<button
													type="button"
													class="btn-confirm-yes"
													disabled={isDeleting}
													onclick={() => handleDelete(config.id)}
												>
													Yes
												</button>
												<button
													type="button"
													class="btn-confirm-no"
													onclick={() => (deletingConfigId = null)}
												>
													No
												</button>
											</div>
										{:else}
											<button type="button" class="btn-action" onclick={() => startEdit(config)}>Edit</button>
											<button
												type="button"
												class="btn-action btn-delete"
												onclick={() => (deletingConfigId = config.id)}
											>
												Delete
											</button>
										{/if}
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

	/* Toast / Banner Messages */
	.toast-banner {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: 0.625rem 1rem;
		border-radius: var(--radius-sm);
		font-size: 0.8125rem;
		margin-bottom: 1.25rem;
	}

	.toast-error {
		background: rgba(239, 68, 68, 0.12);
		border: 1px solid rgba(239, 68, 68, 0.3);
		color: #ef4444;
	}

	.toast-success {
		background: rgba(16, 185, 129, 0.12);
		border: 1px solid rgba(16, 185, 129, 0.3);
		color: #10b981;
	}

	.toast-dismiss {
		background: transparent;
		border: none;
		color: inherit;
		font-size: 1.125rem;
		cursor: pointer;
		padding: 0 0.25rem;
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

	label,
	.form-group-label {
		display: block;
		margin-bottom: 0.375rem;
		font-size: 0.75rem;
		color: var(--text-muted);
		font-weight: 500;
	}

	/* Segmented Scope Control */
	.segmented-control {
		display: flex;
		gap: 0.25rem;
		background: var(--bg-root);
		padding: 0.25rem;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		width: fit-content;
	}

	.segmented-btn {
		background: transparent;
		border: none;
		color: var(--text-muted);
		padding: 0.375rem 0.75rem;
		font-size: 0.8125rem;
		font-weight: 500;
		border-radius: var(--radius-sm);
		cursor: pointer;
		transition: all 0.15s ease;
		display: flex;
		align-items: center;
		gap: 0.375rem;
	}

	.segmented-btn.active {
		background: var(--bg-surface);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
	}

	.segmented-btn:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	.scope-hint {
		font-size: 0.75rem;
		color: var(--text-subtle);
		margin-top: 0.375rem;
	}

	.lock-icon {
		font-size: 0.75rem;
	}

	.org-scope-box {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		padding: 0.5rem 0.75rem;
		background: var(--bg-root);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
	}

	.org-badge-large {
		font-family: var(--font-mono);
		font-size: 0.75rem;
		font-weight: 600;
		padding: 0.2rem 0.5rem;
		border-radius: var(--radius-sm);
		background: rgba(59, 130, 246, 0.15);
		color: #3b82f6;
		border: 1px solid rgba(59, 130, 246, 0.3);
		text-transform: uppercase;
		letter-spacing: 0.05em;
	}

	.org-scope-desc {
		font-size: 0.8125rem;
		color: var(--text-muted);
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

	select.input-error,
	input.input-error {
		border-color: #ef4444;
	}

	.field-error-text {
		color: #ef4444;
		font-size: 0.75rem;
		margin-top: 0.25rem;
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

	.project-name {
		color: var(--text-primary);
		font-weight: 500;
	}

	.org-badge {
		display: inline-block;
		font-family: var(--font-mono);
		font-size: 0.6875rem;
		font-weight: 600;
		padding: 0.125rem 0.4rem;
		border-radius: var(--radius-sm);
		background: rgba(59, 130, 246, 0.15);
		color: #3b82f6;
		border: 1px solid rgba(59, 130, 246, 0.3);
		text-transform: uppercase;
		letter-spacing: 0.05em;
	}

	.channel-target,
	.mono-cell {
		font-family: var(--font-mono);
		font-size: 0.75rem;
		color: var(--text-muted);
		font-variant-numeric: tabular-nums;
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

	.actions-cell {
		white-space: nowrap;
	}

	.btn-action {
		background: transparent;
		border: 1px solid var(--border-color);
		color: var(--text-muted);
		padding: 0.25rem 0.5rem;
		border-radius: var(--radius-sm);
		font-size: 0.75rem;
		cursor: pointer;
		margin-right: 0.25rem;
	}

	.btn-action:hover {
		background: var(--bg-surface-hover);
		color: var(--text-primary);
	}

	.btn-delete:hover {
		border-color: rgba(239, 68, 68, 0.5);
		color: #ef4444;
	}

	.delete-confirm-box {
		display: inline-flex;
		align-items: center;
		gap: 0.375rem;
		font-size: 0.75rem;
	}

	.confirm-text {
		color: #ef4444;
		font-weight: 500;
	}

	.btn-confirm-yes {
		background: #ef4444;
		color: white;
		border: none;
		padding: 0.2rem 0.4rem;
		border-radius: var(--radius-sm);
		font-size: 0.75rem;
		cursor: pointer;
	}

	.btn-confirm-no {
		background: var(--bg-root);
		color: var(--text-muted);
		border: 1px solid var(--border-color);
		padding: 0.2rem 0.4rem;
		border-radius: var(--radius-sm);
		font-size: 0.75rem;
		cursor: pointer;
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