import { describe, it, expect, vi, beforeEach } from 'vitest';

// sentinel-affected-users decision 21 — the ONE reading of DATA_RETENTION_DAYS. Read fresh on every
// call from `$env/dynamic/private`, so the env is a mutable object varied per test.
const mockEnv: Record<string, string | undefined> = {};
vi.mock('$env/dynamic/private', () => ({ env: mockEnv }));

const { getRetentionDays } = await import('./retention-window');

beforeEach(() => {
	delete mockEnv.DATA_RETENTION_DAYS;
});

describe('getRetentionDays (decision 21)', () => {
	it('is 30 when DATA_RETENTION_DAYS is unset — the cron route’s default, unchanged', () => {
		expect(getRetentionDays()).toBe(30);
	});

	it('is the parsed integer when DATA_RETENTION_DAYS is set, read on every call', () => {
		mockEnv.DATA_RETENTION_DAYS = '45';
		expect(getRetentionDays()).toBe(45);

		mockEnv.DATA_RETENTION_DAYS = '7';
		expect(getRetentionDays()).toBe(7);
	});
});
