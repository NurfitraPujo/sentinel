<script lang="ts">
	// sentinel-dashboard-metrics-improvement D2: the SLO status table. One row per objective (availability for every SLI
	// with a target, latency for every http SLI with one), grouped by SLI in config order. The status is a word in a pill:
	// colour is never the only signal. Display only: no alert reads any of this.
	import { RANGE_LABELS, burn, count, pct, type Range } from '$lib/dashboard-format';
	import { SSE_CAVEAT } from '$lib/slo-status';
	import type { SloRow } from '$lib/server/analytics/slo';
	import type { NatsBreakdown } from '$lib/server/sli/nats-breakdown';

	interface Props {
		rows: SloRow[];
		range: Range | string;
		/** D8: the top NATS contributors, shown under the NATS availability row once it settles; the table never waits on it. */
		natsBreakdown?: Promise<NatsBreakdown | null>;
	}
	let { rows, range, natsBreakdown }: Props = $props();

	let rangeLabel = $derived(RANGE_LABELS[range as Range] ?? range);
	let windows = $derived([...new Set(rows.filter((r) => r.target !== null).map((r) => r.windowDays))]);
	let budgetHeader = $derived(windows.length === 1 ? `${windows[0]}-day budget left` : 'Budget left');
	const barWidth = (b: number | null) => (b === null ? 0 : Math.max(0, Math.min(1, b)) * 100);
	const hour = (iso: string | null) => (iso ? new Date(iso).toISOString().slice(0, 16).replace('T', ' ') + ' UTC' : 'no complete hour yet');
</script>

