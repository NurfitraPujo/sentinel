<script lang="ts">
	import { createEventDispatcher } from 'svelte';
	
	export let isOpen = false;
	export let projects: Array<{ id: string, name: string }> = [];

	let name = '';
	let targetProject = 'All Projects [Org-Wide]';
	let scope = 'ingest';
	let rateLimitRpm = '';
	
	let isSubmitting = false;
	
	const dispatch = createEventDispatcher();
	
	const availableScopes = [
		{ id: 'ingest', label: 'Ingest (Ingest metrics & logs)', desc: 'Allows submitting telemetry events' },
		{ id: 'read', label: 'Read (Query telemetry data)', desc: 'Allows querying dashboards & logs' },
		{ id: 'admin', label: 'Admin (Full privileges)', desc: 'Full administrative access' }
	];

	function handleSubmit() {
		if (!name.trim()) return;
		isSubmitting = true;
		dispatch('create', { 
			name: name.trim(), 
			targetProject, 
			scope, 
			rateLimitRpm: rateLimitRpm ? parseInt(rateLimitRpm, 10) : undefined 
		});
	}

	function handleClose() {
		isOpen = false;
		name = '';
		targetProject = 'All Projects [Org-Wide]';
		scope = 'ingest';
		rateLimitRpm = '';
		isSubmitting = false;
		dispatch('close');
	}
</script>

{#if isOpen}
<div class="fixed inset-0 z-50 flex items-center justify-center bg-black/75 backdrop-blur-sm">
	<div class="bg-gray-900 border border-gray-800 rounded-xl shadow-2xl w-full max-w-md p-6 text-gray-100">
		<div class="flex justify-between items-center mb-5">
			<h2 class="text-lg font-semibold text-gray-100">Create API Key</h2>
			<button on:click={handleClose} class="text-gray-400 hover:text-gray-200 text-sm font-semibold">✕</button>
		</div>

		<form on:submit|preventDefault={handleSubmit} class="space-y-4">
			<div>
				<label for="key-name" class="block text-xs font-semibold text-gray-300 uppercase tracking-wider mb-1">Key Name</label>
				<input 
					type="text" 
					id="key-name" 
					bind:value={name} 
					class="w-full bg-gray-950 border border-gray-800 rounded-lg px-3 py-2 text-sm text-gray-100 placeholder-gray-600 focus:outline-none focus:border-emerald-500 transition-colors" 
					required 
					placeholder="e.g. Production Ingestion Service" 
				/>
			</div>

			<div>
				<label for="target-project" class="block text-xs font-semibold text-gray-300 uppercase tracking-wider mb-1">Target Project</label>
				<select 
					id="target-project" 
					bind:value={targetProject} 
					class="w-full bg-gray-950 border border-gray-800 rounded-lg px-3 py-2 text-sm text-gray-100 focus:outline-none focus:border-emerald-500 transition-colors"
				>
					<option value="All Projects [Org-Wide]">All Projects [Org-Wide]</option>
					{#each projects as project}
						<option value={project.id}>{project.name}</option>
					{/each}
				</select>
			</div>

			<div>
				<label class="block text-xs font-semibold text-gray-300 uppercase tracking-wider mb-2">Scope Scope</label>
				<div class="space-y-2">
					{#each availableScopes as sc}
						<label class="flex items-start gap-2.5 p-2 rounded-lg bg-gray-950/60 border border-gray-800/80 hover:border-gray-700 cursor-pointer transition-colors">
							<input 
								type="radio" 
								name="scope-group" 
								value={sc.id} 
								bind:group={scope} 
								class="mt-1 accent-emerald-500 bg-gray-900" 
							/>
							<div>
								<div class="text-xs font-medium text-gray-200">{sc.label}</div>
								<div class="text-[11px] text-gray-500">{sc.desc}</div>
							</div>
						</label>
					{/each}
				</div>
			</div>

			<div>
				<label for="rate-limit" class="block text-xs font-semibold text-gray-300 uppercase tracking-wider mb-1">Rate Limit Override (RPM)</label>
				<input 
					type="number" 
					id="rate-limit" 
					bind:value={rateLimitRpm} 
					class="w-full bg-gray-950 border border-gray-800 rounded-lg px-3 py-2 text-sm text-gray-100 placeholder-gray-600 focus:outline-none focus:border-emerald-500 transition-colors" 
					placeholder="Default (6000 rpm)" 
				/>
			</div>

			<div class="pt-3 flex justify-end gap-3 border-t border-gray-800">
				<button 
					type="button" 
					on:click={handleClose} 
					class="px-4 py-2 bg-gray-800 text-gray-300 hover:bg-gray-700 rounded-lg text-xs font-medium transition-colors"
				>
					Cancel
				</button>
				<button 
					type="submit" 
					disabled={isSubmitting || !name.trim()}
					class="px-4 py-2 bg-emerald-600 hover:bg-emerald-500 text-gray-950 font-semibold rounded-lg text-xs transition-colors shadow-lg shadow-emerald-950/50 disabled:opacity-50"
				>
					{isSubmitting ? 'Creating...' : 'Create Key'}
				</button>
			</div>
		</form>
	</div>
</div>
{/if}
