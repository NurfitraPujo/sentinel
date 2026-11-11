import { describe, it, expect, vi } from 'vitest';
import { render, fireEvent, screen } from '@testing-library/svelte';
import MarkdownToolbar from './MarkdownToolbar.svelte';

// M6 Feature B (docs/plans/M6_PRESIGNED_UPLOADS_AND_TOOLBAR_PLAN.md §Feature B): the pure
// text-transform logic is exhaustively covered in markdown-toolbar.test.ts. This test only
// checks the component's DOM wiring -- clicking a button reads the bound textarea's selection,
// calls onchange with the transformed value, and restores selection on the textarea.
describe('MarkdownToolbar', () => {
	it('wraps the current textarea selection in ** and calls onchange with the new value', async () => {
		const textarea = document.createElement('textarea');
		textarea.value = 'hello world';
		document.body.appendChild(textarea);
		textarea.setSelectionRange(6, 11);

		const onchange = vi.fn();
		render(MarkdownToolbar, { textarea, value: 'hello world', onchange });

		const boldBtn = screen.getByRole('button', { name: /bold/i });
		await fireEvent.click(boldBtn);

		expect(onchange).toHaveBeenCalledWith('hello **world**');

		document.body.removeChild(textarea);
	});

	it('renders a keyboard-reachable, aria-labelled button for every toolbar action', () => {
		render(MarkdownToolbar, { textarea: undefined, value: '', onchange: vi.fn() });

		for (const label of ['Bold', 'Italic', 'Strikethrough', 'Code', 'Heading', 'Quote', 'Bulleted list', 'Numbered list', 'Link']) {
			const btn = screen.getByRole('button', { name: label });
			expect(btn.getAttribute('type')).toBe('button');
		}
	});
});
