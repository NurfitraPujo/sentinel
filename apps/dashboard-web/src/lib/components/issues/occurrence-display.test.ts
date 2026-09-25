import { describe, it, expect } from 'vitest';
import {
	INLINE_LIMIT,
	BLOCK_LIMIT,
	hasV2Fields,
	formatValue,
	clip,
	levelTone,
	formatStackFrame,
	displayedFrame,
	frameObjects,
	frameLine,
	frameVisibility,
	sourceContextLines,
	geoLine,
	chainDisplayOrder,
	parseCrumbTime,
	anchorMs,
	formatRelative,
	contextCards,
} from './occurrence-display';
import { FLUTTER_DETAIL, MALFORMED_FRAMES, OCCURRED_AT, protoContexts } from './occurrence-fixtures';

describe('limits (§4.3)', () => {
	it('INLINE_LIMIT is 256 and BLOCK_LIMIT is 2048', () => {
		expect(INLINE_LIMIT).toBe(256);
		expect(BLOCK_LIMIT).toBe(2048);
	});
});

describe('hasV2Fields (§4.1)', () => {
	it('is true exactly when level is neither null nor undefined', () => {
		expect(hasV2Fields({ level: 'error' })).toBe(true);
		expect(hasV2Fields({ level: '' })).toBe(true);
		expect(hasV2Fields({ level: null })).toBe(false);
		expect(hasV2Fields({})).toBe(false);
	});
});

describe('formatValue (§4.2 rule 4)', () => {
	it.each([
		['a string verbatim', 'a <b> "c"', false, 'a <b> "c"'],
		['a number', 42, false, '42'],
		['a float', 2.625, false, '2.625'],
		['true', true, false, 'true'],
		['false', false, false, 'false'],
		['null', null, false, 'null'],
		['an object, compact', { a: 1, b: [2] }, false, '{"a":1,"b":[2]}'],
		['an array, compact', [1, 'x'], false, '[1,"x"]'],
		['an object, pretty', { a: 1 }, true, '{\n  "a": 1\n}'],
	])('%s', (_name, value, pretty, expected) => {
		expect(formatValue(value, pretty)).toBe(expected);
	});

	it('undefined is the empty string', () => {
		expect(formatValue(undefined)).toBe('');
	});
});

describe('clip (§4.3)', () => {
	it('returns the text when it is within the limit', () => {
		expect(clip('abc', 3)).toBe('abc');
		expect(clip('', 3)).toBe('');
	});

	it('cuts to the limit in code points and appends …', () => {
		expect(clip('abcdef', 3)).toBe('abc…');
		const emoji = '😀'.repeat(5);
		expect(clip(emoji, 2)).toBe('😀😀…');
		expect(Array.from(clip('x'.repeat(300), INLINE_LIMIT)).length).toBe(INLINE_LIMIT + 1);
	});
});

describe('levelTone (§4.4, U4)', () => {
	it('maps the five levels onto the existing variables', () => {
		expect(levelTone('fatal')).toBe('tone-critical');
		expect(levelTone('error')).toBe('tone-critical');
		expect(levelTone('warning')).toBe('tone-warning');
		expect(levelTone('info')).toBe('tone-info');
		expect(levelTone('debug')).toBe('tone-debug');
	});

	it('anything else, or absent, is tone-neutral', () => {
		for (const v of ['WARNING', 'critical', '', null, undefined, 3, {}]) {
			expect(levelTone(v)).toBe('tone-neutral');
		}
	});

	it('U4: a prototype key is tone-neutral, never a function', () => {
		for (const v of ['constructor', 'toString', 'hasOwnProperty', 'valueOf', '__proto__']) {
			expect(levelTone(v)).toBe('tone-neutral');
		}
	});
});

