import { sql, type SQL } from 'drizzle-orm';
import { db } from '$lib/server/db';
import { personalAgentName } from '$lib/server/cli-personal-keys';
import { isUuid } from '$lib/uuid';
import {
	BURST_EVENT_TYPE,
	systemActorName,
	type ActivityActor,
	type ActivityActorType,
	type ActivityEntry,
	type ActivityPage,
} from '$lib/issue-activity';

/**
 * sentinel-issue-activity TASK-01 — one issue's activity history, newest first, paged, with `occurrence_burst` runs
 * collapsed. Callers have already applied the issue's access rule (the dashboard's `requireIssueAccess`, the agent API's
 * `withAgentIssue`); this module reads only rows of `issueId`.
 *
 * ORDER AND CURSOR. Rows are ordered `(created_at DESC, seq DESC)`: `created_at` is what the timeline means and what
 * `idx_issue_activity_issue_created (issue_id, created_at DESC)` serves, and `seq` (unique, assigned at insert) breaks the
 * ties of rows written in one transaction (they share `now()`) in the order they were written. The cursor is that pair
 * for the OLDEST row a page consumed, `base64url("<created_at, microseconds, UTC>|<seq>")`; the next page reads rows
 * strictly older than it. The timestamp travels as text with microseconds -- a JS Date would truncate to milliseconds
 * and silently skip rows written in the same millisecond. Keyset paging, so a page costs the same at any depth, and rows
 * written after the first page was read never shift a later page.
 *
 * COLLAPSE RULE. A run is a maximal sequence of `occurrence_burst` rows with no other row between them in this issue's
 * order above. A run is ONE entry and is NEVER split across pages: when a page's last entry is a burst run that may
 * continue, the rest of the run is aggregated (one statement, see `burstTail`) before the page is returned, and the
 * cursor moves past the whole run -- so the next page starts at the next non-burst row and no burst row is counted
 * twice or skipped. Consequently `limit` bounds ENTRIES, not rows, and a page's row cost is at most `limit + 1` rows
 * per chunk read plus one aggregate per run that reaches a chunk's end.
 *
 * Rows with a NULL `created_at` (the column has a default and no writer omits it) cannot be ordered by time and are not
 * listed.
 */

export interface IssueActivityPageOptions {
	issueId: string;
	/** The issue's organization: agent ids are resolved to names only within it. */
	organizationId: string;
	limit: number;
	/** A `nextCursor` from a previous page; omitted for the newest page. */
	before?: string | null;
}

export class InvalidActivityCursorError extends Error {
	constructor() {
		super('before must be a nextCursor from a previous page');
		this.name = 'InvalidActivityCursorError';
	}
}

interface Position {
	at: string; // created_at as 'YYYY-MM-DDTHH:MI:SS.USZ'
	seq: number;
}

const CURSOR_TS_RE = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z$/;

export function encodeActivityCursor(pos: Position): string {
	return Buffer.from(`${pos.at}|${pos.seq}`, 'utf8').toString('base64url');
}

export function decodeActivityCursor(raw: string): Position {
	let decoded: string;
	try {
		decoded = Buffer.from(raw, 'base64url').toString('utf8');
	} catch {
		throw new InvalidActivityCursorError();
	}
	const [at, seq, extra] = decoded.split('|');
	if (extra !== undefined || !at || !seq || !CURSOR_TS_RE.test(at) || !/^\d{1,18}$/.test(seq)) {
		throw new InvalidActivityCursorError();
	}
	if (Number.isNaN(Date.parse(at))) throw new InvalidActivityCursorError();
	return { at, seq: Number(seq) };
}

interface RawRow {
	id: string;
	seq: string | number;
	event_type: string;
	actor_type: string;
	actor_id: string;
	old_value: unknown;
	new_value: unknown;
	at: string;
}

const AT_TEXT = sql.raw(`to_char(ia.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')`);

/** `ia` strictly older than `pos` in (created_at DESC, seq DESC) order. The first conjunct is the index range. */
function olderThan(pos: Position | null): SQL {
	if (!pos) return sql`ia.created_at IS NOT NULL`;
	return sql`ia.created_at <= ${pos.at}::timestamptz AND (ia.created_at < ${pos.at}::timestamptz OR ia.seq < ${pos.seq})`;
}

/** The scan statement of a page: the next `n` rows older than `pos`. Exported for the EXPLAIN test. */
export function activityChunkQuery(issueId: string, pos: Position | null, n: number): SQL {
	return sql`
		SELECT ia.id, ia.seq, ia.event_type, ia.actor_type, ia.actor_id, ia.old_value, ia.new_value, ${AT_TEXT} AS at
		  FROM issue_activity ia
		 WHERE ia.issue_id = ${issueId} AND ${olderThan(pos)}
		 ORDER BY ia.created_at DESC, ia.seq DESC
		 LIMIT ${n}`;
}

/**
 * The rest of a burst run older than `pos`: every row between `pos` and the next older NON-burst row (all of them bursts
 * by construction), aggregated. Exported for the EXPLAIN test.
 */
