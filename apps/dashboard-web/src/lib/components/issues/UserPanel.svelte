<!--
	sentinel-occurrence-detail-panels §6.1 (TASK-03): the event's user, on the selected v2 occurrence.
	Shown unmasked (contract decision 1). A row renders only when its value is a non-empty string; with
	no row, the panel renders nothing at all.
-->
<script lang="ts">
	import ExpandableText from './ExpandableText.svelte';
	import { INLINE_LIMIT, geoLine } from './occurrence-display';

	let { user }: { user: unknown } = $props();

	let rows = $derived.by(() => {
		if (user === null || typeof user !== 'object' || Array.isArray(user)) return [];
		const u = user as Record<string, unknown>;
		const text = (v: unknown) => (typeof v === 'string' ? v : '');
		return [
			{ label: 'ID', value: text(u.id) },
			{ label: 'Username', value: text(u.username) },
			{ label: 'Email', value: text(u.email) },
			{ label: 'IP Address', value: text(u.ip_address) },
			{ label: 'Geo', value: geoLine(u.geo) },
		].filter((row) => row.value !== '');
	});
</script>

{#if rows.length > 0}
	<section class="panel">
		<h4>User</h4>
		<dl data-testid="user-panel">
			{#each rows as row (row.label)}
				<dt>{row.label}</dt>
				<dd><ExpandableText text={row.value} limit={INLINE_LIMIT} /></dd>
			{/each}
		</dl>
	</section>
{/if}

<style>
	.panel {
		margin: 0.75rem 0;
	}

	h4 {
		margin-bottom: 0.5rem;
		color: var(--text-subtle);
		font-size: 0.6875rem;
		font-family: var(--font-mono);
		text-transform: uppercase;
		letter-spacing: 0.05em;
	}

	dl {
		display: grid;
		grid-template-columns: max-content 1fr;
		gap: 0.25rem 0.75rem;
		font-size: 0.75rem;
	}

	dt {
		color: var(--text-muted);
	}

	dd {
		margin: 0;
		min-width: 0;
		color: var(--text-secondary);
	}
</style>
