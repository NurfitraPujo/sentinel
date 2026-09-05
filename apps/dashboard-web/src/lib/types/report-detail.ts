// sentinel-user-report-issue §4.2 — the shapes the report-detail page renders, declared once so a
// component, a fixture and a loader cannot drift apart.
//
// These mirror the JSON `GET /api/organizations/{orgId}/reports/{issueId}` returns. They are
// deliberately NOT re-derived from the Drizzle row types: the route serialises dates to RFC 3339
// strings on the way out, and a component that types off the row type would be typed against
// `Date` objects it never receives.

export const REPORT_SOURCE_CHANNELS = [
	'ingestion_sdk',
	'manual_support',
	'api',
	'user_report_api',
] as const;

export type ReportSourceChannel = (typeof REPORT_SOURCE_CHANNELS)[number];

/** §4.2: the one value this feature's door writes. The badge marks this and nothing else. */
export const EXTERNAL_SOURCE_CHANNEL: ReportSourceChannel = 'user_report_api';

export type ReporterKind = 'org_member' | 'external';

export interface ReportReporter {
	kind: ReporterKind;
	displayName: string | null;
	/** Present for both kinds. Never rendered as a `mailto:` link for an external reporter: there
	 *  is no reply channel (§1, OQ-6 — the address is stored and never sent to). */
	email: string | null;
	/** `null` for an org member; the reporting service's opaque id for an external person. */
	externalId: string | null;
	/** `null` for an org member. Lets the page say "first report from this person" vs "ninth". */
	firstSeenAt: string | null;
}

/** §4.3's fixed signal set. `trace_id` and `session_id` are inert in v1 and never appear. */
export const TRIAGE_SIGNALS = ['window', 'release', 'environment', 'trace_id', 'session_id'] as const;

export type TriageSignal = (typeof TRIAGE_SIGNALS)[number];

export interface TriageCandidate {
	issueId: string;
	message: string;
	errorClass: string;
	/** `0.0`–`1.0`. **A rank, not a probability** — comparable within one report, meaningless
	 *  across reports, which is why the UI must never render it as a percentage. */
	score: number;
	signals: string[];
	/** `true` when an `issue_relations` row was actually written (§4.3's 0.7 threshold); `false`
	 *  when it is a suggestion only and a human must confirm it. */
	linked: boolean;
}

export interface Triage {
	ranAt: string;
	/** Which matcher produced these. `window_release` is the only v1 value. */
	strategy: string;
	candidates: TriageCandidate[];
}

export interface Escalation {
	scheduleId: string;
	assigneeId: string;
	escalatedAt: string;
	ackDeadlineAt: string;
	acknowledgedAt: string | null;
	/** `1` on the first assignment, incremented by each timer reassignment, bounded by `max_hops`. */
	hop: number;
	/** `true` once `hop` reached `max_hops` with no acknowledgement. */
	exhausted: boolean;
}

/** §4.4's `ESCALATION_MAX_HOPS` default, restated for the "escalated N of 3" rendering. */
export const ESCALATION_MAX_HOPS = 3;
