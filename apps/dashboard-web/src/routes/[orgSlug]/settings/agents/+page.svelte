<script lang="ts">
	// Manual Issues M5 §7/§9: agent identity + agent-key issuance/revocation, owner/admin only.
	// Key listing reuses GET /api/organizations/[orgId]/keys (already permission-gated on
	// 'read') and filters client-side by agentId, rather than adding a second listing endpoint —
	// there is exactly one source of truth for "which keys exist" (src/lib/db/queries/apikeys.ts)
	// and this page just narrows the view onto it, same as ApiKeyTable does for the org-wide list.
	interface AgentRow {
		id: string;
		name: string;
		kind: 'ai' | 'bot';
		status: 'active' | 'disabled';
		createdAt: string | null;
	}

	interface KeyRow {
		id: string;
		agentId: string | null;
		name: string;
		keyPrefix: string;
		status: string;
		createdAt: string | null;
	}

	interface WebhookRow {
		id: string;
		agentId: string;
		url: string;
		secretPrefix: string;
		eventTypes: string[];
		status: 'active' | 'disabled' | 'failed';
		consecutiveFailures: number;
		createdAt: string | null;
	}

	interface Props {
		data: { orgId: string; orgSlug: string; agents: AgentRow[] };
	}

	let { data }: Props = $props();

	// svelte-ignore state_referenced_locally -- deliberate: seeds initial state once from the
	// server-loaded prop at mount, same as SubscriptionToggle.svelte's identical initializer;
	// this page does not track later changes to `data.agents` after mount (mutations flow
	// through the local `agents` state instead, same as settings/keys/+page.svelte's `keys`).
	let agents = $state<AgentRow[]>(data.agents ?? []);
	let keysByAgent = $state<Record<string, KeyRow[]>>({});
	let keysLoaded = $state(false);

	let newAgentName = $state('');
	let newAgentKind = $state<'ai' | 'bot'>('ai');
	let creating = $state(false);

	let toastMessage = $state<string | null>(null);
	let toastType = $state<'error' | 'success'>('error');
	let newlyIssuedToken = $state<string | null>(null);
	let issuingForAgent = $state<string | null>(null);

	// N3a: webhooks, keyed by agentId, mirroring keysByAgent's shape/lifecycle above.
	let webhooksByAgent = $state<Record<string, WebhookRow[]>>({});
	let webhooksLoaded = $state<Record<string, boolean>>({});
	let newWebhookUrlByAgent = $state<Record<string, string>>({});
	let creatingWebhookForAgent = $state<string | null>(null);
	let newlyIssuedWebhookSecret = $state<string | null>(null);

	function showToast(message: string, type: 'error' | 'success' = 'error') {
		toastMessage = message;
		toastType = type;
		setTimeout(() => {
			if (toastMessage === message) toastMessage = null;
		}, 5000);
	}

	async function loadKeys() {
		try {
			const res = await fetch(`/api/organizations/${data.orgId}/keys`);
			if (!res.ok) return;
			const body = await res.json();
			const grouped: Record<string, KeyRow[]> = {};
			for (const key of (body.keys ?? []) as KeyRow[]) {
				if (!key.agentId) continue;
				(grouped[key.agentId] ??= []).push(key);
			}
			keysByAgent = grouped;
		} finally {
			keysLoaded = true;
		}
	}

	$effect(() => {
		void loadKeys();
	});

	async function loadWebhooks(agentId: string) {
		try {
			const res = await fetch(`/api/organizations/${data.orgId}/agents/${agentId}/webhooks`);
			if (!res.ok) return;
			const body = await res.json();
			webhooksByAgent = { ...webhooksByAgent, [agentId]: body.webhooks ?? [] };
		} finally {
			webhooksLoaded = { ...webhooksLoaded, [agentId]: true };
		}
	}

	$effect(() => {
		for (const agent of agents) {
			if (!webhooksLoaded[agent.id]) void loadWebhooks(agent.id);
		}
	});

	async function createAgent() {
		if (!newAgentName.trim()) {
			showToast('Agent name is required');
			return;
		}
		creating = true;
		try {
			const res = await fetch(`/api/organizations/${data.orgId}/agents`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ name: newAgentName.trim(), kind: newAgentKind }),
			});
			if (!res.ok) {
				const err = await res.json().catch(() => ({ message: 'Failed to create agent' }));
				showToast(err.message || `Error ${res.status}: failed to create agent`);
				return;
			}
			const { agent } = await res.json();
			agents = [agent, ...agents];
			newAgentName = '';
			showToast('Agent created', 'success');
		} catch (err: any) {
			showToast(err?.message || 'Network error while creating agent');
		} finally {
			creating = false;
		}
	}

	async function setStatus(agent: AgentRow, status: 'active' | 'disabled') {
		try {
			const res = await fetch(`/api/organizations/${data.orgId}/agents/${agent.id}`, {
				method: 'PATCH',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ status }),
			});
			if (!res.ok) {
				const err = await res.json().catch(() => ({ message: 'Failed to update agent' }));
				showToast(err.message || `Error ${res.status}: failed to update agent`);
				return;
			}
			agents = agents.map((a) => (a.id === agent.id ? { ...a, status } : a));
			showToast(status === 'disabled' ? 'Agent disabled' : 'Agent re-enabled', 'success');
		} catch (err: any) {
			showToast(err?.message || 'Network error while updating agent');
		}
	}

	async function issueKey(agent: AgentRow) {
		issuingForAgent = agent.id;
		try {
			const res = await fetch(`/api/organizations/${data.orgId}/keys`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ name: `${agent.name} key`, scope: 'agent', agentId: agent.id }),
			});
			if (!res.ok) {
				const err = await res.json().catch(() => ({ message: 'Failed to issue key' }));
				showToast(err.message || `Error ${res.status}: failed to issue key`);
				return;
			}
			const { key, token } = await res.json();
			newlyIssuedToken = token;
			keysByAgent = { ...keysByAgent, [agent.id]: [key, ...(keysByAgent[agent.id] ?? [])] };
			showToast('Agent key issued', 'success');
		} catch (err: any) {
			showToast(err?.message || 'Network error while issuing key');
		} finally {
			issuingForAgent = null;
		}
	}

	async function revokeKey(agentId: string, keyId: string) {
		try {
			const res = await fetch(`/api/organizations/${data.orgId}/keys/${keyId}`, { method: 'DELETE' });
			if (!res.ok) {
				const err = await res.json().catch(() => ({ message: 'Failed to revoke key' }));
				showToast(err.message || `Error ${res.status}: failed to revoke key`);
				return;
			}
			keysByAgent = {
				...keysByAgent,
				[agentId]: (keysByAgent[agentId] ?? []).map((k) => (k.id === keyId ? { ...k, status: 'revoked' } : k)),
			};
			showToast('Agent key revoked', 'success');
		} catch (err: any) {
			showToast(err?.message || 'Network error while revoking key');
		}
	}

	async function createWebhook(agent: AgentRow) {
		const url = (newWebhookUrlByAgent[agent.id] ?? '').trim();
		if (!url) {
			showToast('Webhook URL is required');
			return;
		}
		creatingWebhookForAgent = agent.id;
		try {
			const res = await fetch(`/api/organizations/${data.orgId}/agents/${agent.id}/webhooks`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ url }),
			});
			if (!res.ok) {
				const err = await res.json().catch(() => ({ message: 'Failed to create webhook' }));
				showToast(err.message || `Error ${res.status}: failed to create webhook`);
				return;
			}
			const { webhook, secret } = await res.json();
			newlyIssuedWebhookSecret = secret;
			webhooksByAgent = { ...webhooksByAgent, [agent.id]: [webhook, ...(webhooksByAgent[agent.id] ?? [])] };
			newWebhookUrlByAgent = { ...newWebhookUrlByAgent, [agent.id]: '' };
			showToast('Webhook created', 'success');
		} catch (err: any) {
			showToast(err?.message || 'Network error while creating webhook');
		} finally {
			creatingWebhookForAgent = null;
		}
	}

	async function setWebhookStatus(agentId: string, webhook: WebhookRow, status: 'active' | 'disabled') {
		try {
			const res = await fetch(`/api/organizations/${data.orgId}/agents/${agentId}/webhooks/${webhook.id}`, {
				method: 'PATCH',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ status }),
			});
			if (!res.ok) {
				const err = await res.json().catch(() => ({ message: 'Failed to update webhook' }));
				showToast(err.message || `Error ${res.status}: failed to update webhook`);
				return;
			}
			const { webhook: updated } = await res.json();
			webhooksByAgent = {
				...webhooksByAgent,
				[agentId]: (webhooksByAgent[agentId] ?? []).map((w) => (w.id === webhook.id ? updated : w)),
			};
			showToast(status === 'disabled' ? 'Webhook disabled' : 'Webhook re-enabled', 'success');
		} catch (err: any) {
			showToast(err?.message || 'Network error while updating webhook');
		}
	}

	async function deleteWebhook(agentId: string, webhookId: string) {
		try {
			const res = await fetch(`/api/organizations/${data.orgId}/agents/${agentId}/webhooks/${webhookId}`, {
				method: 'DELETE',
			});
			if (!res.ok) {
				const err = await res.json().catch(() => ({ message: 'Failed to delete webhook' }));
				showToast(err.message || `Error ${res.status}: failed to delete webhook`);
				return;
			}
			webhooksByAgent = {
				...webhooksByAgent,
				[agentId]: (webhooksByAgent[agentId] ?? []).filter((w) => w.id !== webhookId),
			};
			showToast('Webhook deleted', 'success');
		} catch (err: any) {
			showToast(err?.message || 'Network error while deleting webhook');
		}
	}
