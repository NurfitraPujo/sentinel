/**
 * Manual Issues M3/M4 (docs/plans/MANUAL_ISSUES_DESIGN.md §5/§8, Q10): shared visibility-aware
 * polling loop. `CommentThread.svelte` (M3) established the pattern inline -- `setInterval` every
 * ~10s, skip the tick when `document.hidden` -- and M4's `NotificationBell.svelte` needs the exact
 * same cadence for the unread count. Extracted here so the interval/visibility bookkeeping lives
 * in one testable place instead of being copy-pasted a second time.
 *
 * Deliberately does NOT poll immediately on start -- callers that want an initial load do it
 * themselves (same as CommentThread's separate `loadInitial()` `$effect`), keeping this helper to
 * exactly one job: "call `fn` roughly every `intervalMs`, but never while the tab is hidden".
 */
export function startVisiblePolling(fn: () => void | Promise<void>, intervalMs: number): () => void {
	const interval = setInterval(() => {
		if (typeof document !== 'undefined' && document.hidden) return;
		void fn();
	}, intervalMs);

	return () => clearInterval(interval);
}
