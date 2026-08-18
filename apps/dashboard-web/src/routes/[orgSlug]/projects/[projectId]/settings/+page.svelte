<script lang="ts">
	import { AGENT_REPO_PROVIDERS } from '$lib/constants/agent-repo';

	interface AgentSettings {
		fixEnabled: boolean;
		maxPrsPerDay: number | null;
	}

	interface RepoConnection {
		provider: string;
		owner: string;
		repo: string;
		defaultBranch: string;
		testCmd: string;
		agentCmd: string | null;
		cloneDepth: number | null;
	}

	interface Props {
		data: {
			orgId: string;
			orgSlug: string;
			projectId: string;
			projectName: string;
			canManageAgents: boolean;
			agentSettings: AgentSettings;
			repoConnection: RepoConnection | null;
		};
	}

	let { data }: Props = $props();

	// All of the $state initializers below deliberately seed once from server-loaded props, same
	// pattern as settings/agents/+page.svelte's `agents` state -- each needs its own
	// svelte-ignore, the directive does not carry across statements.
	// svelte-ignore state_referenced_locally
	let fixEnabled = $state(data.agentSettings.fixEnabled);
	// svelte-ignore state_referenced_locally
	let maxPrsPerDay = $state<string>(data.agentSettings.maxPrsPerDay?.toString() ?? '');
	let savingSettings = $state(false);

	// svelte-ignore state_referenced_locally
	let repoConnection = $state<RepoConnection | null>(data.repoConnection);
	// svelte-ignore state_referenced_locally
	let provider = $state<string>(data.repoConnection?.provider ?? AGENT_REPO_PROVIDERS[0]);
	// svelte-ignore state_referenced_locally
	let owner = $state(data.repoConnection?.owner ?? '');
	// svelte-ignore state_referenced_locally
	let repo = $state(data.repoConnection?.repo ?? '');
	// svelte-ignore state_referenced_locally
	let defaultBranch = $state(data.repoConnection?.defaultBranch ?? 'main');
	// svelte-ignore state_referenced_locally
	let testCmd = $state(data.repoConnection?.testCmd ?? '');
	// svelte-ignore state_referenced_locally
	let agentCmd = $state(data.repoConnection?.agentCmd ?? '');
	// svelte-ignore state_referenced_locally
	let cloneDepth = $state<string>(data.repoConnection?.cloneDepth?.toString() ?? '');
	let savingRepo = $state(false);
	let disconnecting = $state(false);

	let toastMessage = $state<string | null>(null);
	let toastType = $state<'error' | 'success'>('error');

	function showToast(message: string, type: 'error' | 'success' = 'error') {
		toastMessage = message;
		toastType = type;
		setTimeout(() => {
			if (toastMessage === message) toastMessage = null;
		}, 5000);
	}

	async function saveAgentSettings() {
		savingSettings = true;
		try {
			const trimmedMax = maxPrsPerDay.trim();
			const res = await fetch(`/api/organizations/${data.orgId}/projects/${data.projectId}/agent-settings`, {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({
					fixEnabled,
					maxPrsPerDay: trimmedMax === '' ? null : Number(trimmedMax),
				}),
			});
			if (!res.ok) {
				const err = await res.json().catch(() => ({ message: 'Failed to save agent settings' }));
				showToast(err.message || `Error ${res.status}: failed to save agent settings`);
				return;
			}
			const body = await res.json();
			fixEnabled = body.settings.fixEnabled;
			maxPrsPerDay = body.settings.maxPrsPerDay?.toString() ?? '';
			showToast('Agent settings saved', 'success');
		} catch (err: any) {
			showToast(err?.message || 'Network error while saving agent settings');
		} finally {
			savingSettings = false;
		}
	}

	async function saveRepoConnection() {
		savingRepo = true;
		try {
			const trimmedDepth = cloneDepth.trim();
			const res = await fetch(`/api/organizations/${data.orgId}/projects/${data.projectId}/repo-connection`, {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({
					provider,
					owner,
					repo,
					defaultBranch,
					testCmd,
					agentCmd: agentCmd.trim() === '' ? null : agentCmd,
					cloneDepth: trimmedDepth === '' ? null : Number(trimmedDepth),
				}),
			});
			if (!res.ok) {
				const err = await res.json().catch(() => ({ message: 'Failed to save repo connection' }));
				showToast(err.message || `Error ${res.status}: failed to save repo connection`);
				return;
			}
			const body = await res.json();
			repoConnection = body.connection;
			showToast('Repo connection saved', 'success');
		} catch (err: any) {
			showToast(err?.message || 'Network error while saving repo connection');
		} finally {
			savingRepo = false;
		}
	}

	async function disconnectRepo() {
		disconnecting = true;
		try {
			const res = await fetch(`/api/organizations/${data.orgId}/projects/${data.projectId}/repo-connection`, {
				method: 'DELETE',
			});
			if (!res.ok) {
				const err = await res.json().catch(() => ({ message: 'Failed to disconnect repo' }));
				showToast(err.message || `Error ${res.status}: failed to disconnect repo`);
				return;
			}
			repoConnection = null;
			owner = '';
			repo = '';
			defaultBranch = 'main';
			testCmd = '';
			agentCmd = '';
			cloneDepth = '';
			provider = AGENT_REPO_PROVIDERS[0];
			showToast('Repo disconnected', 'success');
		} catch (err: any) {
			showToast(err?.message || 'Network error while disconnecting repo');
		} finally {
			disconnecting = false;
		}
	}