export function burstTailQuery(issueId: string, pos: Position): SQL {
	return sql`
		WITH boundary AS (
			SELECT ia.created_at, ia.seq
			  FROM issue_activity ia
			 WHERE ia.issue_id = ${issueId} AND ${olderThan(pos)} AND ia.event_type <> ${BURST_EVENT_TYPE}
			 ORDER BY ia.created_at DESC, ia.seq DESC
			 LIMIT 1
		)
		SELECT count(*)::int AS n,
		       (array_agg(${AT_TEXT} ORDER BY ia.created_at ASC, ia.seq ASC))[1] AS oldest_at,
		       (array_agg(ia.seq ORDER BY ia.created_at ASC, ia.seq ASC))[1] AS oldest_seq,
		       (array_agg(${burstCount()} ORDER BY ia.created_at ASC, ia.seq ASC))[1] AS oldest_count
		  FROM issue_activity ia
		 WHERE ia.issue_id = ${issueId} AND ${olderThan(pos)}
		   AND (NOT EXISTS (SELECT 1 FROM boundary)
		        OR (ia.created_at, ia.seq) > (SELECT b.created_at, b.seq FROM boundary b))`;
}

/** A burst row's recorded issue count (`new_value.count`, written by the processor), or NULL. */
function burstCount(): SQL {
	return sql.raw(`CASE WHEN jsonb_typeof(ia.new_value -> 'count') = 'number' THEN (ia.new_value ->> 'count')::bigint END`);
}

function countOf(value: unknown): number | null {
	if (!value || typeof value !== 'object') return null;
	const c = (value as Record<string, unknown>).count;
	return typeof c === 'number' && Number.isFinite(c) ? c : null;
}

interface Building {
	entry: ActivityEntry;
	/** Only for a burst run: still open (the next row read may extend it). */
	open: boolean;
}

function toEntry(row: RawRow): ActivityEntry {
	const actorType = row.actor_type as ActivityActorType;
	return {
		id: row.id,
		seq: Number(row.seq),
		eventType: row.event_type,
		actorType,
		actorId: row.actor_id,
		actor: { type: actorType, id: row.actor_id, name: null, personal: false },
		oldValue: row.old_value ?? null,
		newValue: row.new_value ?? null,
		createdAt: row.at,
	};
}

/**
 * Reads one page. Statements: one chunk read per `limit + 1` rows consumed (normally one), one aggregate per burst run
 * that reaches a chunk's end, then at most three batched lookups for the page's actors/comments.
 */
export async function listIssueActivityPage(options: IssueActivityPageOptions): Promise<ActivityPage> {
	const { issueId, limit } = options;
	let pos: Position | null = options.before ? decodeActivityCursor(options.before) : null;
	const built: Building[] = [];
	let hasMore = false;
	let exhausted = false;

	// Each iteration either appends at least one entry, closes a run, or ends the walk; the bound is a belt-and-braces stop.
	for (let guard = 0; guard < limit + 3 && !exhausted && !hasMore; guard++) {
		const rows = (await db.execute(activityChunkQuery(issueId, pos, limit + 1))) as unknown as RawRow[];
		if (rows.length === 0) {
			exhausted = true;
			break;
		}
		for (const row of rows) {
			const last = built[built.length - 1];
			if (row.event_type === BURST_EVENT_TYPE && last?.open) {
				extendBurst(last.entry, row);
				pos = { at: row.at, seq: Number(row.seq) };
				continue;
			}
			if (last) last.open = false;
			if (built.length === limit) {
				// A row exists past a full page, and it starts a new entry: there is more.
				hasMore = true;
				break;
			}
			built.push(startEntry(row));
			pos = { at: row.at, seq: Number(row.seq) };
		}
		if (hasMore) break;
		if (rows.length < limit + 1) {
			exhausted = true;
			break;
		}
		// The chunk ended inside a burst run: aggregate the rest of the run so it is never split across pages.
		const last = built[built.length - 1];
		if (last?.open && pos) {
			const [tail] = (await db.execute(burstTailQuery(issueId, pos))) as unknown as {
				n: number;
				oldest_at: string | null;
				oldest_seq: string | number | null;
				oldest_count: string | number | null;
			}[];
			if (tail && tail.n > 0 && tail.oldest_at && tail.oldest_seq !== null) {
				const b = last.entry.burst!;
				b.rows += tail.n;
				b.firstAt = tail.oldest_at;
				if (tail.oldest_count !== null) b.occurrencesFrom = Number(tail.oldest_count);
				pos = { at: tail.oldest_at, seq: Number(tail.oldest_seq) };
			}
			last.open = false;
		}
	}

	const entries = built.map((b) => b.entry);
	await resolveNames(entries, issueId, options.organizationId);
	return { entries, nextCursor: hasMore && pos ? encodeActivityCursor(pos) : null, hasMore };
}

function startEntry(row: RawRow): Building {
	const entry = toEntry(row);
	if (row.event_type !== BURST_EVENT_TYPE) return { entry, open: false };
	const count = countOf(row.new_value);
	entry.burst = { rows: 1, firstAt: row.at, lastAt: row.at, occurrencesFrom: count, occurrencesTo: count };
	return { entry, open: true };
}

