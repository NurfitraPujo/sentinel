<script lang="ts">
	// sentinel-dashboard-metrics-improvement D11: one error-budget burn-down per SLO row. x is the SLI's own budget window
	// ending now (independent of the range selector), y is budget left: 100 % at the top, the 0 % line emphasised and
	// negative values drawn below it (floor −100 %). The dashed ideal line runs from 100 % at the window start to 0 % at its
	// end: a line under it is spending faster than the budget allows. Hand-written SVG; the drawing is aria-hidden and a
	// visually hidden table carries a daily reading of the same numbers.
	import { pct } from '$lib/dashboard-format';
	import type { BudgetChart } from '$lib/server/analytics/slo';

	interface Props {
		charts: BudgetChart[];
	}
	let { charts }: Props = $props();

	const W = 240;
	const H = 120;
	const PAD_L = 30;
	const PAD_R = 6;
	const PAD_T = 8;
	const PAD_B = 18;
	const DAY = 86_400_000;
	const plotW = W - PAD_L - PAD_R;
	const plotH = H - PAD_T - PAD_B;
	// y: 100 % (1) at the top, −100 % (−1) at the bottom, so 0 % is the middle line
	const yFor = (v: number) => PAD_T + ((1 - v) / 2) * plotH;
	const xFor = (c: BudgetChart, t: number) => PAD_L + ((t - c.windowStart) / (c.windowEnd - c.windowStart)) * plotW;
	const dayLabel = (t: number) => new Date(t).toISOString().slice(5, 10);

	function geometry(c: BudgetChart) {
		const pts = (c.points ?? []).filter((p) => p.t >= c.windowStart && p.t <= c.windowEnd);
		const line = pts.map((p, i) => `${i === 0 ? 'M' : 'L'}${xFor(c, p.t).toFixed(1)},${yFor(p.v).toFixed(1)}`).join(' ');
		const last = pts.at(-1) ?? null;
		const ticks: number[] = [];
		for (let d = 0; d <= c.windowDays; d++) ticks.push(c.windowStart + d * DAY);
		const every = c.windowDays > 14 ? 7 : c.windowDays > 7 ? 2 : 1;
		// one reading per day: the last point at or before the day's end
		const daily: { day: number; v: number }[] = [];
		for (let d = 1; d <= c.windowDays; d++) {
			const end = c.windowStart + d * DAY;
			const upto = pts.filter((p) => p.t <= end);
			if (upto.length) daily.push({ day: end, v: upto[upto.length - 1].v });
		}
		return { line, last, ticks, every, daily };
	}
</script>

<div class="grid" data-testid="budget-grid">
	{#each charts as c (c.key)}
		{@const g = geometry(c)}
		<figure class="chart" data-testid="budget-{c.key}">
			<figcaption>
				<span class="title">{c.title}</span>
				<span class="now" data-testid="budget-{c.key}-now">{g.last ? pct(g.last.v, 0) : 'no data'} left</span>
			</figcaption>
			{#if !g.last}
				<p class="empty" data-testid="budget-{c.key}-empty">No data in this window.</p>
			{:else}
				<svg viewBox="0 0 {W} {H}" class="svg" aria-hidden="true" focusable="false">
					<line x1={PAD_L} x2={W - PAD_R} y1={yFor(1)} y2={yFor(1)} class="grid-line" />
					<line x1={PAD_L} x2={W - PAD_R} y1={yFor(-1)} y2={yFor(-1)} class="grid-line" />
					<line x1={PAD_L} x2={W - PAD_R} y1={yFor(0)} y2={yFor(0)} class="zero" data-testid="budget-{c.key}-zero"><title>0% budget left</title></line>
					<text x={PAD_L - 4} y={yFor(1) + 3} class="label" text-anchor="end">100%</text>
					<text x={PAD_L - 4} y={yFor(0) + 3} class="label" text-anchor="end">0%</text>
					<text x={PAD_L - 4} y={yFor(-1) + 3} class="label" text-anchor="end">−100%</text>
					{#each g.ticks as t, i (t)}
						<line x1={xFor(c, t)} x2={xFor(c, t)} y1={H - PAD_B} y2={H - PAD_B + 3} class="tick" data-testid="budget-{c.key}-tick" />
						{#if i % g.every === 0}
							<text data-testid="budget-{c.key}-tick-label" x={xFor(c, t)} y={H - 4} class="label" text-anchor="middle">{dayLabel(t)}</text>
						{/if}
					{/each}
					<line x1={xFor(c, c.windowStart)} y1={yFor(1)} x2={xFor(c, c.windowEnd)} y2={yFor(0)} class="ideal" data-testid="budget-{c.key}-ideal"><title>Ideal: budget spent evenly over the window</title></line>
					<path d={g.line} class="spent" class:over={g.last.v <= 0} data-testid="budget-{c.key}-line" />
					<circle cx={xFor(c, g.last.t)} cy={yFor(g.last.v)} r="2.5" class="end" class:over={g.last.v <= 0} data-testid="budget-{c.key}-end"><title>{pct(g.last.v, 0)} budget left</title></circle>
				</svg>
				<table class="visually-hidden" data-testid="budget-{c.key}-table">
					<caption>{c.title}</caption>
					<thead><tr><th scope="col">Day ending</th><th scope="col">Budget left</th></tr></thead>
					<tbody>
						{#each g.daily as d (d.day)}
							<tr><th scope="row">{dayLabel(d.day)}</th><td>{pct(d.v, 0)}</td></tr>
						{/each}
					</tbody>
				</table>
			{/if}
		</figure>
	{/each}
</div>
<p class="note" data-testid="budget-note">Each chart covers its own budget window ending now and does not follow the range selector. A line under the dashed ideal is spending faster than the budget allows.</p>

<style>
	.grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(18rem, 1fr)); gap: 0.75rem; }
	.chart { margin: 0; background: var(--bg-surface); border: 1px solid var(--border-color); border-radius: var(--radius-md); padding: 0.625rem 0.75rem; }
	figcaption { display: flex; justify-content: space-between; align-items: baseline; gap: 0.5rem; margin-bottom: 0.375rem; font-size: 0.8125rem; }
	.title { color: var(--text-secondary); }
	.now { font-variant-numeric: tabular-nums; font-weight: 600; white-space: nowrap; }
	.svg { width: 100%; height: auto; display: block; }
	.grid-line { stroke: var(--border-color-muted); stroke-width: 0.5; }
	.zero { stroke: var(--text-secondary); stroke-width: 1.25; }
	.tick { stroke: var(--border-color); }
	.ideal { stroke: var(--text-muted); stroke-width: 1; stroke-dasharray: 4 3; }
	.spent { fill: none; stroke: var(--status-resolved-text); stroke-width: 1.75; }
	.spent.over, .end.over { stroke: var(--severity-critical-text); }
	.end { fill: var(--status-resolved-text); stroke: none; }
	.end.over { fill: var(--severity-critical-text); }
	.label { fill: var(--text-subtle); font-size: 7px; font-family: var(--font-mono); }
	.empty { color: var(--text-muted); font-size: 0.8125rem; margin: 0.5rem 0; }
	.note { margin: 0.5rem 0 0; font-size: 0.75rem; color: var(--text-muted); }
	.visually-hidden { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap; }
</style>
