import { describe, it, expect } from 'vitest';
import { mergeNewComments, latestCommentTimestamp, countComments, type CommentNode } from './comment-poll';

function makeRoot(id: string, createdAt: string, replies: CommentNode[] = []): CommentNode {
	return {
		id,
		issueId: 'issue-1',
		parentId: null,
		authorType: 'user',
		authorId: 'alice',
		authorName: 'Alice',
		authorEmail: 'alice@example.com',
		blocking: false,
		bodyMd: `body-${id}`,
		createdAt,
		editedAt: null,
		attachments: [],
		replies,
	};
}

function makeReply(id: string, createdAt: string): CommentNode {
	return {
		id,
		issueId: 'issue-1',
		parentId: 'r1',
		authorType: 'agent',
		authorId: 'agent-1',
		authorName: 'Bot',
		authorEmail: null,
		blocking: false,
		bodyMd: `reply-${id}`,
		createdAt,
		editedAt: null,
		attachments: [],
		replies: [],
	};
}

describe('mergeNewComments', () => {
	it('appends genuinely new roots in order', () => {
		const existing = [makeRoot('r1', '2026-01-01T00:00:00Z')];
		const incoming = [makeRoot('r2', '2026-01-02T00:00:00Z')];
		const merged = mergeNewComments(existing, incoming);
		expect(merged.map((r) => r.id)).toEqual(['r1', 'r2']);
	});

	it('replaces an existing root wholesale to pick up new replies, preserving position', () => {
		const existing = [makeRoot('r1', '2026-01-01T00:00:00Z'), makeRoot('r2', '2026-01-02T00:00:00Z')];
		const updatedR1 = makeRoot('r1', '2026-01-01T00:00:00Z', [makeReply('reply-a', '2026-01-03T00:00:00Z')]);
		const merged = mergeNewComments(existing, [updatedR1]);
		expect(merged.map((r) => r.id)).toEqual(['r1', 'r2']);
		expect(merged[0].replies).toHaveLength(1);
		expect(merged[0].replies[0].id).toBe('reply-a');
	});

	it('returns the existing array unchanged when there is nothing new', () => {
		const existing = [makeRoot('r1', '2026-01-01T00:00:00Z')];
		expect(mergeNewComments(existing, [])).toBe(existing);
	});
});

describe('latestCommentTimestamp', () => {
	it('returns null for an empty thread', () => {
		expect(latestCommentTimestamp([])).toBeNull();
	});

	it('finds the latest timestamp across roots and replies', () => {
		const roots = [
			makeRoot('r1', '2026-01-01T00:00:00Z', [makeReply('a', '2026-01-05T00:00:00Z')]),
			makeRoot('r2', '2026-01-03T00:00:00Z'),
		];
		expect(latestCommentTimestamp(roots)).toBe('2026-01-05T00:00:00Z');
	});
});

describe('countComments', () => {
	it('counts roots plus replies', () => {
		const roots = [
			makeRoot('r1', '2026-01-01T00:00:00Z', [makeReply('a', '2026-01-02T00:00:00Z'), makeReply('b', '2026-01-03T00:00:00Z')]),
			makeRoot('r2', '2026-01-04T00:00:00Z'),
		];
		expect(countComments(roots)).toBe(4);
	});
});
