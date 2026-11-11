/**
 * sentinel-issue-activity TASK-01 — the wire shape of an issue's activity history, shared by the dashboard's Activity
 * section (`GET /api/issues/{issueId}/activity`), the agent API (`GET /api/agent/issues/{issueId}/activity`) and the
 * component that renders it. Built by `$lib/db/queries/issue-activity.ts`; not under `$lib/server/` because the page
 * renders it.
 *
 * An entry is one `issue_activity` row, EXCEPT `occurrence_burst`: a run of consecutive burst rows (consecutive in the
 * issue's own timeline, i.e. no other event between them) is ONE entry carrying `burst`. The entry's own fields are those
 * of the run's NEWEST row, so an entry has the same item shape as the events feed's (`seq`, `eventType`, `actorType`,
 * `actorId`, `oldValue`, `newValue`, `createdAt`) whatever its type.
 */

export type ActivityActorType = 'user' | 'agent' | 'system';

export interface ActivityActor {
	type: ActivityActorType;
	id: string;
	/**
	 * What to show for the actor: a user's name (else email); an org agent's name; a personal CLI agent as
	 * "<owner name> (CLI)"; a fixed label for a system actor. Null only when the user/agent row no longer exists.
	 */
	name: string | null;
	/** True for a personal CLI agent (an `agents` row of kind 'personal', acting for `name`'s owner). */
	personal: boolean;
}

export interface ActivityBurst {
	/** How many `occurrence_burst` rows the run collapses (each is "the issue was seen again", throttled per issue). */
	rows: number;
	/** The run's oldest and newest row times (ISO). */
	firstAt: string;
	lastAt: string;
	/** The issue's total occurrence count as recorded by the run's oldest and newest rows (null when not recorded). */
	occurrencesFrom: number | null;
	occurrencesTo: number | null;
}

export interface ActivityComment {
	id: string;
	/** The comment body's first characters, or null when the comment has since been deleted. */
	excerpt: string | null;
	deleted: boolean;
}

export interface ActivityEntry {
	/** The row id (for a burst run: its newest row's). */
	id: string;
	seq: number;
	eventType: string;
	actorType: ActivityActorType;
	actorId: string;
	actor: ActivityActor;
	oldValue: unknown;
	newValue: unknown;
	/** ISO time of the row (for a burst run: its newest row). */
	createdAt: string;
	/** `commented` / `question_asked` only: the comment it posted. */
	comment?: ActivityComment;
	/** `assigned` only: who the issue was assigned to (newValue.assignedTo resolved like an actor). */
	assignee?: ActivityActor;
	/** `occurrence_burst` only: the collapsed run. */
	burst?: ActivityBurst;
}

export interface ActivityPage {
	entries: ActivityEntry[];
	/** Pass back as `before` for the next (older) page; null when this page reached the start of the history. */
	nextCursor: string | null;
	hasMore: boolean;
}

export const ACTIVITY_DEFAULT_LIMIT = 50;
export const ACTIVITY_MIN_LIMIT = 1;
export const ACTIVITY_MAX_LIMIT = 200;

export const BURST_EVENT_TYPE = 'occurrence_burst';

/** Display names of the fixed `system` actor ids the writers use (processor, retention, escalation, triage). */
export const SYSTEM_ACTOR_NAMES: Record<string, string> = {
	'sentinel-processor': 'Sentinel',
	'sentinel-regression-detector': 'Regression detector',
	'sentinel-snooze': 'Snooze',
	'sentinel-retention': 'Retention',
	system: 'System',
};

export function systemActorName(actorId: string): string {
	return SYSTEM_ACTOR_NAMES[actorId] ?? 'System';
}

/**
 * Parses the `limit` query parameter: absent → the default; otherwise a positive integer, clamped to
 * [ACTIVITY_MIN_LIMIT, ACTIVITY_MAX_LIMIT]. Returns null for anything else (the route answers 400).
 */
export function parseActivityLimit(raw: string | null): number | null {
	if (raw === null) return ACTIVITY_DEFAULT_LIMIT;
	if (!/^\d+$/.test(raw)) return null;
	return Math.min(ACTIVITY_MAX_LIMIT, Math.max(ACTIVITY_MIN_LIMIT, Number(raw)));
}

// ---------------------------------------------------------------------------------------------------------------------
// Rendering: one sentence per entry. Pure, so the component, its tests and the CLI's docs agree on the words.
// ---------------------------------------------------------------------------------------------------------------------

const VERBS: Record<string, string> = {
	created: 'first saw this issue',
	occurrence_burst: 'saw new occurrences',
	status_changed: 'changed status',
	assigned: 'assigned this issue',
	unassigned: 'unassigned this issue',
	regressed: 'flagged a regression',
	ai_analysis: 'ran AI analysis',
	linked: 'linked an issue',
	commented: 'commented',
	claimed: 'claimed this issue',
	claim_released: 'released the claim',
	progress_update: 'posted a progress update',
	question_asked: 'asked a question',
	question_answered: 'answered a question',
	moved: 'moved this issue',
	attachment_added: 'added an attachment',
	report_created: 'created this report',
	report_edited: 'edited the report',
};

export function activityVerb(entry: Pick<ActivityEntry, 'eventType' | 'newValue'>): string {
	if (entry.eventType === 'linked' && field(entry.newValue, 'action') === 'unlink') return 'unlinked an issue';
	return VERBS[entry.eventType] ?? entry.eventType.replace(/_/g, ' ');
}

function field(value: unknown, key: string): unknown {
	return value && typeof value === 'object' && !Array.isArray(value) ? (value as Record<string, unknown>)[key] : undefined;
}

