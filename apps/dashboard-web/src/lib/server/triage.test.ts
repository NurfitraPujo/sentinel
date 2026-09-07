import { describe, it, expect } from 'vitest';

/**
 * sentinel-user-report-issue §4.3 / §11 — the triage matcher, at the boundary §11 names:
 * "The triage matcher function (§4.3) | new — a unit boundary below HTTP … Scoring and the link
 * threshold need table-driven cases with dozens of candidate rows. Driving those through HTTP
 * would need a seeded issue set per case and would assert ranking through a bounded array — slow,
 * and it would not catch a ranking regression. The rule *is* the feature."
 *
 * So this file is the rule, stated as a table, with no database, no clock and no environment. The
 * WRITE half of §4.3 — that a `0.7` writes exactly one `linked_to` row plus one `linked` activity
 * row in one transaction, that a `0.4` writes nothing, and that a re-run writes no second row — is
 * a claim about ROWS and is asserted against a real Postgres in `triage.integration.test.ts`; a
 * Drizzle double cannot tell "wrote the right rows" from "wrote nothing" (B10 addendum).
 */
import {
	scoreTriageCandidates,
	resolveReportMatchWindowMinutes,
	triageWindow,
	DEFAULT_REPORT_MATCH_WINDOW_MINUTES,
	TRIAGE_LINK_THRESHOLD,
	TRIAGE_MAX_CANDIDATES,
	TRIAGE_SIGNAL_WEIGHTS,
	TRIAGE_STRATEGY,
	type TriageCandidateRow,
	type TriageMatchContext,
} from './triage';

const AT = (iso: string) => new Date(iso);

function row(overrides: Partial<TriageCandidateRow> & { issueId: string }): TriageCandidateRow {
	return {
		message: 'pq: deadlock detected',
		errorClass: 'PostgresError',
		latestOccurrenceAt: AT('2026-09-04T09:10:00Z'),
		releaseVersions: [],
		...overrides,
	};
}

function context(overrides: Partial<TriageMatchContext> = {}): TriageMatchContext {
	// sentinel-project-environments decision 50: `environment` is no longer nullable on this type
	// and is no longer a scoring signal — the query filters on it, so every row that reaches the
	// scorer is already in this environment. The default here is §4.8a's default for a report that
	// asserted nothing.
	return { release: null, environment: 'production', traceId: null, sessionId: null, ...overrides };
}

describe('§4.3 constants are the contract, not a preference', () => {
	it('fixes the score table, the threshold, the bound and the strategy name', () => {
		// `environment: 0.1` is deliberately ABSENT (decision 50): environment stopped being a
		// signal and became a `WHERE` predicate in findTriageCandidates. This assertion is what
		// catches somebody re-adding it as a tautology that lifts every score by the same 0.1 and
		// silently moves the 0.7 threshold.
		expect(TRIAGE_SIGNAL_WEIGHTS).toEqual({
			window: 0.4,
			release: 0.3,
			trace_id: 0.2,
			session_id: 0.1,
		});
		expect(TRIAGE_LINK_THRESHOLD).toBe(0.7);
		expect(TRIAGE_MAX_CANDIDATES).toBe(5);
		expect(TRIAGE_STRATEGY).toBe('window_release');
		expect(DEFAULT_REPORT_MATCH_WINDOW_MINUTES).toBe(30);
	});
});

