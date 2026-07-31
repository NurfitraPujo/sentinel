<script lang="ts">
	export let keys: Array<{
		id: string;
		name: string;
		prefix: string;
		scopes?: string[];
		scope?: string;
		targetProject?: string;
		projectId?: string | null;
		status: string;
		createdAt: string;
		revokedAt?: string | null;
	}> = [];

	import { createEventDispatcher } from 'svelte';
	const dispatch = createEventDispatcher();

	let confirmRotateId: string | null = null;
	let confirmRotateKey: typeof keys[0] | null = null;

	let confirmRevokeId: string | null = null;
	let confirmRevokeKey: typeof keys[0] | null = null;

	let showRevoked = false;

	$: activeKeys = keys.filter(k => k.status === 'active' || (!k.status && !k.revokedAt));
	$: revokedKeys = keys.filter(k => k.status === 'revoked' || k.revokedAt);

	function openRotateModal(key: typeof keys[0]) {
		confirmRotateKey = key;
		confirmRotateId = key.id;
	}

	function executeRotate() {
		if (confirmRotateId) {
			dispatch('rotate', { id: confirmRotateId });
			confirmRotateId = null;
			confirmRotateKey = null;
		}
	}

	function openRevokeModal(key: typeof keys[0]) {
		confirmRevokeKey = key;
		confirmRevokeId = key.id;
	}

	function executeRevoke() {
		if (confirmRevokeId) {
			dispatch('revoke', { id: confirmRevokeId });
			confirmRevokeId = null;
			confirmRevokeKey = null;
		}
	}

	function formatScopes(key: typeof keys[0]): string[] {
		if (key.scopes && key.scopes.length > 0) return key.scopes;
		if (key.scope) return [key.scope];
		return ['ingest'];
	}
</script>