</script>

<div class="settings-page">
	<header class="settings-header">
		<div>
			<span class="project-badge">Project: {data.projectName}</span>
			<h1>Project settings</h1>
		</div>
	</header>

	{#if toastMessage}
		<div class="toast" class:toast-error={toastType === 'error'} class:toast-success={toastType === 'success'} role="status">
			<span>{toastMessage}</span>
			<button type="button" onclick={() => (toastMessage = null)}>✕</button>
		</div>
	{/if}

	{#if data.canManageAgents}
		<section class="agent-automation">
			<h2>Agent automation</h2>
			<p class="subtitle">
				Let Sentinel's fix agent open pull requests for this project's issues. Owner/admin only.
			</p>

			<div class="field-row">
				<label class="toggle-label">
					<input type="checkbox" bind:checked={fixEnabled} />
					Enable automated fixes
				</label>
			</div>

			<div class="field-row">
				<label for="max-prs">Max PRs per day (optional)</label>
				<input id="max-prs" type="number" min="1" step="1" bind:value={maxPrsPerDay} placeholder="unlimited" />
			</div>

			<button type="button" disabled={savingSettings} onclick={saveAgentSettings}>
				{savingSettings ? 'Saving…' : 'Save agent settings'}
			</button>

			<h3>Repository connection</h3>
			{#if !repoConnection}
				<p class="empty-state">No repository connected yet.</p>
			{:else}
				<p class="repo-summary">
					Connected: <code>{repoConnection.provider}:{repoConnection.owner}/{repoConnection.repo}</code>
					(branch <code>{repoConnection.defaultBranch}</code>)
				</p>
			{/if}

			<div class="field-grid">
				<div class="field-row">
					<label for="provider">Provider</label>
					<select id="provider" bind:value={provider}>
						{#each AGENT_REPO_PROVIDERS as p (p)}
							<option value={p}>{p}</option>
						{/each}
					</select>
				</div>
				<div class="field-row">
					<label for="owner">Owner</label>
					<input id="owner" type="text" bind:value={owner} placeholder="org-or-user" />
				</div>
				<div class="field-row">
					<label for="repo">Repo</label>
					<input id="repo" type="text" bind:value={repo} placeholder="repo-name" />
				</div>
				<div class="field-row">
					<label for="default-branch">Default branch</label>
					<input id="default-branch" type="text" bind:value={defaultBranch} placeholder="main" />
				</div>
				<div class="field-row wide">
					<label for="test-cmd">Test command</label>
					<input id="test-cmd" type="text" bind:value={testCmd} placeholder="pnpm test" />
				</div>
				<div class="field-row wide">
					<label for="agent-cmd">Agent command (optional)</label>
					<input id="agent-cmd" type="text" bind:value={agentCmd} placeholder="defaults to the worker's built-in agent" />
				</div>
				<div class="field-row">
					<label for="clone-depth">Clone depth (optional)</label>
					<input id="clone-depth" type="number" min="1" step="1" bind:value={cloneDepth} placeholder="full history" />
				</div>
			</div>

			<div class="repo-actions">
				<button type="button" disabled={savingRepo} onclick={saveRepoConnection}>
					{savingRepo ? 'Saving…' : repoConnection ? 'Update repo connection' : 'Connect repo'}
				</button>
				{#if repoConnection}
					<button type="button" class="danger" disabled={disconnecting} onclick={disconnectRepo}>
						{disconnecting ? 'Disconnecting…' : 'Disconnect'}
					</button>
				{/if}
			</div>
		</section>
	{/if}
</div>

<style>
	.settings-page {
		max-width: 40rem;
		margin: 0 auto;
		padding: 1.5rem;
		display: flex;
		flex-direction: column;
		gap: 1.25rem;
		color: var(--text-primary);
	}

	.project-badge {
		display: inline-block;
		font-size: 0.7rem;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		opacity: 0.7;
		margin-bottom: 0.25rem;
	}

	.settings-header h1 {
		margin: 0;
		font-size: 1.25rem;
		font-weight: 700;
	}

	.toast {
		display: flex;
		justify-content: space-between;
		align-items: center;
		padding: 0.75rem 1rem;
		border-radius: var(--radius-sm);
		border: 1px solid var(--border-color);
		font-size: 0.8rem;
	}

	.toast-error {
		border-color: #b91c1c;
		color: #fca5a5;
	}

	.toast-success {
		border-color: #15803d;
		color: #86efac;
	}

	.toast button {
		background: none;
		border: none;
		color: inherit;
		cursor: pointer;
		font-weight: 700;
	}

	.agent-automation {
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		background: var(--bg-surface);
		padding: 1rem 1.25rem;
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
	}

	.agent-automation h2 {
		margin: 0;
		font-size: 1rem;
		font-weight: 700;
	}

	.agent-automation h3 {
		margin: 0.5rem 0 0;
		font-size: 0.85rem;
		font-weight: 700;
		border-top: 1px dashed var(--border-color);
		padding-top: 0.75rem;
	}

	.subtitle {
		margin: 0;
		font-size: 0.8rem;
		opacity: 0.7;
	}

	.field-row {
		display: flex;
		flex-direction: column;
		gap: 0.25rem;
		font-size: 0.8rem;
	}

	.toggle-label {
		flex-direction: row;
		align-items: center;
		display: flex;
		gap: 0.5rem;
	}

	.field-grid {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: 0.75rem;
	}

	.field-row.wide {
		grid-column: 1 / -1;
	}

	.field-row input,
	.field-row select {
		background: var(--bg-root);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.5rem 0.625rem;
		font-size: 0.8rem;
	}

	.empty-state {
		font-size: 0.8rem;
		opacity: 0.7;
		margin: 0;
	}

	.repo-summary {
		font-size: 0.8rem;
		margin: 0;
	}

	.repo-actions {
		display: flex;
		gap: 0.5rem;
	}

	button {
		background: var(--bg-surface);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.5rem 0.75rem;
		font-size: 0.75rem;
		font-weight: 600;
		cursor: pointer;
		align-self: flex-start;
	}

	button:hover {
		background: var(--bg-surface-hover);
	}

	button:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}

	button.danger {
		color: #f87171;
		border-color: #b91c1c;
	}
</style>
