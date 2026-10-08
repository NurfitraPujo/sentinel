<script lang="ts">
	// D13 section 4, flow H6: release health for the selected project (and environment). The last 10 releases by window
	// start. "First seen", never "deployed": D9 infers a release's start from its first event, nothing records a deploy.
	import { count, duration, perK } from '$lib/dashboard-format';
	import type { ReleaseRow } from '$lib/server/analytics/service';

	interface Props {
		rows: ReleaseRow[];
		/** Which environment each row is from matters only when the project has more than one in the table. */
		showEnvironment?: boolean;
	}
	let { rows, showEnvironment = false }: Props = $props();
	void duration;
	const changeText = (r: ReleaseRow) => {
		if (r.change === null) return '—';
		const sign = r.change > 0 ? '+' : r.change < 0 ? '−' : '';
		const v = Math.abs(r.change);
		// a rate change is per 1k; an events change is a count
		return r.errorsPer1k !== null ? `${sign}${v.toFixed(1)} per 1k` : `${sign}${count(v)} events`;
	};
</script>

<div class="wrap">
	<table data-testid="release-table">
		<caption class="visually-hidden">Release health: the latest releases, by when their first error was seen</caption>
		<thead>
			<tr>
				<th scope="col">Release</th>
				{#if showEnvironment}<th scope="col">Environment</th>{/if}
				<th scope="col">First seen</th>
				<th scope="col" class="num">API</th>
				<th scope="col" class="num">Jobs</th>
				<th scope="col" class="num">NATS</th>
				<th scope="col" class="num">Other</th>
				<th scope="col" class="num">New issues</th>
				<th scope="col" class="num">Regressions</th>
				<th scope="col" class="num">Errors per 1k</th>
				<th scope="col" class="num">Change</th>
			</tr>
		</thead>
		<tbody>
			{#each rows as r (r.environment + r.version)}
				<tr data-testid="release-row">
					<th scope="row" class="mono">{r.version}</th>
					{#if showEnvironment}<td>{r.environment}</td>{/if}
					<td>{new Date(r.firstSeen).toLocaleString('en-US')}</td>
					<td class="num">{count(r.events.api)}</td>
					<td class="num">{count(r.events.jobs)}</td>
					<td class="num">{count(r.events.nats)}</td>
					<td class="num">{count(r.events.other)}</td>
					<td class="num">{count(r.newIssues)}</td>
					<td class="num">{count(r.regressions)}</td>
					<td class="num">{perK(r.errorsPer1k)}</td>
					<td class="num" data-testid="release-change">{changeText(r)}</td>
				</tr>
			{/each}
		</tbody>
	</table>
</div>

<style>
	.wrap { overflow-x: auto; }
	table { width: 100%; border-collapse: collapse; font-size: 0.8125rem; font-variant-numeric: tabular-nums; }
	th, td { text-align: left; padding: 0.375rem 0.625rem; border-bottom: 1px solid var(--border-color-muted); white-space: nowrap; }
	thead th { font-size: 0.6875rem; text-transform: uppercase; letter-spacing: 0.04em; color: var(--text-muted); font-weight: 500; }
	.num { text-align: right; }
	.mono { font-family: var(--font-mono); font-weight: 500; }
	.visually-hidden { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap; }
</style>
