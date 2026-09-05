// sentinel-user-report-issue §4.2's worked examples, transcribed as typed fixtures.
//
// **Why a module and not an `if (dev)` branch.** fe.md task 1 is explicit: the shapes this feature
// adds may be built against a mock, "a fixture module under `src/lib/` or a Vitest mock, never a
// committed `if (dev)` branch in a component". These are that module. Nothing in `src/routes/`
// imports them — they exist for the component tests beside the components, so the three triage
// states, the suggestion-versus-link distinction and the escalation strip's whole life are each
// pinned to the literal example the contract prints rather than to a shape someone remembered.
//
// Every value below is copied from §4.2's jsonc blocks. If a shipped route disagrees with one of
// them, that is a deviation to report through §10 — not a fixture to quietly edit.

import type { Escalation, ReportReporter, Triage, TriageCandidate } from '$lib/types/report-detail';

/** §4.2's example reporter: an external person, no email, an opaque service id, first seen once. */
export const externalReporterFixture: ReportReporter = {
	kind: 'external',
	displayName: 'Sarah K.',
	email: null,
	externalId: 'u_8f3c41',
	firstSeenAt: '2026-08-19T04:40:11Z',
};

/** The same shape for the path this feature does not change: an org member. */
export const orgMemberReporterFixture: ReportReporter = {
	kind: 'org_member',
	displayName: 'Devni K.',
	email: 'devni@example.com',
	externalId: null,
	firstSeenAt: null,
};

/** §4.2's `context`, verbatim, unknown keys included. */
export const externalReportContextFixture: Record<string, unknown> = {
	page: 'Visits',
	release: 'v31.70',
	brand: 'wayist',
	environment: 'production',
};

export const externalAttachmentsFixture = [
	{
		id: '3b1f0d2c-9a4e-4b77-8f21-6d5c4b3a2019',
		filename: 'screen.png',
		contentType: 'image/png',
		sizeBytes: 184213,
	},
] as const;

/** §4.2's example candidate: above the 0.7 threshold, so the link was actually written. */
export const writtenLinkCandidateFixture: TriageCandidate = {
	issueId: '1f0d1a2b-3c4d-4e5f-8a9b-0c1d2e3f4a5b',
	message: 'pq: deadlock detected',
	errorClass: 'PostgresError',
	score: 0.8,
	signals: ['window', 'release', 'environment'],
	linked: true,
};

/**
 * The other side of §4.3's threshold: `window` alone scores 0.4, below `link_threshold` = 0.7, so
 * nothing was written and a human must confirm. Not printed in §4.2 — derived from §4.3's scoring
 * table, which is the only place the `linked: false` case is specified.
 */
export const suggestionCandidateFixture: TriageCandidate = {
	issueId: '2a1b3c4d-5e6f-4708-9a1b-2c3d4e5f6071',
	message: 'context deadline exceeded',
	errorClass: 'TimeoutError',
	score: 0.4,
	signals: ['window'],
	linked: false,
};

/** State three: a populated list. */
export const triageWithCandidatesFixture: Triage = {
	ranAt: '2026-09-04T09:12:04Z',
	strategy: 'window_release',
	candidates: [writtenLinkCandidateFixture, suggestionCandidateFixture],
};

/** State two, printed in §4.2 under "Empty and absent": looked, and found nothing. */
export const triageWithNoMatchesFixture: Triage = {
	ranAt: '2026-09-04T09:12:04Z',
	strategy: 'window_release',
	candidates: [],
};

/**
 * State one is `null`, and it is a value rather than an absence: "not looked at" — a report from
 * before this feature shipped, an org-member report with no `context`, or a run that failed.
 * Named so a test reads as the three states it is, not as two states and a missing prop.
 */
export const triageNotRunFixture = null;

/** §4.2's example escalation: first hop, deadline live, nobody has acknowledged yet. */
export const escalationHopOneFixture: Escalation = {
	scheduleId: 'a4d2c8e1-77b3-4f0a-9c62-1e5d3b8a7f40',
	assigneeId: 'usr_2Nk4',
	escalatedAt: '2026-09-04T09:12:05Z',
	ackDeadlineAt: '2026-09-04T09:27:05Z',
	acknowledgedAt: null,
	hop: 1,
	exhausted: false,
};

/** After the assignee acknowledged: the clock stops, and so do the control and the countdown. */
export const escalationAcknowledgedFixture: Escalation = {
	...escalationHopOneFixture,
	acknowledgedAt: '2026-09-04T09:19:41Z',
};

/** After one timer reassignment (§4.4): a new assignee, a fresh deadline, `hop` incremented. */
export const escalationHopTwoFixture: Escalation = {
	...escalationHopOneFixture,
	assigneeId: 'usr_7Qm1',
	escalatedAt: '2026-09-04T09:27:05Z',
	ackDeadlineAt: '2026-09-04T09:42:05Z',
	hop: 2,
};

/** §4.4's terminal state: `max_hops` reached with no acknowledgement. Its own visible state. */
export const escalationExhaustedFixture: Escalation = {
	...escalationHopOneFixture,
	assigneeId: 'usr_9Zt8',
	hop: 3,
	exhausted: true,
};