describe('scoreTriageCandidates — §4.3\'s score table', () => {
	// The table the contract states, one row per reachable score in v1.
	const cases: {
		name: string;
		row: TriageCandidateRow;
		context: TriageMatchContext;
		score: number;
		signals: string[];
		linkable: boolean;
	}[] = [
		{
			name: 'window alone is the floor: 0.4, and is BELOW the link threshold',
			row: row({ issueId: 'a' }),
			context: context(),
			score: 0.4,
			signals: ['window'],
			linkable: false,
		},
		{
			name: 'window + release = 0.7, the exact threshold — the only way v1 writes a link',
			row: row({ issueId: 'a', releaseVersions: ['v31.70'] }),
			context: context({ release: 'v31.70' }),
			score: 0.7,
			signals: ['window', 'release'],
			linkable: true,
		},
		{
			// Decision 50: this used to be 0.8 with an `environment` signal. The environment is now a
			// precondition of the row EXISTING, so the top reachable score in v1 is 0.7 and the only
			// thing that separates candidates is the release.
			name: 'window + release in the reported environment = 0.7, and environment adds nothing',
			row: row({ issueId: 'a', releaseVersions: ['v31.70'] }),
			context: context({ release: 'v31.70', environment: 'production' }),
			score: 0.7,
			signals: ['window', 'release'],
			linkable: true,
		},
		{
			// The old 'window + environment alone = 0.5' case. A matching environment can no longer
			// lift a score on its own, because every candidate matches it.
			name: 'the environment alone is 0.4, not 0.5 — it is no longer a signal',
			row: row({ issueId: 'a' }),
			context: context({ environment: 'production' }),
			score: 0.4,
			signals: ['window'],
			linkable: false,
		},
		{
			name: 'a release the report did not name does not fire: the context release is null',
			row: row({ issueId: 'a', releaseVersions: ['v31.70'] }),
			context: context({ release: null }),
			score: 0.4,
			signals: ['window'],
			linkable: false,
		},
		{
			name: 'a DIFFERENT release does not fire — equality, never a prefix or a range',
			row: row({ issueId: 'a', releaseVersions: ['v31.71'] }),
			context: context({ release: 'v31.70' }),
			score: 0.4,
			signals: ['window'],
			linkable: false,
		},
		{
			name: 'the release matches on ANY in-window occurrence, not only the newest',
			row: row({ issueId: 'a', releaseVersions: ['v31.69', 'v31.70', 'v31.71'] }),
			context: context({ release: 'v31.70' }),
			score: 0.7,
			signals: ['window', 'release'],
			linkable: true,
		},
		{
			// The old 'a null context environment does not fire' case, restated for a type that no
			// longer admits null: a DIFFERENT environment cannot change the score either, because
			// the scorer does not look at it at all. Whether the row belongs in this environment is
			// decided by the query, and is asserted in triage.integration.test.ts.
			name: 'a different context environment changes nothing — the scorer does not read it',
			row: row({ issueId: 'a' }),
			context: context({ environment: 'staging' }),
			score: 0.4,
			signals: ['window'],
			linkable: false,
		},
	];

	for (const testCase of cases) {
		it(testCase.name, () => {
			const [candidate] = scoreTriageCandidates([testCase.row], testCase.context);
			expect(candidate.score).toBe(testCase.score);
			expect(candidate.signals).toEqual(testCase.signals);
			expect(candidate.score >= TRIAGE_LINK_THRESHOLD).toBe(testCase.linkable);
		});
	}

	it('carries the issue identity through untouched', () => {
		const [candidate] = scoreTriageCandidates(
			[row({ issueId: 'issue-1', message: 'boom', errorClass: 'TypeError' })],
			context()
		);
		expect(candidate).toEqual({
			issueId: 'issue-1',
			message: 'boom',
			errorClass: 'TypeError',
			score: 0.4,
			signals: ['window'],
		});
	});
});

describe('scoreTriageCandidates — trace_id and session_id are INERT in v1', () => {
	// §4.3: "declared as future signals, carried in `context` and in `signals`, and inert in v1
	// because the design source captures neither". They are weighted (so adding them later does not
	// renumber the scale) and they never fire. A context that carries both scores exactly as one
	// that carries neither -- if this test ever goes red because a score moved, someone has shipped
	// a scale change disguised as a feature.
	it('a context carrying both scores identically to one carrying neither', () => {
		const candidateRow = row({ issueId: 'a' });
		const inert = scoreTriageCandidates([candidateRow], context({ traceId: 't-1', sessionId: 's-1' }));
		const bare = scoreTriageCandidates([candidateRow], context());
		expect(inert).toEqual(bare);
		expect(inert[0].score).toBe(0.4);
		expect(inert[0].signals).toEqual(['window']);
	});

	it('never emits trace_id or session_id in signals, even at the top score', () => {
		const [candidate] = scoreTriageCandidates(
			[row({ issueId: 'a', releaseVersions: ['v1'] })],
			context({ release: 'v1', environment: 'production', traceId: 't-1', sessionId: 's-1' })
		);
		expect(candidate.signals).not.toContain('trace_id');
		expect(candidate.signals).not.toContain('session_id');
		expect(candidate.score).toBe(0.7);
	});

	it('signals is never empty for a candidate', () => {
		const scored = scoreTriageCandidates(
			[row({ issueId: 'a' }), row({ issueId: 'b' }), row({ issueId: 'c' })],
			context()
		);
		for (const candidate of scored) {
			expect(candidate.signals.length).toBeGreaterThan(0);
		}
	});
});

