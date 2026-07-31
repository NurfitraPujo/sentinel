<script lang="ts">
	import ApiKeyTable from '$lib/components/keys/ApiKeyTable.svelte';
	import ApiKeyCreateModal from '$lib/components/keys/ApiKeyCreateModal.svelte';

	let isModalOpen = false;
	let keys = [
		{
			id: '1',
			name: 'Development Key',
			prefix: 'sent_org_dev_',
			scopes: ['Read/Query'],
			targetProject: 'All Projects [Org-Wide]',
			status: 'active',
			createdAt: new Date().toISOString()
		}
	];
	let projects = [
		{ id: 'proj_1', name: 'Frontend App' },
		{ id: 'proj_2', name: 'Backend API' }
	];

	let createModal: ApiKeyCreateModal;

	async function handleCreate(event: CustomEvent) {
		const { name, targetProject, scopes, rateLimitOverride } = event.detail;
		const newToken = 'sent_org_live_secret_token_' + Date.now();
		keys = [...keys, {
			id: Date.now().toString(),
			name,
			prefix: 'sent_org_',
			scopes,
			targetProject,
			status: 'active',
			createdAt: new Date().toISOString()
		}];
		createModal.setCreatedToken(newToken);
	}

	async function handleRotate(event: CustomEvent) {
		const { id } = event.detail;
		alert(`Rotated key ${id}`);
	}

	async function handleRevoke(event: CustomEvent) {
		const { id } = event.detail;
		keys = keys.filter(k => k.id !== id);
	}
</script>

<div class="keys-container">
	<div class="keys-header">
		<div>
			<h1 class="page-title">Organization API Keys</h1>
			<p class="subtitle">Manage secret API keys (`sent_org_...`) for org-wide event ingestion and query access</p>
		</div>
		<button on:click={() => isModalOpen = true} class="btn-create">
			Create API Key
		</button>
	</div>

	<div class="table-wrapper">
		<ApiKeyTable {keys} on:rotate={handleRotate} on:revoke={handleRevoke} />
	</div>
</div>

<ApiKeyCreateModal 
	bind:this={createModal}
	bind:isOpen={isModalOpen} 
	{projects} 
	on:create={handleCreate} 
/>

<style>
	.keys-container {
		max-width: 1100px;
		margin: 0 auto;
	}

	.keys-header {
		display: flex;
		justify-content: space-between;
		align-items: flex-start;
		border-bottom: 1px solid var(--border-color);
		padding-bottom: 0.75rem;
		margin-bottom: 1.5rem;
	}

	.page-title {
		font-size: 1.25rem;
		font-weight: 600;
		color: var(--text-primary);
		margin-bottom: 0.25rem;
	}

	.subtitle {
		font-size: 0.8125rem;
		color: var(--text-muted);
	}

	.btn-create {
		background: var(--color-primary);
		color: var(--text-primary);
		padding: 0.45rem 1rem;
		border-radius: var(--radius-sm);
		font-size: 0.8125rem;
		font-weight: 500;
		border: none;
		cursor: pointer;
		transition: background 0.15s ease;
	}

	.btn-create:hover {
		background: var(--color-primary-hover);
	}

	.table-wrapper {
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		overflow: hidden;
	}
</style>
