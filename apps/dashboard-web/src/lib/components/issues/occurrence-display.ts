// sentinel-occurrence-detail-panels: the pure helpers every occurrence panel shares.
//
// Two rules hold for everything in this module (spec §4.2):
//  - Event data is untrusted. Nothing here builds HTML, a URL, a class or a style from it; a value is
//    shown as text through `formatValue`, and a long one is cut through `clip` or `ExpandableText`.
//  - Every table keyed by event data is a `Map` (or `Set`), read with `.get()`/`.has()` only. A context
//    name, level or key can be `constructor`, `toString` or `__proto__` (spec T26), and a plain-object
//    lookup on one returns an `Object.prototype` function instead of `undefined`.

/** Every inline value: table cells, facts, headings, crumb fields (§4.3). */
export const INLINE_LIMIT = 256;
/** The message, a frame or source line, `request.data`, a crumb's `data`, a non-object context (§4.3). */
export const BLOCK_LIMIT = 2048;

/**
 * Whether an occurrence row carries the v2 fields (§4.1). Every row the v2 processor writes has a
 * non-null `level`, and a row whose `level` is NULL has all 17 v2 columns NULL (spec T8). `undefined`
 * counts as null: existing page fixtures, and a stale loader build, omit the keys.
 */
export function hasV2Fields(occurrence: { level?: unknown }): boolean {
	return occurrence.level !== null && occurrence.level !== undefined;
}

/**
 * A stored value as text (§4.2 rule 4): a string verbatim, a number or boolean through `String`,
 * `null` as `null`, an object or array as JSON (compact, or indented when `pretty`). Nothing is parsed
 * as HTML, Markdown or a URL.
 */
export function formatValue(value: unknown, pretty = false): string {
	if (typeof value === 'string') return value;
	if (value === undefined) return '';
	if (value === null) return 'null';
	if (typeof value === 'object') return pretty ? JSON.stringify(value, null, 2) : JSON.stringify(value);
	return String(value);
}

/**
 * The first `limit` code points of `text` plus `…` when it is longer, else `text` (§4.3). Used only
 * where `ExpandableText`'s button cannot go: an `<option>` label and the tag link's text.
 */
export function clip(text: string, limit: number): string {
	const points = Array.from(text);
	return points.length > limit ? points.slice(0, limit).join('') + '…' : text;
}

// §4.4: the five levels onto variables that exist in app.css. A Map, so `constructor` is not a key.
const LEVEL_TONES = new Map<string, string>([
	['fatal', 'tone-critical'],
	['error', 'tone-critical'],
	['warning', 'tone-warning'],
	['info', 'tone-info'],
	['debug', 'tone-debug'],
]);

/** The tone class for a level (§4.4). Anything that is not one of the five, or absent, is `tone-neutral`. */
export function levelTone(level: unknown): string {
	if (typeof level !== 'string') return 'tone-neutral';
	return LEVEL_TONES.get(level) ?? 'tone-neutral';
}

// ---------------------------------------------------------------------------------------------------
// Frames (§5.2, TASK-04 and TASK-05)

/**
 * The frame as the JSONB stores it. `column`, `symbolicated` and `raw` are sentinel-sourcemaps §4.7's
 * additions, all optional: a pre-feature occurrence carries none of them.
 */
export interface StackFrame {
	file: string;
	line: number;
	function: string;
	column?: number;
	symbolicated?: boolean;
	raw?: { file: string; line: number; column?: number; function: string };
}

// sentinel-sourcemaps §4.7 / flow `D5`: `:column` is appended when present and > 0 — `0` is the
// wire's "unknown" (§4.3) and a pre-feature frame has no key at all, so both render exactly the
// pre-feature string. The 50-char truncation stays (decision 8: original paths are verbatim).
export function formatStackFrame(frame: { file: string; line: number; column?: number; function: string }): string {
	let file = frame.file;
	if (file.length > 50) {
		const start = file.slice(0, 20);
		const end = file.slice(-25);
		file = start + '...' + end;
	}
	const column = frame.column && frame.column > 0 ? `:${frame.column}` : '';
	return `at ${frame.function} (${file}:${frame.line}${column})`;
}

