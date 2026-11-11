<!--
	sentinel-occurrence-spans: the spans badge of one trace (spans / spans pending / no spans) and, when the trace has no
	spans and the organization is allow-listed for span pull, the "Pull spans" button. Used by the issue page (one per
	occurrence) and by the trace page's empty waterfall (`variant="panel"`, which also says WHY there are no spans).

	Clicking POSTs /api/spans/pull and then polls GET /api/spans/pull every SPAN_POLL_MS for at most SPAN_POLL_MAX_MS: `available`
	refreshes the page's data (the waterfall renders, the badge flips), a terminal `none` says why ("not found in the
	trace store"), and running out the clock says the pull is still pending. Everything the server says is rendered as text.
-->
<script lang="ts">
	import { onDestroy, untrack } from 'svelte';
	import { SPAN_POLL_MAX_MS, SPAN_POLL_MS, spansReasonText, type SpansReason, type SpansStatus } from '$lib/spans/status';

	let {
		organizationId,
		traceId,
		status: initialStatus,
		reason: initialReason = null,
		enabled,
		variant = 'badge',
	}: {
		organizationId: string;
		traceId: string;
		status: SpansStatus;
		reason?: SpansReason | null;
		/** Whether on-demand pull is offered for this organization (allow-listed). */
		enabled: boolean;
		variant?: 'badge' | 'panel';
	} = $props();

	// svelte-ignore state_referenced_locally -- seeded from the loader, re-seeded by the effect below when it reloads.
	let status = $state<SpansStatus>(initialStatus);
	// svelte-ignore state_referenced_locally
	let reason = $state<SpansReason | null>(initialReason);
	let busy = $state(false);
	let notice = $state('');
	let timer: ReturnType<typeof setTimeout> | null = null;

	// A reload (invalidateAll, or navigating to another occurrence) hands in new values; take them unless a pull is running.
	$effect(() => {
		const s = initialStatus;
		const r = initialReason;
		if (!untrack(() => busy)) {
			status = s;
			reason = r;
		}
	});
	onDestroy(stop);

	function stop() {
		if (timer) clearTimeout(timer);
		timer = null;
	}

	const label = $derived(status === 'available' ? 'spans' : status === 'pending' ? 'spans pending' : 'no spans');

	async function call(method: 'GET' | 'POST'): Promise<{ ok: boolean; code: number; body: any; retryAfter: string | null }> {
		const init: RequestInit =
			method === 'POST'
				? { method, headers: { 'content-type': 'application/json' }, body: JSON.stringify({ organizationId, traceId }) }
				: { method };
		const url = method === 'GET' ? `/api/spans/pull?${new URLSearchParams({ organizationId, traceId })}` : '/api/spans/pull';
		const res = await fetch(url, init);
		let body: any = null;
		try {
			body = await res.json();
		} catch {
			/* a non-JSON error body */
		}
		return { ok: res.ok, code: res.status, body, retryAfter: res.headers.get('retry-after') };
	}

	function apply(body: { status: SpansStatus; reason: SpansReason | null }) {
		status = body.status;
		reason = body.reason ?? null;
	}

	async function pull() {
		busy = true;
		notice = '';
		try {
			const r = await call('POST');
			if (!r.ok) {
				notice =
					r.code === 429
						? (r.body?.message ?? 'Too many on-demand pulls for this organization this hour.')
						: r.code === 403
							? 'Span pull is not enabled for this organization.'
							: r.code === 404
								? 'This trace is not available to you.'
								: 'The pull could not be queued. Try again.';
				busy = false;
				return;
			}
			apply(r.body);
			if (status === 'available') return finish();
			if (status === 'pending') return poll(Date.now());
			busy = false;
		} catch {
			notice = 'The pull could not be queued. Try again.';
			busy = false;
		}
	}

	function poll(startedAt: number) {
		stop();
		timer = setTimeout(async () => {
			try {
				const r = await call('GET');
				if (r.ok) apply(r.body);
			} catch {
				/* keep polling until the clock runs out */
			}
			if (status === 'available') return finish();
			if (status === 'none') {
				// The worker finished and nothing came back: `reason` (not_found / too_large / failed) says why.
				busy = false;
				return;
			}
			if (Date.now() - startedAt >= SPAN_POLL_MAX_MS) {
				notice = 'Still pending: the trace has not been fetched yet. Check back in a few minutes.';
				busy = false;
				return;
			}
			poll(startedAt);
		}, SPAN_POLL_MS);
	}

	async function finish() {
		stop();
		busy = false;
		// The waterfall, the badges and the issue-level link are all loader data.
		// Imported here, not at the top: kit's client runtime touches `window` on import, which a server render of this
		// component (and the node-environment SSR tests) must not do.
		const { invalidateAll } = await import('$app/navigation');
		await invalidateAll();
	}
</script>

<span class="spans-control" class:panel={variant === 'panel'} data-testid="spans-control" data-status={status}>
	{#if variant === 'panel'}
		<span class="panel-text" data-testid="spans-empty-reason">
			{#if status === 'pending'}
				The spans of this trace are being pulled from the trace store. This can take a couple of minutes.
			{:else}
				{spansReasonText(reason)}
			{/if}
		</span>
	{:else}
		<span class="spans-badge {status}" data-testid="spans-badge">{label}</span>
	{/if}
	{#if status === 'none' && enabled}
		<button type="button" class="pull-spans" data-testid="pull-spans" disabled={busy} onclick={pull}>
			{busy ? 'Pulling…' : 'Pull spans'}
		</button>
	{:else if busy && status === 'pending'}
		<span class="pulling" data-testid="spans-polling">Pulling…</span>
	{/if}
	{#if notice}<span class="notice" role="status" data-testid="spans-notice">{notice}</span>{/if}
</span>

<style>
	.spans-control { display: inline-flex; flex-wrap: wrap; align-items: center; gap: 0.4rem; }
	.spans-control.panel { display: flex; margin: 0.75rem 0; padding: 0.75rem; border: 1px dashed var(--border-color); border-radius: var(--radius-md); color: var(--text-secondary); }
	.spans-badge { padding: 0.125rem 0.375rem; border-radius: var(--radius-sm); font-family: var(--font-mono); font-size: 0.6875rem; border: 1px solid var(--border-color); background: var(--bg-root); color: var(--text-muted); }
	.spans-badge.available { color: var(--color-primary); background: var(--color-primary-subtle); }
	.spans-badge.pending { color: var(--status-open-text); background: var(--status-open-bg); border-color: var(--status-open-border); }
	.pull-spans { font-size: 0.75rem; padding: 0.125rem 0.5rem; cursor: pointer; }
	.pulling, .notice { font-size: 0.75rem; color: var(--text-muted); }
</style>
