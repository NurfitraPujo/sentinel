<script lang="ts">
	export let status: 'unresolved' | 'resolved' | 'ignored' = 'unresolved';
	export let regressionStatus: 'none' | 'regressed' = 'none';

	// Rewritten from Tailwind utility classes, which never resolved — this project has no Tailwind.
	// The variant is now a class name resolved by the scoped styles below rather than a string of
	// utilities; app.css already carries a token trio per status, which is what these map onto.
	$: variant = regressionStatus === 'regressed'
		? 'regressed'
		: status === 'resolved'
			? 'resolved'
			: status === 'ignored'
				? 'ignored'
				: 'unresolved';
</script>

<span class="badge {variant}">
	{#if regressionStatus === 'regressed'}
		<span class="dot pulse"></span>
		Regressed
	{:else}
		<span class="dot"></span>
		{status.charAt(0).toUpperCase() + status.slice(1)}
	{/if}
</span>

<style>
	.badge {
		display: inline-flex;
		align-items: center;
		gap: 0.375rem;
		padding: 0.125rem 0.625rem;
		border-radius: 999px;
		font-size: 0.75rem;
		font-weight: 500;
		border: 1px solid transparent;
	}

	.dot {
		width: 0.375rem;
		height: 0.375rem;
		border-radius: 999px;
		background-color: currentColor;
	}

	.pulse {
		animation: pulse 1.6s ease-in-out infinite;
	}

	@keyframes pulse {
		50% { opacity: 0.35; }
	}

	.regressed {
		background-color: var(--severity-critical-bg);
		color: var(--severity-critical-text);
		border-color: var(--severity-critical-border);
	}

	.resolved {
		background-color: var(--status-resolved-bg);
		color: var(--status-resolved-text);
		border-color: rgba(16, 185, 129, 0.3);
	}

	.ignored {
		background-color: var(--status-ignored-bg);
		color: var(--status-ignored-text);
		border-color: rgba(148, 163, 184, 0.3);
	}

	.unresolved {
		background-color: var(--status-open-bg);
		color: var(--status-open-text);
		border-color: rgba(245, 158, 11, 0.3);
	}
</style>
