<script lang="ts">
	import ApiKeyTable from '$lib/components/keys/ApiKeyTable.svelte';
	import ApiKeyCreateModal from '$lib/components/keys/ApiKeyCreateModal.svelte';

	let isModalOpen = false;
	let keys = [
		{
			id: '1',
			name: 'Development Key',
			prefix: 'sk_dev_',
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
		// API call to create key
		// Mock response
		const newToken = 'sk_test_raw_secret_token_' + Date.now();
		keys = [...keys, {
			id: Date.now().toString(),
			name,
			prefix: 'sk_test_',
			scopes,
			targetProject,
			status: 'active',
			createdAt: new Date().toISOString()
		}];
		createModal.setCreatedToken(newToken);
	}

	async function handleRotate(event: CustomEvent) {
		const { id } = event.detail;
		// API call to rotate key
		alert(`Rotated key ${id}`);
	}

	async function handleRevoke(event: CustomEvent) {
		const { id } = event.detail;
		// API call to revoke key
		keys = keys.filter(k => k.id !== id);
	}
</script>

<div class="max-w-7xl mx-auto py-6 sm:px-6 lg:px-8">
	<div class="px-4 py-6 sm:px-0">
		<div class="flex justify-between items-center mb-6">
			<h1 class="text-2xl font-semibold text-gray-900">Organization API Keys</h1>
			<button on:click={() => isModalOpen = true} class="inline-flex items-center px-4 py-2 border border-transparent text-sm font-medium rounded-md shadow-sm text-white bg-indigo-600 hover:bg-indigo-700">
				Create API Key
			</button>
		</div>

		<div class="bg-white shadow overflow-hidden sm:rounded-lg">
			<ApiKeyTable {keys} on:rotate={handleRotate} on:revoke={handleRevoke} />
		</div>
	</div>
</div>

<ApiKeyCreateModal 
	bind:this={createModal}
	bind:isOpen={isModalOpen} 
	{projects} 
	on:create={handleCreate} 
/>