describe('frameVisibility (F3): Sentry frameIsVisible in crashing-first order', () => {
	const frames = (pattern: string) => pattern.split('').map((c) => ({ in_app: c === 'A' }));
	const visibleIndices = (v: boolean[]) => v.flatMap((on, i) => (on ? [i] : []));
	const hiddenRuns = (v: boolean[]) => {
		const runs: number[][] = [];
		v.forEach((on, i) => {
			if (on) return;
			const last = runs.at(-1);
			if (last && last.at(-1) === i - 1) last.push(i);
			else runs.push([i]);
		});
		return runs;
	};

	it('[S,S,A,S,S,S,A,S] shows 0, 2, 3, 6, 7 and hides the runs {1} and {4,5}', () => {
		const v = frameVisibility(frames('SSASSSAS'));
		expect(visibleIndices(v)).toEqual([0, 2, 3, 6, 7]);
		expect(hiddenRuns(v)).toEqual([[1], [4, 5]]);
	});

	it('all system frames, or all app frames: every frame visible', () => {
		expect(frameVisibility(frames('SSSS'))).toEqual([true, true, true, true]);
		expect(frameVisibility(frames('AAA'))).toEqual([true, true, true]);
		expect(frameVisibility([])).toEqual([]);
	});

	it('only in_app === true counts as app', () => {
		expect(frameVisibility([{ in_app: 'true' }, { in_app: true }, {}, { in_app: 1 }])).toEqual([
			true,
			true,
			true,
			false,
		]);
	});
});

describe('frame shape (§5.2)', () => {
	it('formatStackFrame and displayedFrame are the page\'s, moved verbatim', () => {
		expect(formatStackFrame({ file: 'a.ts', line: 3, function: 'f' })).toBe('at f (a.ts:3)');
		expect(formatStackFrame({ file: 'a.ts', line: 3, column: 0, function: 'f' })).toBe('at f (a.ts:3)');
		expect(formatStackFrame({ file: 'a.ts', line: 3, column: 9, function: 'f' })).toBe('at f (a.ts:3:9)');
		expect(formatStackFrame({ file: 'x'.repeat(60), line: 1, function: 'f' })).toBe(
			`at f (${'x'.repeat(20)}...${'x'.repeat(25)}:1)`,
		);
		const raw = { file: 'r.js', line: 1, function: 'r' };
		const frame = { file: 'a.ts', line: 3, function: 'f', raw };
		expect(displayedFrame(frame, true)).toBe(raw);
		expect(displayedFrame(frame, false)).toBe(frame);
		expect(displayedFrame({ file: 'a.ts', line: 3, function: 'f' }, true)).toEqual({ file: 'a.ts', line: 3, function: 'f' });
	});

	it('frameObjects keeps only non-null, non-array objects', () => {
		expect(frameObjects(MALFORMED_FRAMES)).toEqual([
			{ line: 3, in_app: true },
			{ file: null, function: { a: 1 }, line: 'x', raw: 'str', in_app: false },
		]);
		expect(frameObjects(null)).toEqual([]);
		expect(frameObjects('frames')).toEqual([]);
		expect(frameObjects({ 0: {} })).toEqual([]);
	});

	it('frameLine coerces a malformed frame instead of throwing, in both modes', () => {
		const [a, b] = frameObjects(MALFORMED_FRAMES);
		for (const raw of [false, true]) {
			expect(frameLine(a, raw)).toBe('at <unknown> (<unknown>:3)');
			expect(frameLine(b, raw)).toBe('at {"a":1} (<unknown>:0)');
		}
	});

	it('frameLine of a well-formed frame is exactly formatStackFrame(displayedFrame(frame, showRaw))', () => {
		const frame = {
			function: 'submitOrder',
			file: 'src/cart/checkout.ts',
			line: 42,
			column: 7,
			raw: { function: 'n', file: 'https://cdn.example.com/assets/app.3f9a1c.min.js', line: 1, column: 18234 },
		};
		expect(frameLine(frame, false)).toBe('at submitOrder (src/cart/checkout.ts:42:7)');
		expect(frameLine(frame, true)).toBe('at n (https://cdn.example.com/assets/app.3f9a1c.min.js:1:18234)');
		for (const raw of [false, true]) {
			expect(frameLine(frame, raw)).toBe(formatStackFrame(displayedFrame(frame, raw)));
		}
	});
});

