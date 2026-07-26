<script lang="ts">
	export let keys: Array<{
		id: string;
		name: string;
		prefix: string;
		scopes: string[];
		targetProject: string;
		status: string;
		createdAt: string;
	}> = [];

	import { createEventDispatcher } from 'svelte';
	const dispatch = createEventDispatcher();

	function handleRotate(id: string) {
		dispatch('rotate', { id });
	}

	function handleRevoke(id: string) {
		dispatch('revoke', { id });
	}
</script>

<div class="api-key-table-container">
	<table class="min-w-full divide-y divide-gray-200">
		<thead class="bg-gray-50">
			<tr>
				<th class="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">Name</th>
				<th class="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">Prefix</th>
				<th class="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">Scope</th>
				<th class="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">Target</th>
				<th class="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">Status</th>
				<th class="px-6 py-3 text-right text-xs font-medium text-gray-500 uppercase tracking-wider">Actions</th>
			</tr>
		</thead>
		<tbody class="bg-white divide-y divide-gray-200">
			{#each keys as key}
				<tr>
					<td class="px-6 py-4 whitespace-nowrap text-sm font-medium text-gray-900">{key.name}</td>
					<td class="px-6 py-4 whitespace-nowrap text-sm text-gray-500 font-mono">{key.prefix}••••</td>
					<td class="px-6 py-4 whitespace-nowrap text-sm text-gray-500">
						{#each key.scopes as scope}
							<span class="inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium bg-blue-100 text-blue-800 mr-2">
								{scope}
							</span>
						{/each}
					</td>
					<td class="px-6 py-4 whitespace-nowrap text-sm text-gray-500">{key.targetProject}</td>
					<td class="px-6 py-4 whitespace-nowrap text-sm text-gray-500">
						<span class={`inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium ${key.status === 'active' ? 'bg-green-100 text-green-800' : 'bg-red-100 text-red-800'}`}>
							{key.status}
						</span>
					</td>
					<td class="px-6 py-4 whitespace-nowrap text-right text-sm font-medium">
						<button on:click={() => handleRotate(key.id)} class="text-indigo-600 hover:text-indigo-900 mr-4">Rotate</button>
						<button on:click={() => handleRevoke(key.id)} class="text-red-600 hover:text-red-900">Revoke</button>
					</td>
				</tr>
			{/each}
			{#if keys.length === 0}
				<tr>
					<td colspan="6" class="px-6 py-4 text-center text-sm text-gray-500">No API keys found.</td>
				</tr>
			{/if}
		</tbody>
	</table>
</div>