// `D4`: in raw mode a frame with `raw` renders `raw`'s values; a frame without `raw` renders as
// itself in both modes (it was never rewritten, so its raw form IS its form).
export function displayedFrame(frame: StackFrame, raw: boolean): { file: string; line: number; column?: number; function: string } {
	return raw && frame.raw ? frame.raw : frame;
}

type FrameObject = Record<string, unknown>;

function isObject(value: unknown): value is FrameObject {
	return value !== null && typeof value === 'object' && !Array.isArray(value);
}

/**
 * The stored frames that are objects (§5.2 "Frame shape"). Every other element (`null`, a number, a
 * string, an array) is skipped: not rendered, not counted, not given to `frameVisibility`. The
 * processor never writes such a frame, but a hand-written row can, and `formatStackFrame` would throw
 * on it (a 500 under SSR).
 */
export function frameObjects(frames: unknown): FrameObject[] {
	return Array.isArray(frames) ? frames.filter(isObject) : [];
}

const isFiniteNumber = (v: unknown): v is number => typeof v === 'number' && Number.isFinite(v);

function frameText(v: unknown): string {
	if (typeof v === 'string') return v;
	if (v === undefined || v === null) return '<unknown>';
	return formatValue(v);
}

/**
 * The frame line, `formatStackFrame(displayedFrame(frame, showRaw))` for a well-formed frame, with the
 * frame coerced first so a hand-written one never throws: `raw` is used only when it is an object,
 * `file`/`function` become text, `line` is `0` (the wire's "unknown") unless it is a finite number,
 * and `column` is dropped unless it is one.
 */
export function frameLine(frame: FrameObject, showRaw: boolean): string {
	const source = { ...frame, raw: isObject(frame.raw) ? frame.raw : undefined } as unknown as StackFrame;
	const shown = displayedFrame(source, showRaw) as unknown as FrameObject;
	return formatStackFrame({
		file: frameText(shown.file),
		function: frameText(shown.function),
		line: isFiniteNumber(shown.line) ? shown.line : 0,
		column: isFiniteNumber(shown.column) ? shown.column : undefined,
	});
}

/**
 * Which frames are visible before any "Show n more frames" click (§5.2): Sentry's `frameIsVisible`
 * (`crashContent/stackTrace/content.tsx`), translated to Sentinel's crashing-frame-first order.
 * Collapsing is active only when some frames are in app and some are not; then frame `i` is visible
 * iff it is in app, or the frame before it (its callee, Sentry's `nextFrame`) is, or it is the
 * crashing frame (`i === 0`, Sentry's "last non-app frame").
 */
export function frameVisibility(frames: FrameObject[]): boolean[] {
	const app = frames.map((f) => f.in_app === true);
	const active = app.some((a) => a) && app.some((a) => !a);
	return app.map((a, i) => !active || a || (i > 0 && app[i - 1]) || i === 0);
}

export interface ContextLine {
	/** The source line number, or `null` when the gutter is omitted for the whole frame. */
	lineno: number | null;
	text: string;
	current: boolean;
}

/**
 * A frame's numbered source context (§5.2), or `null` when `context_line` is not a string (a
 * `pre_context`/`post_context` without it has no line to anchor). Numbering anchors on `raw.line`
 * when `raw` is an object, else `line`: the SDK read the context at ITS line, and symbolication
 * rewrites `line`, never the context. If that base is not an integer ≥ 1, or the first pre line would
 * be below 1, every gutter is omitted rather than printing `0` or a negative number.
 */
