/**
 * sentinel-occurrence-spans — whether a trace's spans can be shown, as the issue page, the trace page, the agent API
 * and the CLI all say it. Not under `$lib/server/` because the components import the labels.
 *
 *  - `available`  the viewer can see at least one span of the trace (the same guard as the waterfall).
 *  - `pending`    a pull of the trace is queued or running (span_pull_queue stage < 2) and nothing is visible yet.
 *  - `none`       nothing to show; `SpansReason` says why.
 */
export type SpansStatus = 'available' | 'pending' | 'none';

/**
 * Why a `none` is a `none`:
 *  - `not_pulled`   the trace was never queued (the automatic pull is throttled to one trace per issue per window).
 *  - `not_found`    a pull finished and the trace store had no such trace (or an empty one).
 *  - `too_large`    a pull finished and the trace was over the size bound.
 *  - `failed`       a pull gave up after repeated errors from the trace store.
 *  - `not_allowed`  the organization is not (or no longer) allow-listed for span pull.
 *  - `not_visible`  spans were stored but none is visible to this viewer.
 */
export type SpansReason = 'not_pulled' | 'not_found' | 'too_large' | 'failed' | 'not_allowed' | 'not_visible';

export const SPANS_STATUSES: readonly SpansStatus[] = ['available', 'pending', 'none'];

export interface SpansState {
	status: SpansStatus;
	/** Set only when `status` is `none`. */
	reason: SpansReason | null;
}

/** The sentence the trace page shows in place of an empty waterfall. */
export function spansReasonText(reason: SpansReason | null): string {
	switch (reason) {
		case 'not_found':
			return 'The trace store has no spans for this trace (it may be older than its retention, or the trace was never sampled).';
		case 'too_large':
			return 'This trace is too large to pull from the trace store.';
		case 'failed':
			return 'Pulling this trace from the trace store failed repeatedly. Try again later.';
		case 'not_allowed':
			return 'Span pull is not enabled for this organization.';
		case 'not_visible':
			return 'Spans for this trace were pulled, but none is visible to you.';
		case 'not_pulled':
		default:
			return 'The spans of this trace have not been pulled yet. Only some traces are pulled automatically.';
	}
}

/** The UI polls a pull's status every 5 s for at most 3 minutes (the first fetch is at most ~2 minutes after the error). */
export const SPAN_POLL_MS = 5000;
export const SPAN_POLL_MAX_MS = 3 * 60 * 1000;