describe('sourceContextLines (§5.2)', () => {
	const pre = ['p1', 'p2', 'p3', 'p4', 'p5'];
	const post = ['q1', 'q2', 'q3', 'q4', 'q5'];

	it('numbers pre, current and post from line', () => {
		const lines = sourceContextLines({ line: 102, pre_context: pre, context_line: 'cur', post_context: post })!;
		expect(lines.map((l) => l.lineno)).toEqual([97, 98, 99, 100, 101, 102, 103, 104, 105, 106, 107]);
		expect(lines.filter((l) => l.current)).toEqual([{ lineno: 102, text: 'cur', current: true }]);
	});

	it('anchors on raw.line when raw is an object (the line the SDK read the context at)', () => {
		const lines = sourceContextLines({ line: 88, raw: { line: 1 }, context_line: 'cur', post_context: ['a'] })!;
		expect(lines.map((l) => l.lineno)).toEqual([1, 2]);
		const stringRaw = sourceContextLines({ line: 88, raw: 'str', context_line: 'cur' })!;
		expect(stringRaw.map((l) => l.lineno)).toEqual([88]);
	});

	it('omits every gutter when the numbering would reach below 1 or the base is not an integer', () => {
		expect(sourceContextLines({ line: 2, pre_context: pre, context_line: 'c' })!.every((l) => l.lineno === null)).toBe(true);
		expect(sourceContextLines({ line: 0, context_line: 'c' })!.every((l) => l.lineno === null)).toBe(true);
		expect(sourceContextLines({ line: 1.5, context_line: 'c' })!.every((l) => l.lineno === null)).toBe(true);
		expect(sourceContextLines({ context_line: 'c' })!.every((l) => l.lineno === null)).toBe(true);
	});

	it('keeps only string elements of array contexts, and nothing without a context_line', () => {
		const lines = sourceContextLines({ line: 10, pre_context: ['a', 3, null, 'b'], context_line: 'c', post_context: 'x' })!;
		expect(lines.map((l) => l.text)).toEqual(['a', 'b', 'c']);
		expect(sourceContextLines({ line: 10, pre_context: pre })).toBeNull();
		expect(sourceContextLines({ line: 10, context_line: 7 })).toBeNull();
	});
});

describe('geoLine (§6.1)', () => {
	it('city, subdivision, region, then the country code in parentheses', () => {
		expect(geoLine({ city: 'Jakarta', subdivision: 'Jakarta', region: 'Indonesia', country_code: 'ID' })).toBe(
			'Jakarta, Jakarta, Indonesia (ID)',
		);
	});

	it('skips empty or non-string parts, and uses the code alone when nothing else is set', () => {
		expect(geoLine({ city: '', subdivision: null, region: 'Indonesia' })).toBe('Indonesia');
		expect(geoLine({ country_code: 'ID' })).toBe('ID');
		expect(geoLine({ city: 'Bandung', country_code: '' })).toBe('Bandung');
		expect(geoLine({ city: 3, region: { a: 1 } })).toBe('');
		expect(geoLine({})).toBe('');
	});

	it('is empty when geo is not an object', () => {
		for (const geo of [null, undefined, 'Jakarta', 7, ['Jakarta']]) {
			expect(geoLine(geo)).toBe('');
		}
	});
});

describe('chainDisplayOrder (§9.1)', () => {
	it('is the stored values reversed: the exception raised last comes first', () => {
		const values = [{ type: 'Oldest' }, { type: 'Middle' }, { type: 'Newest' }];
		expect(chainDisplayOrder(values).map((v) => v.type)).toEqual(['Newest', 'Middle', 'Oldest']);
		expect(values.map((v) => v.type)).toEqual(['Oldest', 'Middle', 'Newest']);
		expect(chainDisplayOrder([])).toEqual([]);
	});
});

describe('breadcrumb time (§8.2, B5)', () => {
	it('formatRelative, pinned', () => {
		expect(formatRelative(-4200)).toBe('-4.2s');
		expect(formatRelative(1500)).toBe('+1.5s');
		expect(formatRelative(0)).toBe('0.0s');
		expect(formatRelative(-125000)).toBe('-2m 5s');
		expect(formatRelative(-7380000)).toBe('-2h 3m');
		expect(formatRelative(-3600)).toBe('-3.6s');
		expect(formatRelative(59999)).toBe('+60.0s');
		expect(formatRelative(60000)).toBe('+1m 0s');
		expect(formatRelative(3600000)).toBe('+1h 0m');
	});

	it('anchorMs reads a Date and an ISO string alike', () => {
		const expected = Date.parse(OCCURRED_AT);
		expect(anchorMs(new Date(OCCURRED_AT))).toBe(expected);
		expect(anchorMs(OCCURRED_AT)).toBe(expected);
	});

	it('anchorMs of an Invalid Date, null, undefined, a number or garbage is null', () => {
		for (const v of [new Date('nope'), null, undefined, 0, 'nope', {}] as unknown[]) {
			expect(anchorMs(v as never)).toBeNull();
		}
	});

	it('parseCrumbTime cuts or pads the fraction to milliseconds, on every engine', () => {
		expect(parseCrumbTime('2026-09-25T02:00:00.123456789Z')).toBe(Date.parse('2026-09-25T02:00:00.123Z'));
		expect(parseCrumbTime('2026-09-25T02:00:00.1Z')).toBe(Date.parse('2026-09-25T02:00:00.100Z'));
		expect(parseCrumbTime('2026-09-25T02:00:00Z')).toBe(Date.parse('2026-09-25T02:00:00.000Z'));
		expect(parseCrumbTime('2026-09-25T09:00:00.5+07:00')).toBe(Date.parse('2026-09-25T02:00:00.500Z'));
	});

	it('parseCrumbTime of a non-match or an impossible date is null', () => {
		for (const v of ['2026-13-45T99:00:00Z', 'yesterday-ish', '2026-09-25 02:00:00Z', '2026-09-25T02:00:00', '', 7, null]) {
			expect(parseCrumbTime(v)).toBeNull();
		}
	});
});