export function sourceContextLines(frame: FrameObject): ContextLine[] | null {
	if (typeof frame.context_line !== 'string') return null;
	const strings = (v: unknown) => (Array.isArray(v) ? v.filter((s): s is string => typeof s === 'string') : []);
	const pre = strings(frame.pre_context);
	const post = strings(frame.post_context);
	const rawLine = isObject(frame.raw) ? frame.raw.line : undefined;
	const base = rawLine ?? frame.line;
	const numbered = typeof base === 'number' && Number.isInteger(base) && base >= 1 && base - pre.length >= 1;
	const at = (n: number) => (numbered ? (base as number) + n : null);
	return [
		...pre.map((text, i) => ({ lineno: at(i - pre.length), text, current: false })),
		{ lineno: at(0), text: frame.context_line, current: true },
		...post.map((text, i) => ({ lineno: at(1 + i), text, current: false })),
	];
}

// ---------------------------------------------------------------------------------------------------
// User (§6.1, TASK-03)

const nonEmpty = (v: unknown): v is string => typeof v === 'string' && v !== '';

/**
 * The geo line: the non-empty strings among `city`, `subdivision`, `region` joined with `, `, then
 * ` (country_code)` (or the code alone). Every stored field is shown verbatim: the enrichment writes
 * the country NAME into `region` (spec T14), but an SDK-sent geo may use it differently, so nothing is
 * relabelled and no country name is derived. `''` when `geo` is not an object.
 */
export function geoLine(geo: unknown): string {
	if (!isObject(geo)) return '';
	const parts = [geo.city, geo.subdivision, geo.region].filter(nonEmpty).join(', ');
	if (!nonEmpty(geo.country_code)) return parts;
	return parts === '' ? geo.country_code : `${parts} (${geo.country_code})`;
}

// ---------------------------------------------------------------------------------------------------
// Exception chain (§9, TASK-05)

/**
 * The chain in display order, outermost first: the stored values reversed. The SDK stores them oldest
 * first (`errors.mdx`, spec T12), and Sentry displays them newest first by default
 * (`crashContent/exception/content.tsx`, `isStacktraceNewestFirst()`). A copy; the input is untouched.
 */
export function chainDisplayOrder<T>(values: T[]): T[] {
	return [...values].reverse();
}

// ---------------------------------------------------------------------------------------------------
// Breadcrumb time (§8.2, TASK-02)

const CRUMB_TIME = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(\.\d+)?(Z|[+-]\d{2}:\d{2})$/;

/**
 * An RFC 3339 timestamp as epoch milliseconds, or `null`. The fraction is cut or padded to exactly
 * three digits before `Date.parse` (the processor writes nanoseconds; ECMAScript guarantees only
 * milliseconds, spec T24), so the result is the same on every engine.
 */
export function parseCrumbTime(s: unknown): number | null {
	if (typeof s !== 'string') return null;
	const m = CRUMB_TIME.exec(s);
	if (!m) return null;
	const millis = (m[2] ?? '.').slice(1).padEnd(3, '0').slice(0, 3);
	const ms = Date.parse(`${m[1]}.${millis}${m[3]}`);
	return Number.isNaN(ms) ? null : ms;
}

/**
 * The relative-time anchor, the occurrence's `createdAt`, AS THE LOAD RETURNED IT (§8.2, review finding
 * odp-sr3-crumb-anchor-date): a `Date` on the real page (spec T29), an ISO string only in hand-written
 * test data. A `Date` is read with `getTime()`, never stringified and re-parsed: `String(date)` is not
 * ISO, and `toISOString()` throws on an Invalid Date.
 */
export function anchorMs(v: string | Date | null | undefined): number | null {
	if (v instanceof Date) {
		const t = v.getTime();
		return Number.isFinite(t) ? t : null;
	}
	if (typeof v === 'string') return parseCrumbTime(v);
	return null;
}

