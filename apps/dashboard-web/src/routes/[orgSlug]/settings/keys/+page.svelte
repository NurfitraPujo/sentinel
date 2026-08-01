<script lang="ts">
	import ApiKeyTable from '$lib/components/keys/ApiKeyTable.svelte';
	import ApiKeyCreateModal from '$lib/components/keys/ApiKeyCreateModal.svelte';

	export let data: {
		orgId: string;
		orgSlug: string;
		keys: any[];
		projects: Array<{ id: string; name: string }>;
	};

	let isModalOpen = false;
	let isModalSubmitting = false;
	$: keys = data.keys || [];
	$: projects = data.projects || [];

	let newlyCreatedToken: string | null = null;
	let copiedToken = false;

	let toastMessage: string | null = null;
	let toastType: 'error' | 'success' = 'error';

	function showToast(message: string, type: 'error' | 'success' = 'error') {
		toastMessage = message;
		toastType = type;
		setTimeout(() => {
			if (toastMessage === message) toastMessage = null;
		}, 5000);
	}

	async function copyToClipboard(text: string) {
		try {
			await navigator.clipboard.writeText(text);
			copiedToken = true;
			setTimeout(() => { copiedToken = false; }, 4000);
		} catch (e) {
			showToast('Failed to copy to clipboard');
		}
	}

	async function handleCreate(event: CustomEvent) {
		const { name, targetProject, scope, rateLimitRpm } = event.detail;

		try {
			const res = await fetch(`/api/organizations/${data.orgId}/keys`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({
					name,
					scope,
					projectId: targetProject !== 'All Projects [Org-Wide]' ? targetProject : undefined,
					rateLimitRpm
				})
			});

			if (!res.ok) {
				const err = await res.json().catch(() => ({ message: 'Failed to create key' }));
				showToast(err.message || `Error ${res.status}: Failed to create key`);
				// D26: the modal stays open on a create failure, so it must be told its submit
				// finished — otherwise the button is stuck disabled reading "Creating..." forever.
				isModalSubmitting = false;
				return;
			}

			const { key, token } = await res.json();

			// Single-exposure secret token banner
			newlyCreatedToken = token;
			isModalOpen = false;

			// Add new key to local state
			keys = [key, ...keys];
			showToast('API Key created successfully', 'success');
		} catch (err: any) {
			showToast(err?.message || 'Network error while creating key');
			isModalSubmitting = false;
		}
	}

	async function handleRotate(event: CustomEvent) {
		const { id } = event.detail;

		try {
			const res = await fetch(`/api/organizations/${data.orgId}/keys/${id}/rotate`, {
				method: 'POST'
			});

			if (!res.ok) {
				const err = await res.json().catch(() => ({ message: 'Failed to rotate key' }));
				showToast(err.message || `Error ${res.status}: Failed to rotate key`);
				return;
			}

			const { key, token } = await res.json();

			// Expose rotated raw token once in top inline tray
			newlyCreatedToken = token;

			// D36: rotation creates a NEW key row server-side (the old one is revoked, not deleted
			// or replaced — see rotateApiKey in lib/db/queries/apikeys.ts). Previously this mapped
			// the old row directly to the new key's data, which overwrote it in place: the
			// just-revoked key vanished from the table until the next full reload instead of moving
			// into the Revoked section. Mark the old row revoked in local state and prepend the new
			// row, mirroring what the server actually did.
			keys = [
				key,
				...keys.map(k => (k.id === id ? { ...k, status: 'revoked', revokedAt: new Date().toISOString() } : k))
			];
			showToast('API Key rotated successfully. Old key invalidated.', 'success');
		} catch (err: any) {
			showToast(err?.message || 'Network error while rotating key');
		}
	}

	async function handleRevoke(event: CustomEvent) {
		const { id } = event.detail;

		try {
			const res = await fetch(`/api/organizations/${data.orgId}/keys/${id}`, {
				method: 'DELETE'
			});

			if (!res.ok) {
				const err = await res.json().catch(() => ({ message: 'Failed to revoke key' }));
				showToast(err.message || `Error ${res.status}: Failed to revoke key`);
				return;
			}

			// Mark as revoked locally
			keys = keys.map(k => (k.id === id ? { ...k, status: 'revoked', revokedAt: new Date().toISOString() } : k));
			showToast('API Key revoked successfully', 'success');
		} catch (err: any) {
			showToast(err?.message || 'Network error while revoking key');
		}
	}
