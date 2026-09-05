<script lang="ts">
	// sentinel-user-report-issue §4.4 / §4.2, fe.md rows S8–S12 — the escalation strip.
	//
	// `escalation === null` and an escalation object mean different things (§4.2), so the null case
	// renders NOTHING here: the report was never escalated, the claim box above already says
	// whether anyone holds it, and a strip saying "not escalated" would read as a state the
	// feature entered rather than one it never did.
	//
	// **The Acknowledge control is the assignee's alone, and the gate is in this component** — not
	// only in the route's `403`. It is absent from the DOM for anyone else, not disabled and not
	// hidden by CSS, because a control you can see and cannot use is worse than no control (S10).
	//
	// **No second poller.** fe.md is explicit: the page already polls at ~10 s and the deadline is
	// rendered from the value we already have — an absolute time plus a countdown computed client
	// side. The interval below only re-renders the countdown; it fetches nothing.
	import { onMount } from 'svelte';
	import { ESCALATION_MAX_HOPS, type Escalation } from '$lib/types/report-detail';

	interface Props {
		issueId: string;
		escalation: Escalation | null;
		currentUserId: string;
		/** `issues.waiting_on`, for the trap below. */
		waitingOn?: string | null;
		/** Whether the reporter is external, for the same trap. */
		reporterIsExternal?: boolean;
		maxHops?: number;
	}

	let {
		issueId,
		escalation,
		currentUserId,
		waitingOn = null,
		reporterIsExternal = false,
		maxHops = ESCALATION_MAX_HOPS,
	}: Props = $props();

	let now = $state(Date.now());
	let acknowledging = $state(false);
	let ackError = $state<string | null>(null);
	/** Set locally on a successful ack so the control and countdown go without a reload. */
	let acknowledgedLocally = $state(false);

	onMount(() => {
		const timer = setInterval(() => (now = Date.now()), 1000);
		return () => clearInterval(timer);
	});

	let acknowledgedAt = $derived(
		acknowledgedLocally ? new Date().toISOString() : (escalation?.acknowledgedAt ?? null)
	);

	let isAssignee = $derived(escalation !== null && escalation.assigneeId === currentUserId);

	// The trap fe.md task 4 says to SURFACE and not to fix: `waiting_on = 'reporter'` on a report
	// whose reporter is external can never clear on its own, because there is no reply channel
	// (§1, OQ-6). Changing when `waiting_on` clears is a different slug.
	let strandedOnReporter = $derived(reporterIsExternal && waitingOn === 'reporter');

	function formatAbsolute(value: string): string {
		const date = new Date(value);
		return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
	}

	function countdown(deadline: string, from: number): string {
		const target = new Date(deadline).getTime();
		if (Number.isNaN(target)) return '';
		const remaining = target - from;
		const overdue = remaining < 0;
		const seconds = Math.floor(Math.abs(remaining) / 1000);
		const minutes = Math.floor(seconds / 60);
		const hours = Math.floor(minutes / 60);
		const body =
			hours > 0
				? `${hours}h ${minutes % 60}m`
				: minutes > 0
					? `${minutes}m ${seconds % 60}s`
					: `${seconds}s`;
		return overdue ? `${body} overdue` : `${body} left`;
	}

	async function acknowledge() {
		ackError = null;
		acknowledging = true;
		try {
			const res = await fetch(`/api/issues/${issueId}/escalation/ack`, { method: 'POST' });
			if (!res.ok) {
				const body = await res.json().catch(() => ({}));
				throw new Error(body.message || `Failed to acknowledge (${res.status})`);
			}
			acknowledgedLocally = true;
		} catch (err) {
			ackError = err instanceof Error ? err.message : 'Failed to acknowledge';
		} finally {
			acknowledging = false;
		}
	}
</script>

