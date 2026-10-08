<script lang="ts">
	// sentinel-reporting-and-dashboard D13 section 1, flows H2-H4, H8: the health row. One tile per SLI with a target
	// (availability vs target with budget left, latency vs target), errors per 1k for API, jobs and NATS, error events by
	// D19 group against the previous period, new issues, regressions and the open backlog. Every count links to /issues
	// where the list has a matching filter (D12). Colour is state only.
	import Tile from './Tile.svelte';
	import { GROUP_KEYS, GROUP_LABELS, count, delta, listHref, pct, perK } from '$lib/dashboard-format';
	import type { HealthData, SliTile } from '$lib/server/analytics/service';

	interface Props {
		data: HealthData;
		filters: { project: string; environment: string; range: string };
		apiService?: string;
	}
	let { data, filters, apiService }: Props = $props();

	let scope = $derived({ project: filters.project, environment: filters.environment });
	let targeted = $derived(data.slis.filter((s) => s.availabilityTarget !== null || s.latencyTarget !== null));
	const byKind = (kind: SliTile['kind']) => data.slis.find((s) => s.kind === kind);
	const rateTiles = [
		{ kind: 'http' as const, label: 'API errors per 1k requests', id: 'api' },
		{ kind: 'jobs' as const, label: 'Job errors per 1k runs', id: 'jobs' },
		{ kind: 'nats' as const, label: 'NATS errors per 1k messages', id: 'nats' },
	];
	const trend = (cur: number, prev: number) => {
		const d = delta(cur, prev);
		return `${d.label} vs previous period`;
	};
	// an increase in errors is the one state worth colouring
	const errState = (cur: number, prev: number) => (cur > prev ? ('warn' as const) : undefined);
	let totalEvents = $derived(GROUP_KEYS.reduce((a, g) => a + data.events.current[g], 0));
	let prevEvents = $derived(GROUP_KEYS.reduce((a, g) => a + data.events.previous[g], 0));
</script>

<div class="health" data-testid="health-row">
	{#if targeted.length > 0}
		<div class="tiles" data-testid="sli-tiles">
			{#each targeted as s (s.id)}
				{@const availOk = s.availability !== null && s.availabilityTarget !== null ? s.availability >= s.availabilityTarget : undefined}
				{@const latOk = s.latency !== null && s.latencyTarget !== null ? s.latency >= s.latencyTarget : undefined}
				<div class="sli-tile" data-testid="sli-tile-{s.id}">
					<Tile
						label="{s.id} availability"
						value={pct(s.availability)}
						sub="target {pct(s.availabilityTarget, 1)} · budget left {s.budgetRemaining === null ? 'no data' : pct(s.budgetRemaining, 0)}"
						state={availOk === undefined ? undefined : availOk ? 'ok' : 'bad'}
						testId="sli-availability-{s.id}"
					/>
					{#if s.latencyTarget !== null}
						<Tile
							label="{s.id} latency"
							value={pct(s.latency)}
							sub="under the objective · target {pct(s.latencyTarget, 0)}"
							state={latOk === undefined ? undefined : latOk ? 'ok' : 'bad'}
							testId="sli-latency-{s.id}"
						/>
					{/if}
				</div>
			{/each}
		</div>
		<p class="note" data-testid="sli-note">
			The edge sees long-lived streams as slow requests and has no route label to exclude them, so latency reads lower than
			ordinary requests feel.
		</p>
	{/if}

	<div class="tiles">
		{#each rateTiles as t (t.id)}
			{@const s = byKind(t.kind)}
			<Tile label={t.label} value={perK(s?.errorsPer1k)} sub={s?.errorsPer1k === null || s === undefined ? 'no data in this range' : `${count(s.events ?? 0)} errors / ${count(s.total)}`} testId="per1k-{t.id}" />
		{/each}
	</div>

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
		<Tile label="All error events" value={count(totalEvents)} sub={trend(totalEvents, prevEvents)} testId="group-total" />
	</div>

	<div class="tiles">
		<Tile label="New issues" value={count(data.newIssues.current)} sub={trend(data.newIssues.current, data.newIssues.previous)} href={filters.range === '24h' ? listHref('new24h', scope) : null} testId="tile-new-issues" />
		<Tile label="Regressions" value={count(data.regressions.current)} sub={trend(data.regressions.current, data.regressions.previous)} state={errState(data.regressions.current, data.regressions.previous)} href={listHref('regressions', scope)} testId="tile-regressions" />
		<Tile label="Open backlog" value={count(data.backlog.open)} sub="{count(data.backlog.unassigned)} unassigned" href={listHref('open', scope)} testId="tile-backlog" />
	</div>
</div>

<style>
	.health { display: flex; flex-direction: column; gap: 0.625rem; }
	.tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(11rem, 1fr)); gap: 0.5rem; }
	.sli-tile { display: contents; }
	.note { margin: 0; font-size: 0.75rem; color: var(--text-muted); }
</style>