/** `crumb − occurrence` as `-4.2s`, `-2m 5s` or `-2h 3m` (§8.2). */
export function formatRelative(deltaMs: number): string {
	const sign = deltaMs < 0 ? '-' : deltaMs > 0 ? '+' : '';
	const a = Math.abs(deltaMs);
	if (a < 60_000) return `${sign}${(a / 1000).toFixed(1)}s`;
	if (a < 3_600_000) return `${sign}${Math.floor(a / 60_000)}m ${Math.floor((a % 60_000) / 1000)}s`;
	return `${sign}${Math.floor(a / 3_600_000)}h ${Math.floor((a % 3_600_000) / 60_000)}m`;
}

// ---------------------------------------------------------------------------------------------------
// Context cards (§7, TASK-01)

/** A context field as a row: `label` is the §7.2 label, or `null` when the key is shown raw. */
export interface ContextRow {
	key: string;
	label: string | null;
	value: string;
}

/**
 * One context card. `title` is the well-known title, or `null` for an unknown context (whose raw key
 * is the title). Exactly one of `rows` (an object value) and `scalar` (anything else) is set.
 */
export interface ContextCard {
	key: string;
	title: string | null;
	headline: string | null;
	rows: ContextRow[] | null;
	scalar: string | null;
}

// §7.1: the contract's list and order; the titles.
const CONTEXT_TITLES = new Map<string, string>([
	['device', 'Device'],
	['os', 'Operating System'],
	['client_os', 'Client Operating System'],
	['app', 'App'],
	['runtime', 'Runtime'],
	['browser', 'Browser'],
	['culture', 'Culture'],
	['trace', 'Trace'],
]);
const CONTEXT_ORDER = new Map([...CONTEXT_TITLES.keys()].map((key, i) => [key, i]));

const OS_FIELDS: [string, string][] = [
	['name', 'Name'], ['version', 'Version'], ['build', 'Build'], ['kernel_version', 'Kernel Version'],
	['rooted', 'Rooted'], ['distribution_name', 'Distribution'], ['distribution_version', 'Distribution Version'],
	['distribution_pretty_name', 'Distribution Name'], ['raw_description', 'Raw Description'],
];

// §7.2: each card's well-known fields, in render order, with their labels.
const CONTEXT_FIELDS = new Map<string, Map<string, string>>([
	['device', new Map([
		['name', 'Name'], ['family', 'Family'], ['model', 'Model'], ['model_id', 'Model ID'],
		['manufacturer', 'Manufacturer'], ['brand', 'Brand'], ['arch', 'Architecture'], ['simulator', 'Simulator'],
		['device_type', 'Device Type'], ['orientation', 'Orientation'], ['screen_resolution', 'Screen Resolution'],
		['screen_width_pixels', 'Screen Width'], ['screen_height_pixels', 'Screen Height'],
		['screen_density', 'Screen Density'], ['screen_dpi', 'Screen DPI'], ['battery_level', 'Battery Level'],
		['battery_status', 'Battery Status'], ['charging', 'Charging'], ['online', 'Online'],
		['low_memory', 'Low Memory'], ['memory_size', 'Memory Size'], ['free_memory', 'Free Memory'],
		['usable_memory', 'Usable Memory'], ['storage_size', 'Storage Size'], ['free_storage', 'Free Storage'],
		['external_storage_size', 'External Storage Size'], ['external_free_storage', 'External Free Storage'],
		['processor_count', 'Processor Count'], ['processor_frequency', 'Processor Frequency'],
		['cpu_description', 'CPU'], ['boot_time', 'Boot Time'], ['timezone', 'Timezone'],
	])],
	['os', new Map(OS_FIELDS)],
	// Relay has no client-OS type: `client_os` holds an OS context (§7.1).
	['client_os', new Map(OS_FIELDS)],
	['app', new Map([
		['app_name', 'Name'], ['app_identifier', 'Identifier'], ['app_version', 'Version'], ['app_build', 'Build'],
		['build_type', 'Build Type'], ['app_start_time', 'Start Time'], ['in_foreground', 'In Foreground'],
		['app_memory', 'Memory'], ['device_app_hash', 'Device App Hash'], ['view_names', 'View Names'],
	])],
	['runtime', new Map([['name', 'Name'], ['version', 'Version'], ['build', 'Build'], ['raw_description', 'Raw Description']])],
	['browser', new Map([['name', 'Name'], ['version', 'Version']])],
	['culture', new Map([
		['locale', 'Locale'], ['display_name', 'Display Name'], ['calendar', 'Calendar'], ['timezone', 'Timezone'],
		['is_24_hour_format', '24-Hour Format'],
	])],
	['trace', new Map([
		['trace_id', 'Trace ID'], ['span_id', 'Span ID'], ['parent_span_id', 'Parent Span ID'], ['op', 'Operation'],
		['status', 'Status'], ['origin', 'Origin'], ['data', 'Data'],
	])],
]);

