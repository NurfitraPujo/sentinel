<script lang="ts">
	// D13 / flow H9: "analytics as of <time>", with a warning when Databend is more than 10 minutes behind Postgres.
	import type { FreshnessData } from '$lib/server/analytics/service';
	interface Props {
		data: FreshnessData;
	}
	let { data }: Props = $props();
	let asOf = $derived(data.analyticsAsOf ? new Date(data.analyticsAsOf).toLocaleString('en-US') : null);
</script>

<p class="freshness" data-testid="freshness">
	{#if asOf}analytics as of {asOf}{:else}no analytics data yet{/if}
	{#if data.warn}
		<span class="warn" role="status" data-testid="freshness-warning">Analytics is {data.lagMinutes} minutes behind the live data; recent errors may be missing here.</span>
	{/if}
</p>

<style>
	.freshness { margin: 0; font-size: 0.75rem; color: var(--text-muted); }
	.warn { margin-left: 0.5rem; color: var(--status-open-text); }
</style>
