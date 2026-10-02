<!--
	sentinel-span-ingest D6: the span waterfall on the trace page. Spans (mobile transactions and core's pulled
	traces) on ONE time axis, one lane per service, nested by parent, a span whose parent is not stored drawn as a
	root, and each error occurrence pinned to the span carrying its `spanId` — errors with no matching span sit in
	an "errors" lane by time. Hand-written like TrendBarChart / IssueTimeline: no chart library.

	The layout is `$lib/spans/waterfall.ts`'s (pure, tested there); this file only draws it. Each span is a
	focusable button: hover or focus (and Enter/Space to pin) opens a detail card with name, op, duration,
	status and a few attributes. Span text is untrusted client data and is only ever rendered as text.
-->
<script lang="ts">
	import {
		SPAN_RENDER_CAP,
		buildWaterfall,
		formatDuration,
		type WaterfallError,
		type WaterfallRow,
		type WaterfallSpan,
	} from '$lib/spans/waterfall';

	let {
		spans,
		errors,
		truncated = false,
		retentionDays,
	}: {
		spans: WaterfallSpan[];
		errors: WaterfallError[];
		/** The loader matched more spans than it returned (it returns at most SPAN_RENDER_CAP). */
		truncated?: boolean;
		retentionDays: number;
	} = $props();

	let wf = $derived(buildWaterfall(spans, errors));
	let truncatedNotice = $derived(truncated || wf.truncated > 0);

	// One open detail at a time: hovered/focused, or pinned open with Enter/Space/click.
	let hovered = $state<string | null>(null);
	let pinned = $state<string | null>(null);
	let active = $derived(pinned ?? hovered);

	const rowKey = (laneKey: string, i: number) => `${laneKey}#${i}`;
	function toggle(key: string) {
		pinned = pinned === key ? null : key;
	}

	// Axis labels: 0 and the end, plus quarter marks.
	let ticks = $derived(
		[0, 25, 50, 75, 100].map((pct) => ({ pct, label: formatDuration(((wf.endUs - wf.startUs) * pct) / 100) })),
	);

	function describe(r: WaterfallRow): string {
		const parts = [r.span.name || r.span.op || r.span.spanId, formatDuration(r.span.durationUs)];
		if (r.failed) parts.push(`status ${r.span.status}`);
		if (r.errors.length > 0) parts.push(`${r.errors.length} ${r.errors.length === 1 ? 'error' : 'errors'}`);
		return parts.join(', ');
	}

	const attrEntries = (a: Record<string, unknown>) => Object.entries(a ?? {}).slice(0, 8);
	const shown = (v: unknown) => (typeof v === 'string' ? v : JSON.stringify(v));
</script>