{#if escalation !== null}
	<section
		class="escalation-strip"
		class:escalation-exhausted={escalation.exhausted}
		class:escalation-acknowledged={acknowledgedAt !== null}
		data-testid="escalation-strip"
	>
		{#if escalation.exhausted}
			<!-- S12: its own visible state, and it must not read as an ordinary assignment. -->
			<p class="escalation-headline" data-testid="escalation-exhausted">
				Nobody acknowledged
			</p>
			<p class="escalation-body">
				The rotation was tried {maxHops} times and stopped. It stays with
				<strong>{escalation.assigneeId}</strong> and will not move again on its own.
			</p>
		{:else if acknowledgedAt !== null}
			<p class="escalation-headline" data-testid="escalation-acknowledged">Acknowledged</p>
			<p class="escalation-body">
				<strong>{escalation.assigneeId}</strong> acknowledged at {formatAbsolute(acknowledgedAt)}.
			</p>
		{:else}
			<p class="escalation-headline" data-testid="escalation-live">On call</p>
			<p class="escalation-body">
				Assigned to <strong>{escalation.assigneeId}</strong> by the on-call rotation — nobody
				clicked Claim.
			</p>
		{/if}

		<p class="escalation-meta">
			<span data-testid="escalation-hop">escalated {escalation.hop} of {maxHops}</span>
			{#if !escalation.exhausted && acknowledgedAt === null}
				<span data-testid="escalation-deadline">
					acknowledge by {formatAbsolute(escalation.ackDeadlineAt)}
				</span>
				<span class="escalation-countdown" data-testid="escalation-countdown">
					{countdown(escalation.ackDeadlineAt, now)}
				</span>
			{/if}
		</p>

		{#if ackError}
			<div class="error-banner" role="alert">{ackError}</div>
		{/if}

		{#if isAssignee && acknowledgedAt === null && !escalation.exhausted}
			<button
				type="button"
				class="btn-acknowledge"
				data-testid="escalation-acknowledge"
				disabled={acknowledging}
				onclick={acknowledge}
			>
				{acknowledging ? 'Acknowledging…' : 'Acknowledge'}
			</button>
		{/if}
	</section>
{/if}

{#if strandedOnReporter}
	<p class="waiting-trap" data-testid="waiting-on-reporter-trap">
		This report is waiting on its reporter, and the reporter is external — Sentinel has no reply
		channel to them, so this will not clear on its own. Change the status here when you are done
		waiting.
	</p>
{/if}

<style>
	.escalation-strip {
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-left: 3px solid var(--color-primary);
		border-radius: var(--radius-md);
		padding: 0.75rem 0.875rem;
	}

	/* S12: not an ordinary assignment, and it does not look like one. */
	.escalation-exhausted {
		border-left-color: var(--severity-critical-text, #ef4444);
		background: var(--severity-critical-bg);
	}

	.escalation-acknowledged {
		border-left-color: var(--status-resolved-text, #10b981);
	}

	.escalation-headline {
		font-size: 0.75rem;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		font-weight: 700;
		color: var(--text-primary);
		margin: 0 0 0.25rem 0;
	}

	.escalation-body {
		font-size: 0.8125rem;
		color: var(--text-muted);
		margin: 0;
	}

	.escalation-meta {
		display: flex;
		flex-wrap: wrap;
		gap: 0.625rem;
		font-size: 0.6875rem;
		color: var(--text-subtle);
		margin: 0.375rem 0 0 0;
	}

	.escalation-countdown {
		font-family: var(--font-mono);
	}

	.btn-acknowledge {
		margin-top: 0.5rem;
		background: var(--color-primary);
		color: #fff;
		border: none;
		border-radius: var(--radius-sm);
		padding: 0.375rem 0.75rem;
		font-size: 0.75rem;
		font-weight: 600;
		cursor: pointer;
	}

	.btn-acknowledge:hover:not(:disabled) {
		background: var(--color-primary-hover);
	}

	.btn-acknowledge:disabled {
		opacity: 0.6;
		cursor: default;
	}

	.waiting-trap {
		font-size: 0.75rem;
		color: #f59e0b;
		background: rgba(245, 158, 11, 0.12);
		border-radius: var(--radius-sm);
		padding: 0.5rem 0.625rem;
		margin: 0;
	}

	.error-banner {
		background: var(--severity-critical-bg);
		color: var(--severity-critical-text);
		border-radius: var(--radius-sm);
		padding: 0.375rem 0.625rem;
		font-size: 0.75rem;
		margin-top: 0.5rem;
	}
</style>
