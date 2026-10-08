<script lang="ts">
	// sentinel-reporting-and-dashboard D13 section 3: one SLI's traffic as bars with its good ratio as a line over them.
	// Hand-written SVG, no chart library; the drawing is aria-hidden and a visually hidden table carries the numbers.
	interface Point {
		bucket: string;
		total: number;
		errors: number;
	}
	interface Props {
		id: string;
		points: Point[];
		/** Availability target as a fraction, drawn as a dashed line when present. */
		goal: number | null;
		formatBucket?: (s: string) => string;
		testId: string;
	}
	let { id, points, goal, formatBucket = (s) => s, testId }: Props = $props();

	const SLOT = 20;
	const HEIGHT = 110;
	const PAD_TOP = 8;
	const PAD_BOTTOM = 24;
	const plotH = HEIGHT - PAD_TOP - PAD_BOTTOM;
	let width = $derived(Math.max(1, points.length) * SLOT);
	let peak = $derived(Math.max(1, ...points.map((p) => p.total)));
	const ratio = (p: Point) => (p.total === 0 ? null : 1 - p.errors / p.total);
	// the line's y axis spans 90%..100% so a 99.5% target is legible; values below 90% clamp to the floor
	const FLOOR = 0.9;
	const yFor = (r: number) => PAD_TOP + plotH - ((Math.min(1, Math.max(FLOOR, r)) - FLOOR) / (1 - FLOOR)) * plotH;
	let line = $derived(
		points
			.map((p, i) => ({ r: ratio(p), x: i * SLOT + SLOT / 2 }))
			.filter((p): p is { r: number; x: number } => p.r !== null)
			.map((p, i) => `${i === 0 ? 'M' : 'L'}${p.x},${yFor(p.r)}`)
			.join(' ')
	);
</script>

<figure class="chart" data-testid={testId}>
	<figcaption class="chart-heading">{id}: requests, errors and good ratio</figcaption>
	{#if points.length === 0}
		<p class="chart-empty" data-testid="{testId}-empty">No data in this range.</p>
	{:else}
		<svg viewBox="0 0 {width} {HEIGHT}" class="svg" aria-hidden="true" focusable="false">
			<line x1="0" x2={width} y1={PAD_TOP + plotH} y2={PAD_TOP + plotH} class="axis" />
			{#each points as p, i (p.bucket)}
				<rect x={i * SLOT + 2} y={PAD_TOP + plotH - (p.total / peak) * plotH} width={SLOT - 4} height={(p.total / peak) * plotH} class="bar">
					<title>{formatBucket(p.bucket)} · {Number(p.total).toLocaleString('en-US')} total, {Number(p.errors).toLocaleString('en-US')} errors</title>
				</rect>
				{#if i === 0 || i === points.length - 1 || i % 6 === 0}
					<text x={i * SLOT + SLOT / 2} y={HEIGHT - 8} class="label" text-anchor="middle">{formatBucket(p.bucket)}</text>
				{/if}
			{/each}
			{#if goal !== null}
				<line x1="0" x2={width} y1={yFor(goal)} y2={yFor(goal)} class="target" data-testid="{testId}-target"><title>Target {(goal * 100).toFixed(2)}%</title></line>
			{/if}
			{#if line}<path d={line} class="ratio" data-testid="{testId}-line" />{/if}
		</svg>
		<table class="visually-hidden" data-testid="{testId}-table">
			<caption>{id}: requests, errors and good ratio</caption>
			<thead><tr><th scope="col">Bucket</th><th scope="col">Total</th><th scope="col">Errors</th><th scope="col">Good ratio</th></tr></thead>
			<tbody>
				{#each points as p (p.bucket)}
					<tr><th scope="row">{formatBucket(p.bucket)}</th><td>{p.total}</td><td>{p.errors}</td><td>{ratio(p) === null ? 'no data' : `${((ratio(p) as number) * 100).toFixed(2)}%`}</td></tr>
				{/each}
			</tbody>
		</table>
	{/if}
</figure>

<style>
	.chart { margin: 0; }
	.chart-heading { font-size: 0.8125rem; color: var(--text-secondary); margin-bottom: 0.375rem; }
	.chart-empty { color: var(--text-muted); font-size: 0.8125rem; margin: 0.5rem 0; }
	.svg { width: 100%; height: auto; max-height: 10rem; display: block; }
	.axis { stroke: var(--border-color); }
	.bar { fill: var(--color-primary-subtle); stroke: var(--color-primary); stroke-width: 0.5; }
	.ratio { fill: none; stroke: var(--text-primary); stroke-width: 1.5; }
	.target { stroke: var(--status-open-text); stroke-width: 1; stroke-dasharray: 3 3; }
	.label { fill: var(--text-subtle); font-size: 8px; font-family: var(--font-mono); }
	.visually-hidden { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap; }
</style>
