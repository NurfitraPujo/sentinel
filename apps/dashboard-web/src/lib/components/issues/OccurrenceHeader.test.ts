import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import OccurrenceHeader from './OccurrenceHeader.svelte';
import { FLUTTER_OCCURRENCE, LONG_OCCURRENCE } from './occurrence-fixtures';

// sentinel-occurrence-detail-panels §5.1 (TASK-04): a v2 card's own message and facts.

const ISSUE_MESSAGE = 'Issue-level message';
const codePoints = (s: string | null | undefined) => Array.from(s ?? '').length;

function facts(container: HTMLElement) {
	const dl = container.querySelector('[data-testid="occurrence-facts"]')!;
	return Array.from(dl.querySelectorAll('dt')).map((dt) => [
		dt.textContent,
		(dt.nextElementSibling as HTMLElement).textContent,
	]);
}

describe('OccurrenceHeader', () => {
	it("H1: the occurrence's own message and its facts, in order", () => {
		const { container } = render(OccurrenceHeader, { occurrence: FLUTTER_OCCURRENCE, issueMessage: ISSUE_MESSAGE });
		const message = screen.getByTestId('occurrence-message');
		expect(message.tagName).toBe('DIV');
		expect(message.textContent).toBe(FLUTTER_OCCURRENCE.message);
		expect(message.textContent).not.toBe(ISSUE_MESSAGE);
		expect(facts(container)).toEqual([
			['Level', 'error'],
			['Release', '2.14.0+2140'],
			['Dist', '2140'],
			['Event ID', FLUTTER_OCCURRENCE.eventId],
			['Transaction', 'CheckoutScreen'],
			['SDK', 'sentinel.dart.flutter 0.3.0'],
		]);
	});

	it('H2: a v2 row with no message of its own shows the issue message', () => {
		render(OccurrenceHeader, { occurrence: { ...FLUTTER_OCCURRENCE, message: null }, issueMessage: ISSUE_MESSAGE });
		expect(screen.getByTestId('occurrence-message').textContent).toBe(ISSUE_MESSAGE);
	});

	it('H2: an empty message falls back too, and an SDK with only a name shows the name', () => {
		const { container } = render(OccurrenceHeader, {
			occurrence: { level: 'info', message: '', sdk: { name: 'sentinel.go', version: '' } },
			issueMessage: ISSUE_MESSAGE,
		});
		expect(screen.getByTestId('occurrence-message').textContent).toBe(ISSUE_MESSAGE);
		expect(facts(container)).toEqual([
			['Level', 'info'],
			['SDK', 'sentinel.go'],
		]);
	});

	it('an SDK that is not an object, or has no name or version, renders no SDK row', () => {
		for (const sdk of [null, 'sentinel.go', {}, { name: 3 }]) {
			const { container, unmount } = render(OccurrenceHeader, { occurrence: { level: 'error', sdk }, issueMessage: 'm' });
			expect(facts(container).map(([dt]) => dt)).toEqual(['Level']);
			unmount();
		}
	});

	it('H3: the level badge tone', () => {
		const cases: [string, string][] = [
			['error', 'tone-critical'],
			['fatal', 'tone-critical'],
			['warning', 'tone-warning'],
			['info', 'tone-info'],
			['debug', 'tone-debug'],
			['bogus', 'tone-neutral'],
			['constructor', 'tone-neutral'],
		];
		for (const [level, tone] of cases) {
			const { unmount } = render(OccurrenceHeader, { occurrence: { level }, issueMessage: 'm' });
			const badge = screen.getByTestId('level-badge');
			expect(badge.classList.contains(tone), `${level} -> ${tone}`).toBe(true);
			expect(badge.textContent).toBe(level);
			unmount();
		}
	});

	it('TR7: a long message shows 2048 code points; each fact shows 256', () => {
		const { container } = render(OccurrenceHeader, { occurrence: LONG_OCCURRENCE, issueMessage: 'm' });
		const pre = screen.getByTestId('occurrence-message').querySelector('pre')!;
		expect(codePoints(pre.textContent)).toBe(2049);
		expect(pre.textContent!.endsWith('…')).toBe(true);
		const dds = Array.from(container.querySelectorAll('[data-testid="occurrence-facts"] dd'));
		expect(Array.from(container.querySelectorAll('[data-testid="occurrence-facts"] dt')).map((d) => d.textContent)).toEqual([
			'Level',
			'Release',
			'Dist',
			'Event ID',
			'Server',
			'Transaction',
			'SDK',
		]);
		for (const dd of dds) {
			const text = dd.querySelector('.expandable-text')!.firstChild!.textContent!;
			expect(codePoints(text)).toBe(257);
			expect(text.endsWith('…')).toBe(true);
		}
		expect(screen.getAllByTestId('expand-text')).toHaveLength(8);
	});
});