</script>

<div class="agents-page">
	<header class="agents-header">
		<div>
			<h1>Agents</h1>
			<p class="subtitle">
				AI/bot identities that can claim and work manual reports on this organization's behalf. Owner/admin only.
			</p>
		</div>
	</header>

	{#if toastMessage}
		<div class="toast" class:toast-error={toastType === 'error'} class:toast-success={toastType === 'success'} role="status">
			<span>{toastMessage}</span>
			<button type="button" onclick={() => (toastMessage = null)}>✕</button>
		</div>
	{/if}

	{#if newlyIssuedToken}
		<div class="secret-tray" role="alert">
			<div class="secret-tray-head">
				<strong>Save this agent key now — it will never be shown again.</strong>
				<button type="button" onclick={() => (newlyIssuedToken = null)}>✕</button>
			</div>
			<code class="secret-token">{newlyIssuedToken}</code>
		</div>
	{/if}

	{#if newlyIssuedWebhookSecret}
		<div class="secret-tray" role="alert">
			<div class="secret-tray-head">
				<strong>Save this webhook secret now — it will never be shown again.</strong>
				<button type="button" onclick={() => (newlyIssuedWebhookSecret = null)}>✕</button>
			</div>
			<code class="secret-token">{newlyIssuedWebhookSecret}</code>
		</div>
	{/if}

	<section class="create-agent">
		<input
			type="text"
			placeholder="Agent name (e.g. AutoFix Agent)"
			bind:value={newAgentName}
			maxlength="255"
		/>
		<select bind:value={newAgentKind}>
			<option value="ai">AI</option>
			<option value="bot">Bot</option>
		</select>
		<button type="button" disabled={creating} onclick={createAgent}>
			{creating ? 'Creating…' : '+ Create agent'}
		</button>
	</section>

	{#if agents.length === 0}
		<p class="empty-state">No agents yet. Create one above to let it claim and work manual reports.</p>
	{:else}
		<ul class="agent-list">
			{#each agents as agent (agent.id)}
				<li class="agent-card">
					<div class="agent-card-head">
						<div>
							<span class="agent-name">{agent.kind === 'ai' ? '🤖' : '⚙️'} {agent.name}</span>
							<span class="agent-status" class:is-disabled={agent.status === 'disabled'}>{agent.status}</span>
						</div>
						<div class="agent-actions">
							<button type="button" onclick={() => issueKey(agent)} disabled={issuingForAgent === agent.id}>
								{issuingForAgent === agent.id ? 'Issuing…' : 'Issue key'}
							</button>
							{#if agent.status === 'active'}
								<button type="button" class="danger" onclick={() => setStatus(agent, 'disabled')}>Disable</button>
							{:else}
								<button type="button" onclick={() => setStatus(agent, 'active')}>Re-enable</button>
							{/if}
						</div>
					</div>

					<div class="agent-keys">
						{#if !keysLoaded}
							<p class="empty-state small">Loading keys…</p>
						{:else if (keysByAgent[agent.id] ?? []).length === 0}
							<p class="empty-state small">No keys issued yet.</p>
						{:else}
							<ul>
								{#each keysByAgent[agent.id] as key (key.id)}
									<li class="key-row">
										<code>{key.keyPrefix}…</code>
										<span class="key-status" class:is-revoked={key.status === 'revoked'}>{key.status}</span>
										{#if key.status === 'active'}
											<button type="button" class="danger small" onclick={() => revokeKey(agent.id, key.id)}>Revoke</button>
										{/if}
									</li>
								{/each}
							</ul>
						{/if}
					</div>

					<div class="agent-webhooks">
						<div class="webhooks-head">Webhooks</div>
						{#if !webhooksLoaded[agent.id]}
							<p class="empty-state small">Loading webhooks…</p>
						{:else if (webhooksByAgent[agent.id] ?? []).length === 0}
							<p class="empty-state small">No webhooks registered.</p>
						{:else}
							<ul>
								{#each webhooksByAgent[agent.id] as webhook (webhook.id)}
									<li class="webhook-row">
										<div class="webhook-row-main">
											<code>{webhook.url}</code>
											<span class="key-status" class:is-revoked={webhook.status !== 'active'}>
												{webhook.status}
											</span>
										</div>
										<div class="webhook-row-meta">
											<span>{webhook.secretPrefix}…</span>
											<span>{webhook.eventTypes.length === 0 ? 'all events' : webhook.eventTypes.join(', ')}</span>
											{#if webhook.consecutiveFailures > 0}
												<span class="webhook-failures">{webhook.consecutiveFailures} consecutive failures</span>
											{/if}
										</div>
										<div class="webhook-row-actions">
											{#if webhook.status === 'active'}
												<button type="button" class="danger small" onclick={() => setWebhookStatus(agent.id, webhook, 'disabled')}>
													Disable
												</button>
											{:else}
												<button type="button" class="small" onclick={() => setWebhookStatus(agent.id, webhook, 'active')}>
													Re-enable
												</button>
											{/if}
											<button type="button" class="danger small" onclick={() => deleteWebhook(agent.id, webhook.id)}>
												Delete
											</button>
										</div>
									</li>
								{/each}
							</ul>
						{/if}

						<div class="webhook-create">
							<input
								type="text"
								placeholder="https://example.com/webhooks/sentinel"
								value={newWebhookUrlByAgent[agent.id] ?? ''}
								oninput={(e) => (newWebhookUrlByAgent = { ...newWebhookUrlByAgent, [agent.id]: (e.currentTarget as HTMLInputElement).value })}
							/>
							<button
								type="button"
								disabled={creatingWebhookForAgent === agent.id}
								onclick={() => createWebhook(agent)}
							>
								{creatingWebhookForAgent === agent.id ? 'Adding…' : '+ Add webhook'}
							</button>
						</div>
					</div>
				</li>
			{/each}
		</ul>
	{/if}
</div>

<style>
	.agents-page {
		max-width: 56rem;
		margin: 0 auto;
		padding: 1.5rem;
		display: flex;
		flex-direction: column;
		gap: 1.25rem;
		color: var(--text-primary);
	}

	.agents-header h1 {
		margin: 0;
		font-size: 1.25rem;
		font-weight: 700;
	}

	.subtitle {
		margin: 0.25rem 0 0;
		font-size: 0.8rem;
		color: var(--text-primary);
		opacity: 0.7;
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

	.secret-tray {
		border: 2px solid #d97706;
		border-radius: var(--radius-sm);
		padding: 0.875rem 1rem;
		background: var(--bg-surface);
	}

	.secret-tray-head {
		display: flex;
		justify-content: space-between;
		align-items: center;
		gap: 0.5rem;
		margin-bottom: 0.5rem;
		font-size: 0.8rem;
	}

	.secret-tray-head button {
		background: none;
		border: none;
		color: inherit;
		cursor: pointer;
	}

	.secret-token {
		display: block;
		word-break: break-all;
		font-family: monospace;
		font-size: 0.8rem;
		padding: 0.5rem;
		background: var(--bg-root);
		border-radius: var(--radius-sm);
	}

	.create-agent {
		display: flex;
		gap: 0.5rem;
		flex-wrap: wrap;
	}

	.create-agent input,
	.create-agent select {
		background: var(--bg-root);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.5rem 0.625rem;
		font-size: 0.8rem;
	}

	.create-agent input {
		flex: 1;
		min-width: 12rem;
	}

	.create-agent button,
	.agent-actions button,
	.key-row button {
		background: var(--bg-surface);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.5rem 0.75rem;
		font-size: 0.75rem;
		font-weight: 600;
		cursor: pointer;
	}

	.create-agent button:hover,
	.agent-actions button:hover,
	.key-row button:hover {
		background: var(--bg-surface-hover);
	}

	.create-agent button:disabled,
	.agent-actions button:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}

	.danger {
		color: #f87171;
		border-color: #b91c1c;
	}

	.empty-state {
		font-size: 0.8rem;
		opacity: 0.7;
	}

	.empty-state.small {
		margin: 0.25rem 0 0;
	}

	.agent-list {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
	}

	.agent-card {
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		background: var(--bg-surface);
		padding: 0.875rem 1rem;
	}

	.agent-card-head {
		display: flex;
		justify-content: space-between;
		align-items: center;
		flex-wrap: wrap;
		gap: 0.5rem;
	}

	.agent-name {
		font-weight: 600;
		font-size: 0.85rem;
	}

	.agent-status {
		margin-left: 0.5rem;
		font-size: 0.65rem;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		padding: 0.125rem 0.375rem;
		border-radius: 999px;
		background: rgba(34, 197, 94, 0.15);
		color: #4ade80;
	}

	.agent-status.is-disabled {
		background: rgba(248, 113, 113, 0.15);
		color: #f87171;
	}

	.agent-actions {
		display: flex;
		gap: 0.5rem;
	}

	.agent-keys {
		margin-top: 0.625rem;
		padding-top: 0.625rem;
		border-top: 1px dashed var(--border-color);
	}

	.agent-keys ul {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.375rem;
	}

	.key-row {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		font-size: 0.75rem;
	}

	.key-status {
		font-size: 0.65rem;
		text-transform: uppercase;
		opacity: 0.7;
	}

	.key-status.is-revoked {
		color: #f87171;
	}

	.key-row button.small {
		padding: 0.25rem 0.5rem;
	}

	.agent-webhooks {
		margin-top: 0.625rem;
		padding-top: 0.625rem;
		border-top: 1px dashed var(--border-color);
	}

	.webhooks-head {
		font-size: 0.7rem;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		opacity: 0.7;
		margin-bottom: 0.375rem;
	}

	.agent-webhooks ul {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.5rem;
	}

	.webhook-row {
		display: flex;
		flex-direction: column;
		gap: 0.25rem;
		font-size: 0.75rem;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.5rem 0.625rem;
	}

	.webhook-row-main {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		flex-wrap: wrap;
	}

	.webhook-row-meta {
		display: flex;
		gap: 0.75rem;
		flex-wrap: wrap;
		font-size: 0.7rem;
		opacity: 0.75;
	}

	.webhook-failures {
		color: #f87171;
	}

	.webhook-row-actions {
		display: flex;
		gap: 0.5rem;
	}

	.webhook-create {
		display: flex;
		gap: 0.5rem;
		margin-top: 0.5rem;
	}

	.webhook-create input {
		flex: 1;
		min-width: 12rem;
		background: var(--bg-root);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.4rem 0.5rem;
		font-size: 0.75rem;
	}

	.webhook-create button {
		background: var(--bg-surface);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.4rem 0.75rem;
		font-size: 0.75rem;
		font-weight: 600;
		cursor: pointer;
	}

	.webhook-create button:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}
</style>
