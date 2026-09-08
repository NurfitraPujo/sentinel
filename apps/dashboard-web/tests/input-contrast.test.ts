import { describe, it, expect } from 'vitest';
import { readFileSync } from 'fs';
import path from 'path';

const APP_CSS = readFileSync(path.join(process.cwd(), 'src/app.css'), 'utf-8');

// Regression guard for the /onboarding bug shipped in v0.2.3-staging: the readable-input styling in
// app.css is keyed on ATTRIBUTE selectors (input[type="text"] and friends). An <input> written
// without a `type` attribute behaves as a text field but matches none of them, so it fell through to
// the generic `color: inherit` rule — near-white --text-primary text on the browser's default white
// input background. You could type, but not see what you typed.
describe('app.css input styling', () => {
	it('styles a typeless <input> alongside the explicit text-like types', () => {
		// Both the base rule and the :focus rule must cover it, or a focused field loses the
		// background again the moment it is clicked into.
		expect(APP_CSS).toContain('input:not([type]),');
		expect(APP_CSS).toContain('input:not([type]):focus,');
	});

	it('the rule that sets a readable background also sets a colour', () => {
		// The pairing is what matters: a background without a colour (or the reverse) is how the
		// original bug read on screen.
		const block = APP_CSS.slice(APP_CSS.indexOf('input:not([type]),'));
		const firstRule = block.slice(0, block.indexOf('}'));
		expect(firstRule).toContain('background-color: var(--bg-root)');
		expect(firstRule).toContain('color: var(--text-primary)');
	});
});