function extendBurst(entry: ActivityEntry, row: RawRow) {
	const b = entry.burst!;
	b.rows += 1;
	b.firstAt = row.at;
	const count = countOf(row.new_value);
	if (count !== null) b.occurrencesFrom = count;
}

// ---------------------------------------------------------------------------------------------------------------------
// Names. The same rules as the rest of the dashboard: a user is their name (else email), an org agent its name, a
// personal CLI agent "<owner name> (CLI)" from the owner's CURRENT name (personalAgentName, as the login flow stamps it),
// a system actor a fixed label. Agent ids resolve only within the issue's organization.
// ---------------------------------------------------------------------------------------------------------------------

async function resolveNames(entries: ActivityEntry[], issueId: string, organizationId: string): Promise<void> {
	const userIds = new Set<string>();
	const agentIds = new Set<string>();
	const commentIds = new Set<string>();
	for (const e of entries) {
		if (e.actorType === 'user') userIds.add(e.actorId);
		if (e.actorType === 'agent' && isUuid(e.actorId)) agentIds.add(e.actorId.toLowerCase());
		if ((e.eventType === 'commented' || e.eventType === 'question_asked') && isUuid(commentIdOf(e))) {
			commentIds.add(commentIdOf(e)!.toLowerCase());
		}
		if (e.eventType === 'assigned') {
			const a = assigneeOf(e);
			if (a?.type === 'user') userIds.add(a.id);
			if (a?.type === 'agent' && isUuid(a.id)) agentIds.add(a.id.toLowerCase());
		}
	}

	const [users, agents, comments] = await Promise.all([
		userIds.size
			? (db.execute(sql`SELECT id, name, email FROM "user" WHERE id IN (${sql.join([...userIds].map((id) => sql`${id}`), sql`, `)})`) as unknown as Promise<
					{ id: string; name: string | null; email: string }[]
				>)
			: Promise.resolve([]),
		agentIds.size
			? (db.execute(sql`
					SELECT a.id::text AS id, a.name, a.kind, o.name AS owner_name, o.email AS owner_email
					  FROM agents a LEFT JOIN "user" o ON o.id = a.owner_user_id
					 WHERE a.org_id = ${organizationId} AND a.id IN (${sql.join([...agentIds].map((id) => sql`${id}::uuid`), sql`, `)})`) as unknown as Promise<
					{ id: string; name: string; kind: string; owner_name: string | null; owner_email: string | null }[]
				>)
			: Promise.resolve([]),
		commentIds.size
			? (db.execute(sql`
					SELECT c.id::text AS id, left(c.body_md, 280) AS excerpt
					  FROM issue_comments c
					 WHERE c.issue_id = ${issueId} AND c.id IN (${sql.join([...commentIds].map((id) => sql`${id}::uuid`), sql`, `)})`) as unknown as Promise<
					{ id: string; excerpt: string }[]
				>)
			: Promise.resolve([]),
	]);

	const userName = new Map(users.map((u) => [u.id, (u.name ?? '').trim() || u.email]));
	const agentName = new Map(
		agents.map((a) => [
			a.id,
			a.kind === 'personal' && a.owner_email !== null
				? { name: personalAgentName({ name: a.owner_name, email: a.owner_email }), personal: true }
				: { name: a.name, personal: a.kind === 'personal' },
		])
	);
	const excerpt = new Map(comments.map((c) => [c.id, c.excerpt]));

	const resolve = (type: ActivityActorType, id: string): ActivityActor => {
		if (type === 'user') return { type, id, name: userName.get(id) ?? null, personal: false };
		if (type === 'agent') {
			const a = agentName.get(id.toLowerCase());
			return { type, id, name: a?.name ?? null, personal: a?.personal ?? false };
		}
		return { type: 'system', id, name: systemActorName(id), personal: false };
	};

	for (const e of entries) {
		e.actor = resolve(e.actorType, e.actorId);
		if (e.eventType === 'commented' || e.eventType === 'question_asked') {
			const id = commentIdOf(e);
			if (id) {
				const text = isUuid(id) ? excerpt.get(id.toLowerCase()) : undefined;
				e.comment = { id, excerpt: text ?? null, deleted: text === undefined };
			}
		}
		if (e.eventType === 'assigned') {
			const a = assigneeOf(e);
			if (a) e.assignee = resolve(a.type, a.id);
		}
	}
}

function commentIdOf(e: ActivityEntry): string | null {
	const v = e.newValue as Record<string, unknown> | null;
	return v && typeof v === 'object' && typeof v.commentId === 'string' ? v.commentId : null;
}

function assigneeOf(e: ActivityEntry): { type: ActivityActorType; id: string } | null {
	const v = e.newValue as Record<string, unknown> | null;
	if (!v || typeof v !== 'object' || typeof v.assignedTo !== 'string') return null;
	const type = v.assigneeType === 'agent' ? 'agent' : 'user';
	return { type, id: v.assignedTo };
}
