import { db, type Tx } from '$lib/server/db';
import { sql, and, eq, inArray } from 'drizzle-orm';
import { issueRelations, issueActivity } from '$lib/db/schema';
import { env } from '$env/dynamic/private';

/**
 * sentinel-user-report-issue contract §4.3 — triage, the v1 `window_release` matcher.
 *
 * §11 makes the matcher a NAMED TEST BOUNDARY below HTTP ("the rule *is* the feature"), so this
 * file is split three ways on purpose:
 *
 *   1. `scoreTriageCandidates` — **pure**. No database, no clock, no environment. Takes candidate
 *      rows and the report's context and returns the ranked, bounded candidate list. Every
 *      scoring, ranking and bounding case in the contract is a table row against this function.
 *   2. `findTriageCandidates` — **one bounded query**, scoped to one project and one time window.
 *   3. `runTriage` / `writeTriageLinks` — the write discipline, which is the part that is not
 *      tuning.
 *
 * What the contract fixes, restated here because a reader of this file must not have to go and
 * find it: the candidate set is `issues` rows with `issue_type = 'system_error'` in the REPORT'S
 * OWN PROJECT having at least one `error_occurrences` row inside the window — never across
 * projects, never across organizations. `trace_id` and `session_id` are declared signals and are
 * **inert in v1** (the design source captures neither), so they are weighted here and never fire.
 */

/** §4.2 `triage.strategy`. The only value in v1; named so a later strategy cannot silently
 *  reinterpret rows produced by this one. */
export const TRIAGE_STRATEGY = 'window_release';

/** §4.3: only a high-confidence match writes a durable row. */
export const TRIAGE_LINK_THRESHOLD = 0.7;

/** §4.3: a bound, so §4.2's `candidates` array is never a page. */
export const TRIAGE_MAX_CANDIDATES = 5;

/** §4.3's score table. `window` is the floor: it is true for every candidate by construction,
 *  which is exactly why `0.4 + 0.3` (release) is the only way to reach the `0.7` threshold in v1.
 *
 *  **`environment: 0.1` is GONE, and its absence is the point** — sentinel-project-environments
 *  decision 50. It has not been re-weighted or moved; environment has stopped being a SIGNAL and
 *  become a PRECONDITION. `findTriageCandidates` now filters on `i.environment` in its `WHERE`
 *  clause, so every row that reaches this scorer is already in the reported environment and a
 *  signal for it would fire on every candidate and rank nothing. Scoring a tautology at 0.1 would
 *  have quietly lifted every score by the same amount and moved the `0.7` threshold instead. */
export const TRIAGE_SIGNAL_WEIGHTS = {
	window: 0.4,
	release: 0.3,
	trace_id: 0.2,
	session_id: 0.1,
} as const;

export type TriageSignal = keyof typeof TRIAGE_SIGNAL_WEIGHTS;

/** §4.3: `context.occurred_at` (or server receipt time) ± this many minutes, org-independent. */
export const DEFAULT_REPORT_MATCH_WINDOW_MINUTES = 30;

/**
 * Resolves `REPORT_MATCH_WINDOW_MINUTES`. An unset, unparseable or non-positive value falls back
 * to the contract default rather than to "no window" — a misconfigured window must not become an
 * unbounded scan of every occurrence the project has ever recorded.
 */
export function resolveReportMatchWindowMinutes(raw: string | undefined): number {
	if (raw === undefined || raw.trim() === '') return DEFAULT_REPORT_MATCH_WINDOW_MINUTES;
	const parsed = Number(raw);
	if (!Number.isFinite(parsed) || parsed <= 0) return DEFAULT_REPORT_MATCH_WINDOW_MINUTES;
	return Math.floor(parsed);
}

/**
 * One candidate `system_error` issue, with everything the scorer needs and nothing else. The
 * release values arrive as the DISTINCT set observed on that issue's in-window occurrences — not as
 * a pre-computed boolean — so that "`context.release` is non-null AND equals an in-window
 * occurrence's `release_version`" is decided by the pure function the contract makes a boundary,
 * and is testable without a database.
 *
 * There is deliberately no `environments` here any more (decision 50): the query filters on the
 * issue's own `environment` column, so the set would have exactly one member and it would always be
 * the one the caller asked for.
 */