<div class="api-key-table-container">
	<!-- Active Keys Table -->
	<table class="min-w-full divide-y divide-gray-800 bg-gray-900 text-gray-100 rounded-lg overflow-hidden">
		<thead class="bg-gray-950/80 text-gray-400">
			<tr>
				<th class="px-6 py-3.5 text-left text-xs font-semibold uppercase tracking-wider">Name</th>
				<th class="px-6 py-3.5 text-left text-xs font-semibold uppercase tracking-wider">Prefix</th>
				<th class="px-6 py-3.5 text-left text-xs font-semibold uppercase tracking-wider">Scope</th>
				<th class="px-6 py-3.5 text-left text-xs font-semibold uppercase tracking-wider">Target</th>
				<th class="px-6 py-3.5 text-left text-xs font-semibold uppercase tracking-wider">Status</th>
				<th class="px-6 py-3.5 text-right text-xs font-semibold uppercase tracking-wider">Actions</th>
			</tr>
		</thead>
		<tbody class="divide-y divide-gray-800/60 bg-gray-900">
			{#each activeKeys as key}
				<tr class="hover:bg-gray-800/40 transition-colors">
					<td class="px-6 py-4 whitespace-nowrap text-sm font-medium text-gray-100">{key.name}</td>
					<td class="px-6 py-4 whitespace-nowrap text-sm text-emerald-400 font-mono tracking-wide">{key.prefix || 'sent_'}••••</td>
					<td class="px-6 py-4 whitespace-nowrap text-sm">
						{#each formatScopes(key) as sc}
							<span class="inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium bg-emerald-950 text-emerald-300 border border-emerald-800/50 uppercase tracking-wider">
								{sc}
							</span>
						{/each}
					</td>
					<td class="px-6 py-4 whitespace-nowrap text-sm text-gray-400">
						{key.targetProject || (key.projectId ? key.projectId : 'All Projects [Org-Wide]')}
					</td>
					<td class="px-6 py-4 whitespace-nowrap text-sm">
						<span class="inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium bg-emerald-900/40 text-emerald-400 border border-emerald-700/40">
							Active
						</span>
					</td>
					<td class="px-6 py-4 whitespace-nowrap text-right text-sm font-medium">
						<button on:click={() => openRotateModal(key)} class="text-amber-400 hover:text-amber-300 mr-4 font-medium transition-colors">Rotate</button>
						<button on:click={() => openRevokeModal(key)} class="text-rose-400 hover:text-rose-300 font-medium transition-colors">Revoke</button>
					</td>
				</tr>
			{/each}

			{#if activeKeys.length === 0}
				<tr>
					<td colspan="6" class="px-6 py-8 text-center text-sm text-gray-400 bg-gray-900/50">
						No active API keys found. Click "Create API Key" to provision a new credential.
					</td>
				</tr>
			{/if}
		</tbody>
	</table>

	<!-- Revoked Keys Collapsible Section -->
	{#if revokedKeys.length > 0}
		<div class="mt-6 border-t border-gray-800 pt-4">
			<button 
				type="button" 
				on:click={() => showRevoked = !showRevoked}
				class="flex items-center justify-between w-full text-left px-2 py-2 text-sm font-medium text-gray-400 hover:text-gray-200 transition-colors"
			>
				<span>Revoked Keys ({revokedKeys.length})</span>
				<span class="text-xs">{showRevoked ? '▲ Hide' : '▼ Show'}</span>
			</button>

			{#if showRevoked}
				<table class="min-w-full divide-y divide-gray-800 bg-gray-950/60 text-gray-400 rounded-lg overflow-hidden mt-2 border border-gray-800">
					<thead class="bg-gray-950 text-gray-500">
						<tr>
							<th class="px-6 py-3 text-left text-xs font-medium uppercase tracking-wider">Name</th>
							<th class="px-6 py-3 text-left text-xs font-medium uppercase tracking-wider">Prefix</th>
							<th class="px-6 py-3 text-left text-xs font-medium uppercase tracking-wider">Scope</th>
							<th class="px-6 py-3 text-left text-xs font-medium uppercase tracking-wider">Status</th>
						</tr>
					</thead>
					<tbody class="divide-y divide-gray-800/40">
						{#each revokedKeys as key}
							<tr class="opacity-60">
								<td class="px-6 py-3 whitespace-nowrap text-sm text-gray-400 line-through">{key.name}</td>
								<td class="px-6 py-3 whitespace-nowrap text-sm font-mono text-gray-500">{key.prefix || 'sent_'}••••</td>
								<td class="px-6 py-3 whitespace-nowrap text-sm text-gray-500">{formatScopes(key).join(', ')}</td>
								<td class="px-6 py-3 whitespace-nowrap text-sm">
									<span class="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-red-950 text-red-400 border border-red-900/40">
										Revoked
									</span>
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			{/if}
		</div>
	{/if}
</div>

<!-- Rotate Confirmation Modal (Two-Step Impact Preview) -->
{#if confirmRotateId && confirmRotateKey}
<div class="fixed inset-0 z-50 flex items-center justify-center bg-black/75 backdrop-blur-sm">
	<div class="bg-gray-900 border border-amber-500/30 rounded-xl shadow-2xl w-full max-w-lg p-6 text-gray-100">
		<div class="flex items-center gap-3 mb-4">
			<div class="p-2 bg-amber-950 border border-amber-700/50 rounded-lg text-amber-400">
				⚠️
			</div>
			<div>
				<h3 class="text-lg font-semibold text-amber-200">Confirm API Key Rotation</h3>
				<p class="text-xs text-gray-400">Target Key: <span class="font-mono text-amber-300">{confirmRotateKey.name}</span></p>
			</div>
		</div>

		<div class="bg-amber-950/40 border border-amber-800/40 rounded-lg p-4 mb-5 text-xs text-amber-200/90 leading-relaxed space-y-2">
			<p><strong>Security Impact Preview:</strong></p>
			<ul class="list-disc list-inside space-y-1 text-gray-300">
				<li>The old key secret will be <strong>revoked immediately</strong> via NATS cache invalidation.</li>
				<li>Services currently using this key will experience authentication failures until updated.</li>
				<li>A new secret token will be generated and displayed <strong>only once</strong>.</li>
			</ul>
		</div>

		<div class="flex justify-end gap-3">
			<button 
				type="button" 
				on:click={() => { confirmRotateId = null; confirmRotateKey = null; }}
				class="px-4 py-2 bg-gray-800 text-gray-300 hover:bg-gray-700 rounded-lg text-xs font-medium transition-colors"
			>
				Cancel
			</button>
			<button 
				type="button" 
				on:click={executeRotate}
				class="px-4 py-2 bg-amber-600 hover:bg-amber-500 text-gray-950 font-semibold rounded-lg text-xs transition-colors shadow-lg shadow-amber-950/50"
			>
				Confirm Rotation
			</button>
		</div>
	</div>
</div>
{/if}

<!-- Revoke Confirmation Modal (Two-Step Impact Preview) -->
{#if confirmRevokeId && confirmRevokeKey}
<div class="fixed inset-0 z-50 flex items-center justify-center bg-black/75 backdrop-blur-sm">
	<div class="bg-gray-900 border border-rose-500/30 rounded-xl shadow-2xl w-full max-w-lg p-6 text-gray-100">
		<div class="flex items-center gap-3 mb-4">
			<div class="p-2 bg-rose-950 border border-rose-700/50 rounded-lg text-rose-400">
				🚨
			</div>
			<div>
				<h3 class="text-lg font-semibold text-rose-200">Confirm API Key Revocation</h3>
				<p class="text-xs text-gray-400">Target Key: <span class="font-mono text-rose-300">{confirmRevokeKey.name}</span></p>
			</div>
		</div>

		<div class="bg-rose-950/40 border border-rose-800/40 rounded-lg p-4 mb-5 text-xs text-rose-200/90 leading-relaxed space-y-2">
			<p><strong>Permanent Action Warning:</strong></p>
			<ul class="list-disc list-inside space-y-1 text-gray-300">
				<li>This API key will be permanently invalidated across all ingestor nodes.</li>
				<li>All future events tagged with this key will be rejected.</li>
				<li>This action cannot be undone.</li>
			</ul>
		</div>

		<div class="flex justify-end gap-3">
			<button 
				type="button" 
				on:click={() => { confirmRevokeId = null; confirmRevokeKey = null; }}
				class="px-4 py-2 bg-gray-800 text-gray-300 hover:bg-gray-700 rounded-lg text-xs font-medium transition-colors"
			>
				Cancel
			</button>
			<button 
				type="button" 
				on:click={executeRevoke}
				class="px-4 py-2 bg-rose-600 hover:bg-rose-500 text-white font-semibold rounded-lg text-xs transition-colors shadow-lg shadow-rose-950/50"
			>
				Revoke Key
			</button>
		</div>
	</div>
</div>
{/if}
