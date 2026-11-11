<script lang="ts">
	// sentinel-user-report-issue §4.2 / §4.3, fe.md rows S2, S3, S4, S6 — automatic triage.
	//
	// **Three states, three renderings, and they are not a shared box with a different string.**
	// §4.2 is explicit that `null` and `{"candidates": []}` mean different things, and rendering
	// them the same is the defect rows S3 and S4 exist to catch:
	//   - `triage === null`        — nothing was looked at. Dashed, muted, and it says why nothing
	//                                ran rather than reporting a result.
	//   - `candidates.length === 0` — it was looked at and nothing matched. A solid panel with the
	//                                run's own `ranAt` and `strategy`, because a run happened.
	//   - a populated list          — the candidates, with score and signals.
	//
	// **A suggestion is not a link.** §4.3 writes an `issue_relations` row only above the 0.7
	// threshold; everything below is returned as `linked: false` and is never written. Confirming
	// one posts to the EXISTING `POST /api/issues/{issueId}/relations` route rather than to
	// anything this feature adds, so the resulting relation and its `issue_activity` row are
	// attributed to the human who confirmed it — `created_by_type = 'user'` — and not to `system`.
	// That attribution is the whole reason the affordance exists.
	//
	// `score` renders as a rank inside this one report, never as a percentage: §4.2 calls it "a
	// rank, not a probability — comparable within one report, meaningless across reports", and a
	// "80%" invites exactly the cross-report comparison the contract says is meaningless.
	import type { Triage, TriageCandidate } from '$lib/types/report-detail';

	interface Props {
		issueId: string;
		triage: Triage | null;
		/** Confirming a suggestion writes a relation, so it is offered only to a write role. */
		canWrite?: boolean;
		/** Called after a successful confirm, so the page can refresh its relations panel. */
		onconfirm?: (candidate: TriageCandidate) => void;
	}

	let { issueId, triage, canWrite = false, onconfirm }: Props = $props();

	let confirmingIssueId = $state<string | null>(null);
	let confirmedIssueIds = $state<string[]>([]);
	let confirmError = $state<string | null>(null);

	function formatRanAt(value: string): string {
		const date = new Date(value);
		return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
	}

	// Two decimals, and the word "rank" beside it. Not `Intl.NumberFormat('percent')`.
	function formatScore(score: number): string {
		return score.toFixed(2);
	}

	async function confirmSuggestion(candidate: TriageCandidate) {
		confirmError = null;
		confirmingIssueId = candidate.issueId;
		try {
			const res = await fetch(`/api/issues/${issueId}/relations`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ targetIssueId: candidate.issueId, relationType: 'linked_to' }),
			});
			if (!res.ok) {
				const body = await res.json().catch(() => ({}));
				throw new Error(body.message || `Failed to link (${res.status})`);
			}
			confirmedIssueIds = [...confirmedIssueIds, candidate.issueId];
			onconfirm?.(candidate);
		} catch (err) {
			confirmError = err instanceof Error ? err.message : 'Failed to link';
		} finally {
			confirmingIssueId = null;
		}
	}
</script>

