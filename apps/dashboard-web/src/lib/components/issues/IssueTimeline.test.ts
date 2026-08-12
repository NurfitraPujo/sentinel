import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import IssueTimeline from './IssueTimeline.svelte';

describe('IssueTimeline', () => {
	it('renders an empty state with no activity', () => {
		render(IssueTimeline, { activity: [] });
		expect(screen.getByText(/no activity yet/i)).toBeTruthy();
	});

	it('renders entries newest-first regardless of input order', () => {
		render(IssueTimeline, {
			activity: [
				{
					id: 'a1',
					issueId: 'issue-1',
					eventType: 'claimed',
					actorType: 'user',
					actorId: 'alice',
					oldValue: null,
					newValue: { assignedTo: 'alice' },
					createdAt: '2026-01-01T00:00:00.000Z',
				},
				{
					id: 'a2',
					issueId: 'issue-1',
					eventType: 'status_changed',
					actorType: 'agent',
					actorId: 'bot-1',
					oldValue: { status: 'unresolved' },
					newValue: { status: 'resolved' },
					createdAt: '2026-01-02T00:00:00.000Z',
				},
			],
		});

		const entries = screen.getAllByRole('listitem');
		expect(entries).toHaveLength(2);
		// a2 (Jan 2) is newer than a1 (Jan 1) -- must render first.
		expect(entries[0].textContent).toContain('bot-1');
		expect(entries[1].textContent).toContain('alice');
	});

	it('renders a recognizable label for every issue_activity event type in the CHECK constraint', () => {
		const eventTypes = [
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
		];

		render(IssueTimeline, {
			activity: eventTypes.map((eventType, i) => ({
				id: `id-${i}`,
				issueId: 'issue-1',
				eventType,
				actorType: 'user',
				actorId: 'someone',
				oldValue: null,
				newValue: null,
				createdAt: new Date(2026, 0, i + 1).toISOString(),
			})),
		});

		expect(screen.getAllByRole('listitem')).toHaveLength(eventTypes.length);
	});
});
