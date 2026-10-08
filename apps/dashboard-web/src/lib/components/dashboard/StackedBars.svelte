<script lang="ts">
	// sentinel-reporting-and-dashboard D13 section 2 / section 5: a labelled bar chart, hand-written inline SVG (no chart
	// library), in the style of TrendBarChart. `mode='stacked'` stacks the series in each bucket (error events by D19
	// group or by service); `mode='grouped'` puts them side by side (opened vs resolved per week).
	//
	// Colour is state-only (PRODUCT.md): series are told apart by a luminance ramp of the one primary hue plus a pattern
	// of position in the legend, never by a rainbow. The drawing is aria-hidden; the visually hidden <table> carries every
	// number, one row per bucket, so a screen reader and the keyboard get the same data (parity with TrendBarChart).
	// Every bar's tooltip is a <title> child of its <rect>, never a title attribute.
	interface Series {
		key: string;
		label: string;
	}
	interface Bucket {
		/** The bucket's start, in whatever form the caller writes it (`formatBucket` is applied for display). */
		start: string;
		values: Record<string, number>;
	}
	interface Marker {
		/** Bucket start the marker sits on. */
		start: string;
		label: string;
	}
	interface Props {
		heading: string;
		series: Series[];
		buckets: Bucket[];
		mode?: 'stacked' | 'grouped';
		formatBucket?: (start: string) => string;
		/** Release-start markers (D13): drawn as a tick on the bucket and listed in the table's caption. */
		markers?: Marker[];
		labelEvery?: number;
		testId: string;
	}
	let { heading, series, buckets, mode = 'stacked', formatBucket = (s) => s, markers = [], labelEvery = 6, testId }: Props = $props();

	const SLOT = 20;
	const HEIGHT = 120;
	const PAD_TOP = 8;
	const PAD_BOTTOM = 26;
	let width = $derived(Math.max(1, buckets.length) * SLOT);
	let totals = $derived(buckets.map((b) => series.reduce((a, s) => a + (b.values[s.key] ?? 0), 0)));
	let peak = $derived(
		mode === 'stacked'
			? Math.max(1, ...totals)
			: Math.max(1, ...buckets.flatMap((b) => series.map((s) => b.values[s.key] ?? 0)))
	);
	const plotH = HEIGHT - PAD_TOP - PAD_BOTTOM;
	// luminance ramp of the primary hue; the nth series is the nth step
	const shade = (i: number, n: number) => `oklch(${0.45 + (0.4 * i) / Math.max(1, n - 1)} 0.14 255)`;
	let sum = $derived(totals.reduce((a, b) => a + b, 0));

	function bars(b: Bucket, bi: number) {
		const x0 = bi * SLOT;
		if (mode === 'stacked') {
			let y = PAD_TOP + plotH;
			return series.map((s, si) => {
				const v = b.values[s.key] ?? 0;
				const h = (v / peak) * plotH;
				y -= h;
				return { s, si, v, x: x0 + 2, y, w: SLOT - 4, h };
			});
		}
		const w = (SLOT - 4) / series.length;
		return series.map((s, si) => {
			const v = b.values[s.key] ?? 0;
			const h = (v / peak) * plotH;
			return { s, si, v, x: x0 + 2 + si * w, y: PAD_TOP + plotH - h, w, h };
		});
	}
	const markerIndex = (start: string) => buckets.findIndex((b) => b.start === start);
</script>

<figure class="chart" data-testid={testId}>
	<figcaption class="chart-heading">{heading}: <strong>{Number(sum).toLocaleString('en-US')}</strong></figcaption>
	{#if buckets.length === 0}
		<p class="chart-empty" data-testid="{testId}-empty">No data in this range.</p>
	{:else}
		<ul class="legend" data-testid="{testId}-legend">
			{#each series as s, si (s.key)}
				<li><span class="swatch" style="background: {shade(si, series.length)}"></span>{s.label}</li>
			{/each}
		</ul>
		<svg viewBox="0 0 {width} {HEIGHT}" class="svg" aria-hidden="true" focusable="false">
			<line x1="0" x2={width} y1={PAD_TOP + plotH} y2={PAD_TOP + plotH} class="axis" />
			{#each buckets as b, bi (b.start)}
				{#each bars(b, bi) as bar (bar.s.key)}
					{#if bar.h > 0}
						<rect x={bar.x} y={bar.y} width={bar.w} height={bar.h} fill={shade(bar.si, series.length)} data-testid="{testId}-bar">
							<title>{formatBucket(b.start)} · {bar.s.label}: {Number(bar.v).toLocaleString('en-US')}</title>
						</rect>
					{/if}
				{/each}
				{#if bi === 0 || bi === buckets.length - 1 || bi % labelEvery === 0}
					<text x={bi * SLOT + SLOT / 2} y={HEIGHT - 8} class="label" text-anchor="middle">{formatBucket(b.start)}</text>
				{/if}
			{/each}
			{#each markers as m (m.start + m.label)}
				{#if markerIndex(m.start) >= 0}
					<line x1={markerIndex(m.start) * SLOT + SLOT / 2} x2={markerIndex(m.start) * SLOT + SLOT / 2} y1={PAD_TOP} y2={PAD_TOP + plotH} class="marker" data-testid="{testId}-marker">
						<title>{m.label}</title>
					</line>
				{/if}
			{/each}
		</svg>
		<table class="visually-hidden" data-testid="{testId}-table">
			<caption>
				{heading}
				{#if markers.length}· first seen: {markers.map((m) => m.label).join(', ')}{/if}
			</caption>
			<thead>
				<tr><th scope="col">Bucket</th>{#each series as s (s.key)}<th scope="col">{s.label}</th>{/each}</tr>
			</thead>
			<tbody>
				{#each buckets as b (b.start)}
					<tr><th scope="row">{formatBucket(b.start)}</th>{#each series as s (s.key)}<td>{b.values[s.key] ?? 0}</td>{/each}</tr>
				{/each}
			</tbody>
		</table>
	{/if}
</figure>

<style>
	.chart { margin: 0; }
	.chart-heading { font-size: 0.8125rem; color: var(--text-secondary); margin-bottom: 0.375rem; }
	.chart-empty { color: var(--text-muted); font-size: 0.8125rem; margin: 0.5rem 0; }
	.legend { display: flex; flex-wrap: wrap; gap: 0.25rem 0.875rem; list-style: none; padding: 0; margin: 0 0 0.375rem; font-size: 0.75rem; color: var(--text-muted); }
	.swatch { display: inline-block; width: 0.625rem; height: 0.625rem; border-radius: 2px; margin-right: 0.3125rem; vertical-align: -1px; }
	.svg { width: 100%; height: auto; max-height: 11rem; display: block; }
	.axis { stroke: var(--border-color); stroke-width: 1; }
	.label { fill: var(--text-subtle); font-size: 8px; font-family: var(--font-mono); }
	.marker { stroke: var(--status-open-text); stroke-width: 1; stroke-dasharray: 2 2; }
	.visually-hidden { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap; }
</style>
