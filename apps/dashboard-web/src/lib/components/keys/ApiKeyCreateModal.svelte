<script lang="ts">
	import { createEventDispatcher } from 'svelte';
	
	export let isOpen = false;
	export let projects: Array<{ id: string, name: string }> = [];

	let name = '';
	let targetProject = 'All Projects [Org-Wide]';
	let scopes = ['Read/Query'];
	let rateLimitOverride = '';
	
	let createdToken = '';
	
	const dispatch = createEventDispatcher();
	
	const availableScopes = ['Ingest-Only', 'Read/Query', 'Admin'];

	function handleSubmit() {
		// Emit create event
		dispatch('create', { name, targetProject, scopes, rateLimitOverride });
	}

	function handleClose() {
		isOpen = false;
		createdToken = '';
		dispatch('close');
	}
	
	export function setCreatedToken(token: string) {
		createdToken = token;
	}
</script>

{#if isOpen}
<div class="fixed inset-0 z-50 flex items-center justify-center bg-gray-900 bg-opacity-50">
	<div class="bg-white rounded-lg shadow-xl w-full max-w-md p-6">
		<h2 class="text-lg font-medium text-gray-900 mb-4">Create API Key</h2>
		
		{#if createdToken}
			<div class="bg-yellow-50 border-l-4 border-yellow-400 p-4 mb-4">
				<div class="flex">
					<div class="ml-3">
						<p class="text-sm text-yellow-700">
							Please copy your new API key now. You won't be able to see it again!
						</p>
						<div class="mt-2 text-sm font-mono bg-white p-2 border border-yellow-200 rounded">
							{createdToken}
						</div>
					</div>
				</div>
			</div>
			<div class="mt-5 sm:mt-6">
				<button type="button" on:click={handleClose} class="w-full inline-flex justify-center rounded-md border border-transparent shadow-sm px-4 py-2 bg-indigo-600 text-base font-medium text-white hover:bg-indigo-700 focus:outline-none focus:ring-2 focus:ring-offset-2 focus:ring-indigo-500 sm:text-sm">
					Done
				</button>
			</div>
		{:else}
			<form on:submit|preventDefault={handleSubmit}>
				<div class="space-y-4">
					<div>
						<label for="name" class="block text-sm font-medium text-gray-700">Name</label>
						<input type="text" id="name" bind:value={name} class="mt-1 block w-full rounded-md border-gray-300 shadow-sm focus:border-indigo-500 focus:ring-indigo-500 sm:text-sm" required placeholder="e.g. Production Backend" />
					</div>
					
					<div>
						<label for="targetProject" class="block text-sm font-medium text-gray-700">Target Project</label>
						<select id="targetProject" bind:value={targetProject} class="mt-1 block w-full rounded-md border-gray-300 shadow-sm focus:border-indigo-500 focus:ring-indigo-500 sm:text-sm">
							<option value="All Projects [Org-Wide]">All Projects [Org-Wide]</option>
							{#each projects as project}
								<option value={project.id}>{project.name}</option>
							{/each}
						</select>
					</div>

					<div>
						<label class="block text-sm font-medium text-gray-700 mb-2">Scopes</label>
						<div class="space-y-2">
							{#each availableScopes as scope}
								<div class="flex items-center">
									<input id={`scope-${scope}`} type="checkbox" value={scope} bind:group={scopes} class="h-4 w-4 rounded border-gray-300 text-indigo-600 focus:ring-indigo-500" />
									<label for={`scope-${scope}`} class="ml-2 block text-sm text-gray-900">{scope}</label>
								</div>
							{/each}
						</div>
					</div>
					
					<div>
						<label for="rateLimit" class="block text-sm font-medium text-gray-700">Rate Limit Override (Optional)</label>
						<input type="number" id="rateLimit" bind:value={rateLimitOverride} class="mt-1 block w-full rounded-md border-gray-300 shadow-sm focus:border-indigo-500 focus:ring-indigo-500 sm:text-sm" placeholder="req/sec" />
					</div>
				</div>

				<div class="mt-5 sm:mt-6 sm:flex sm:flex-row-reverse">
					<button type="submit" class="w-full inline-flex justify-center rounded-md border border-transparent shadow-sm px-4 py-2 bg-indigo-600 text-base font-medium text-white hover:bg-indigo-700 focus:outline-none focus:ring-2 focus:ring-offset-2 focus:ring-indigo-500 sm:ml-3 sm:w-auto sm:text-sm">
						Create
					</button>
					<button type="button" on:click={handleClose} class="mt-3 w-full inline-flex justify-center rounded-md border border-gray-300 shadow-sm px-4 py-2 bg-white text-base font-medium text-gray-700 hover:bg-gray-50 focus:outline-none focus:ring-2 focus:ring-offset-2 focus:ring-indigo-500 sm:mt-0 sm:w-auto sm:text-sm">
						Cancel
					</button>
				</div>
			</form>
		{/if}
	</div>
</div>
{/if}