export interface TriageCandidateRow {
	issueId: string;
	message: string;
	errorClass: string;
	/** The most recent in-window occurrence. §4.3's first tie-break. */
	latestOccurrenceAt: Date;
	/** DISTINCT non-null `release_version` values on this issue's in-window occurrences. */
	releaseVersions: string[];
}

/** The report-side half of the match, read off `external_issue_reports.context`. */
export interface TriageMatchContext {
	release: string | null;
	/**
	 * **Not nullable, and that is decision 50/53 expressed in the type.** This is the RESOLVED
	 * environment — `resolveReportEnvironment(context.environment)` from
	 * `$lib/db/queries/reports` — the same value the report's `issues.environment` was written with,
	 * never the raw client assertion. Under a hard filter a `null` matches no row, so a nullable
	 * type here would let a caller hand this function the raw value and get an empty candidate list
	 * on the majority path with no error anywhere.
	 */
	environment: string;
	/** Carried for §4.2's `signals` vocabulary and **inert in v1** (§4.3). */
	traceId: string | null;
	/** Carried for §4.2's `signals` vocabulary and **inert in v1** (§4.3). */
	sessionId: string | null;
}

/** §4.2's `triage.candidates[]` entry, minus `linked` — which is not a property of the score but
 *  of whether a row was actually written, and is therefore decided by `runTriage`. */
export interface ScoredTriageCandidate {
	issueId: string;
	message: string;
	errorClass: string;
	score: number;
	signals: TriageSignal[];
}

export interface TriageCandidate extends ScoredTriageCandidate {
	/** §4.2: `true` when an `issue_relations` row was actually written (or already existed for
	 *  this pair), `false` when this is a suggestion only and a human must confirm it. */
	linked: boolean;
}

export interface TriageResult {
	ranAt: string;
	strategy: typeof TRIAGE_STRATEGY;
	candidates: TriageCandidate[];
}

/**
 * §4.3's score, deterministic and additive, clamped to [0.0, 1.0], then §4.3's ranking and bound.
 *
 * Pure: same inputs, same output, no I/O. `window` is added for every row because a row only
 * reaches this function by having an in-window occurrence — that is what makes `0.4` the floor
 * and what makes the threshold arithmetic mean "the report named a release and a `system_error`
 * issue occurred in that same release inside the window".
 *
 * Ties break on the most recent in-window occurrence, then on `issueId` ascending, so the order
 * is stable across runs (§4.3). Floating-point addition is rounded to three decimals before
 * comparison and before it leaves this function: `0.4 + 0.3` is `0.7000000000000001` in IEEE-754
 * and would otherwise be both an ugly `score` in the JSON and — worse — a value that compares
 * differently against `TRIAGE_LINK_THRESHOLD` than the contract's table says it does.
 */
export function scoreTriageCandidates(
	rows: TriageCandidateRow[],
	context: TriageMatchContext,
	maxCandidates: number = TRIAGE_MAX_CANDIDATES
): ScoredTriageCandidate[] {
	const scored = rows.map((row) => {
		const signals: TriageSignal[] = ['window'];
		let score = TRIAGE_SIGNAL_WEIGHTS.window;

		if (context.release !== null && row.releaseVersions.includes(context.release)) {
			signals.push('release');
			score += TRIAGE_SIGNAL_WEIGHTS.release;
		}
		// `trace_id` and `session_id` are deliberately NOT evaluated. They are weighted above so
		// that adding them later does not renumber the scale, but no v1 client can send one and no
		// v1 score depends on one (§4.3, Decisions taken OQ-4). A context that carries them scores
		// exactly as one that does not.

		return {
			row,
			candidate: {
				issueId: row.issueId,
				message: row.message,
				errorClass: row.errorClass,
				score: Math.round(Math.min(Math.max(score, 0), 1) * 1000) / 1000,
				signals,
			},
		};
	});

	scored.sort((a, b) => {
		if (b.candidate.score !== a.candidate.score) return b.candidate.score - a.candidate.score;
		const recency = b.row.latestOccurrenceAt.getTime() - a.row.latestOccurrenceAt.getTime();
		if (recency !== 0) return recency;
		return a.row.issueId < b.row.issueId ? -1 : a.row.issueId > b.row.issueId ? 1 : 0;
	});

	return scored.slice(0, Math.max(0, maxCandidates)).map((entry) => entry.candidate);
}