<section class="triage-panel" data-testid="triage-panel">
	<h2 class="section-heading">Likely service errors</h2>

	{#if triage === null}
		<!-- S4. Deliberately unlike the S3 rendering below: dashed and unfilled, no run metadata,
		     and a sentence about the matcher rather than about a result. -->
		<div class="triage-not-run" data-testid="triage-not-run">
			<p class="triage-not-run-title">Not triaged yet</p>
			<p class="triage-not-run-body">
				No matcher has looked at this report. Reports that arrive without a service context —
				an org-member report, or one filed before automatic triage shipped — are never matched.
			</p>
		</div>
	{:else if triage.candidates.length === 0}
		<!-- S3. A run happened and found nothing: solid panel, and it shows the run itself. -->
		<div class="triage-no-matches" data-testid="triage-no-matches">
			<p class="triage-no-matches-title">No matching service errors</p>
			<p class="triage-no-matches-body">
				The <code>{triage.strategy}</code> matcher ran and found nothing in this project inside the
				report's time window.
			</p>
			<p class="triage-run-meta">Triaged {formatRanAt(triage.ranAt)}</p>
		</div>
	{:else}
		<div data-testid="triage-candidates">
			<p class="triage-run-meta">
				<code>{triage.strategy}</code> · triaged {formatRanAt(triage.ranAt)} ·
				{triage.candidates.length}
				{triage.candidates.length === 1 ? 'candidate' : 'candidates'}
			</p>

			{#if confirmError}
				<div class="error-banner" role="alert">{confirmError}</div>
			{/if}

			<ul class="candidate-list">
				{#each triage.candidates as candidate (candidate.issueId)}
					{@const confirmed = confirmedIssueIds.includes(candidate.issueId)}
					{@const linked = candidate.linked || confirmed}
					<li
						class="candidate"
						class:candidate-linked={linked}
						class:candidate-suggestion={!linked}
						data-testid={linked ? 'triage-candidate-linked' : 'triage-candidate-suggestion'}
					>
						<div class="candidate-head">
							{#if linked}
								<span class="candidate-tag tag-linked" data-testid="candidate-tag-linked">
									linked
								</span>
							{:else}
								<span class="candidate-tag tag-suggestion" data-testid="candidate-tag-suggestion">
									suggestion — not linked
								</span>
							{/if}
							<a class="candidate-message" href="/issues/{candidate.issueId}">
								{candidate.message}
							</a>
						</div>

						<div class="candidate-meta">
							<span class="candidate-class">{candidate.errorClass}</span>
							<span class="candidate-score" data-testid="candidate-score">
								rank {formatScore(candidate.score)} in this report
							</span>
							{#each candidate.signals as signal}
								<span class="signal-chip">{signal}</span>
							{/each}
						</div>

						{#if !linked}
							<p class="candidate-suggestion-note">
								Nothing has been written. Confirming records the link as yours.
							</p>
							{#if canWrite}
								<button
									type="button"
									class="btn-confirm"
									data-testid="candidate-confirm"
									disabled={confirmingIssueId === candidate.issueId}
									onclick={() => confirmSuggestion(candidate)}
								>
									{confirmingIssueId === candidate.issueId ? 'Linking…' : 'Confirm link'}
								</button>
							{/if}
						{/if}
					</li>
				{/each}
			</ul>
		</div>
	{/if}
</section>

<style>
	.triage-panel {
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		padding: 0.875rem 1rem;
	}

	.section-heading {
		font-size: 0.75rem;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--text-subtle);
		font-weight: 600;
		margin: 0 0 0.625rem 0;
	}

	/* S4: dashed, unfilled, no run metadata. */
	.triage-not-run {
		border: 1px dashed var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.75rem 0.875rem;
		background: transparent;
	}

	.triage-not-run-title {
		font-size: 0.8125rem;
		font-weight: 600;
		color: var(--text-muted);
		margin: 0 0 0.25rem 0;
	}

	.triage-not-run-body {
		font-size: 0.75rem;
		color: var(--text-subtle);
		margin: 0;
	}

	/* S3: solid and filled, and it reports a run that happened. */
	.triage-no-matches {
		border: 1px solid var(--border-color);
		border-left: 3px solid var(--color-primary);
		border-radius: var(--radius-sm);
		padding: 0.75rem 0.875rem;
		background: var(--bg-root);
	}

	.triage-no-matches-title {
		font-size: 0.8125rem;
		font-weight: 600;
		color: var(--text-primary);
		margin: 0 0 0.25rem 0;
	}

	.triage-no-matches-body {
		font-size: 0.75rem;
		color: var(--text-muted);
		margin: 0 0 0.375rem 0;
	}

	.triage-run-meta {
		font-size: 0.6875rem;
		color: var(--text-subtle);
		margin: 0 0 0.5rem 0;
	}

	.candidate-list {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.5rem;
	}

	.candidate {
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.625rem 0.75rem;
	}

	.candidate-linked {
		background: var(--bg-root);
		border-left: 3px solid var(--color-primary);
	}

	/* A suggestion reads as not-yet-a-link: dashed edge, no fill, and it says so in words. */
	.candidate-suggestion {
		background: transparent;
		border-style: dashed;
	}

	.candidate-head {
		display: flex;
		align-items: baseline;
		gap: 0.5rem;
		flex-wrap: wrap;
	}

	.candidate-message {
		color: var(--text-primary);
		font-size: 0.8125rem;
		font-weight: 500;
		text-decoration: none;
	}

	.candidate-message:hover {
		text-decoration: underline;
	}

	.candidate-tag {
		font-size: 0.625rem;
		text-transform: uppercase;
		letter-spacing: 0.02em;
		border-radius: 3px;
		padding: 0.0625rem 0.3rem;
		font-weight: 600;
	}

	.tag-linked {
		background: var(--status-resolved-bg);
		color: var(--status-resolved-text);
	}

	.tag-suggestion {
		background: transparent;
		color: var(--text-muted);
		border: 1px dashed var(--border-color);
	}

	.candidate-meta {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.375rem;
		margin-top: 0.3125rem;
		font-size: 0.6875rem;
		color: var(--text-subtle);
	}

	.candidate-class {
		font-family: var(--font-mono);
		color: var(--text-muted);
	}

	.signal-chip {
		border: 1px solid var(--border-color);
		border-radius: 3px;
		padding: 0 0.25rem;
		font-family: var(--font-mono);
	}

	.candidate-suggestion-note {
		font-size: 0.6875rem;
		color: var(--text-subtle);
		margin: 0.375rem 0 0.375rem 0;
	}

	.btn-confirm {
		background: transparent;
		color: var(--text-primary);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.25rem 0.625rem;
		font-size: 0.75rem;
		font-weight: 600;
		cursor: pointer;
	}

	.btn-confirm:hover:not(:disabled) {
		border-color: var(--color-primary);
	}

	.btn-confirm:disabled {
		opacity: 0.6;
		cursor: default;
	}

	.error-banner {
		background: var(--severity-critical-bg);
		color: var(--severity-critical-text);
		border-radius: var(--radius-sm);
		padding: 0.375rem 0.625rem;
		font-size: 0.75rem;
		margin-bottom: 0.5rem;
	}
</style>
