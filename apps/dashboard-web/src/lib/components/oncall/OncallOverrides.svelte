<script lang="ts">
	// sentinel-user-report-issue §4.4a, fe.md rows O7–O9 — overrides: add, list, remove.
	//
	// **A `409` is surfaced as a sentence naming the override it collides with** (O8), not as a
	// toast saying "error". The API answers `409` with a message; the component pairs it with the
	// range it found locally, so the reader is told *which* interval is in the way and does not have
	// to open the list and compare dates by eye.
	import type { OncallCandidate, OncallOverride } from '$lib/types/oncall';
	import { toDatetimeLocalValue } from './oncall-format';

	interface Props {
		overrides: OncallOverride[];
		candidates: OncallCandidate[];
		canWrite: boolean;
		onadd?: (input: { userId: string; startsAt: string; endsAt: string }) => Promise<void>;
		onremove?: (overrideId: string) => Promise<void>;
		/** The API's message for the last failed add, if any. */
		addError?: string | null;
	}

	let { overrides, candidates, canWrite, onadd, onremove, addError = null }: Props = $props();

	let userId = $state('');
	let startsAt = $state('');
	let endsAt = $state('');
	let busy = $state(false);

	function nameFor(id: string): string {
		return candidates.find((c) => c.userId === id)?.name ?? id;
	}

	function formatRange(override: OncallOverride): string {
		return `${new Date(override.startsAt).toLocaleString()} → ${new Date(override.endsAt).toLocaleString()}`;
	}

	/**
	 * The sentence O8 asks for. `collidesWith` is computed from the range the user just tried, so
	 * the message names a real row rather than repeating the status code. If nothing overlaps
	 * locally — the list is stale, say — the API's own message is shown unchanged rather than a
	 * fabricated collision.
	 */
	let collision = $derived.by(() => {
		if (!addError || !startsAt || !endsAt) return null;
		const start = new Date(startsAt).getTime();
		const end = new Date(endsAt).getTime();
		if (Number.isNaN(start) || Number.isNaN(end)) return null;
		return (
			overrides.find(
				(o) => new Date(o.startsAt).getTime() < end && new Date(o.endsAt).getTime() > start
			) ?? null
		);
	});

	async function add() {
		if (!onadd || !userId || !startsAt || !endsAt) return;
		busy = true;
		try {
			await onadd({
				userId,
				startsAt: new Date(startsAt).toISOString(),
				endsAt: new Date(endsAt).toISOString(),
			});
		} finally {
			busy = false;
		}
	}

	function prefillNow() {
		if (startsAt) return;
		startsAt = toDatetimeLocalValue(new Date());
	}
</script>

<section class="overrides">
	<h3 class="subsection-heading">Overrides</h3>
	<p class="subsection-hint">
		An override wins over the rotation for its interval — the week somebody is away, or a swap.
	</p>

	{#if overrides.length === 0}
		<p class="overrides-empty" data-testid="overrides-empty">
			No overrides. The rotation decides every hour.
		</p>
	{:else}
		<ul class="override-list" data-testid="override-list">
			{#each overrides as override (override.id)}
				<li class="override">
					<span class="override-who">{nameFor(override.userId)}</span>
					<span class="override-range">{formatRange(override)}</span>
					<span class="override-note">interrupts the rotation for this interval</span>
					{#if canWrite}
						<button
							type="button"
							class="btn-remove"
							aria-label="Remove the override for {nameFor(override.userId)}"
							onclick={() => onremove?.(override.id)}>Remove</button
						>
					{/if}
				</li>
			{/each}
		</ul>
	{/if}

	{#if canWrite}
		{#if addError}
			<p class="override-error" role="alert" data-testid="override-error">
				{#if collision}
					That range overlaps the existing override for {nameFor(collision.userId)},
					{formatRange(collision)}. Remove or shorten that one first.
				{:else}
					{addError}
				{/if}
			</p>
		{/if}

		<div class="override-form" data-testid="override-form">
			<label class="field">
				<span class="field-label">Person</span>
				<select bind:value={userId}>
					<option value="">Select a member…</option>
					{#each candidates as candidate (candidate.userId)}
						<option value={candidate.userId}>{candidate.name}</option>
					{/each}
				</select>
			</label>
			<label class="field">
				<span class="field-label">From</span>
				<input type="datetime-local" bind:value={startsAt} onfocus={prefillNow} />
			</label>
			<label class="field">
				<span class="field-label">Until</span>
				<input type="datetime-local" bind:value={endsAt} />
			</label>
			<button
				type="button"
				class="btn-primary"
				disabled={busy || !userId || !startsAt || !endsAt}
				onclick={add}
			>
				{busy ? 'Adding…' : 'Add override'}
			</button>
		</div>
	{/if}
</section>

<style>
	.overrides {
		margin-top: 1rem;
	}

	.subsection-heading {
		font-size: 0.75rem;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--text-subtle);
		font-weight: 600;
		margin: 0 0 0.25rem 0;
	}

	.subsection-hint {
		font-size: 0.75rem;
		color: var(--text-muted);
		margin: 0 0 0.5rem 0;
	}

	.override-list {
		list-style: none;
		margin: 0 0 0.625rem 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.375rem;
	}

	.override {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		flex-wrap: wrap;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.375rem 0.625rem;
		font-size: 0.8125rem;
	}

	.override-who {
		font-weight: 600;
		color: var(--text-primary);
	}

	.override-range {
		font-family: var(--font-mono);
		font-size: 0.6875rem;
		color: var(--text-muted);
	}

	.override-note {
		font-size: 0.6875rem;
		color: var(--text-subtle);
	}

	.overrides-empty {
		font-size: 0.75rem;
		color: var(--text-subtle);
		margin: 0 0 0.625rem 0;
	}

	.override-error {
		font-size: 0.75rem;
		color: var(--severity-critical-text);
		background: var(--severity-critical-bg);
		border-radius: var(--radius-sm);
		padding: 0.5rem 0.625rem;
		margin: 0 0 0.5rem 0;
	}

	.override-form {
		display: flex;
		gap: 0.5rem;
		align-items: flex-end;
		flex-wrap: wrap;
	}

	.field {
		display: flex;
		flex-direction: column;
		gap: 0.1875rem;
	}

	.field-label {
		font-size: 0.6875rem;
		color: var(--text-subtle);
	}

	.field select,
	.field input {
		background: var(--bg-root);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		font-size: 0.8125rem;
		padding: 0.3125rem 0.5rem;
	}

	.btn-primary {
		background: var(--color-primary);
		color: #fff;
		border: none;
		border-radius: var(--radius-sm);
		padding: 0.375rem 0.75rem;
		font-size: 0.75rem;
		font-weight: 600;
		cursor: pointer;
	}

	.btn-primary:disabled {
		opacity: 0.5;
		cursor: default;
	}

	.btn-remove {
		margin-left: auto;
		background: transparent;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		color: var(--text-muted);
		font-size: 0.6875rem;
		padding: 0.125rem 0.375rem;
		cursor: pointer;
	}
</style>