/**
 * Whatever runs the candidate query. `db` in production; a `db.transaction` callback's `tx` in the
 * timezone regression test, which is the only way to pin `SET LOCAL TIME ZONE` and the SELECT to
 * the SAME pooled session — the claim that the window bounds are session-independent is not
 * observable otherwise.
 */
export type TriageExecutor = Pick<typeof db, 'execute'> | Tx;

export interface FindTriageCandidatesOptions {
	projectId: string;
	windowStart: Date;
	windowEnd: Date;
	release: string | null;
	/** The RESOLVED environment. A hard predicate since decision 50 — see TriageMatchContext. */
	environment: string;
	limit?: number;
}

/**
 * §4.3's candidate query — ONE query, scoped to one project and one window, with an explicit
 * bound.
 *
 * **`AND i.environment = $environment` is a HARD filter, and it is the newest thing here**
 * (sentinel-project-environments decision 50). After decision 7's identity split one error across
 * N environments is N `issues` rows, and `LIMIT 5` is a hard SQL bound — so without this predicate
 * one error would consume N of the 5 candidate slots, and the release-over-environment ordering
 * would let a same-release row from the WRONG environment outrank a different-release row from the
 * right one. What it gives up, stated rather than discovered: there is now no cross-environment
 * auto-link at all. The value bound here must be the RESOLVED environment (§4.8a's
 * `resolveReportEnvironment`), never the raw client assertion — a `null` would match no row.
 *
 * **Why the LATERAL.** The bound has to be applied by the database (otherwise a busy project's
 * whole window crosses the wire), and the ORDER BY that makes a `LIMIT 5` correct has to agree
 * with the pure scorer's ranking. Ordering by `release_match DESC, latest DESC, id ASC` is exactly
 * ordering by score descending with §4.3's tie-breaks, because the only reachable scores in v1 are
 * 0.7 > 0.4 in precisely that lexicographic order (0.8 and 0.5 were the environment signal, which
 * decision 50 removed). The scorer re-derives the score and the order from the returned rows
 * regardless; the SQL ordering only decides WHICH rows are returned.
 *
 * The correlated `LATERAL` also fixes the plan: the driving side is `issues` filtered to one
 * project, and each iteration is an index scan of `idx_occurrences_issue_created`
 * (`error_occurrences (issue_id, created_at DESC)`) — the index §4.6 adds for exactly this. A
 * flat `JOIN ... GROUP BY` lets the planner pick the global `idx_error_occurrences_created_at`
 * instead and scan every project's occurrences in the window, which is both the wrong index and
 * a cross-tenant read the WHERE then throws away.
 */
