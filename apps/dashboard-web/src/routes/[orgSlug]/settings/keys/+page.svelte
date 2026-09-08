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
		const { name, targetProject, scope, rateLimitRpm, expiresInDays } = event.detail;

		try {
			const res = await fetch(`/api/organizations/${data.orgId}/keys`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({
					name,
					scope,
					projectId: targetProject !== 'All Projects [Org-Wide]' ? targetProject : undefined,
					rateLimitRpm,
					expiresInDays
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

<div class="keys-page">
	<div class="page-inner">
		<div class="page-header">
			<div>
				<h1 class="page-title">Organization API Keys</h1>
				<p class="page-subtitle">
					Manage secret API keys (<code>sent_org_...</code>) for org-wide telemetry ingestion and API query access.
				</p>
			</div>
			<button on:click={() => isModalOpen = true} class="btn btn-primary">
				Create API Key
			</button>
		</div>

		{#if toastMessage}
			<div class="toast" class:toast-error={toastType === 'error'} class:toast-success={toastType !== 'error'}>
				<span>{toastMessage}</span>
				<button on:click={() => toastMessage = null} class="toast-dismiss" aria-label="Dismiss">✕</button>
			</div>
		{/if}

		{#if newlyCreatedToken}
			<div class="secret-tray">
				<div class="secret-tray-head">
					<div>
						<span class="secret-badge">Save Secret Token Now</span>
						<h3 class="secret-title">Single-Exposure API Key Secret</h3>
						<p class="secret-desc">
							This secret token will <strong>never be shown again</strong>. Store it securely in your secret manager or environment variables.
						</p>
					</div>
					<button on:click={() => newlyCreatedToken = null} class="secret-dismiss" title="Dismiss secret banner" aria-label="Dismiss secret banner">
						✕
					</button>
				</div>

				<div class="secret-row">
					<div class="secret-value">{newlyCreatedToken}</div>
					<button on:click={() => copyToClipboard(newlyCreatedToken!)} class="btn btn-copy">
						{copiedToken ? '✓ Copied' : 'Copy Secret'}
					</button>
				</div>
			</div>
		{/if}

		<div class="table-card">
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

<style>
	/* Rewritten from Tailwind utility classes, which never resolved — this project has no Tailwind. */
	.keys-page {
		min-height: calc(100vh - 120px);
		padding: 1.5rem 1rem;
	}

	.page-inner {
		max-width: 72rem;
		margin: 0 auto;
	}

	.page-header {
		display: flex;
		justify-content: space-between;
		align-items: flex-start;
		gap: 1rem;
		border-bottom: 1px solid var(--border-color);
		padding-bottom: 1rem;
		margin-bottom: 1.5rem;
	}

	.page-title {
		font-size: 1.125rem;
		font-weight: 700;
		color: var(--text-primary);
	}

	.page-subtitle {
		font-size: 0.75rem;
		color: var(--text-muted);
		margin-top: 0.25rem;
		line-height: 1.6;
	}

	.page-subtitle code {
		font-family: var(--font-mono);
		color: var(--status-resolved-text);
		background-color: var(--bg-root);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.0625rem 0.375rem;
	}

	.btn {
		display: inline-flex;
		align-items: center;
		gap: 0.375rem;
		padding: 0.5rem 1rem;
		border-radius: var(--radius-md);
		font-size: 0.75rem;
		font-weight: 600;
		border: none;
		cursor: pointer;
		white-space: nowrap;
	}

	.btn-primary {
		background-color: var(--status-resolved-text);
		color: var(--bg-root);
	}

	.btn-primary:hover {
		opacity: 0.9;
	}

	.toast {
		display: flex;
		justify-content: space-between;
		align-items: center;
		gap: 1rem;
		margin-bottom: 1rem;
		padding: 0.75rem 1rem;
		border-radius: var(--radius-md);
		font-size: 0.75rem;
		font-weight: 500;
		border: 1px solid transparent;
	}

	.toast-error {
		background-color: var(--severity-critical-bg);
		border-color: var(--severity-critical-border);
		color: var(--severity-critical-text);
	}

	.toast-success {
		background-color: var(--status-resolved-bg);
		border-color: rgba(16, 185, 129, 0.3);
		color: var(--status-resolved-text);
	}

	.toast-dismiss {
		background: none;
		border: none;
		color: inherit;
		font-weight: 700;
		cursor: pointer;
		opacity: 0.75;
	}

	.toast-dismiss:hover {
		opacity: 1;
	}

	/* The one-time secret. Loud on purpose: it is the only time this value is ever shown. */
	.secret-tray {
		margin-bottom: 1.5rem;
		background-color: var(--status-open-bg);
		border: 2px solid var(--status-open-text);
		border-radius: var(--radius-lg);
		padding: 1.25rem;
	}

	.secret-tray-head {
		display: flex;
		align-items: flex-start;
		justify-content: space-between;
		gap: 1rem;
	}

	.secret-badge {
		display: inline-flex;
		align-items: center;
		padding: 0.125rem 0.5rem;
		border-radius: var(--radius-sm);
		font-size: 0.625rem;
		font-weight: 800;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		background-color: var(--status-open-text);
		color: var(--bg-root);
	}

	.secret-title {
		font-size: 0.875rem;
		font-weight: 600;
		color: var(--status-open-text);
		margin-top: 0.25rem;
	}

	.secret-desc {
		font-size: 0.75rem;
		color: var(--text-muted);
		margin-top: 0.25rem;
		line-height: 1.5;
	}

	.secret-dismiss {
		background: none;
		border: none;
		color: var(--status-open-text);
		font-size: 0.875rem;
		font-weight: 700;
		cursor: pointer;
		padding: 0.25rem;
	}

	.secret-row {
		margin-top: 1rem;
		display: flex;
		align-items: center;
		gap: 0.75rem;
	}

	.secret-value {
		flex: 1;
		background-color: var(--bg-root);
		border: 1px solid rgba(245, 158, 11, 0.5);
		border-radius: var(--radius-md);
		padding: 0.625rem 1rem;
		font-family: var(--font-mono);
		font-size: 0.875rem;
		color: var(--status-resolved-text);
		user-select: all;
		word-break: break-all;
	}

	.btn-copy {
		background-color: var(--status-open-text);
		color: var(--bg-root);
		font-weight: 700;
		padding: 0.625rem 1rem;
		flex-shrink: 0;
	}

	.table-card {
		background-color: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-lg);
		overflow: hidden;
	}
</style>
