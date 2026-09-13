<script lang="ts">
	// sentinel-issue-trend §4.2, flows `D1`–`D5`, decisions 7, 11, 13 and 16 — one labelled bar
	// chart of a dense series, hand-written inline SVG. The detail renders it twice: the hourly
	// (local clock, no "UTC" word) and the daily (UTC calendar days, and the heading says so). The
	// two differ only in what they are handed — the heading, how a slot's start is written, the
	// label cadence and the suffix on the last bar — so the clock rules live in the caller.
	//
	// **The series is rendered as given.** It arrives dense from the loader (§3) — one entry per
	// slot, `0` where nothing happened, oldest first, the last entry the current partial slot — and
	// the daily one is already summed per UTC day (decision 11). Nothing here fills a gap or sums.
	//
	// **Every bar's tooltip is a `<title>` CHILD of its `<rect>`** — never a `title` attribute,
	// which shows no native tooltip on an SVG element in Chromium or Firefox (flow `D4`).
	//
	// **The drawing is `aria-hidden`; the visually-hidden `<table>` carries the numbers** for a
	// screen reader, one row per point (flow `D5`; the library's `Sparkline` idiom). The heading is
	// visible and carries the window word and the sum — never a bare `Occurrences`, which is the
	// lifetime `issues.count` above (§3, decision 7).
	//
	// Every count goes through `Number(n).toLocaleString()` and the noun is always the plural
	// `occurrences` (§3) — one format, so a component test asserts one literal.
	interface Point {
		start: string;
		count: number;
	}

	interface Props {
		/** The window word: `Last 24 hours` or `Last 30 days (UTC)`. The heading is `{heading}: {sum}{headingSuffix}`. */
		heading: string;
		/** Decision 16: ` (data kept for {retentionDays} days)`, after the sum. Empty otherwise. */
		headingSuffix?: string;
		/** Dense, oldest first. */
		points: Point[];
		/** How a slot's start is written in titles, labels and the table — the caller's clock. */
		formatStart: (start: string) => string;
		/** Appended to the LAST bar's title only — ` (current hour)` on the hourly chart. */
		lastSuffix?: string;
		/** Guidance, not acceptance: a label under the first, the last and every n-th slot. */
		labelEvery?: number;
		/** The short form for those axis labels (`14:00`, `Sep 13`); defaults to `formatStart`. */
		formatLabel?: (start: string) => string;
		testId: string;
	}

	let {
		heading,
		headingSuffix = '',
		points,
		formatStart,
		lastSuffix = '',
		labelEvery = 6,
		formatLabel,
		testId,
	}: Props = $props();

	let axisLabel = $derived(formatLabel ?? formatStart);

	// viewBox geometry. The SVG keeps its aspect ratio (no `preserveAspectRatio="none"`) because
	// it carries text; it scales to the figure's width as a whole.
	const SLOT = 20;
	const GAP = 4;
	const PLOT_HEIGHT = 80;
	const TOP_PAD = 14; // room for the extreme's inline label
	const LABEL_ROW = 16;
	const BASELINE_Y = TOP_PAD + PLOT_HEIGHT;
	const HEIGHT = BASELINE_Y + LABEL_ROW;

	let width = $derived(points.length * SLOT);
	let sum = $derived(points.reduce((acc, p) => acc + p.count, 0));
	let max = $derived(Math.max(...points.map((p) => p.count), 0) || 1);
	let maxIndex = $derived(points.findIndex((p) => p.count === max));

	let bars = $derived(
		points.map((p, i) => {
			const h = (p.count / max) * PLOT_HEIGHT;
			const isLast = i === points.length - 1;
			return {
				x: i * SLOT + GAP / 2,
				y: BASELINE_Y - h,
				height: h,
				width: SLOT - GAP,
				centre: i * SLOT + SLOT / 2,
				title: `${Number(p.count).toLocaleString()} occurrences · ${formatStart(p.start)}${isLast ? lastSuffix : ''}`,
				labelled: i === 0 || isLast || i % labelEvery === 0,
			};
		})
	);
</script>

<figure class="trend-chart" data-testid={testId}>
	<figcaption>
		<h3 class="trend-heading" data-testid="{testId}-heading">{heading}: {Number(sum).toLocaleString()}{headingSuffix}</h3>
	</figcaption>

	<svg class="chart" viewBox="0 0 {width} {HEIGHT}" aria-hidden="true" focusable="false">
		{#each bars as bar, i (i)}
			<rect class="bar" x={bar.x} y={bar.y} width={bar.width} height={bar.height}>
				<title>{bar.title}</title>
			</rect>
		{/each}
		<line class="baseline" x1="0" y1={BASELINE_Y} x2={width} y2={BASELINE_Y} />
		{#each bars as bar, i (i)}
			{#if bar.labelled}
				<text class="axis-label" x={bar.centre} y={HEIGHT - 4} text-anchor="middle">{axisLabel(points[i].start)}</text>
			{/if}
		{/each}
		<!-- Guidance (§4.2): the extreme labelled inline, as the library's BarChart does. -->
		{#if sum > 0 && maxIndex >= 0}
			<text class="extreme-label" x={bars[maxIndex].centre} y={bars[maxIndex].y - 3} text-anchor="middle">{Number(max).toLocaleString()}</text>
		{/if}
	</svg>

	<table class="visually-hidden" data-testid="{testId}-table">
		<caption>{heading}: {Number(sum).toLocaleString()}{headingSuffix}</caption>
		<thead>
			<tr>
				<th scope="col">Start</th>
				<th scope="col">Occurrences</th>
			</tr>
		</thead>
		<tbody>
			{#each points as point, i (i)}
				<tr>
					<td>{formatStart(point.start)}{i === points.length - 1 ? lastSuffix : ''}</td>
					<td>{Number(point.count).toLocaleString()}</td>
				</tr>
			{/each}
		</tbody>
	</table>
</figure>

<style>
	.trend-chart {
		margin: 0;
		min-width: 0;
	}

	.trend-heading {
		font-size: 0.8125rem;
		font-family: var(--font-mono);
		font-weight: 500;
		color: var(--text-muted);
		margin-bottom: 0.5rem;
	}

	.chart {
		display: block;
		width: 100%;
		height: auto;
		overflow: visible;
	}

	.bar {
		fill: var(--color-primary);
		opacity: 0.85;
	}

	.bar:hover {
		opacity: 1;
	}

	.baseline {
		stroke: var(--border-color);
		stroke-width: 1;
	}

	.axis-label,
	.extreme-label {
		font-family: var(--font-mono);
		font-size: 9px;
		fill: var(--text-subtle);
	}

	.extreme-label {
		fill: var(--text-muted);
	}

	/* The screen-reader table: in the accessibility tree, out of the layout. */
	.visually-hidden {
		position: absolute;
		width: 1px;
		height: 1px;
		margin: -1px;
		padding: 0;
		overflow: hidden;
		clip: rect(0, 0, 0, 0);
		white-space: nowrap;
		border: 0;
	}
</style>
