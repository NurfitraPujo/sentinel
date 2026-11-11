<script lang="ts">
	// One dense number with a label, an optional sub-line and an optional link into /issues (D12). Colour is state only:
	// `state` is 'ok' | 'bad' | 'warn' | undefined, never decoration.
	interface Props {
		label: string;
		value: string;
		sub?: string;
		state?: 'ok' | 'bad' | 'warn';
		href?: string | null;
		testId?: string;
	}
	let { label, value, sub, state, href = null, testId }: Props = $props();
</script>

<div class="tile {state ? `state-${state}` : ''}" data-testid={testId}>
	<span class="tile-label">{label}</span>
	{#if href}
		<a class="tile-value" {href}>{value}</a>
	{:else}
		<span class="tile-value">{value}</span>
	{/if}
	{#if sub}<span class="tile-sub">{sub}</span>{/if}
</div>

<style>
	.tile { display: flex; flex-direction: column; gap: 0.125rem; padding: 0.625rem 0.75rem; background: var(--bg-surface); border: 1px solid var(--border-color); border-radius: var(--radius-md); min-width: 0; }
	.tile-label { font-size: 0.6875rem; letter-spacing: 0.04em; text-transform: uppercase; color: var(--text-muted); }
	.tile-value { font-size: 1.25rem; font-weight: 600; color: var(--text-primary); font-variant-numeric: tabular-nums; text-decoration: none; }
	a.tile-value:hover { text-decoration: underline; }
	.tile-sub { font-size: 0.75rem; color: var(--text-muted); font-variant-numeric: tabular-nums; }
	.state-ok .tile-value { color: var(--status-resolved-text); }
	.state-bad { border-color: var(--severity-critical-border); }
	.state-bad .tile-value { color: var(--severity-critical-text); }
	.state-warn .tile-value { color: var(--status-open-text); }
</style>
