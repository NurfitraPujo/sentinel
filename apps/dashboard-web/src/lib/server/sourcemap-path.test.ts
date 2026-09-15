import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import crypto from 'node:crypto';
import { normalizeMinifiedPath, h16, sha256Hex, sourcemapStorageKey } from './sourcemap-path';

/**
 * sentinel-sourcemaps contract §4.4 step 1 / §11 (decision 22): the TypeScript path normaliser
 * against the ONE shared fixture, `tests/fixtures/sourcemaps/path-normalisation.json`, that the Go
 * twin (`apps/processor-go/symbolicate/normalize_test.go`) also reads. Every row, read from the
 * file at test time -- never a copy of it -- so an edit to the fixture is an edit to this test, and
 * the two implementations cannot drift without one of the two suites going red.
 */

const FIXTURE = path.resolve(
	path.dirname(fileURLToPath(import.meta.url)),
	'../../../../../tests/fixtures/sourcemaps/path-normalisation.json'
);

interface NormalisationCase {
	in: string;
	out: string;
}

const cases: NormalisationCase[] = JSON.parse(readFileSync(FIXTURE, 'utf8'));

describe('normalizeMinifiedPath — every row of the shared fixture', () => {
	it('reads a non-empty fixture with the cases the contract names', () => {
		expect(cases.length).toBeGreaterThan(0);
		const inputs = cases.map((c) => c.in);
		// The seven §4.4 step 1 (c) cases, a port, host-with-no-path, an upper-case scheme, a query
		// containing a slash, a fragment, both, and a cache-buster on an absolute URL.
		for (const required of [
			'webpack://ns/./src/x.ts',
			'webpack:///./src/x.ts',
			'file:///Users/a/x.js',
			'blob:https://app.example.com/uuid',
			'chrome-extension://abc/x.js',
			'/assets/x.js',
			'x.js',
			'https://app.example.com:8443/a.js',
			'https://app.example.com',
			'HTTPS://app.example.com/a.js',
			'/a.js?x=/b',
			'/a.js#L1',
			'/a.js?x=1#L1',
		]) {
			expect(inputs).toContain(required);
		}
		expect(inputs.some((s) => /^https?:\/\/.*\?v=/.test(s))).toBe(true);
	});

	it.each(cases.map((c) => [c.in, c.out]))('%j → %j', (input, expected) => {
		expect(normalizeMinifiedPath(input)).toBe(expected);
	});

	it('is idempotent on every fixture output — a stored path re-normalises to itself', () => {
		for (const c of cases) {
			expect(normalizeMinifiedPath(c.out)).toBe(c.out);
		}
	});
});

describe('h16 / sha256Hex / sourcemapStorageKey (§4.1 phase 2, decisions 5 and 17)', () => {
	it('h16 is the first 16 hex of sha256 over the string', () => {
		const full = crypto.createHash('sha256').update('2026.09.14-3f2a1c').digest('hex');
		expect(h16('2026.09.14-3f2a1c')).toBe(full.slice(0, 16));
		expect(h16('2026.09.14-3f2a1c')).toMatch(/^[0-9a-f]{16}$/);
	});

	it('sha256Hex is the 64-hex digest of the raw bytes', () => {
		expect(sha256Hex(new Uint8Array(0))).toBe(
			'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855'
		);
	});

	it('builds sourcemaps/{project}/{h16(release)}/{h16(path)}-{sha256}.map with no user string in it', () => {
		const sha = 'a'.repeat(64);
		const key = sourcemapStorageKey('7c9e6679-7425-40de-944b-e07fc1f90ae7', 'v1/../../x', '/../etc/passwd', sha);
		expect(key).toBe(
			`sourcemaps/7c9e6679-7425-40de-944b-e07fc1f90ae7/${h16('v1/../../x')}/${h16('/../etc/passwd')}-${sha}.map`
		);
		expect(key).not.toContain('..');
		expect(key).not.toContain('passwd');
		expect(key.length).toBeLessThanOrEqual(1024);
	});

	it('folds the path in, so identical bytes under two paths are two distinct keys (decision 17)', () => {
		const sha = 'b'.repeat(64);
		expect(sourcemapStorageKey('p', 'r', '/a.js', sha)).not.toBe(sourcemapStorageKey('p', 'r', '/b.js', sha));
	});
});
