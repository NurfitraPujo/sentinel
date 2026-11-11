<script lang="ts">
	// sentinel-issue-trend §4.1, flows `L1` / `L10`, decisions 7, 10 and 13 — the per-card
	// sparkline. Hand-written inline SVG: 24 bars, no axis, no numbers on the shape.
	//
	// **Scaled per card.** The y-range is THIS series' own maximum — the library reference's rule,
	// `Sparkline.tsx: max = Math.max(...values, 0) || 1` — never the page's. A card shows shape (did
	// this start, spike or stop); magnitude across cards is what `sort=count` answers. An all-zero
	// series has `max = 1`, every bar is height 0, and the baseline `<line>` is what remains: a
	// quiet issue draws a flat baseline, and a card with NO drawing means the page is broken.
	//
	// The series arrives dense from the loader (§3): 24 numbers, oldest first, `0` where no bucket
	// row exists. Nothing here fills gaps — `values.length` IS the number of bars.
	//
	// The list carries no hidden table per card (20 × 24 cells is noise to a screen reader); the
	// summed `aria-label` is the accessible name, and the detail page has the table (§4.2).
	interface Props {
		/** `trend24h`: dense, oldest first. */
		values: number[];
	}

	let { values }: Props = $props();

	// viewBox units: one unit per bar on the x-axis, `HEIGHT` units tall. `preserveAspectRatio="none"`
	// stretches it to the card's width; the strokes below are `non-scaling` so that stretch does not
	// thicken them.
	const HEIGHT = 20;
	const BAR_WIDTH = 0.7;

	let sum = $derived(values.reduce((acc, n) => acc + n, 0));
	let max = $derived(Math.max(...values, 0) || 1);
	let bars = $derived(
		values.map((n, i) => {
			const h = (n / max) * HEIGHT;
			return { x: i + (1 - BAR_WIDTH) / 2, y: HEIGHT - h, height: h };
		})
	);
</script>

<svg
	class="sparkline"
	role="img"
	aria-label="{Number(sum).toLocaleString()} occurrences in the last 24 hours"
	viewBox="0 0 {values.length} {HEIGHT}"
	preserveAspectRatio="none"
	data-testid="issue-sparkline"
>
	{#each bars as bar, i (i)}
		<rect class="bar" x={bar.x} y={bar.y} width={BAR_WIDTH} height={bar.height} />
	{/each}
	<line class="baseline" x1="0" y1={HEIGHT} x2={values.length} y2={HEIGHT} vector-effect="non-scaling-stroke" />
</svg>

<style>
	.sparkline {
		display: block;
		width: 100%;
		height: 20px;
		margin-bottom: 0.625rem;
		overflow: visible;
	}

	.bar {
		fill: var(--color-primary);
		opacity: 0.85;
	}

	.baseline {
		stroke: var(--border-color);
		stroke-width: 1;
	}
</style>
