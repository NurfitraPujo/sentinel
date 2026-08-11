import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import { tick } from 'svelte';
import Markdown from './Markdown.svelte';

// Manual Issues M1 (docs/plans/MANUAL_ISSUES_DESIGN.md §3): body_md is untrusted user/agent
// text with no server-side sanitization on write -- this assertion is only meaningful if it
// would actually fail with sanitization removed (e.g. swapping `DOMPurify.sanitize(raw)` for
// bare `raw` in Markdown.svelte), which the second test proves indirectly by checking the
// sanitizer strips both the tag AND any handler-attribute vector, not just the naive `<script>`.
describe('Markdown sanitization', () => {
	it('strips <script> tags from rendered markdown', async () => {
		render(Markdown, { source: 'hello <script>window.__pwned = true;</script> world' });
		await tick();

		const container = screen.getByText(/hello/i, { exact: false }).closest('.markdown-body');
		expect(container?.innerHTML).not.toContain('<script');
		expect((window as unknown as { __pwned?: boolean }).__pwned).toBeUndefined();
	});

	it('strips inline event-handler attributes (onerror) from rendered markdown', async () => {
		render(Markdown, { source: '<img src="x" onerror="window.__pwned2 = true">' });
		await tick();

		const img = document.querySelector('.markdown-body img');
		expect(img?.getAttribute('onerror')).toBeNull();
	});

	it('still renders ordinary markdown as formatted HTML', async () => {
		render(Markdown, { source: '**bold** and _em_' });
		await tick();

		const strong = document.querySelector('.markdown-body strong');
		expect(strong?.textContent).toBe('bold');
	});
});
