<script lang="ts">
	// CONTEXT.md "Claim" / DECISIONS.md D24: claims are only ever self-acquired — nothing assigns
	// an issue *to* an agent on its behalf. This picker therefore offers NO agent options (the M5
	// version listed the org's agents here, which was the UI half of the defect the server now
	// 400s). An existing agent claim is still DISPLAYED (🤖), and picking "Unassigned" over it is
	// the deliberate admin release override — the server journals it as claim_released.
	interface Assignee {
		type: 'user' | 'agent';
		id: string;
		name: string;
		avatarUrl?: string;
	}

	interface Props {
		assignee?: Assignee | null;
		onAssign?: (assignee: Assignee | null) => void;
	}

	let { assignee = null, onAssign = () => {} }: Props = $props();

	let isOpen = $state(false);

	function toggle() {
		isOpen = !isOpen;
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
			<p class="picker-empty">Agents can't be assigned — they claim issues themselves.</p>
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
