import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { sentryProjectId } from './sentry-project-id';

// P6.1 (sentinel-sentry-compat-ingest spec §11.2): the derivation agrees with the Go one on the SAME file, so
// neither half can pass against a private copy. The Go half is tests/unit/sentry_compat_auth_test.go
// (TestSentryProjectIDGolden, TestSentryProjectIDVectorsShared).
const VECTORS = join(dirname(fileURLToPath(import.meta.url)), '../../../../tests/fixtures/sentry/project-id-vectors.json');
const vectors = JSON.parse(readFileSync(VECTORS, 'utf8')) as {
	valid: { uuid: string; sentry_project_id: string }[];
	invalid: string[];
};

describe('sentryProjectId matches the Go golden vector', () => {
	it('reads at least the seven valid rows of spec §3.4', () => {
		expect(vectors.valid.length).toBeGreaterThanOrEqual(7);
		expect(vectors.invalid.length).toBeGreaterThanOrEqual(7);
	});

	for (const row of vectors.valid) {
		it(`${row.uuid} -> ${row.sentry_project_id}`, () => {
			expect(sentryProjectId(row.uuid)).toBe(row.sentry_project_id);
		});
	}

	for (const bad of vectors.invalid) {
		it(`throws on ${JSON.stringify(bad)}`, () => {
			expect(() => sentryProjectId(bad)).toThrow();
		});
	}

	it('stays exact at the 2^52 - 1 maximum, below Number.MAX_SAFE_INTEGER', () => {
		const max = sentryProjectId('ffffffff-ffff-ffff-ffff-ffffffffffff');
		expect(max).toBe('4503599627370495');
		expect(Number(max)).toBeLessThan(Number.MAX_SAFE_INTEGER);
		expect(String(Number(max))).toBe(max);
	});

	it('is case-insensitive and dash-insensitive', () => {
		expect(sentryProjectId('0192F3A4-5B6C-4D7E-8F90-A1B2C3D4E5F6')).toBe('27690631804612');
		expect(sentryProjectId('0192f3a45b6c4d7e8f90a1b2c3d4e5f6')).toBe('27690631804612');
	});
});
