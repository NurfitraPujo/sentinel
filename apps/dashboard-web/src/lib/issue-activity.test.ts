import { describe, it, expect } from 'vitest';
import {
	activityDetail,
	activityDetailIsMarkdown,
	activityVerb,
	actorLabel,
	parseActivityLimit,
	timeAgo,
	type ActivityEntry,
} from './issue-activity';

// sentinel-issue-activity TASK-01: the words the Activity section renders for each entry type. Pure, so pinned here.

const base = (over: Partial<ActivityEntry>): ActivityEntry => ({
	id: 'a',
	seq: 1,
	eventType: 'commented',
	actorType: 'user',
	actorId: 'u-1',
	actor: { type: 'user', id: 'u-1', name: 'Grace Hopper', personal: false },
	oldValue: null,
	newValue: null,
	createdAt: '2026-10-01T12:00:00.000000Z',
	...over,
});

describe('activityDetail / activityVerb', () => {
	it('status: from → to, with the resolving version and snooze facts', () => {
		const e = base({ eventType: 'status_changed', oldValue: { status: 'unresolved' }, newValue: { status: 'resolved', resolvedInVersion: '1.4.0' } });
		expect(activityVerb(e)).toBe('changed status');
		expect(activityDetail(e)).toBe('unresolved → resolved in 1.4.0');
		const snooze = base({ eventType: 'status_changed', oldValue: { status: 'unresolved' }, newValue: { status: 'ignored', snoozeOccurrences: 100 } });
		expect(activityDetail(snooze)).toBe('unresolved → ignored snoozed for 100 occurrences');
		const woke = base({ eventType: 'status_changed', oldValue: { status: 'ignored' }, newValue: { status: 'unresolved', reason: 'snooze_occurrences_reached' } });
		expect(activityDetail(woke)).toBe('ignored → unresolved (snooze occurrences reached)');
	});

	it('regression: the release it came back in and the one it was resolved in', () => {
		expect(activityDetail(base({ eventType: 'regressed', newValue: { releaseVersion: '1.5.0', previousResolvedVersion: '1.4.0' } }))).toBe(
			'seen again in 1.5.0 (resolved in 1.4.0)'
		);
		expect(activityDetail(base({ eventType: 'regressed', newValue: {} }))).toBe('seen again after it was resolved');
	});

	it('comment: the excerpt, or that it was deleted', () => {
		expect(activityDetail(base({ comment: { id: 'c', excerpt: 'Looking into it', deleted: false } }))).toBe('Looking into it');
		expect(activityDetail(base({ comment: { id: 'c', excerpt: null, deleted: true } }))).toBe('(comment deleted)');
	});

	it('claims, releases, progress, assignment and links', () => {
		expect(activityVerb(base({ eventType: 'claimed' }))).toBe('claimed this issue');
		expect(activityDetail(base({ eventType: 'claim_released', newValue: { reason: 'stale' } }))).toBe('released automatically: the claim went stale');
		expect(activityDetail(base({ eventType: 'claim_released', newValue: { force: true } }))).toBe('force-released');
		expect(activityDetail(base({ eventType: 'progress_update', newValue: { messageMd: 'Bisected it' } }))).toBe('Bisected it');
		expect(
			activityDetail(
				base({ eventType: 'assigned', newValue: { reason: 'oncall_escalation' }, assignee: { type: 'user', id: 'u', name: 'Ada', personal: false } })
			)
		).toBe('to Ada (on-call escalation)');
		expect(activityVerb(base({ eventType: 'linked', newValue: { action: 'unlink' } }))).toBe('unlinked an issue');
		expect(activityDetail(base({ eventType: 'linked', newValue: { relationType: 'duplicate_of', targetIssueId: null, targetRedacted: true } }))).toBe(
			'duplicate of an issue you cannot see'
		);
	});

	it('burst: how many bursts and the occurrence count they spanned', () => {
		const burst = (rows: number, from: number | null, to: number | null) =>
			base({ eventType: 'occurrence_burst', burst: { rows, firstAt: 'x', lastAt: 'y', occurrencesFrom: from, occurrencesTo: to } });
		expect(activityVerb(burst(1, 1, 1))).toBe('saw new occurrences');
		expect(activityDetail(burst(170, 300, 1469))).toBe('170 bursts, 300 occurrences → 1,469');
		expect(activityDetail(burst(1, 42, 42))).toBe('1 burst, 42 occurrences in total');
		expect(activityDetail(burst(3, null, null))).toBe('3 bursts');
	});

	it('an unknown event type falls back to its words and no detail', () => {
		const e = base({ eventType: 'some_new_type', newValue: { x: 1 } });
		expect(activityVerb(e)).toBe('some new type');
		expect(activityDetail(e)).toBeNull();
	});
});

describe('actorLabel', () => {
	it('is the name, else what kind of actor is gone -- never the raw id', () => {
		expect(actorLabel({ type: 'agent', id: 'a-1', name: 'Ada Lovelace (CLI)', personal: true })).toBe('Ada Lovelace (CLI)');
		expect(actorLabel({ type: 'agent', id: 'a-1', name: null, personal: false })).toBe('Deleted agent');
		expect(actorLabel({ type: 'user', id: 'u-1', name: null, personal: false })).toBe('Deleted user');
	});
});

describe('parseActivityLimit', () => {
	it('defaults, clamps to [1, 200] and refuses non-integers', () => {
		expect(parseActivityLimit(null)).toBe(50);
		expect(parseActivityLimit('0')).toBe(1);
		expect(parseActivityLimit('500')).toBe(200);
		expect(parseActivityLimit('25')).toBe(25);
		for (const bad of ['-1', 'abc', '1.5', '']) expect(parseActivityLimit(bad)).toBeNull();
	});
});

describe('timeAgo', () => {
	it('matches the notification bell', () => {
		const now = Date.parse('2026-10-01T12:00:00Z');
		expect(timeAgo('2026-10-01T11:59:40Z', now)).toBe('just now');
		expect(timeAgo('2026-10-01T11:55:00Z', now)).toBe('5m ago');
		expect(timeAgo('2026-10-01T09:00:00Z', now)).toBe('3h ago');
		expect(timeAgo('2026-09-28T12:00:00Z', now)).toBe('3d ago');
	});
});

describe('activityDetailIsMarkdown', () => {
	it('is true for authored text (a live comment excerpt, a progress note) and false for our own words', () => {
		const live = { id: 'c', excerpt: '**hi**', deleted: false };
		expect(activityDetailIsMarkdown(base({ eventType: 'commented', comment: live }))).toBe(true);
		expect(activityDetailIsMarkdown(base({ eventType: 'question_asked', comment: live }))).toBe(true);
		expect(activityDetailIsMarkdown(base({ eventType: 'progress_update', newValue: { messageMd: 'x' } }))).toBe(true);
		expect(activityDetailIsMarkdown(base({ eventType: 'commented', comment: { id: 'c', excerpt: null, deleted: true } }))).toBe(false);
		expect(activityDetailIsMarkdown(base({ eventType: 'commented' }))).toBe(false);
		expect(activityDetailIsMarkdown(base({ eventType: 'status_changed', newValue: { status: 'resolved' } }))).toBe(false);
	});
});
