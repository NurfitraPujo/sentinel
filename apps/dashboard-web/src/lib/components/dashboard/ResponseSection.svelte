<script lang="ts">
	// D13 section 6, flow H7: how fast issues get handled. MTTA and MTTR (median and p90), regression rate, change failure
	// rate, and user reports waiting.
	import Tile from './Tile.svelte';
	import { count, duration, listHref, pct } from '$lib/dashboard-format';
	import type { ResponseData } from '$lib/server/analytics/service';

	interface Props {
		data: ResponseData;
		filters: { project: string; environment: string };
	}
	let { data, filters }: Props = $props();
	const q = (r: ResponseData['mtta']) => (r ? `p90 ${duration(r.p90)} · ${count(r.count)} issues` : 'nothing in this range');
</script>

<div class="tiles" data-testid="response-section">
	<Tile label="MTTA (median)" value={duration(data.mtta?.p50)} sub={q(data.mtta)} testId="mtta" />
	<Tile label="MTTR (median)" value={duration(data.mttr?.p50)} sub={q(data.mttr)} testId="mttr" />
	<Tile label="Regression rate" value={pct(data.regressionRate, 0)} sub="regressions ÷ resolutions" testId="regression-rate" />
	<Tile label="Change failure rate" value={pct(data.changeFailureRate, 0)} sub="{count(data.releasesInRange)} releases first seen in range" testId="change-failure-rate" />
	<Tile label="User reports waiting" value={count(data.reportsWaiting)} href={listHref('reports', filters)} testId="reports-waiting" />
</div>

<style>
	.tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(11rem, 1fr)); gap: 0.5rem; }
</style>
