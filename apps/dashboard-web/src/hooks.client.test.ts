import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { handleError } from './hooks.client';

describe('hooks.client handleError version skew recovery', () => {
	let originalWindow: any;
	let reloadMock: any;
	let sessionStorageMock: Record<string, string>;

	beforeEach(() => {
		sessionStorageMock = {};
		reloadMock = vi.fn();

		vi.stubGlobal('sessionStorage', {
			getItem: vi.fn((key: string) => sessionStorageMock[key] ?? null),
			setItem: vi.fn((key: string, value: string) => {
				sessionStorageMock[key] = value;
			}),
			removeItem: vi.fn((key: string) => {
				delete sessionStorageMock[key];
			}),
			clear: vi.fn(() => {
				sessionStorageMock = {};
			})
		});

		vi.stubGlobal('location', {
			reload: reloadMock
		});
	});

	afterEach(() => {
		vi.unstubAllGlobals();
	});

	it('triggers window.location.reload() when error is a dynamic import failure', async () => {
		const result = await handleError({
			error: new TypeError('Failed to fetch dynamically imported module: https://sentinel.daya.ai/_app/immutable/chunks/abc.js'),
			event: {} as any,
			status: 500,
			message: 'Internal Error'
		});

		expect(reloadMock).toHaveBeenCalledTimes(1);
		expect(result?.message).toBe('Updating to the latest version...');
	});

	it('does not trigger reload loop if reloaded recently (< 10s)', async () => {
		sessionStorageMock['sentinel_version_skew_reload'] = String(Date.now());

		const result = await handleError({
			error: new TypeError('Failed to fetch dynamically imported module: https://sentinel.daya.ai/_app/immutable/chunks/abc.js'),
			event: {} as any,
			status: 500,
			message: 'Internal Error'
		});

		expect(reloadMock).not.toHaveBeenCalled();
		expect(result?.message).toBe('Internal Error');
	});

	it('returns regular error message without reload for non-skew errors', async () => {
		const result = await handleError({
			error: new Error('ReferenceError: foo is not defined'),
			event: {} as any,
			status: 500,
			message: 'Custom message'
		});

		expect(reloadMock).not.toHaveBeenCalled();
		expect(result?.message).toBe('Custom message');
	});
});
