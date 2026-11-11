<script lang="ts">
	// sentinel-user-report-issue §4.4a, fe.md row O4 — the rotation as an ordered list.
	//
	// **An ineligible member is greyed with the reason, never silently dropped.** §4.4a returns
	// `eligible: false` for a member whose org role has since fallen below `ISSUE_WRITE_ROLES`, and
	// it returns them *for exactly this purpose*: the rotation skips them, but removing them from
	// the list would change the rota behind the person who set it. Dropping them from the render is
	// the same defect one layer up.
	//
	// Reordering is up/down buttons rather than drag: keyboard-reachable, no dependency, and the
	// position numbers are the thing that has to be right.
	import type { OncallMember } from '$lib/types/oncall';

	interface Props {
		members: OncallMember[];
		canWrite: boolean;
		onreorder?: (members: OncallMember[]) => void;
		onremove?: (userId: string) => void;
	}

	let { members, canWrite, onreorder, onremove }: Props = $props();

	function move(index: number, delta: number) {
		const target = index + delta;
		if (target < 0 || target >= members.length) return;
		const next = [...members];
		[next[index], next[target]] = [next[target], next[index]];
		onreorder?.(next.map((member, position) => ({ ...member, position })));
	}
</script>

{#if members.length === 0}
	<p class="rotation-empty" data-testid="rotation-empty">
		Nobody is on the rotation yet. Escalation resolves nobody until at least one member is added.
	</p>
{:else}
	<ol class="rotation" data-testid="rotation-list">
		{#each members as member, index (member.userId)}
			<li
				class="rotation-member"
				class:ineligible={!member.eligible}
				data-testid={member.eligible ? 'rotation-member' : 'rotation-member-ineligible'}
			>
				<span class="position">{index + 1}</span>
				<span class="member-name">{member.name ?? member.userId}</span>
				{#if !member.eligible}
					<span class="ineligible-reason" data-testid="rotation-ineligible-reason">
						skipped — their organization role no longer allows issue write access
					</span>
				{/if}
				{#if canWrite}
					<span class="member-actions">
						<button
							type="button"
							class="btn-move"
							aria-label="Move {member.name ?? member.userId} up"
							disabled={index === 0}
							onclick={() => move(index, -1)}>↑</button
						>
						<button
							type="button"
							class="btn-move"
							aria-label="Move {member.name ?? member.userId} down"
							disabled={index === members.length - 1}
							onclick={() => move(index, 1)}>↓</button
						>
						<button
							type="button"
							class="btn-remove"
							aria-label="Remove {member.name ?? member.userId} from the rotation"
							onclick={() => onremove?.(member.userId)}>Remove</button
						>
					</span>
				{/if}
			</li>
		{/each}
	</ol>
{/if}

<style>
	.rotation {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.375rem;
	}

	.rotation-member {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		flex-wrap: wrap;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.375rem 0.625rem;
		font-size: 0.8125rem;
		color: var(--text-primary);
	}

	.ineligible {
		opacity: 0.55;
		border-style: dashed;
	}

	.position {
		font-family: var(--font-mono);
		font-size: 0.6875rem;
		color: var(--text-subtle);
		min-width: 1.25rem;
	}

	.member-name {
		font-weight: 500;
	}

	.ineligible-reason {
		font-size: 0.6875rem;
		color: var(--text-subtle);
	}

	.member-actions {
		margin-left: auto;
		display: flex;
		gap: 0.25rem;
	}

	.btn-move,
	.btn-remove {
		background: transparent;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		color: var(--text-muted);
		font-size: 0.6875rem;
		padding: 0.125rem 0.375rem;
		cursor: pointer;
	}

	.btn-move:disabled {
		opacity: 0.4;
		cursor: default;
	}

	.rotation-empty {
		font-size: 0.75rem;
		color: var(--text-subtle);
		margin: 0;
	}
</style>
