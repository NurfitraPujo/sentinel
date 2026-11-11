import { describe, it, expect } from 'vitest';
import { applyMarkdownAction } from './markdown-toolbar';

describe('applyMarkdownAction', () => {
	describe('wrap actions with a selection', () => {
		it('wraps a selection in ** for bold', () => {
			const result = applyMarkdownAction('hello world', 6, 11, 'bold');
			expect(result.text).toBe('hello **world**');
			expect(result.text.slice(result.selStart, result.selEnd)).toBe('world');
		});

		it('wraps a selection in _ for italic', () => {
			const result = applyMarkdownAction('hello world', 6, 11, 'italic');
			expect(result.text).toBe('hello _world_');
			expect(result.text.slice(result.selStart, result.selEnd)).toBe('world');
		});

		it('wraps a selection in ` for code', () => {
			const result = applyMarkdownAction('hello world', 6, 11, 'code');
			expect(result.text).toBe('hello `world`');
			expect(result.text.slice(result.selStart, result.selEnd)).toBe('world');
		});

		it('wraps a selection in ~~ for strikethrough', () => {
			const result = applyMarkdownAction('hello world', 6, 11, 'strikethrough');
			expect(result.text).toBe('hello ~~world~~');
			expect(result.text.slice(result.selStart, result.selEnd)).toBe('world');
		});

		it('wraps a selection in the middle of text, preserving surrounding text', () => {
			const result = applyMarkdownAction('a bold word here', 2, 6, 'bold');
			expect(result.text).toBe('a **bold** word here');
		});
	});

	describe('wrap actions with an empty selection', () => {
		it('inserts a selected placeholder for bold', () => {
			const result = applyMarkdownAction('hello ', 6, 6, 'bold');
			expect(result.text).toBe('hello **text**');
			expect(result.selStart).toBe(8);
			expect(result.selEnd).toBe(12);
			expect(result.text.slice(result.selStart, result.selEnd)).toBe('text');
		});

		it('inserts a selected placeholder for italic', () => {
			const result = applyMarkdownAction('', 0, 0, 'italic');
			expect(result.text).toBe('_text_');
			expect(result.text.slice(result.selStart, result.selEnd)).toBe('text');
		});

		it('inserts a selected placeholder for code', () => {
			const result = applyMarkdownAction('', 0, 0, 'code');
			expect(result.text).toBe('`text`');
			expect(result.text.slice(result.selStart, result.selEnd)).toBe('text');
		});

		it('inserts a selected placeholder for strikethrough', () => {
			const result = applyMarkdownAction('', 0, 0, 'strikethrough');
			expect(result.text).toBe('~~text~~');
			expect(result.text.slice(result.selStart, result.selEnd)).toBe('text');
		});
	});

	describe('per-line prefix actions', () => {
		it('applies a heading prefix to a single line', () => {
			const result = applyMarkdownAction('hello', 0, 5, 'heading');
			expect(result.text).toBe('## hello');
		});

		it('applies a quote prefix across multiple selected lines', () => {
			const text = 'line one\nline two\nline three';
			const selStart = 0;
			const selEnd = text.length;
			const result = applyMarkdownAction(text, selStart, selEnd, 'quote');
			expect(result.text).toBe('> line one\n> line two\n> line three');
		});

		it('applies a ul prefix to only the lines touched by the selection, not the whole doc', () => {
			const text = 'first\nsecond\nthird';
			// selection spans just "second" (partial selection within the middle line)
			const selStart = text.indexOf('second') + 1;
			const selEnd = text.indexOf('second') + 3;
			const result = applyMarkdownAction(text, selStart, selEnd, 'ul');
			expect(result.text).toBe('first\n- second\nthird');
		});

		it('applies an ol prefix', () => {
			const result = applyMarkdownAction('step', 0, 4, 'ol');
			expect(result.text).toBe('1. step');
		});

		it('applies a line prefix with a collapsed (empty) selection at the cursor line', () => {
			const text = 'line one\nline two';
			const cursor = text.indexOf('line two');
			const result = applyMarkdownAction(text, cursor, cursor, 'heading');
			expect(result.text).toBe('line one\n## line two');
		});

		it('adjusts the returned selection to account for inserted prefix characters', () => {
			const text = 'aaa\nbbb';
			const result = applyMarkdownAction(text, 0, text.length, 'quote');
			// two lines, each prefixed with "> " (2 chars) => 4 chars added total
			expect(result.selStart).toBe(0 + 2);
			expect(result.selEnd).toBe(text.length + 4);
		});
	});

	describe('link action', () => {
		it('wraps a selection as the link label and selects the url placeholder', () => {
			const result = applyMarkdownAction('see docs', 4, 8, 'link');
			expect(result.text).toBe('see [docs](url)');
			expect(result.text.slice(result.selStart, result.selEnd)).toBe('url');
		});

		it('inserts a placeholder label and selects the url placeholder when there is no selection', () => {
			const result = applyMarkdownAction('see ', 4, 4, 'link');
			expect(result.text).toBe('see [text](url)');
			expect(result.text.slice(result.selStart, result.selEnd)).toBe('url');
		});
	});
});