</script>

<div class="keys-container min-h-screen bg-gray-950 text-gray-100 p-6">
	<div class="max-w-6xl mx-auto">
		<!-- Page Header -->
		<div class="flex justify-between items-start border-b border-gray-800 pb-4 mb-6">
			<div>
				<h1 class="text-xl font-bold tracking-tight text-gray-100">Organization API Keys</h1>
				<p class="text-xs text-gray-400 mt-1">
					Manage secret API keys (<code class="text-emerald-400 bg-gray-900 px-1.5 py-0.5 rounded border border-gray-800">sent_org_...</code>) for org-wide telemetry ingestion and API query access.
				</p>
			</div>
			<button 
				on:click={() => isModalOpen = true} 
				class="px-4 py-2 bg-emerald-600 hover:bg-emerald-500 text-gray-950 font-semibold rounded-lg text-xs transition-colors shadow-lg shadow-emerald-950/40"
			>
				+ Create API Key
			</button>
		</div>

		<!-- Toast Notification Banner -->
		{#if toastMessage}
			<div class={`mb-4 px-4 py-3 rounded-lg text-xs font-medium border flex justify-between items-center ${toastType === 'error' ? 'bg-rose-950/80 border-rose-800 text-rose-200' : 'bg-emerald-950/80 border-emerald-800 text-emerald-200'}`}>
				<span>{toastMessage}</span>
				<button on:click={() => toastMessage = null} class="text-xs font-bold opacity-75 hover:opacity-100">✕</button>
			</div>
		{/if}

		<!-- Single-Exposure Raw Secret Token Alert Tray -->
		{#if newlyCreatedToken}
			<div class="mb-6 bg-amber-950/60 border-2 border-amber-500/80 rounded-xl p-5 shadow-2xl relative">
				<div class="flex items-start justify-between">
					<div class="space-y-1">
						<span class="inline-flex items-center px-2 py-0.5 rounded text-[10px] font-extrabold bg-amber-500 text-gray-950 uppercase tracking-wider">
							Save Secret Token Now
						</span>
						<h3 class="text-sm font-semibold text-amber-200">Single-Exposure API Key Secret</h3>
						<p class="text-xs text-amber-300/80">
							This secret token will <strong>never be shown again</strong>. Store it securely in your secret manager or environment variables.
						</p>
					</div>
					<button 
						on:click={() => newlyCreatedToken = null}
						class="text-amber-400 hover:text-amber-200 text-sm font-bold p-1"
						title="Dismiss secret banner"
					>
						✕
					</button>
				</div>

				<div class="mt-4 flex items-center gap-3">
					<div class="flex-1 bg-gray-950 border border-amber-700/50 rounded-lg px-4 py-2.5 font-mono text-sm text-emerald-400 select-all tracking-wide break-all">
						{newlyCreatedToken}
					</div>
					<button 
						on:click={() => copyToClipboard(newlyCreatedToken!)}
						class="px-4 py-2.5 bg-amber-500 hover:bg-amber-400 text-gray-950 font-bold rounded-lg text-xs transition-colors shrink-0 flex items-center gap-1.5"
					>
						{copiedToken ? '✓ Copied' : 'Copy Secret'}
					</button>
				</div>
			</div>
		{/if}

		<!-- Key Inventory Table -->
		<div class="bg-gray-900 border border-gray-800 rounded-xl overflow-hidden shadow-xl">
			<ApiKeyTable {keys} on:rotate={handleRotate} on:revoke={handleRevoke} />
		</div>
	</div>
</div>

<ApiKeyCreateModal
	bind:isOpen={isModalOpen}
	bind:isSubmitting={isModalSubmitting}
	{projects}
	on:create={handleCreate}
/>