// §7.2: the headline parts; `device` falls back from `model` to `name`, the others join their parts.
const CONTEXT_HEADLINES = new Map<string, string[][]>([
	['device', [['model'], ['name']]],
	['os', [['name', 'version']]],
	['client_os', [['name', 'version']]],
	['app', [['app_name', 'app_version']]],
	['runtime', [['name', 'version']]],
	['browser', [['name', 'version']]],
	['culture', [['locale']]],
]);

const byCodeUnit = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0);
const isPlainRecord = (v: unknown): v is Record<string, unknown> =>
	v !== null && typeof v === 'object' && !Array.isArray(v);

function contextHeadline(key: string, value: Record<string, unknown>): string | null {
	for (const parts of CONTEXT_HEADLINES.get(key) ?? []) {
		const words = parts
			.map((field) => (Object.hasOwn(value, field) ? value[field] : undefined))
			.filter((v): v is string => typeof v === 'string' && v !== '');
		if (words.length > 0) return words.join(' ');
	}
	return null;
}

function contextRows(key: string, value: Record<string, unknown>): ContextRow[] {
	const fields = CONTEXT_FIELDS.get(key) ?? new Map<string, string>();
	const rows: ContextRow[] = [];
	for (const [field, label] of fields) {
		if (Object.hasOwn(value, field) && value[field] !== undefined) {
			rows.push({ key: field, label, value: formatValue(value[field]) });
		}
	}
	const rest = Object.keys(value)
		.filter((field) => !fields.has(field))
		// Sentry: "You can omit `type` if the key name is the type."
		.filter((field) => !(field === 'type' && value[field] === key))
		.filter((field) => value[field] !== undefined)
		.sort(byCodeUnit);
	for (const field of rest) rows.push({ key: field, label: null, value: formatValue(value[field]) });
	return rows;
}

/**
 * The context cards (§7): well-known keys first in the contract's order, every other key after them
 * in code-unit order. An object with no renderable row gives no card; a non-object value is a scalar
 * card. Every table here is a `Map`, so `constructor` or `__proto__` is an unknown key like any other.
 */
export function contextCards(contexts: unknown): ContextCard[] {
	if (!isPlainRecord(contexts)) return [];
	const keys = Object.keys(contexts).sort((a, b) => {
		const ra = CONTEXT_ORDER.get(a) ?? CONTEXT_ORDER.size;
		const rb = CONTEXT_ORDER.get(b) ?? CONTEXT_ORDER.size;
		return ra - rb || byCodeUnit(a, b);
	});
	const cards: ContextCard[] = [];
	for (const key of keys) {
		const value = contexts[key];
		if (value === undefined) continue;
		const title = CONTEXT_TITLES.get(key) ?? null;
		if (isPlainRecord(value)) {
			const rows = contextRows(key, value);
			if (rows.length === 0) continue;
			cards.push({ key, title, headline: contextHeadline(key, value), rows, scalar: null });
		} else {
			cards.push({ key, title, headline: null, rows: null, scalar: formatValue(value, true) });
		}
	}
	return cards;
}
