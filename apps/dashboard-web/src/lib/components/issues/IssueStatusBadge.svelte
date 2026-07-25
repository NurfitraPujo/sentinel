<script lang="ts">
	export let status: 'unresolved' | 'resolved' | 'ignored' = 'unresolved';
	export let regressionStatus: 'none' | 'regressed' = 'none';

	$: badgeColor = regressionStatus === 'regressed'
		? 'bg-red-500/10 text-red-500 border-red-500/30'
		: status === 'resolved'
			? 'bg-emerald-500/10 text-emerald-500 border-emerald-500/30'
			: status === 'ignored'
				? 'bg-zinc-500/10 text-zinc-400 border-zinc-500/30'
				: 'bg-amber-500/10 text-amber-500 border-amber-500/30';
</script>

<span class={`inline-flex items-center gap-1.5 px-2.5 py-0.5 rounded-full text-xs font-medium border ${badgeColor}`}>
	{#if regressionStatus === 'regressed'}
		<span class="w-1.5 h-1.5 rounded-full bg-red-500 animate-pulse"></span>
		Regressed
	{:else}
		<span class="w-1.5 h-1.5 rounded-full bg-current"></span>
		{status.charAt(0).toUpperCase() + status.slice(1)}
	{/if}
</span>
