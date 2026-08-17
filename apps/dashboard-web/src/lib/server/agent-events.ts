/**
 * N1b (events feed): shared constants for `GET /api/agent/events`. Kept out of the route file
 * per B12 (shared constants belong in `$lib/server/`, not route files, since `+page.server.ts` /
 * `+server.ts` route files may only export handler-shaped bindings the SvelteKit build enforces).
 *
 * Mirrors the full `issue_activity.event_type` CHECK constraint exactly as documented at
 * `schema.ts`'s `issueActivity` definition (source of truth: the migrations listed there) --
 * 'status_changed' (not 'status_change'), plus the Manual Issues M1 additions.
 */
export const AGENT_EVENT_TYPES = [
	'status_changed',
	'assigned',
	'unassigned',
	'regressed',
	'ai_analysis',
	'linked',
	'commented',
	'claimed',
	'claim_released',
	'progress_update',
	'question_asked',
	'question_answered',
	'moved',
	'attachment_added',
	'report_edited',
	'report_created',
	// N7a (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md, A01/A06/R2): written by
	// apps/processor-go/store/store.go's StoreEvent, not by any dashboard-web code path.
	// 'created' fires once per genuinely-new issue; 'occurrence_burst' fires at most once per
	// issue per OCCURRENCE_EVENT_MIN_INTERVAL_SECONDS on repeat occurrences. No backfill for
	// pre-existing issues — see the 1723400000 migration header.
	'created',
	'occurrence_burst',
	// N8 (docs/audits/AGENT_AUTOMATION_AUDIT_2026-08-14.md A04, DECISIONS.md D20): synthesized in
	// the events feed from an `issue_tombstones` row (queries/events.ts), never written into
	// issue_activity -- the issue and its activity have been deleted by retention. Still part of
	// the issue_activity CHECK set (1723700000 migration) so this documented chain stays whole.
	'issue_deleted',
] as const;

export type AgentEventType = (typeof AGENT_EVENT_TYPES)[number];

/**
 * N1b's 2-second lag guard: events feed queries never return rows created within the last
 * `EVENTS_LAG_GUARD_INTERVAL` of `now()`, so a slightly-behind read replica / concurrent
 * transaction that hasn't committed yet can't produce a gap a poller reads past and never
 * revisits (seq is a bigint identity, not a `createdAt` ordering guarantee).
 */
export const EVENTS_LAG_GUARD_INTERVAL = '2 seconds';

export const EVENTS_DEFAULT_LIMIT = 50;
export const EVENTS_MIN_LIMIT = 1;
export const EVENTS_MAX_LIMIT = 200;
