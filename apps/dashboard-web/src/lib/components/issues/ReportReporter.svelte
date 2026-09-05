<script lang="ts">
	// sentinel-user-report-issue §4.2 / fe.md row S1 — who reported this.
	//
	// An additive branch on `reporter.kind`, not a rewrite: an `org_member` report renders exactly
	// the sentence the page rendered before this feature, so the existing manual-support path is
	// untouched (fe.md, "Explicitly OUT of scope").
	//
	// Three things are load-bearing on the `external` branch:
	//   - `displayName` may be null and gets a plain fallback, never an empty span.
	//   - `email` NEVER renders as a `mailto:` link. There is no reply channel (§1); OQ-6 stores
	//     the address and never sends to it, so a link that composes a mail nobody may send is a
	//     trap, not a convenience.
	//   - name and id are attacker-controlled (§12) and are plain text, escaped by Svelte's own
	//     interpolation. They are NOT Markdown and must not go through `Markdown.svelte`.
	import type { ReportReporter } from '$lib/types/report-detail';

	interface Props {
		reporter: ReportReporter;
		projectName: string;
		projectIsInbox?: boolean;
		/** When this report itself arrived. Used only to tell a first-time reporter from a
		 *  returning one — §4.2's stated purpose for `firstSeenAt`. */
		reportedAt?: string | Date | null;
	}

	let { reporter, projectName, projectIsInbox = false, reportedAt = null }: Props = $props();

	const EXTERNAL_FALLBACK_NAME = 'Anonymous reporter';

	function toDate(value: string | Date | null | undefined): Date | null {
		if (!value) return null;
		const date = value instanceof Date ? value : new Date(value);
		return Number.isNaN(date.getTime()) ? null : date;
	}

	let firstSeen = $derived(toDate(reporter.firstSeenAt));
	let reported = $derived(toDate(reportedAt));

	// "first report from this person" vs "ninth" (§4.2). Sentinel does not count an external
	// reporter's reports anywhere, so the honest signal available here is whether this report is
	// the one that created the `external_reporters` row. A minute of slack absorbs the ordinary
	// gap between the reporter row being upserted and the issue row being written.
	const FIRST_REPORT_SLACK_MS = 60_000;

	let isFirstReport = $derived(
		firstSeen !== null &&
			reported !== null &&
			reported.getTime() - firstSeen.getTime() <= FIRST_REPORT_SLACK_MS
	);

	let firstSeenLabel = $derived(firstSeen ? firstSeen.toLocaleDateString() : null);
</script>

{#if reporter.kind === 'external'}
	<p class="report-meta" data-testid="reporter-external">
		Reported by
		<strong>{reporter.displayName ?? EXTERNAL_FALLBACK_NAME}</strong>
		<span class="reporter-kind-tag">external</span>
		in <strong>{projectName}</strong>
		{#if projectIsInbox}<span class="inbox-tag">Triage</span>{/if}
	</p>
	<p class="reporter-secondary">
		{#if reporter.externalId}
			<span class="reporter-external-id" data-testid="reporter-external-id"
				>id {reporter.externalId}</span
			>
		{:else}
			<span class="reporter-external-id">no service id</span>
		{/if}
		{#if firstSeenLabel}
			<span class="reporter-first-seen" data-testid="reporter-first-seen">
				{isFirstReport
					? 'first report from this person'
					: `reporting since ${firstSeenLabel}`}
			</span>
		{/if}
		{#if reporter.email}
			<!-- Deliberately not a mailto: link. See the header comment. -->
			<span class="reporter-email" data-testid="reporter-email">{reporter.email}</span>
			<span class="reporter-no-reply">no reply channel</span>
		{/if}
	</p>
{:else}
	<p class="report-meta" data-testid="reporter-org-member">
		Reported by {reporter.displayName ?? reporter.email ?? 'unknown'} in
		<strong>{projectName}</strong>
		{#if projectIsInbox}<span class="inbox-tag">Triage</span>{/if}
	</p>
{/if}

<style>
	.report-meta {
		font-size: 0.8125rem;
		color: var(--text-muted);
		margin: 0;
	}

	.reporter-secondary {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.5rem;
		font-size: 0.6875rem;
		color: var(--text-subtle);
		margin: 0.25rem 0 0 0;
	}

	.reporter-external-id {
		font-family: var(--font-mono);
	}

	.reporter-email {
		font-family: var(--font-mono);
	}

	.reporter-kind-tag {
		font-size: 0.625rem;
		text-transform: uppercase;
		letter-spacing: 0.02em;
		background: rgba(99, 102, 241, 0.15);
		color: #6366f1;
		border-radius: 3px;
		padding: 0.0625rem 0.3rem;
	}

	.reporter-no-reply {
		border: 1px dashed var(--border-color);
		border-radius: 3px;
		padding: 0 0.25rem;
	}

	.inbox-tag {
		margin-left: 0.375rem;
		font-size: 0.625rem;
		text-transform: uppercase;
		color: var(--text-muted);
		border: 1px solid var(--border-color);
		border-radius: 3px;
		padding: 0 0.25rem;
	}
</style>
