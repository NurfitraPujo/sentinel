<script lang="ts">
	// keyPrefix mirrors the API's field name (apps/dashboard-web/src/lib/db/queries/apikeys.ts:26,47
	// select `keyPrefix`, not `prefix`). Using the wrong field name here meant every row always fell
	// back to the literal 'sent_' placeholder — the ONLY purpose of this column is telling
	// sent_org_ keys apart from sent_live_ keys, and the fallback made that impossible (D25).
	export let keys: Array<{
		id: string;
		name: string;
		keyPrefix?: string;
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
	<table class="key-table">
		<thead>
			<tr>
				<th>Name</th>
				<th>Prefix</th>
				<th>Scope</th>
				<th>Target</th>
				<th>Status</th>
				<th class="align-right">Actions</th>
			</tr>
		</thead>
		<tbody>
			{#each activeKeys as key}
				<tr>
					<td class="cell-name">{key.name}</td>
					<td class="cell-prefix">{key.keyPrefix || 'sent_'}••••</td>
					<td>
						{#each formatScopes(key) as sc}
							<span class="pill pill-scope">{sc}</span>
						{/each}
					</td>
					<td class="cell-muted">
						{key.targetProject || (key.projectId ? key.projectId : 'All Projects [Org-Wide]')}
					</td>
					<td>
						<span class="pill pill-active">Active</span>
					</td>
					<td class="align-right">
						<button on:click={() => openRotateModal(key)} class="link-action link-rotate">Rotate</button>
						<button on:click={() => openRevokeModal(key)} class="link-action link-revoke">Revoke</button>
					</td>
				</tr>
			{/each}

			{#if activeKeys.length === 0}
				<tr>
					<td colspan="6" class="cell-empty">
						No API keys found. Click "Create API Key" to provision a new credential.
					</td>
				</tr>
			{/if}
		</tbody>
	</table>

	{#if revokedKeys.length > 0}
		<div class="revoked-section">
			<button type="button" on:click={() => showRevoked = !showRevoked} class="revoked-toggle">
				<span>Revoked Keys ({revokedKeys.length})</span>
				<span class="revoked-toggle-hint">{showRevoked ? '▲ Hide' : '▼ Show'}</span>
			</button>

			{#if showRevoked}
				<table class="key-table key-table-revoked">
					<thead>
						<tr>
							<th>Name</th>
							<th>Prefix</th>
							<th>Scope</th>
							<th>Status</th>
						</tr>
					</thead>
					<tbody>
						{#each revokedKeys as key}
							<tr class="row-revoked">
								<td class="cell-struck">{key.name}</td>
								<td class="cell-prefix cell-prefix-muted">{key.keyPrefix || 'sent_'}••••</td>
								<td class="cell-muted">{formatScopes(key).join(', ')}</td>
								<td>
									<span class="pill pill-revoked">Revoked</span>
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			{/if}
		</div>
	{/if}
</div>

{#if confirmRotateId && confirmRotateKey}
<div class="modal-backdrop">
	<div class="modal modal-warn">
		<div class="modal-head">
			<div class="modal-icon modal-icon-warn">⚠️</div>
			<div>
				<h3 class="modal-title modal-title-warn">Confirm API Key Rotation</h3>
				<p class="modal-subtitle">Target Key: <span class="mono">{confirmRotateKey.name}</span></p>
			</div>
		</div>

		<div class="modal-note modal-note-warn">
			<p><strong>Security Impact Preview:</strong></p>
			<ul>
				<li>The old key secret will be <strong>revoked immediately</strong> via NATS cache invalidation.</li>
				<li>Services currently using this key will experience authentication failures until updated.</li>
				<li>A new secret token will be generated and displayed <strong>only once</strong>.</li>
			</ul>
		</div>

		<div class="modal-actions">
			<button type="button" on:click={() => { confirmRotateId = null; confirmRotateKey = null; }} class="btn btn-cancel">
				Cancel
			</button>
			<button type="button" on:click={executeRotate} class="btn btn-warn">
				Confirm Rotation
			</button>
		</div>
	</div>
</div>
{/if}

{#if confirmRevokeId && confirmRevokeKey}
<div class="modal-backdrop">
	<div class="modal modal-danger">
		<div class="modal-head">
			<div class="modal-icon modal-icon-danger">🚨</div>
			<div>
				<h3 class="modal-title modal-title-danger">Confirm API Key Revocation</h3>
				<p class="modal-subtitle">Target Key: <span class="mono">{confirmRevokeKey.name}</span></p>
			</div>
		</div>

		<div class="modal-note modal-note-danger">
			<p><strong>Permanent Action Warning:</strong></p>
			<ul>
				<li>This API key will be permanently invalidated across all ingestor nodes.</li>
				<li>All future events tagged with this key will be rejected.</li>
				<li>This action cannot be undone.</li>
			</ul>
		</div>

		<div class="modal-actions">
			<button type="button" on:click={() => { confirmRevokeId = null; confirmRevokeKey = null; }} class="btn btn-cancel">
				Cancel
			</button>
			<button type="button" on:click={executeRevoke} class="btn btn-danger">
				Revoke Key
			</button>
		</div>
	</div>
</div>
{/if}

<style>
	/* Rewritten from Tailwind utility classes, which never resolved — this project has no Tailwind
	   (no config, no dependency, none in the lockfile, none in git history). The tokens below are
	   app.css's; the previous slate/emerald/amber literals were Tailwind palette names that
	   happened to match some of them. */
	.api-key-table-container {
		width: 100%;
	}

	.key-table {
		width: 100%;
		border-collapse: collapse;
		background-color: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		overflow: hidden;
		font-size: 0.875rem;
	}

	.key-table thead {
		background-color: var(--bg-root);
	}

	.key-table th {
		padding: 0.75rem 1rem;
		text-align: left;
		font-size: 0.6875rem;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted);
		white-space: nowrap;
	}

	.key-table td {
		padding: 0.875rem 1rem;
		white-space: nowrap;
		border-top: 1px solid var(--border-color-muted);
	}

	.key-table tbody tr:hover {
		background-color: var(--bg-surface-hover);
	}

	.align-right {
		text-align: right;
	}

	.cell-name {
		font-weight: 500;
		color: var(--text-primary);
	}

	.cell-muted {
		color: var(--text-muted);
	}

	.cell-prefix {
		font-family: var(--font-mono);
		color: var(--status-resolved-text);
		letter-spacing: 0.03em;
	}

	.cell-prefix-muted {
		color: var(--text-subtle);
	}

	.cell-struck {
		color: var(--text-muted);
		text-decoration: line-through;
	}

	.cell-empty {
		padding: 2rem 1rem;
		text-align: center;
		color: var(--text-muted);
	}

	.pill {
		display: inline-flex;
		align-items: center;
		padding: 0.125rem 0.625rem;
		border-radius: 999px;
		font-size: 0.6875rem;
		font-weight: 500;
		text-transform: uppercase;
		letter-spacing: 0.05em;
	}

	.pill + .pill {
		margin-left: 0.25rem;
	}

	.pill-scope,
	.pill-active {
		background-color: var(--status-resolved-bg);
		color: var(--status-resolved-text);
		border: 1px solid rgba(16, 185, 129, 0.3);
	}

	.pill-active {
		text-transform: none;
		letter-spacing: normal;
	}

	.pill-revoked {
		background-color: var(--severity-critical-bg);
		color: var(--severity-critical-text);
		border: 1px solid var(--severity-critical-border);
		text-transform: none;
		letter-spacing: normal;
	}

	.link-action {
		background: none;
		border: none;
		padding: 0;
		font-size: 0.875rem;
		font-weight: 500;
		cursor: pointer;
	}

	.link-action + .link-action {
		margin-left: 1rem;
	}

	.link-rotate {
		color: var(--status-open-text);
	}

	.link-revoke {
		color: var(--severity-critical-text);
	}

	.link-action:hover {
		text-decoration: underline;
	}

	.revoked-section {
		margin-top: 1.5rem;
		border-top: 1px solid var(--border-color);
		padding-top: 1rem;
	}

	.revoked-toggle {
		display: flex;
		align-items: center;
		justify-content: space-between;
		width: 100%;
		padding: 0.5rem;
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 0.875rem;
		font-weight: 500;
		cursor: pointer;
	}

	.revoked-toggle:hover {
		color: var(--text-primary);
	}

	.revoked-toggle-hint {
		font-size: 0.75rem;
	}

	.key-table-revoked {
		margin-top: 0.5rem;
		background-color: var(--bg-root);
	}

	.row-revoked {
		opacity: 0.6;
	}

	.modal-backdrop {
		position: fixed;
		inset: 0;
		z-index: 50;
		display: flex;
		align-items: center;
		justify-content: center;
		background-color: rgba(0, 0, 0, 0.75);
		padding: 1rem;
	}

	.modal {
		width: 100%;
		max-width: 32rem;
		padding: 1.5rem;
		background-color: var(--bg-surface);
		border-radius: var(--radius-lg);
		box-shadow: 0 20px 40px rgba(0, 0, 0, 0.5);
		color: var(--text-primary);
	}

	.modal-warn {
		border: 1px solid rgba(245, 158, 11, 0.3);
	}

	.modal-danger {
		border: 1px solid var(--severity-critical-border);
	}

	.modal-head {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		margin-bottom: 1rem;
	}

	.modal-icon {
		padding: 0.5rem;
		border-radius: var(--radius-md);
		line-height: 1;
	}

	.modal-icon-warn {
		background-color: var(--status-open-bg);
		border: 1px solid rgba(245, 158, 11, 0.3);
	}

	.modal-icon-danger {
		background-color: var(--severity-critical-bg);
		border: 1px solid var(--severity-critical-border);
	}

	.modal-title {
		font-size: 1.0625rem;
		font-weight: 600;
	}

	.modal-title-warn {
		color: var(--status-open-text);
	}

	.modal-title-danger {
		color: var(--severity-critical-text);
	}

	.modal-subtitle {
		font-size: 0.75rem;
		color: var(--text-muted);
	}

	.mono {
		font-family: var(--font-mono);
	}

	.modal-note {
		border-radius: var(--radius-md);
		padding: 1rem;
		margin-bottom: 1.25rem;
		font-size: 0.75rem;
		line-height: 1.6;
	}

	.modal-note-warn {
		background-color: var(--status-open-bg);
		border: 1px solid rgba(245, 158, 11, 0.3);
	}

	.modal-note-danger {
		background-color: var(--severity-critical-bg);
		border: 1px solid var(--severity-critical-border);
	}

	.modal-note ul {
		margin: 0.5rem 0 0;
		padding-left: 1.25rem;
		color: var(--text-primary);
	}

	.modal-note li + li {
		margin-top: 0.25rem;
	}

	.modal-actions {
		display: flex;
		justify-content: flex-end;
		gap: 0.75rem;
	}

	.btn {
		padding: 0.5rem 1rem;
		border-radius: var(--radius-md);
		font-size: 0.75rem;
		font-weight: 500;
		border: none;
		cursor: pointer;
	}

	.btn-cancel {
		background-color: var(--bg-surface-hover);
		color: var(--text-primary);
	}

	.btn-warn {
		background-color: var(--status-open-text);
		color: var(--bg-root);
		font-weight: 600;
	}

	.btn-danger {
		background-color: var(--severity-critical-text);
		color: var(--text-primary);
		font-weight: 600;
	}

	.btn:hover {
		opacity: 0.9;
	}
</style>
