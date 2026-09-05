import { describe, it, expect, beforeEach } from 'vitest';
import {
	resolveReportUploadRpm,
	checkReportUploadRateLimit,
	resetRateLimitKey,
	DEFAULT_REPORT_UPLOAD_RPM,
} from './rate-limit';

/**
 * sentinel-user-report-issue §4.5: `REPORT_UPLOAD_RPM`, the throttle that applies to
 * `POST /api/report/attachments` only, on top of the key's own `rate_limit_rpm`.
 */

beforeEach(() => {
	resetRateLimitKey('report-upload:key-1');
});

describe('resolveReportUploadRpm', () => {
	it('defaults to the contract value of 10 when unset or blank', () => {
		expect(DEFAULT_REPORT_UPLOAD_RPM).toBe(10);
		expect(resolveReportUploadRpm(undefined)).toBe(10);
		expect(resolveReportUploadRpm('  ')).toBe(10);
	});

	it('falls back to the default rather than to "no limit" on garbage, 0 or a negative value', () => {
		expect(resolveReportUploadRpm('not-a-number')).toBe(10);
		expect(resolveReportUploadRpm('0')).toBe(10);
		expect(resolveReportUploadRpm('-5')).toBe(10);
	});

	it('honours a configured positive value, floored to a whole request', () => {
		expect(resolveReportUploadRpm('3')).toBe(3);
		expect(resolveReportUploadRpm('2.9')).toBe(2);
	});
});

describe('checkReportUploadRateLimit', () => {
	it('allows up to the limit, then returns a positive Retry-After', () => {
		for (let i = 0; i < 3; i++) {
			expect(checkReportUploadRateLimit('key-1', 3).allowed).toBe(true);
		}
		const denied = checkReportUploadRateLimit('key-1', 3);
		expect(denied.allowed).toBe(false);
		expect(denied.retryAfter).toBeGreaterThan(0);
	});

	it('is keyed per API key row, so one key cannot spend another key\'s budget', () => {
		for (let i = 0; i < 3; i++) checkReportUploadRateLimit('key-1', 3);
		expect(checkReportUploadRateLimit('key-1', 3).allowed).toBe(false);
		resetRateLimitKey('report-upload:key-2');
		expect(checkReportUploadRateLimit('key-2', 3).allowed).toBe(true);
	});
});
