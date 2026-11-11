// The one tag-link encoder, shared by sentinel-occurrence-detail-panels (spec §6.3) and sentinel-issue-tag-facets
// (spec §6.1). Contract decisions 6 and 7: each part is encoded before the join, the joined string is encoded again
// as one query value, and URLSearchParams is never used to build the href.
export function formatTagParam(key: string, value: string): string {
	return encodeURIComponent(key) + ':' + encodeURIComponent(value);
}

export function tagSearchHref(key: string, value: string): string {
	return '/search?tag=' + encodeURIComponent(formatTagParam(key, value));
}

// sentinel-issue-tag-facets spec §6.1-§6.2 (TASK-03): the bounds a rollup key and value have, the `tag`
// parameter parser, and the free-text box's `key:value` grammar. Appended below the shared seed, which
// is left byte-identical (plan §5.1).

/** A tag key: what the proto and the rollup accept (K3). A key may contain ':'. */
export const TAG_KEY_PATTERN = /^[a-zA-Z0-9_.:-]{1,32}$/;
/** A named rollup value is 1 to 1024 bytes (V3). */
export const MAX_TAG_VALUE_BYTES = 1024;
/** At most this many tag filters per search; the rest are ignored and listed, never an error. */
export const MAX_TAG_FILTERS = 10;

export interface TagFilter {
	key: string;
	value: string;
}

const utf8 = new TextEncoder();

function validPair(key: string, value: string): TagFilter | null {
	if (!TAG_KEY_PATTERN.test(key)) return null; // P5: no row can have it (K3)
	if (value === '' || utf8.encode(value).length > MAX_TAG_VALUE_BYTES) return null; // P3, P6 (V3)
	return { key, value };
}

/**
 * One `url.searchParams.getAll('tag')` entry, already query-decoded once. Exactly one literal ':' must
 * remain (each side was encodeURIComponent-ed before the join, decision 8); each side is then
 * decodeURIComponent-ed. Returns null, and the entry is ignored, for P1 (no ':'), P2 (more than one),
 * P3 (an empty side), P4 (a decode throws), P5 (a key outside TAG_KEY_PATTERN) and P6 (a value over
 * 1024 UTF-8 bytes). The value is not trimmed or normalised (V1).
 */
export function parseTagParam(raw: string): TagFilter | null {
	const at = raw.indexOf(':');
	if (at < 0 || raw.indexOf(':', at + 1) >= 0) return null; // P1, P2
	const rawKey = raw.slice(0, at);
	const rawValue = raw.slice(at + 1);
	if (rawKey === '' || rawValue === '') return null; // P3
	let key: string;
	let value: string;
	try {
		key = decodeURIComponent(rawKey);
		value = decodeURIComponent(rawValue);
	} catch {
		return null; // P4
	}
	return validPair(key, value);
}

const WHITESPACE = /\s/;
const BARE_KEY_CHAR = /[a-zA-Z0-9_.-]/;

interface Token {
	start: number;
	end: number;
	text: string;
	/** A ':' outside any double-quoted segment. */
	unquotedColon: boolean;
}

// Whitespace separates tokens except inside double quotes; a backslash inside quotes escapes the next
// character for the purpose of finding the closing quote.
function tokenize(text: string): Token[] {
	const tokens: Token[] = [];
	let i = 0;
	while (i < text.length) {
		if (WHITESPACE.test(text[i])) {
			i++;
			continue;
		}
		const start = i;
		let inQuote = false;
		let unquotedColon = false;
		while (i < text.length && (inQuote || !WHITESPACE.test(text[i]))) {
			const c = text[i];
			if (inQuote) {
				if (c === '\\' && i + 1 < text.length) i++;
				else if (c === '"') inQuote = false;
			} else if (c === '"') inQuote = true;
			else if (c === ':') unquotedColon = true;
			i++;
		}
		tokens.push({ start, end: i, text: text.slice(start, i), unquotedColon });
	}
	return tokens;
}

// A quoted string starting at `from` (which is a '"'): '\"' is '"', '\\' is '\', any other '\x' is kept
// as '\x'. Returns the unescaped content and the index after the closing quote, or null if unterminated.
function readQuoted(s: string, from: number): { value: string; next: number } | null {
	let out = '';
	for (let i = from + 1; i < s.length; i++) {
		const c = s[i];
		if (c === '\\' && i + 1 < s.length) {
			const n = s[i + 1];
			out += n === '"' || n === '\\' ? n : c + n;
			i++;
		} else if (c === '"') {
			return { value: out, next: i + 1 };
		} else {
			out += c;
		}
	}
	return null;
}

// tag := key ':' value; key := bare-key | quoted (a key containing ':' must be quoted); value := quoted |
// bare-value (non-whitespace, not starting with '"', may contain ':').
function parseTagToken(tok: string): TagFilter | null {
	let key: string;
	let i: number;
	if (tok.startsWith('"')) {
		const q = readQuoted(tok, 0);
		if (!q) return null;
		key = q.value;
		i = q.next;
	} else {
		i = 0;
		while (i < tok.length && BARE_KEY_CHAR.test(tok[i])) i++;
		key = tok.slice(0, i);
	}
	if (key === '' || tok[i] !== ':') return null;
	i++;
	let value: string;
	if (tok[i] === '"') {
		const q = readQuoted(tok, i);
		if (!q || q.next !== tok.length) return null;
		value = q.value;
	} else {
		value = tok.slice(i);
	}
	return validPair(key, value);
}

export interface ParsedSearchText {
	/** The box's valid `key:value` tokens, in order. */
	filters: TagFilter[];
	/** What goes to full-text search: the original string when it holds no tag token and no dropped
	 *  token, otherwise the string with those tokens' spans removed (search.ts re-splits on whitespace). */
	remainder: string;
	/** Tokens with an unquoted ':' that are not a valid tag: dropped, never sent to full-text search. */
	ignored: string[];
}

/** Sentry-style `key:value` tokens in the search box (spec §6.2). Every other token is full-text search. */
export function parseSearchText(text: string): ParsedSearchText {
	const filters: TagFilter[] = [];
	const ignored: string[] = [];
	const removed: [number, number][] = [];
	for (const tok of tokenize(text)) {
		const tag = parseTagToken(tok.text);
		if (tag) {
			filters.push(tag);
			removed.push([tok.start, tok.end]);
		} else if (tok.unquotedColon) {
			ignored.push(tok.text);
			removed.push([tok.start, tok.end]);
		}
	}
	if (removed.length === 0) return { filters, remainder: text, ignored };
	let remainder = '';
	let at = 0;
	for (const [s, e] of removed) {
		remainder += text.slice(at, s);
		at = e;
	}
	remainder += text.slice(at);
	return { filters, remainder, ignored };
}

function quote(s: string): string {
	return '"' + s.replace(/[\\"]/g, (c) => '\\' + c) + '"';
}

/**
 * The canonical box token for a filter: the key quoted iff it contains ':'; the value quoted iff it
 * contains whitespace, '"' or '\', or starts with '"'; inside quotes '"' is written '\"' and '\' '\\'.
 */
export function formatSearchToken(key: string, value: string): string {
	const k = key.includes(':') ? quote(key) : key;
	const v = /[\s"\\]/.test(value) || value.startsWith('"') ? quote(value) : value;
	return `${k}:${v}`;
}
