import { describe, it, expect } from 'vitest';
import { render, fireEvent, screen, within } from '@testing-library/svelte';
import StackFrames from './StackFrames.svelte';
import { FLUTTER_OCCURRENCE, MALFORMED_FRAMES, V2_JS_OCCURRENCE } from './occurrence-fixtures';
import { formatStackFrame, frameVisibility, frameObjects } from './occurrence-display';

// sentinel-occurrence-detail-panels §5.2 (TASK-04, TASK-05's renderer).

const codePoints = (s: string | null | undefined) => Array.from(s ?? '').length;
const lineText = (frame: HTMLElement) =>
	frame.querySelector('.frame-line .expandable-text')!.firstChild!.textContent;

describe('StackFrames', () => {
	it('F1: each frame line, the in-app mark and the module', async () => {
		const frames = FLUTTER_OCCURRENCE.stacktrace as any[];
		render(StackFrames, { frames, showRaw: false });
		// Collapsing is active (mixed in-app), so every hidden run is expanded first.
		const toggles = screen.getAllByTestId('show-frames');
		expect(toggles.length).toBeGreaterThan(0);
		for (const button of toggles) await fireEvent.click(button);
		const rendered = screen.getAllByTestId('frame');
		expect(rendered).toHaveLength(20);
		rendered.forEach((li, i) => {
			expect(lineText(li)).toBe(formatStackFrame(frames[i]));
			const inApp = frames[i].in_app === true;
			expect(li.getAttribute('data-in-app')).toBe(inApp ? 'true' : 'false');
			expect(within(li).queryByTestId('in-app')?.textContent ?? null).toBe(inApp ? 'in app' : null);
			expect(within(li).queryByTestId('frame-module')?.textContent ?? null).toBe(frames[i].module ?? null);
		});
	});

	it('F2: frame 0 renders 11 context lines numbered 97-107, the current one 102', () => {
		render(StackFrames, { frames: FLUTTER_OCCURRENCE.stacktrace, showRaw: false });
		const first = screen.getAllByTestId('frame')[0];
		const lines = Array.from(within(first).getByTestId('frame-context').querySelectorAll('li'));
		expect(lines).toHaveLength(11);
		expect(lines.map((li) => li.querySelector('.lineno')!.textContent)).toEqual(
			Array.from({ length: 11 }, (_, i) => String(97 + i)),
		);
		const current = lines.filter((li) => li.getAttribute('aria-current') === 'true');
		expect(current).toHaveLength(1);
		expect(current[0].querySelector('.lineno')!.textContent).toBe('102');
		expect(current[0].querySelector('code')!.textContent).toBe((FLUTTER_OCCURRENCE.stacktrace as any[])[0].context_line);
		expect(current[0].classList.contains('current')).toBe(true);
	});

	it('F2: a symbolicated frame numbers from raw.line, in both modes', () => {
		const frame = {
			file: 'src/a.ts',
			line: 88,
			function: 'f',
			in_app: true,
			symbolicated: true,
			raw: { file: 'a.min.js', line: 1, column: 5, function: 'n' },
			context_line: 'cur',
			post_context: ['next'],
		};
		for (const showRaw of [false, true]) {
			const { unmount } = render(StackFrames, { frames: [frame], showRaw });
			const numbers = Array.from(screen.getByTestId('frame-context').querySelectorAll('.lineno')).map((n) => n.textContent);
			expect(numbers).toEqual(['1', '2']);
			unmount();
		}
	});

	it('F2: no gutter when numbering would go below 1; no context without context_line', () => {
		const { unmount } = render(StackFrames, {
			frames: [{ file: 'a', line: 2, function: 'f', pre_context: ['1', '2', '3', '4', '5'], context_line: 'c' }],
			showRaw: false,
		});
		const ctx = screen.getByTestId('frame-context');
		expect(ctx.querySelectorAll('li')).toHaveLength(6);
		expect(ctx.querySelectorAll('.lineno')).toHaveLength(0);
		unmount();

		render(StackFrames, { frames: [{ file: 'a', line: 9, function: 'f', pre_context: ['x'] }], showRaw: false });
		expect(screen.queryByTestId('frame-context')).toBeNull();
	});

	it('F4: [S,S,A,S,S,S,A,S] collapses to two runs, each expanding on its own', async () => {
		const pattern = 'SSASSSAS';
		const frames = pattern.split('').map((c, i) => ({ file: `f${i}.dart`, line: i + 1, function: `fn${i}`, in_app: c === 'A' }));
		render(StackFrames, { frames, showRaw: false });
		const shown = () => screen.getAllByTestId('frame').map((li) => lineText(li));
		expect(shown()).toEqual([0, 2, 3, 6, 7].map((i) => formatStackFrame(frames[i])));
		const buttons = screen.getAllByTestId('show-frames');
		expect(buttons.map((b) => b.textContent)).toEqual(['Show 1 more frame', 'Show 2 more frames']);
		expect(buttons.every((b) => b.getAttribute('aria-expanded') === 'false')).toBe(true);

		await fireEvent.click(buttons[1]);
		expect(shown()).toEqual([0, 2, 3, 4, 5, 6, 7].map((i) => formatStackFrame(frames[i])));
		expect(buttons[1].textContent).toBe('Hide 2 more frames');
		expect(buttons[1].getAttribute('aria-expanded')).toBe('true');
		expect(buttons[0].textContent).toBe('Show 1 more frame');

		await fireEvent.click(buttons[1]);
		expect(shown()).toEqual([0, 2, 3, 6, 7].map((i) => formatStackFrame(frames[i])));
	});

	it('F5: showRaw swaps each line to its raw form and keeps visibility', () => {
		const frames = V2_JS_OCCURRENCE.stacktrace as any[];
		const { unmount } = render(StackFrames, { frames, showRaw: false });
		const before = screen.getAllByTestId('frame');
		expect(before.map(lineText)).toEqual(frames.map((f) => formatStackFrame(f)));
		const marks = before.map((li) => li.getAttribute('data-in-app'));
		unmount();

		render(StackFrames, { frames, showRaw: true });
		const after = screen.getAllByTestId('frame');
		expect(after.map(lineText)).toEqual(frames.map((f) => formatStackFrame(f.raw ?? f)));
		expect(after.map((li) => li.getAttribute('data-in-app'))).toEqual(marks);
	});

	it('F7: malformed frames mount without throwing; only the two objects render', () => {
		expect(frameVisibility(frameObjects(MALFORMED_FRAMES))).toEqual([true, true]);
		for (const showRaw of [false, true]) {
			const { unmount } = render(StackFrames, { frames: MALFORMED_FRAMES, showRaw });
			const items = screen.getAllByTestId('frame');
			expect(items.map(lineText)).toEqual(['at <unknown> (<unknown>:3)', 'at {"a":1} (<unknown>:0)']);
			expect(screen.queryByTestId('show-frames')).toBeNull();
			unmount();
		}
	});

	it('an empty or non-array frames prop renders nothing', () => {
		for (const frames of [[], null, 'x', { 0: {} }, [null, 1]]) {
			const { container, unmount } = render(StackFrames, { frames, showRaw: false });
			expect(container.querySelector('[data-testid="frames"]')).toBeNull();
			unmount();
		}
	});

	it('TR4: a 3 000-code-point function/file, context line and module each collapse', () => {
		const long = 'y'.repeat(3000);
		render(StackFrames, {
			frames: [{ function: long, file: long, line: 3, in_app: true, module: long, context_line: long }],
			showRaw: false,
		});
		const frame = screen.getByTestId('frame');
		const line = lineText(frame)!;
		expect(codePoints(line)).toBe(2049);
		expect(line.endsWith('…')).toBe(true);
		const code = within(frame).getByTestId('frame-context').querySelector('code .expandable-text')!.firstChild!.textContent!;
		expect(codePoints(code)).toBe(2049);
		const module = within(frame).getByTestId('frame-module').querySelector('.expandable-text')!.firstChild!.textContent!;
		expect(codePoints(module)).toBe(257);
		expect(within(frame).getAllByTestId('expand-text')).toHaveLength(3);
	});
});
