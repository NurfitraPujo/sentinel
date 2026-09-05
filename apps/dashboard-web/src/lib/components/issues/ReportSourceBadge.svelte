<script lang="ts">
	// sentinel-user-report-issue OQ-8, fe.md rows L1/L2 — the per-row source badge.
	//
	// **It marks one kind only.** `user_report_api` is the value this feature's door writes
	// (§4.2), and an org-member report carries no badge at all: marking both is marking neither,
	// which is the whole point of L2. Every other `source_channel` value is likewise unbadged —
	// `ingestion_sdk` and `api` are not what this filter separates.
	import { EXTERNAL_SOURCE_CHANNEL } from '$lib/types/report-detail';

	interface Props {
		/** `issues.source_channel`, which is a plain varchar and so `string` on the way out. */
		sourceChannel: string | null;
	}

	let { sourceChannel }: Props = $props();
</script>

{#if sourceChannel === EXTERNAL_SOURCE_CHANNEL}
	<span class="source-badge" data-testid="source-badge" title="Submitted by a service on behalf of an end user">
		external
	</span>
{/if}

<style>
	.source-badge {
		display: inline-block;
		font-size: 0.625rem;
		text-transform: uppercase;
		letter-spacing: 0.02em;
		background: rgba(99, 102, 241, 0.15);
		color: #6366f1;
		border-radius: 3px;
		padding: 0.0625rem 0.3rem;
		font-weight: 600;
	}
</style>