<div class="wrap">
	<table data-testid="slo-table">
		<caption class="visually-hidden">Service level objectives: good ratio, error budget and burn rate per objective</caption>
		<thead>
			<tr>
				<th scope="col">Service</th>
				<th scope="col">Objective</th>
				<th scope="col" class="num">Last hour</th>
				<th scope="col" class="num" data-testid="slo-range-header">{rangeLabel}<span class="hint"> · follows the range</span></th>
				<th scope="col" class="num">Target</th>
				<th scope="col" class="num" data-testid="slo-budget-header">{budgetHeader}</th>
				<th scope="col" class="num">Burn 1h · 6h · 24h</th>
				<th scope="col">Status</th>
			</tr>
		</thead>
		<tbody>
			{#each rows as r, i (r.key)}
				<tr data-testid="slo-row-{r.key}" class:group-start={i === 0 || rows[i - 1].sliId !== r.sliId}>
					<th scope="row" class="mono">{#if i === 0 || rows[i - 1].sliId !== r.sliId}{r.sliId}{:else}<span class="visually-hidden">{r.sliId}</span>{/if}</th>
					<td data-testid="slo-objective-{r.key}">
						{r.objective}
						{#if r.kind === 'latency'}
							<button type="button" class="info" aria-label={SSE_CAVEAT} title={SSE_CAVEAT} data-testid="slo-sse-{r.key}">ⓘ</button>
						{/if}
					</td>
					<td class="num" title={hour(r.lastHour.hourStart)}>{pct(r.lastHour.ratio)}</td>
					<td class="num" data-testid="slo-range-{r.key}">
						{pct(r.range.ratio)}
						<span class="sub">{count(r.range.failed)} failed ({r.failedLabel}) / {count(r.range.total)} total</span>
					</td>
					<td class="num">{r.target === null ? '—' : pct(r.target, 1)}</td>
					<td class="num" data-testid="slo-budget-{r.key}">
						{#if r.target === null}—{:else}
							{r.budgetLeft === null ? 'no data' : pct(r.budgetLeft, 0)}
							{#if windows.length > 1}<span class="sub" data-testid="slo-budget-window-{r.key}">{r.windowDays}-day window</span>{/if}
							<span class="bar" aria-hidden="true"><span class="fill {r.status.state}" style="width: {barWidth(r.budgetLeft)}%"></span></span>
						{/if}
					</td>
					<td class="num">
						{#each [r.burn.h1, r.burn.h6, r.burn.h24] as b, bi (bi)}
							{#if bi > 0}<span class="sep">&nbsp;·&nbsp;</span>{/if}<span title={b === null ? 'low traffic' : undefined}>{r.target === null ? '—' : burn(b)}</span>
						{/each}
					</td>
					<td><span class="pill {r.status.state}" title={r.status.reason} data-testid="slo-status-{r.key}">{r.status.label}</span></td>
				</tr>
				{#if natsBreakdown && r.sliKind === 'nats' && r.kind === 'availability'}
					{#await natsBreakdown}
						<tr class="breakdown" data-testid="nats-breakdown-loading"><td colspan="8"><span class="muted">Loading top consumers…</span></td></tr>
					{:then nb}
						{#if nb}
							<tr class="breakdown" data-testid="nats-breakdown">
								<td colspan="8">
									{#if nb.status !== 'ok'}
										<span class="muted" data-testid="nats-breakdown-unavailable">breakdown unavailable</span>
									{:else if nb.rows.length === 0}
										<span class="muted">No failed handler runs in {rangeLabel.toLowerCase()}.</span>
									{:else}
										<span class="b-head">Top consumers by failed runs · {rangeLabel.toLowerCase()}</span>
										<ol class="b-list">
											{#each nb.rows as c (c.stream + '/' + c.consumer)}
												<li data-testid="nats-contributor">
													<span class="mono">{c.consumer}</span><span class="muted"> ({c.stream || 'no stream'})</span>
													<span class="b-num">{count(c.errors)} failed / {count(c.runs)} runs · {c.rate === null ? 'no data' : pct(c.rate, 1)}</span>
												</li>
											{/each}
										</ol>
									{/if}
								</td>
							</tr>
						{/if}
					{/await}
				{/if}
			{/each}
		</tbody>
	</table>
</div>

<style>
	.wrap { overflow-x: auto; }
	table { width: 100%; border-collapse: collapse; font-size: 0.8125rem; font-variant-numeric: tabular-nums; }
	th, td { text-align: left; padding: 0.375rem 0.625rem; border-bottom: 1px solid var(--border-color-muted); vertical-align: top; }
	thead th { font-size: 0.6875rem; text-transform: uppercase; letter-spacing: 0.04em; color: var(--text-muted); font-weight: 500; white-space: nowrap; }
	.hint { text-transform: none; letter-spacing: 0; color: var(--text-subtle); }
	tr.group-start:not(:first-child) th, tr.group-start:not(:first-child) td { border-top: 1px solid var(--border-color); }
	.num { text-align: right; white-space: nowrap; }
	.mono { font-family: var(--font-mono); font-weight: 500; }
	.sub { display: block; font-size: 0.6875rem; color: var(--text-muted); }
	.breakdown td { padding-top: 0; font-size: 0.75rem; }
	.muted { color: var(--text-muted); }
	.b-head { color: var(--text-muted); text-transform: uppercase; letter-spacing: 0.04em; font-size: 0.6875rem; }
	.b-list { margin: 0.25rem 0 0; padding-left: 1.25rem; display: flex; flex-direction: column; gap: 0.125rem; }
	.b-num { margin-left: 0.5rem; font-variant-numeric: tabular-nums; }
	.sep { color: var(--text-subtle); }
	.info { color: var(--text-muted); cursor: help; margin-left: 0.25rem; background: none; border: 0; padding: 0; font: inherit; }
	.bar { display: inline-block; vertical-align: middle; width: 3.5rem; height: 0.4rem; margin-left: 0.375rem; background: var(--bg-surface-hover); border-radius: 2px; overflow: hidden; }
	.fill { display: block; height: 100%; background: var(--status-resolved-text); }
	.fill.burning { background: var(--status-open-text); }
	.fill.burning-fast, .fill.breached { background: var(--severity-critical-text); }
	.fill.no-data, .fill.untracked { background: var(--text-subtle); }
	.pill { display: inline-block; padding: 0.0625rem 0.5rem; border-radius: 999px; font-size: 0.75rem; font-weight: 600; border: 1px solid; white-space: nowrap; }
	.pill.ok { color: var(--status-resolved-text); background: var(--status-resolved-bg); border-color: var(--status-resolved-border); }
	.pill.burning { color: var(--status-open-text); background: var(--status-open-bg); border-color: var(--status-open-border); }
	.pill.burning-fast, .pill.breached { color: var(--severity-critical-text); background: var(--severity-critical-bg); border-color: var(--severity-critical-border); }
	.pill.no-data, .pill.untracked { color: var(--status-ignored-text); background: var(--status-ignored-bg); border-color: var(--status-ignored-border); }
	.visually-hidden { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap; }
</style>