export async function findTriageCandidates(
	options: FindTriageCandidatesOptions,
	executor: TriageExecutor = db
): Promise<TriageCandidateRow[]> {
	const limit = Math.max(1, options.limit ?? TRIAGE_MAX_CANDIDATES);

	// Bound as ISO strings and cast, never as `Date` objects: `db.execute` hands parameters to
	// postgres-js's `unsafe` path, which does not run its Date serializer and throws on one. The
	// same reason agent-work.ts's keyset cursor binds `.toISOString()`.
	//
	// The cast is `::timestamptz`, and that is load-bearing. `error_occurrences.created_at` is
	// `TIMESTAMP WITH TIME ZONE` (packages/db-migrations/migrations/1716508800_init.sql), so a
	// `::timestamp` cast would DROP the `Z` off the ISO literal and Postgres would then re-read the
	// naive value in the SESSION's `TimeZone` — silently sliding the window by the server's UTC
	// offset and returning zero candidates on any deployment whose `timezone` GUC is not UTC, with
	// no error and no log. `::timestamptz` parses the offset and is session-independent. The
	// `::timestamp` in `agent-work.ts`'s keyset cursor carries the same bug; do not copy it.
	const windowStartIso = options.windowStart.toISOString();
	const windowEndIso = options.windowEnd.toISOString();

	const result = await executor.execute(sql`
		SELECT
			i.id AS issue_id,
			i.message AS message,
			i.error_class AS error_class,
			occ.latest_occurrence_at AS latest_occurrence_at,
			occ.release_versions AS release_versions
		FROM issues i
		CROSS JOIN LATERAL (
			SELECT
				max(eo.created_at) AS latest_occurrence_at,
				array_remove(array_agg(DISTINCT eo.release_version), NULL) AS release_versions
			FROM error_occurrences eo
			WHERE eo.issue_id = i.id
			  AND eo.created_at >= ${windowStartIso}::timestamptz
			  AND eo.created_at <= ${windowEndIso}::timestamptz
		) occ
		WHERE i.project_id = ${options.projectId}
		  AND i.environment = ${options.environment}
		  AND i.issue_type = 'system_error'
		  AND occ.latest_occurrence_at IS NOT NULL
		ORDER BY
			coalesce(${options.release} = ANY(occ.release_versions), false) DESC,
			occ.latest_occurrence_at DESC,
			i.id ASC
		LIMIT ${limit}
	`);

	const rows = result as unknown as Record<string, unknown>[];

	return rows.map((row) => ({
		issueId: row.issue_id as string,
		message: row.message as string,
		errorClass: row.error_class as string,
		latestOccurrenceAt: new Date(row.latest_occurrence_at as string),
		releaseVersions: (row.release_versions as string[] | null) ?? [],
	}));
}

/**
 * §4.3's write discipline, and the only place in this feature that writes an `issue_relations`
 * row from a heuristic.
 *
 * - `relation_type = 'linked_to'`, **never** `duplicate_of`: that type carries a 2-cycle guard and
 *   is read as a strong human claim, and a heuristic must not mint one.
 * - `created_by_type = 'system'`.
 * - Exactly one `linked` `issue_activity` row per link, `actor_type = 'system'`, in the SAME
 *   transaction as the link — hence the explicit `tx` parameter rather than an ambient `db`.
 * - Idempotent per pair: a pair that already has a `linked_to` row is reported as `linked` and
 *   writes nothing, so re-running triage for a report never produces a second row.
 *
 * Returns the set of issue ids that are linked after this call (whether written now or already
 * present), so the caller can set §4.2's `linked` flag from what the database actually holds.
 */
export async function writeTriageLinks(
	tx: Tx,
	reportIssueId: string,
	targetIssueIds: string[]
): Promise<Set<string>> {
	const linked = new Set<string>();

	for (const targetIssueId of targetIssueIds) {
		const existing = await tx
			.select({ id: issueRelations.id })
			.from(issueRelations)
			.where(
				and(
					eq(issueRelations.sourceIssueId, reportIssueId),
					eq(issueRelations.targetIssueId, targetIssueId),
					eq(issueRelations.relationType, 'linked_to')
				)
			);

		if (existing.length > 0) {
			linked.add(targetIssueId);
			continue;
		}

		await tx.insert(issueRelations).values({
			sourceIssueId: reportIssueId,
			targetIssueId,
			relationType: 'linked_to',
			createdByType: 'system',
			createdBy: TRIAGE_STRATEGY,
		});

		await tx.insert(issueActivity).values({
			issueId: reportIssueId,
			eventType: 'linked',
			actorType: 'system',
			actorId: TRIAGE_STRATEGY,
			newValue: {
				relationType: 'linked_to',
				targetIssueId,
				strategy: TRIAGE_STRATEGY,
			},
		});

		linked.add(targetIssueId);
	}

	return linked;
}

export interface RunTriageInput {
	/** The `user_report` issue this triage is for. */
	issueId: string;
	/** The report's own project. The candidate set never leaves it. */
	projectId: string;
	/** `context.occurred_at`, or server receipt time when the client did not send one (§4.3). */
	occurredAt: Date;
	context: TriageMatchContext;
	/** Test seam only — the wall clock used for `ranAt`. */
	now?: Date;
	windowMinutes?: number;
}

