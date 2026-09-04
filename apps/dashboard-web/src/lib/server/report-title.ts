/**
 * sentinel-user-report-issue contract §4.5a — title derivation.
 *
 * §4.1's `title` is optional and `issues.message` is NOT NULL, so a title always exists after
 * creation. When the caller omits one the server derives it, and OQ-2 fixes HOW: by truncation,
 * never by inference. **No LLM, no summarisation, no classification — this feature has no
 * inference budget (§1).**
 *
 * This is a pure function on purpose (§11: "The title derivation function (§4.5a) | new — a pure
 * function"). It reads no clock, no database and no environment, so its table-driven test needs
 * none of them, and the endpoint asserts only that it drives the same function.
 *
 * The rule, in the contract's own order:
 *
 *   1. Take `text`, trimmed, collapse internal whitespace to single spaces.
 *   2. Truncate to 80 characters, cutting at the last whitespace boundary at or before 80 so a
 *      word is not split; append `…` when anything was cut. Count Unicode CODE POINTS, not bytes,
 *      and never split a grapheme cluster.
 *   3. If `context.page` is a non-empty string, the title is `"<page> — <truncated text>"`;
 *      otherwise the truncated text alone.
 *   4. Clamp the whole thing to 200 characters (§4.1's bound, which `issues.message` inherits).
 *   5. If `text` is somehow empty after all of that, the title is `"User report"`.
 *
 * Three readings the contract leaves to the implementation, recorded here because a later reader
 * will otherwise re-derive them from the tests:
 *
 * - **A single word longer than the limit has no whitespace boundary to cut at.** "so a word is
 *   not split" is a preference the input can make impossible; the fallback is a hard cut at the
 *   limit, on a grapheme boundary. Returning the whole 8000-character word instead would defeat
 *   the truncation entirely.
 * - **The `…` is appended after the truncation, so a cut title is 81 code points, not 80.** Step 2
 *   says "truncate to 80 … append `…` when anything was cut", in that order.
 * - **Step 5's subject is `text`, not the derived title.** An empty `text` yields exactly
 *   `"User report"`, with no `page` prefix — `"Visits — "` is not a title, and the contract writes
 *   the fallback as a whole value rather than as a substitution into step 3.
 */

/** §4.5a step 2. Code points, not bytes: a 2-byte character is one character here. */
export const TITLE_TRUNCATE_CODE_POINTS = 80;

/** §4.5a step 4 / §4.1's `title` bound, which `issues.message` inherits. */
export const TITLE_MAX_CODE_POINTS = 200;

/** §4.5a step 5. `issues.message` is NOT NULL and there is no path that may fail here. */
export const TITLE_FALLBACK = 'User report';

/** U+2026 HORIZONTAL ELLIPSIS, one code point — never the three-dot ASCII spelling. */
const ELLIPSIS = '…';

/** U+2014 EM DASH with a space either side, exactly as §4.5a writes `"<page> — <text>"`. */
const PAGE_SEPARATOR = ' — ';

/**
 * Splits into grapheme clusters when the runtime has `Intl.Segmenter` (Node 16+, every browser
 * this app supports), and into code points otherwise. The fallback still satisfies "count Unicode
 * code points" and still never splits a surrogate pair — it can only split a combining sequence
 * or a ZWJ emoji, which is strictly better than splitting a code unit.
 */
function graphemes(value: string): string[] {
	const Segmenter = (Intl as { Segmenter?: typeof Intl.Segmenter }).Segmenter;
	if (typeof Segmenter === 'function') {
		const segmenter = new Segmenter(undefined, { granularity: 'grapheme' });
		return Array.from(segmenter.segment(value), (s) => s.segment);
	}
	return Array.from(value);
}

/** Code-point length, so a surrogate pair counts as one. `String.length` counts UTF-16 units. */
function codePointLength(value: string): number {
	let n = 0;
	for (const _ of value) n += 1;
	return n;
}

/**
 * Cuts `value` to at most `limit` code points without splitting a grapheme cluster. Whole clusters
 * are taken while the running code-point count still fits, so a cluster that would straddle the
 * limit is dropped rather than halved.
 */
function clampToCodePoints(value: string, limit: number): string {
	if (codePointLength(value) <= limit) return value;

	let out = '';
	let used = 0;
	for (const cluster of graphemes(value)) {
		const size = codePointLength(cluster);
		if (used + size > limit) break;
		out += cluster;
		used += size;
	}
	return out;
}

/** §4.5a step 1: trim, then collapse every internal whitespace run to a single space. */
export function normalizeReportText(text: string): string {
	return text.trim().replace(/\s+/g, ' ');
}

/**
 * §4.5a step 2 in isolation, exported because the endpoint's evidence and the table-driven test
 * both want the truncation without the `page` prefix and the outer clamp on top of it.
 */
export function truncateAtWordBoundary(
	normalized: string,
	limit: number = TITLE_TRUNCATE_CODE_POINTS
): string {
	if (codePointLength(normalized) <= limit) return normalized;

	// The candidate window is the first `limit` code points, cut on a grapheme boundary.
	const window = clampToCodePoints(normalized, limit);

	// "cutting at the last whitespace boundary at or before 80". After step 1 the only internal
	// whitespace is a single U+0020, so the last space inside the window is that boundary. A word
	// longer than the window has none, and then the hard cut above stands (see the header note).
	const lastSpace = window.lastIndexOf(' ');
	const cut = lastSpace > 0 ? window.slice(0, lastSpace) : window;

	return `${cut}${ELLIPSIS}`;
}

export interface DeriveReportTitleInput {
	/** §4.1's `text`, as the caller sent it — trimming and collapsing happen here. */
	text: string;
	/** §4.1's `context.page`. Absent, null, or whitespace-only all mean "no prefix". */
	page?: string | null;
}

/**
 * Derives `issues.message` for a report whose caller supplied no `title` (§4.5a).
 *
 * Total: every input, including an empty one, yields a non-empty title, because `issues.message`
 * is NOT NULL and §4.5a says there is no path that may fail here.
 */
export function deriveReportTitle(input: DeriveReportTitleInput): string {
	const normalized = normalizeReportText(input.text ?? '');

	// Step 5, evaluated on `text` (see the header note on why this is not a step-3 substitution).
	if (normalized.length === 0) return TITLE_FALLBACK;

	const truncated = truncateAtWordBoundary(normalized);

	const page = typeof input.page === 'string' ? normalizeReportText(input.page) : '';
	const composed = page.length > 0 ? `${page}${PAGE_SEPARATOR}${truncated}` : truncated;

	return clampToCodePoints(composed, TITLE_MAX_CODE_POINTS);
}