function str(value: unknown): string | null {
	return typeof value === 'string' && value.length > 0 ? value : null;
}

const plural = (n: number, one: string, many = `${one}s`) => `${n.toLocaleString('en-US')} ${n === 1 ? one : many}`;

/**
 * The second line of an entry: what changed, in words. Null when there is nothing beyond the verb. Free-form `old/new`
 * payloads of an unknown type are not rendered (the JSON stays available to API clients).
 */
export function activityDetail(entry: ActivityEntry): string | null {
	const oldV = entry.oldValue;
	const newV = entry.newValue;
	switch (entry.eventType) {
		case 'status_changed': {
			const from = str(field(oldV, 'status'));
			const to = str(field(newV, 'status'));
			const parts: string[] = [];
			if (to) parts.push(from && from !== to ? `${from} → ${to}` : to);
			const version = str(field(newV, 'resolvedInVersion'));
			if (to === 'resolved' && version) parts.push(`in ${version}`);
			const snoozedUntil = str(field(newV, 'snoozedUntil'));
			if (snoozedUntil) parts.push(`snoozed until ${new Date(snoozedUntil).toLocaleString('en-US')}`);
			const snoozeOcc = field(newV, 'snoozeOccurrences');
			if (typeof snoozeOcc === 'number') parts.push(`snoozed for ${plural(snoozeOcc, 'occurrence')}`);
			const reason = str(field(newV, 'reason'));
			if (reason) parts.push(`(${reason.replace(/_/g, ' ')})`);
			return parts.length ? parts.join(' ') : null;
		}
		case 'regressed': {
			const release = str(field(newV, 'releaseVersion'));
			const previous = str(field(newV, 'previousResolvedVersion')) ?? str(field(oldV, 'resolvedInVersion'));
			if (release && previous) return `seen again in ${release} (resolved in ${previous})`;
			if (release) return `seen again in ${release}`;
			return 'seen again after it was resolved';
		}
		case 'commented':
		case 'question_asked': {
			if (!entry.comment) return null;
			return entry.comment.deleted ? '(comment deleted)' : entry.comment.excerpt;
		}
		case 'question_answered':
			return null;
		case 'progress_update':
			return str(field(newV, 'messageMd'));
		case 'claimed':
			return null;
		case 'claim_released': {
			if (field(newV, 'reason') === 'stale') return 'released automatically: the claim went stale';
			if (field(newV, 'force') === true) return 'force-released';
			return null;
		}
		case 'assigned': {
			const who = entry.assignee?.name ?? null;
			const reason = str(field(newV, 'reason'));
			if (who && reason === 'oncall_escalation') return `to ${who} (on-call escalation)`;
			return who ? `to ${who}` : null;
		}
		case 'linked': {
			const type = str(field(newV, 'relationType'));
			if (field(newV, 'targetRedacted') === true) return type ? `${type.replace(/_/g, ' ')} an issue you cannot see` : null;
			return type ? type.replace(/_/g, ' ') : null;
		}
		case 'occurrence_burst': {
			const b = entry.burst;
			if (!b) return null;
			const range =
				b.occurrencesFrom !== null && b.occurrencesTo !== null && b.occurrencesTo > b.occurrencesFrom
					? `, ${plural(b.occurrencesFrom, 'occurrence')} → ${b.occurrencesTo.toLocaleString('en-US')}`
					: b.occurrencesTo !== null
						? `, ${plural(b.occurrencesTo, 'occurrence')} in total`
						: '';
			return `${plural(b.rows, 'burst')}${range}`;
		}
		case 'created': {
			const errorClass = str(field(newV, 'errorClass'));
			return errorClass;
		}
		case 'moved':
			return null;
		default:
			return null;
	}
}

/**
 * Whether `activityDetail(entry)` is user/agent-authored Markdown (a comment excerpt or a progress note) that the
 * Activity section renders through the shared sanitizing `Markdown` component, rather than plain words of our own.
 * A deleted comment's "(comment deleted)" is ours, so it stays plain text.
 */
export function activityDetailIsMarkdown(entry: ActivityEntry): boolean {
	switch (entry.eventType) {
		case 'commented':
		case 'question_asked':
			return !!entry.comment && !entry.comment.deleted && entry.comment.excerpt !== null;
		case 'progress_update':
			return true;
		default:
			return false;
	}
}

/** "just now" / "5m ago" / "3h ago" / "2d ago" -- the notification bell's format; the absolute time goes in a tooltip. */
export function timeAgo(iso: string, now: number = Date.now()): string {
	const t = Date.parse(iso);
	if (Number.isNaN(t)) return '';
	const min = Math.floor((now - t) / 60_000);
	if (min < 1) return 'just now';
	if (min < 60) return `${min}m ago`;
	const hr = Math.floor(min / 60);
	if (hr < 24) return `${hr}h ago`;
	return `${Math.floor(hr / 24)}d ago`;
}

/** The absolute time for a tooltip, in the issue page's `formatDate` style. */
export function absoluteTime(iso: string): string {
	const d = new Date(iso);
	if (Number.isNaN(d.getTime())) return '';
	return d.toLocaleString('en-US', {
		year: 'numeric',
		month: 'short',
		day: 'numeric',
		hour: '2-digit',
		minute: '2-digit',
		second: '2-digit',
	});
}

/** What to call an entry's actor. Never a raw id when a name exists; a missing user/agent says what it was. */
export function actorLabel(actor: ActivityActor): string {
	if (actor.name) return actor.name;
	if (actor.type === 'agent') return 'Deleted agent';
	if (actor.type === 'user') return 'Deleted user';
	return 'System';
}