/**
 * The whole step: window, one bounded query, the pure score, then the write discipline.
 *
 * Triage never changes `issues.status`, never sets `waiting_on` and never writes a comment
 * (§4.3) — the only rows it can produce are the `issue_relations` / `issue_activity` pair above.
 *
 * Throws on failure. Callers that must not fail because triage did (§4.1's intake, §4.2's read)
 * catch it and report `triage: null`; that decision belongs to them, not here, because "the
 * report is still created" is a property of the caller's transaction boundary and not of this
 * function.
 */
export async function runTriage(input: RunTriageInput): Promise<TriageResult> {
	const windowMinutes = input.windowMinutes ?? resolveReportMatchWindowMinutes(env.REPORT_MATCH_WINDOW_MINUTES);
	const windowMs = windowMinutes * 60 * 1000;

	const rows = await findTriageCandidates({
		projectId: input.projectId,
		windowStart: new Date(input.occurredAt.getTime() - windowMs),
		windowEnd: new Date(input.occurredAt.getTime() + windowMs),
		release: input.context.release,
		environment: input.context.environment,
		limit: TRIAGE_MAX_CANDIDATES,
	});

	const scored = scoreTriageCandidates(rows, input.context, TRIAGE_MAX_CANDIDATES);
	const linkable = scored.filter((candidate) => candidate.score >= TRIAGE_LINK_THRESHOLD);

	let linked = new Set<string>();
	if (linkable.length > 0) {
		linked = await db.transaction(async (tx) =>
			writeTriageLinks(
				tx,
				input.issueId,
				linkable.map((candidate) => candidate.issueId)
			)
		);
	}

	return {
		ranAt: (input.now ?? new Date()).toISOString(),
		strategy: TRIAGE_STRATEGY,
		candidates: scored.map((candidate) => ({ ...candidate, linked: linked.has(candidate.issueId) })),
	};
}

/**
 * The read-only half, for §4.2. Same window, same bounded query and the same pure scorer as
 * `runTriage`, but it **writes nothing**: `linked` is read back from the `issue_relations` rows
 * that already exist for the pair, so it reports what the database actually holds rather than
 * what the score would have written.
 *
 * A `GET` must not mutate. §4.3's link is written once, by the intake path that owns the report's
 * creation; this function is how the detail endpoint renders it afterwards, and re-rendering a
 * report a hundred times cannot produce a hundredth link or a second one.
 */
export async function readTriage(input: RunTriageInput): Promise<TriageResult> {
	const windowMinutes = input.windowMinutes ?? resolveReportMatchWindowMinutes(env.REPORT_MATCH_WINDOW_MINUTES);
	const windowMs = windowMinutes * 60 * 1000;

	const rows = await findTriageCandidates({
		projectId: input.projectId,
		windowStart: new Date(input.occurredAt.getTime() - windowMs),
		windowEnd: new Date(input.occurredAt.getTime() + windowMs),
		release: input.context.release,
		environment: input.context.environment,
		limit: TRIAGE_MAX_CANDIDATES,
	});

	const scored = scoreTriageCandidates(rows, input.context, TRIAGE_MAX_CANDIDATES);

	const linked = new Set<string>();
	if (scored.length > 0) {
		const existing = await db
			.select({ targetIssueId: issueRelations.targetIssueId })
			.from(issueRelations)
			.where(
				and(
					eq(issueRelations.sourceIssueId, input.issueId),
					eq(issueRelations.relationType, 'linked_to'),
					inArray(
						issueRelations.targetIssueId,
						scored.map((candidate) => candidate.issueId)
					)
				)
			);
		for (const row of existing) {
			linked.add(row.targetIssueId);
		}
	}

	return {
		ranAt: (input.now ?? new Date()).toISOString(),
		strategy: TRIAGE_STRATEGY,
		candidates: scored.map((candidate) => ({ ...candidate, linked: linked.has(candidate.issueId) })),
	};
}

/**
 * §4.3's window as `runTriage`/`readTriage` compute it, exported so a caller that needs to say
 * which window a result came from (a test, a status file, an EXPLAIN) does not re-derive it.
 */
export function triageWindow(occurredAt: Date, windowMinutes: number): { windowStart: Date; windowEnd: Date } {
	const windowMs = windowMinutes * 60 * 1000;
	return {
		windowStart: new Date(occurredAt.getTime() - windowMs),
		windowEnd: new Date(occurredAt.getTime() + windowMs),
	};
}
