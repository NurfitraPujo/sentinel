<script lang="ts">
	// D13 section 2: error events over time, stacked by D19 group, or by service when one project is selected, with
	// release-start markers for one project. Hourly buckets for 24h and 7d, daily for 28d and 90d.
	import StackedBars from './StackedBars.svelte';
	import { GROUP_KEYS, GROUP_LABELS } from '$lib/dashboard-format';
	import type { TimelineData } from '$lib/server/analytics/service';

	interface Props {
		data: TimelineData;
	}
	let { data }: Props = $props();

	// at most this many services get a series; the rest are summed into "Other services" so the stack stays legible
	const MAX_SERVICES = 6;
	let stacking = $derived.by(() => {
		if (!data.byService) return { series: GROUP_KEYS.map((g) => ({ key: g as string, label: GROUP_LABELS[g] })), keyOf: (r: { group: string; service: string | null }) => r.group };
		const totals = new Map<string, number>();
		for (const r of data.byGroup) {
			const k = r.service ?? 'unattributed';
			totals.set(k, (totals.get(k) ?? 0) + r.n);
		}
		const top = [...totals.entries()].sort((a, b) => b[1] - a[1]).slice(0, MAX_SERVICES).map(([k]) => k);
		const series = top.map((k) => ({ key: k, label: k === 'unattributed' ? 'Unattributed' : k }));
		if (totals.size > top.length) series.push({ key: '__other', label: 'Other services' });
		return { series, keyOf: (r: { group: string; service: string | null }) => { const k = r.service ?? 'unattributed'; return top.includes(k) ? k : '__other'; } };
	});
	let buckets = $derived.by(() => {
		const by = new Map<string, Record<string, number>>();
		for (const r of data.byGroup) {
			const v = by.get(r.bucket) ?? {};
			const k = stacking.keyOf(r);
			v[k] = (v[k] ?? 0) + r.n;
			by.set(r.bucket, v);
		}
		return [...by.entries()].sort((a, b) => a[0].localeCompare(b[0])).map(([start, values]) => ({ start, values }));
	});
	const fmt = (s: string) => (data.bucket === 'hour' ? s.slice(5, 16).replace(' ', ' ') : s.slice(0, 10));
	// a marker sits on the bucket that contains the release's first-seen instant
	let markers = $derived(
		data.releaseMarkers
			.map((m) => {
				const iso = m.at.slice(0, data.bucket === 'hour' ? 13 : 10);
				const start = buckets.find((b) => b.start.replace(' ', 'T').startsWith(iso))?.start;
				return start ? { start, label: m.version } : null;
			})
			.filter((m): m is { start: string; label: string } => m !== null)
	);
</script>

<StackedBars
	heading={data.byService ? 'Error events by service' : 'Error events by group'}
	series={stacking.series}
	{buckets}
	formatBucket={fmt}
	{markers}
	labelEvery={data.bucket === 'hour' ? 12 : 4}
	testId="timeline"
/>
