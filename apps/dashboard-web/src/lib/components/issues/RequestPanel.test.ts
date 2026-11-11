import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import RequestPanel from './RequestPanel.svelte';
import { FLUTTER_DETAIL, LONG_DETAIL } from './occurrence-fixtures';

// sentinel-occurrence-detail-panels §6.2 (TASK-03).

const codePoints = (s: string | null | undefined) => Array.from(s ?? '').length;
const shown = (el: Element) => el.querySelector('.expandable-text')!.firstChild!.textContent!;

describe('RequestPanel', () => {
	it('R1: method, URL as text, query, sorted headers with the redacted value verbatim, and a pretty body', () => {
		const { container } = render(RequestPanel, { request: FLUTTER_DETAIL.request });
		const panel = screen.getByTestId('request-panel');
		expect(panel.querySelector('h4')!.textContent).toBe('Request');
		expect(panel.querySelector('.request-line .method')!.textContent).toBe('POST');
		expect(panel.querySelector('.request-line .request-url')!.textContent).toBe(
			'https://api.example.com/v2/orders/88412/confirm',
		);
		const query = panel.querySelector('dl.request-query')!;
		expect(query.querySelector('dt')!.textContent).toBe('Query');
		expect(query.querySelector('dd')!.textContent).toBe('retry=1&source=app');

		const table = screen.getByTestId('request-headers');
		expect(table.querySelector(':scope > tbody')).not.toBeNull();
		const headerRows = Array.from(table.querySelectorAll('tbody > tr')).map((tr) => [
			tr.querySelector('th')!.textContent,
			tr.querySelector('td')!.textContent,
		]);
		expect(headerRows).toEqual([
			['Accept', 'application/json'],
			['Authorization', '***REDACTED***'],
			['Content-Type', 'application/json'],
			['User-Agent', 'Example/2.14.0 (Android 14; Pixel 7)'],
			['x-request-id', 'r-7f3a9c'],
		]);
		expect(table.querySelector('th')!.getAttribute('scope')).toBe('row');

		expect(screen.getByTestId('request-body').querySelector('pre')!.textContent).toBe(
			JSON.stringify((FLUTTER_DETAIL.request as any).data, null, 2),
		);
		expect(container.querySelectorAll('a')).toHaveLength(0);
	});

	it('headers sort by lower-cased name, then by name; non-string values are skipped', () => {
		render(RequestPanel, { request: { headers: { b: '2', A: '1', a: '0', B: '3', n: 5 } } });
		const names = Array.from(screen.getByTestId('request-headers').querySelectorAll('th')).map((th) => th.textContent);
		expect(names).toEqual(['A', 'a', 'B', 'b']);
	});

	it.each([[null], [{}], [{ env: { A: 'b' } }], ['GET /'], [{ method: '', headers: {}, data: null }]])(
		'R2: %j renders nothing',
		(request) => {
			const { container } = render(RequestPanel, { request });
			expect(screen.queryByTestId('request-panel')).toBeNull();
			expect(container.querySelector('h4')).toBeNull();
		},
	);

	it('R3: a string body is verbatim; 0 and false are shown; null is not', () => {
		const body = (data: unknown) => {
			const { unmount } = render(RequestPanel, { request: { method: 'POST', data } });
			const el = screen.queryByTestId('request-body');
			const text = el ? el.querySelector('pre')!.textContent : null;
			unmount();
			return text;
		};
		expect(body('a=1&b="2"')).toBe('a=1&b="2"');
		expect(body(0)).toBe('0');
		expect(body(false)).toBe('false');
		expect(body(null)).toBeNull();
		expect(body(undefined)).toBeNull();
	});

	it('TR2: the long request collapses every field before any click', () => {
		render(RequestPanel, { request: LONG_DETAIL.request });
		const table = screen.getByTestId('request-headers');
		const td = table.querySelector('td')!;
		expect(shown(td)).toBe('h'.repeat(256) + '…');
		expect(td.querySelector('button')!.textContent).toBe('Show all (1024 characters)');
		const th = table.querySelector('th')!;
		expect(codePoints(shown(th))).toBe(257);
		expect(shown(th).endsWith('…')).toBe(true);

		const bodyPre = screen.getByTestId('request-body').querySelector('pre')!;
		expect(bodyPre.textContent).toBe('d'.repeat(2048) + '…');
		expect(screen.getByTestId('request-body').querySelector('button')!.textContent).toBe('Show all (65536 characters)');

		const panel = screen.getByTestId('request-panel');
		for (const el of [panel.querySelector('.request-url')!, panel.querySelector('.request-query dd')!]) {
			expect(codePoints(shown(el))).toBe(257);
		}
		expect(screen.getAllByTestId('expand-text')).toHaveLength(6);
	});
});