<section class="waterfall" data-testid="waterfall" aria-label="Span waterfall">
	{#if wf.spanCount === 0}
		<p class="empty" data-testid="waterfall-empty">
			No spans for this trace. Spans are kept {retentionDays} days.
		</p>
	{:else}
		<div class="axis" aria-hidden="true">
			<div class="label-col"></div>
			<div class="track-col axis-track">
				{#each ticks as t (t.pct)}
					<span class="tick" style="left: {t.pct}%">{t.label}</span>
				{/each}
			</div>
		</div>

		{#each wf.lanes as lane (lane.key)}
			<div class="lane" data-testid="waterfall-lane" data-lane={lane.key}>
				<h3 class="lane-title">
					<span>{lane.label}</span>
					<span class="lane-source">{lane.source === 'mobile' ? 'mobile' : 'core'}</span>
				</h3>
				<ul class="rows">
					{#each lane.rows as r, i (rowKey(lane.key, i))}
						{@const key = rowKey(lane.key, i)}
						<li class="row" data-testid="waterfall-row" data-span-id={r.span.spanId} data-depth={r.depth}>
							<div class="label-col" style="padding-left: {r.depth * 12}px" title={r.span.name}>
								<span class="name">{r.span.name || r.span.op || r.span.spanId}</span>
								{#if r.span.op && r.span.op !== r.span.name}<span class="op">{r.span.op}</span>{/if}
							</div>
							<div class="track-col">
								<button
									type="button"
									class="bar"
									class:failed={r.failed}
									data-testid="waterfall-bar"
									style="left: {r.offsetPct}%; width: {r.widthPct}%"
									aria-label={describe(r)}
									aria-expanded={active === key}
									aria-controls="wf-detail-{key}"
									onmouseenter={() => (hovered = key)}
									onmouseleave={() => (hovered = null)}
									onfocus={() => (hovered = key)}
									onblur={() => (hovered = null)}
									onclick={() => toggle(key)}
								></button>
								{#each r.errors as p (p.error.id)}
									<a
										class="pin"
										data-testid="waterfall-error"
										style="left: {p.offsetPct}%"
										href="/issues/{p.error.issueId}?occurrence={p.error.id}"
										aria-label="Error: {p.error.errorClass ?? 'error'} {p.error.message ?? ''}"
										title="{p.error.errorClass ?? 'Error'}: {p.error.message ?? ''}">◆</a
									>
								{/each}
							</div>
							{#if active === key}
								<div class="detail" id="wf-detail-{key}" role="region" data-testid="waterfall-detail" aria-label="Span details">
									<dl>
										<dt>Name</dt>
										<dd>{r.span.name || '—'}</dd>
										{#if r.span.op}<dt>Op</dt><dd>{r.span.op}</dd>{/if}
										<dt>Duration</dt>
										<dd>{formatDuration(r.span.durationUs)}</dd>
										<dt>Status</dt>
										<dd>{r.span.status || 'unset'}</dd>
										{#if r.span.kind}<dt>Kind</dt><dd>{r.span.kind}</dd>{/if}
										{#if r.span.environment}<dt>Environment</dt><dd>{r.span.environment}</dd>{/if}
										{#each attrEntries(r.span.attributes) as [k, v] (k)}
											<dt>{k}</dt>
											<dd>{shown(v)}</dd>
										{/each}
									</dl>
								</div>
							{/if}
						</li>
					{/each}
				</ul>
			</div>
		{/each}
	{/if}

	{#if wf.unmatchedErrors.length > 0}
		<div class="lane errors-lane" data-testid="waterfall-errors-lane">
			<h3 class="lane-title"><span>errors</span><span class="lane-source">by time</span></h3>
			<div class="row">
				<div class="label-col">
					<span class="name">{wf.unmatchedErrors.length} {wf.unmatchedErrors.length === 1 ? 'error' : 'errors'} not on a span</span>
				</div>
				<div class="track-col">
					{#each wf.unmatchedErrors as p (p.error.id)}
						<a
							class="pin"
							data-testid="waterfall-unmatched-error"
							style="left: {p.offsetPct}%"
							href="/issues/{p.error.issueId}?occurrence={p.error.id}"
							aria-label="Error: {p.error.errorClass ?? 'error'} {p.error.message ?? ''}"
							title="{p.error.errorClass ?? 'Error'}: {p.error.message ?? ''}">◆</a
						>
					{/each}
				</div>
			</div>
		</div>
	{/if}

	{#if truncatedNotice}
		<p class="notice" data-testid="waterfall-truncated">
			This trace has more than {SPAN_RENDER_CAP.toLocaleString()} spans; only the first {SPAN_RENDER_CAP.toLocaleString()} are drawn.
		</p>
	{/if}
	<p class="notice" data-testid="waterfall-retention">Spans older than {retentionDays} days have been removed and are not shown.</p>
</section>

<style>
	.waterfall { margin: 1rem 0 1.5rem; background: var(--bg-surface); border: 1px solid var(--border-color); border-radius: var(--radius-md); padding: 0.75rem; }
	.empty, .notice { color: var(--text-muted); font-size: 0.8rem; margin: 0.4rem 0 0; }
	.axis, .row { display: grid; grid-template-columns: minmax(8rem, 14rem) 1fr; gap: 0.75rem; align-items: center; position: relative; }
	.axis-track { position: relative; height: 1.1rem; }
	.tick { position: absolute; transform: translateX(-50%); font-family: var(--font-mono); font-size: 0.65rem; color: var(--text-muted); }
	.tick:first-child { transform: none; }
	.tick:last-child { transform: translateX(-100%); }
	.lane { margin-top: 0.75rem; }
	.lane-title { display: flex; gap: 0.5rem; align-items: baseline; font-size: 0.8rem; margin: 0 0 0.25rem; color: var(--text-secondary); }
	.lane-source { font-family: var(--font-mono); font-size: 0.65rem; color: var(--text-subtle); border: 1px solid var(--border-color); border-radius: var(--radius-sm); padding: 0 0.3rem; }
	.rows { list-style: none; margin: 0; padding: 0; }
	.row { min-height: 1.5rem; border-top: 1px solid var(--border-color-muted); }
	.label-col { overflow: hidden; white-space: nowrap; text-overflow: ellipsis; font-size: 0.75rem; color: var(--text-secondary); }
	.label-col .op { margin-left: 0.4rem; font-family: var(--font-mono); font-size: 0.65rem; color: var(--text-subtle); }
	.track-col { position: relative; height: 1.5rem; }
	.bar { position: absolute; top: 0.3rem; height: 0.9rem; padding: 0; border: 1px solid var(--color-primary); border-radius: var(--radius-sm); background: var(--color-primary-subtle); cursor: pointer; }
	.bar:hover, .bar:focus-visible, .bar[aria-expanded='true'] { background: var(--color-primary); outline: none; border-color: var(--border-color-focus); }
	.bar.failed { border-color: var(--severity-critical-border); background: var(--severity-critical-bg); }
	.bar.failed:hover, .bar.failed:focus-visible, .bar.failed[aria-expanded='true'] { background: var(--severity-critical-text); }
	.pin { position: absolute; top: 0; transform: translateX(-50%); line-height: 1.5rem; color: var(--severity-critical-text); text-decoration: none; font-size: 0.8rem; }
	.pin:hover, .pin:focus-visible { color: var(--text-primary); }
	.detail { grid-column: 1 / -1; background: var(--bg-surface-elevated); border: 1px solid var(--border-color); border-radius: var(--radius-sm); padding: 0.5rem 0.75rem; margin: 0.2rem 0 0.4rem; font-size: 0.75rem; }
	.detail dl { display: grid; grid-template-columns: max-content 1fr; gap: 0.15rem 0.75rem; margin: 0; }
	.detail dt { color: var(--text-muted); font-family: var(--font-mono); }
	.detail dd { margin: 0; color: var(--text-primary); word-break: break-word; }
	.errors-lane .name { color: var(--severity-critical-text); }
	@media (max-width: 640px) {
		.axis, .row { grid-template-columns: minmax(5rem, 8rem) 1fr; gap: 0.4rem; }
	}
</style>