describe('contextCards (§7, U4)', () => {
	it('well-known keys first in the contract order, then the rest in code-unit order', () => {
		const cards = contextCards({ zeta: { a: 1 }, trace: { op: 'x' }, Alpha: { a: 1 }, client_os: { name: 'Windows' }, device: { name: 'd' } });
		expect(cards.map((c) => c.key)).toEqual(['device', 'client_os', 'trace', 'Alpha', 'zeta']);
		expect(cards.map((c) => c.title)).toEqual(['Device', 'Client Operating System', 'Trace', null, null]);
	});

	it('the Flutter contexts: order, titles, headlines, labelled rows then the rest', () => {
		const cards = contextCards(FLUTTER_DETAIL.contexts);
		expect(cards.map((c) => c.key)).toEqual(['device', 'os', 'app', 'runtime', 'culture', 'trace', 'flutter']);
		expect(cards.map((c) => c.headline)).toEqual(['Pixel 7', 'Android 14', 'Daya 2.14.0', 'Dart 3.3.0 (stable)', 'id_ID', null, null]);
		const device = cards[0].rows!;
		expect(device.slice(0, 3).map((r) => r.label)).toEqual(['Name', 'Family', 'Model']);
		expect(device.at(-1)).toEqual({ key: 'custom_field', label: null, value: 'custom value' });
		expect(device.some((r) => r.key === 'type')).toBe(false);
		expect(device.find((r) => r.key === 'simulator')!.value).toBe('false');
		expect(cards[6].rows).toEqual([
			{ key: 'has_render_view', label: null, value: 'true' },
			{ key: 'renderer', label: null, value: 'Impeller' },
		]);
	});

	it('client_os takes the os fields, and its type "os" is an ordinary row', () => {
		const [card] = contextCards({ client_os: { type: 'os', name: 'Windows', version: '11' } });
		expect(card.headline).toBe('Windows 11');
		expect(card.rows).toEqual([
			{ key: 'name', label: 'Name', value: 'Windows' },
			{ key: 'version', label: 'Version', value: '11' },
			{ key: 'type', label: null, value: 'os' },
		]);
	});

	it('a non-object context is a scalar, pretty-printed; an empty one is no card', () => {
		const cards = contextCards({ os: '***REDACTED***', arr: [1, 2], device: {}, app: { type: 'app' }, n: 3 });
		expect(cards.map((c) => [c.key, c.scalar])).toEqual([
			['os', '***REDACTED***'],
			['arr', JSON.stringify([1, 2], null, 2)],
			['n', '3'],
		]);
	});

	it.each([[null], [undefined], ['x'], [[{ device: {} }]], [{}]])('%j gives no cards', (contexts) => {
		expect(contextCards(contexts)).toEqual([]);
	});

	it('U4: prototype keys are ordinary unknown keys; nothing is a function', () => {
		const cards = contextCards(protoContexts());
		expect(cards.map((c) => c.key)).toEqual(['device', '__proto__', 'constructor', 'hasOwnProperty', 'toString']);
		expect(cards.map((c) => c.title)).toEqual(['Device', null, null, null, null]);
		expect(cards[0].rows).toEqual([
			{ key: 'model', label: 'Model', value: 'P7' },
			{ key: 'constructor', label: null, value: 'Pixel' },
		]);
		expect(cards[2].rows).toEqual([{ key: 'name', label: null, value: 'c' }]);
		expect(cards[3].scalar).toBe('x');
		for (const card of cards) {
			expect(typeof card.title === 'string' || card.title === null).toBe(true);
			expect(card.rows === null || Array.isArray(card.rows)).toBe(true);
			for (const row of card.rows ?? []) expect(typeof row.value).toBe('string');
		}
	});
});
