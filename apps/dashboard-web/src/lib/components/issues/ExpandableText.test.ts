import { describe, it, expect } from 'vitest';
import { render, fireEvent, screen } from '@testing-library/svelte';
import ExpandableText from './ExpandableText.svelte';
import { INLINE_LIMIT, BLOCK_LIMIT } from './occurrence-display';

// U3 (sentinel-occurrence-detail-panels §4.3): the one truncation component. Length is counted in
// code points; over the limit it shows the first `limit` code points, `…` and a button.

const codePoints = (s: string | null | undefined) => Array.from(s ?? '').length;

describe('ExpandableText (U3)', () => {
	it('renders a short text as the bare text, with no button', () => {
		const { container } = render(ExpandableText, { text: 'short', limit: INLINE_LIMIT });
		expect(container.querySelector('span.expandable-text')?.textContent).toBe('short');
		expect(screen.queryByTestId('expand-text')).toBeNull();
	});

	it('a 1 KiB header value shows 256 code points plus …, then all of it after the click', async () => {
		const value = 'h'.repeat(1024);
		const { container } = render(ExpandableText, { text: value, limit: INLINE_LIMIT });
		const span = container.querySelector('span.expandable-text')!;
		const button = screen.getByTestId('expand-text');
		expect(span.firstChild?.textContent).toBe('h'.repeat(256) + '…');
		expect(button.textContent).toBe('Show all (1024 characters)');
		expect(button.getAttribute('aria-expanded')).toBe('false');

		await fireEvent.click(button);
		expect(span.firstChild?.textContent).toBe(value);
		expect(button.textContent).toBe('Show less');
		expect(button.getAttribute('aria-expanded')).toBe('true');

		await fireEvent.click(button);
		expect(span.firstChild?.textContent).toBe('h'.repeat(256) + '…');
	});

	it('the 64 KiB body, as a block, shows 2048 plus … in a <pre>, then all 65 536', async () => {
		const body = 'd'.repeat(65536);
		const { container } = render(ExpandableText, { text: body, limit: BLOCK_LIMIT, block: true });
		const pre = container.querySelector('div.expandable-text.block > pre')!;
		expect(pre.textContent).toBe('d'.repeat(2048) + '…');
		const button = screen.getByTestId('expand-text');
		expect(button.parentElement?.matches('div.expandable-text.block')).toBe(true);
		expect(button.textContent).toBe('Show all (65536 characters)');
		await fireEvent.click(button);
		expect(codePoints(pre.textContent)).toBe(65536);
	});

	it('300 emoji truncate at 256 code points without a lone surrogate', () => {
		const emoji = '😀'.repeat(300);
		const { container } = render(ExpandableText, { text: emoji, limit: INLINE_LIMIT });
		const shown = container.querySelector('span.expandable-text')!.firstChild!.textContent!;
		expect(shown).toBe('😀'.repeat(256) + '…');
		expect(/[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/.test(shown)).toBe(false);
		expect(screen.getByTestId('expand-text').textContent).toBe('Show all (300 characters)');
	});

	it('a block under its limit is a <pre> with no button', () => {
		const { container } = render(ExpandableText, { text: 'a\n  b', limit: BLOCK_LIMIT, block: true });
		expect(container.querySelector('div.expandable-text.block > pre')?.textContent).toBe('a\n  b');
		expect(screen.queryByTestId('expand-text')).toBeNull();
	});

	it('renders markup as text', () => {
		const { container } = render(ExpandableText, { text: '<b>x</b>', limit: INLINE_LIMIT });
		expect(container.querySelector('b')).toBeNull();
		expect(container.textContent).toBe('<b>x</b>');
	});
});
