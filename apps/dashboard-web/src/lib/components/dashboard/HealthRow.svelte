<script lang="ts">
	// sentinel-reporting-and-dashboard D13 section 1, flows H2-H4, H8, as the "Error tracking" half (sentinel-dashboard-
	// metrics-improvement D1/D9): captured exceptions per 1k for API, jobs and NATS, captured exceptions by D19 origin
	// against the previous period, new issues, regressions and the open backlog. The SLO's own failures live in the
	// table above, not here. Every count links to /issues where the list has a matching filter (D12). Colour is state only.
	import Tile from './Tile.svelte';
	import { GROUP_KEYS, GROUP_LABELS, backlogSub, count, delta, deltaState, listHref, perK } from '$lib/dashboard-format';
	import type { HealthData, SliTile } from '$lib/server/analytics/service';

	interface Props {
		data: HealthData;
		filters: { project: string; environment: string; range: string; split?: string };
		apiService?: string;
	}
	let { data, filters, apiService }: Props = $props();

	let scope = $derived({ project: filters.project, environment: filters.environment });
	const byKind = (kind: SliTile['kind']) => data.slis.find((s) => s.kind === kind);
	const rateTiles = [
		{ kind: 'http' as const, label: 'Captured API exceptions per 1k requests', id: 'api', unit: 'requests' },
		{ kind: 'jobs' as const, label: 'Captured job exceptions per 1k runs', id: 'jobs', unit: 'runs' },
		{ kind: 'nats' as const, label: 'Captured NATS exceptions per 1k messages', id: 'nats', unit: 'messages' },
	];
	const trend = (cur: number, prev: number) => {
		const d = delta(cur, prev);
		return `${d.label} vs previous period`;
	};
	// D13: an increase of 5% or more is bad, a decrease of 5% or more is good, anything smaller is neutral
	const errState = deltaState;
	let totalEvents = $derived(GROUP_KEYS.reduce((a, g) => a + data.events.current[g], 0));
	let prevEvents = $derived(GROUP_KEYS.reduce((a, g) => a + data.events.previous[g], 0));
</script>

<div class="health" data-testid="health-row">
	<div class="tiles">
		{#each rateTiles as t (t.id)}
			{@const s = byKind(t.kind)}
			<Tile label={t.label} value={perK(s?.errorsPer1k)} sub={s?.errorsPer1k === null || s === undefined ? 'no data in this range' : `${count(s.events ?? 0)} exceptions / ${count(s.total)} ${t.unit}`} testId="per1k-{t.id}" />
		{/each}
	</div>

	<h3 class="sub-h">Captured exceptions by origin</h3>
	<div class="tiles" data-testid="event-groups">
		{#each GROUP_KEYS as g (g)}
			<Tile
				label={GROUP_LABELS[g]}
				value={count(data.events.current[g])}
				sub={trend(data.events.current[g], data.events.previous[g])}
				state={errState(data.events.current[g], data.events.previous[g])}
				href={g === 'api' ? listHref('group:api', scope, { apiService }) : g === 'jobs' ? listHref('group:jobs', scope) : null}
				testId="group-{g}"
			/>
		{/each}
		<Tile label="All captured exceptions" value={count(totalEvents)} sub={trend(totalEvents, prevEvents)} state={errState(totalEvents, prevEvents)} testId="group-total" />
	</div>

	<div class="tiles">
		<Tile label="New issues" value={count(data.newIssues.current)} sub={trend(data.newIssues.current, data.newIssues.previous)} state={errState(data.newIssues.current, data.newIssues.previous)} href={filters.range === '24h' ? listHref('new24h', scope) : null} testId="tile-new-issues" />
		<Tile label="Regressions" value={count(data.regressions.current)} sub={trend(data.regressions.current, data.regressions.previous)} state={errState(data.regressions.current, data.regressions.previous)} href={listHref('regressions', scope)} testId="tile-regressions" />
		<Tile label="Open backlog" value={count(data.backlog.open)} sub={backlogSub(data.backlog)} href={listHref('open', scope)} testId="tile-backlog" />
	</div>
</div>

<style>
	.health { display: flex; flex-direction: column; gap: 0.625rem; }
	.tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(11rem, 1fr)); gap: 0.5rem; }
	.sub-h { margin: 0.25rem 0 0; font-size: 0.75rem; font-weight: 600; color: var(--text-muted); }
</style>
