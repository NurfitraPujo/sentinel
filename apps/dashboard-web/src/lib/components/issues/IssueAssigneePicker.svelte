<script lang="ts">
	// Manual Issues M5 §7: the M3 stub hardcoded a single fake agent ("AutoFix Agent", id '2').
	// This now lists REAL agent rows for `organizationId` via GET
	// /api/organizations/[orgId]/agents (owner/admin only per the design's §9 permission matrix
	// -- a caller without 'manage_agents' sees no agents in the dropdown, which degrades to the
	// empty state below rather than erroring the whole picker). Only active agents are offered,
	// since a disabled agent should not be assignable to new work.
	interface AgentOption {
		id: string;
		name: string;
		status: 'active' | 'disabled';
	}

	interface Assignee {
		type: 'user' | 'agent';
		id: string;
		name: string;
		avatarUrl?: string;
	}

	interface Props {
		assignee?: Assignee | null;
		organizationId?: string | null;
		onAssign?: (assignee: Assignee | null) => void;
	}

	let { assignee = null, organizationId = null, onAssign = () => {} }: Props = $props();

	let isOpen = $state(false);
	let agents = $state<AgentOption[]>([]);
	let agentsLoaded = $state(false);
	let agentsError = $state<string | null>(null);

	async function loadAgents() {
		if (!organizationId || agentsLoaded) return;
		try {
			const res = await fetch(`/api/organizations/${organizationId}/agents`);
			if (!res.ok) {
				// A 403 here means the caller isn't owner/admin -- not a real error, just no agents
				// to offer. Anything else is worth surfacing.
				agentsError = res.status === 403 ? null : `Failed to load agents (${res.status})`;
				agentsLoaded = true;
				return;
			}
			const body = await res.json();
			agents = ((body.agents ?? []) as AgentOption[]).filter((a) => a.status === 'active');
			agentsLoaded = true;
		} catch (err) {
			agentsError = err instanceof Error ? err.message : 'Failed to load agents';
			agentsLoaded = true;
		}
	}

	function toggle() {
		isOpen = !isOpen;
		if (isOpen) void loadAgents();
	}

	function pick(next: Assignee | null) {
		onAssign(next);
		isOpen = false;
	}
</script>

<div class="assignee-picker">
	<button type="button" class="picker-trigger" onclick={toggle}>
		{#if assignee}
			{#if assignee.type === 'agent'}
				🤖 {assignee.name}
			{:else}
				👤 {assignee.name}
			{/if}
		{:else}
			Unassigned
		{/if}
	</button>

	{#if isOpen}
		<div class="picker-menu">
			<button type="button" class="picker-item" onclick={() => pick(null)}>Unassigned</button>

			{#if agentsError}
				<p class="picker-empty" role="alert">{agentsError}</p>
			{:else if !agentsLoaded}
				<p class="picker-empty">Loading agents…</p>
			{:else if agents.length === 0}
				<p class="picker-empty">No agents in this organization yet.</p>
			{:else}
				{#each agents as agent (agent.id)}
					<button
						type="button"
						class="picker-item"
						onclick={() => pick({ type: 'agent', id: agent.id, name: agent.name })}
					>
						🤖 {agent.name}
					</button>
				{/each}
			{/if}
		</div>
	{/if}
</div>

<style>
	.assignee-picker {
		position: relative;
		display: inline-block;
	}

	.picker-trigger {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		background: var(--bg-root);
		color: var(--text-primary);
		padding: 0.375rem 0.75rem;
		font-size: 0.75rem;
		font-weight: 600;
		cursor: pointer;
	}

	.picker-trigger:hover {
		background: var(--bg-surface-hover);
	}

	.picker-menu {
		position: absolute;
		right: 0;
		top: calc(100% + 0.375rem);
		min-width: 14rem;
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		box-shadow: 0 4px 12px rgba(0, 0, 0, 0.15);
		padding: 0.25rem;
		z-index: 10;
	}

	.picker-item {
		display: block;
		width: 100%;
		text-align: left;
		background: transparent;
		border: none;
		border-radius: var(--radius-sm);
		padding: 0.5rem 0.625rem;
		font-size: 0.75rem;
		color: var(--text-primary);
		cursor: pointer;
	}

	.picker-item:hover {
		background: var(--bg-surface-hover);
	}

	.picker-empty {
		margin: 0;
		padding: 0.5rem 0.625rem;
		font-size: 0.7rem;
		color: var(--text-secondary, var(--text-primary));
	}
</style>