describe('scoreTriageCandidates — §4.3\'s ranking', () => {
	it('ranks by score descending', () => {
		const scored = scoreTriageCandidates(
			[
				row({ issueId: 'low' }),
				row({ issueId: 'top', releaseVersions: ['v1'], latestOccurrenceAt: AT('2026-09-04T09:20:00Z') }),
				row({ issueId: 'mid', releaseVersions: ['v1'] }),
			],
			context({ release: 'v1', environment: 'production' })
		);
		// Decision 50 collapsed the old 0.8/0.7/0.4 ladder: `top` and `mid` both score 0.7 now (the
		// environment no longer separates them), so `top` leads on the recency tie-break instead.
		expect(scored.map((candidate) => candidate.issueId)).toEqual(['top', 'mid', 'low']);
		expect(scored.map((candidate) => candidate.score)).toEqual([0.7, 0.7, 0.4]);
	});

	it('breaks a score tie on the most recent in-window occurrence', () => {
		const scored = scoreTriageCandidates(
			[
				row({ issueId: 'older', latestOccurrenceAt: AT('2026-09-04T09:00:00Z') }),
				row({ issueId: 'newest', latestOccurrenceAt: AT('2026-09-04T09:29:00Z') }),
				row({ issueId: 'middle', latestOccurrenceAt: AT('2026-09-04T09:15:00Z') }),
			],
			context()
		);
		expect(scored.map((candidate) => candidate.issueId)).toEqual(['newest', 'middle', 'older']);
	});

	it('breaks a recency tie on issues.id ASCENDING, so the order is stable across runs', () => {
		const at = AT('2026-09-04T09:10:00Z');
		const scored = scoreTriageCandidates(
			[
				row({ issueId: 'ccc', latestOccurrenceAt: at }),
				row({ issueId: 'aaa', latestOccurrenceAt: at }),
				row({ issueId: 'bbb', latestOccurrenceAt: at }),
			],
			context()
		);
		expect(scored.map((candidate) => candidate.issueId)).toEqual(['aaa', 'bbb', 'ccc']);

		// Same rows, different input order: the output order must not move.
		const reshuffled = scoreTriageCandidates(
			[
				row({ issueId: 'bbb', latestOccurrenceAt: at }),
				row({ issueId: 'ccc', latestOccurrenceAt: at }),
				row({ issueId: 'aaa', latestOccurrenceAt: at }),
			],
			context()
		);
		expect(reshuffled).toEqual(scored);
	});

	it('applies score BEFORE recency: an older release match outranks a newer window-only row', () => {
		const scored = scoreTriageCandidates(
			[
				row({ issueId: 'newer-window-only', latestOccurrenceAt: AT('2026-09-04T09:29:00Z') }),
				row({
					issueId: 'older-release-match',
					latestOccurrenceAt: AT('2026-09-04T08:41:00Z'),
					releaseVersions: ['v1'],
				}),
			],
			context({ release: 'v1' })
		);
		expect(scored.map((candidate) => candidate.issueId)).toEqual([
			'older-release-match',
			'newer-window-only',
		]);
	});
});

describe('scoreTriageCandidates — max_candidates = 5', () => {
	it('bounds the array at 5 and keeps the five highest-ranked', () => {
		const rows = Array.from({ length: 12 }, (_, index) =>
			row({
				issueId: `issue-${String(index).padStart(2, '0')}`,
				latestOccurrenceAt: new Date(AT('2026-09-04T09:00:00Z').getTime() + index * 60_000),
			})
		);
		const scored = scoreTriageCandidates(rows, context());
		expect(scored).toHaveLength(TRIAGE_MAX_CANDIDATES);
		// All twelve score 0.4, so ranking falls through to recency: the five newest.
		expect(scored.map((candidate) => candidate.issueId)).toEqual([
			'issue-11',
			'issue-10',
			'issue-09',
			'issue-08',
			'issue-07',
		]);
	});

	it('returns [] for no candidates — "looked, found nothing", never an error', () => {
		expect(scoreTriageCandidates([], context())).toEqual([]);
	});

	it('honours an explicit lower bound', () => {
		const rows = Array.from({ length: 6 }, (_, index) => row({ issueId: `issue-${index}` }));
		expect(scoreTriageCandidates(rows, context(), 2)).toHaveLength(2);
	});
});

describe('resolveReportMatchWindowMinutes', () => {
	// The same fail-safe shape rate-limit.ts's resolveReportUploadRpm uses: a misconfigured window
	// falls back to the contract default, never to "no window" -- which would turn §4.3's bounded
	// scan into every occurrence the project has ever recorded.
	it.each([
		[undefined, 30],
		['', 30],
		['   ', 30],
		['not-a-number', 30],
		['0', 30],
		['-5', 30],
		['15', 15],
		['90', 90],
		['45.9', 45],
	])('resolves %o to %i', (raw, expected) => {
		expect(resolveReportMatchWindowMinutes(raw as string | undefined)).toBe(expected);
	});
});

describe('triageWindow', () => {
	it('is ±windowMinutes around the anchor instant (§4.3)', () => {
		const { windowStart, windowEnd } = triageWindow(AT('2026-09-04T09:12:03Z'), 30);
		expect(windowStart.toISOString()).toBe('2026-09-04T08:42:03.000Z');
		expect(windowEnd.toISOString()).toBe('2026-09-04T09:42:03.000Z');
	});
});
