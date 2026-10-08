<script lang="ts">
	// D14: the three non-data states of a section. `unavailable` says what still works; `loading` is the skeleton shown
	// while the section's own promise is pending.
	interface Props {
		state: 'loading' | 'unavailable' | 'empty';
		name: string;
		message?: string;
	}
	let { state, name, message }: Props = $props();
</script>

<div class="section-state {state}" data-testid="{name}-{state}" role={state === 'unavailable' ? 'status' : undefined}>
	{#if state === 'loading'}
		Loading…
	{:else if state === 'unavailable'}
		Unavailable — {message ?? 'the analytics store could not be reached. The issue list and the rest of the page still work.'}
	{:else}
		{message ?? 'Nothing to show for this selection.'}
	{/if}
</div>

<style>
	.section-state { padding: 0.75rem; border: 1px dashed var(--border-color); border-radius: var(--radius-md); color: var(--text-muted); font-size: 0.8125rem; }
	.unavailable { color: var(--status-open-text); border-color: var(--status-open-border); }
</style>
