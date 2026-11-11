import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

// sentinel-reporting-and-dashboard D6 / contract §6: the statuses of POST /api/cron/sli-collect, at the route, with the
// table replaced by an in-memory store and VictoriaMetrics by a stubbed fetch.
const { mockEnv, upsert } = vi.hoisted(() => ({
	mockEnv: {} as Record<string, string | undefined>,
	upsert: vi.fn(async (_rows: unknown[]) => {}),
}));
vi.mock('$env/dynamic/private', () => ({ env: mockEnv }));
vi.mock('$lib/server/observability/log', () => ({ log: { info: vi.fn(), error: vi.fn(), warn: vi.fn(), debug: vi.fn() } }));

vi.mock('$lib/server/sli/store', () => ({ pgSliStore: { hasRows: async () => true, upsert: (rows: unknown[]) => upsert(rows) } }));

import { POST } from './+server';

const CONFIG = JSON.stringify([{ id: 'core-jobs', kind: 'jobs', project: 'core', source: 'example_core_job_handler_runs_total' }]);
const call = (headers: Record<string, string> = {}) =>
	POST({ request: new Request('http://x/api/cron/sli-collect', { method: 'POST', headers }) } as never);

beforeEach(() => {
	for (const k of Object.keys(mockEnv)) delete mockEnv[k];
	Object.assign(mockEnv, { CRON_SECRET: 's3cret', SERVICE_SLI_CONFIG: CONFIG, VICTORIAMETRICS_URL: 'http://vm', SLI_CLUSTER: 'example-production' });
	upsert.mockClear();
});
afterEach(() => vi.unstubAllGlobals());

const vmOk = () =>
	vi.stubGlobal('fetch', vi.fn(async (u: URL) => {
		const q = new URL(String(u)).searchParams;
		const t = Number(q.get('end'));
		return new Response(JSON.stringify({ status: 'success', data: { result: [{ metric: {}, values: [[t, '7']] }] } }));
	}));

describe('POST /api/cron/sli-collect', () => {
	it('answers a missing secret 401, as retention does', async () => {
		expect((await call()).status).toBe(401);
	});
	it('answers a wrong secret 401, as retention does', async () => {
		expect((await call({ 'x-cron-secret': 'nope' })).status).toBe(401);
	});
	it('answers 500 when the server has no CRON_SECRET, as retention does', async () => {
		delete mockEnv.CRON_SECRET;
		expect((await call({ 'x-cron-secret': 'anything' })).status).toBe(500);
	});
	it('answers 200 and writes rows on success', async () => {
		vmOk();
		const res = await call({ 'x-cron-secret': 's3cret' });
		expect(res.status).toBe(200);
		expect(upsert).toHaveBeenCalledTimes(1);
	});
	it('answers 500 and writes nothing when VictoriaMetrics fails', async () => {
		vi.stubGlobal('fetch', vi.fn(async () => new Response('down', { status: 503 })));
		const res = await call({ 'x-cron-secret': 's3cret' });
		expect(res.status).toBe(500);
		expect(upsert).not.toHaveBeenCalled();
	});
	it('answers 200 skipped when nothing is configured (CCE)', async () => {
		delete mockEnv.SERVICE_SLI_CONFIG;
		const res = await call({ 'x-cron-secret': 's3cret' });
		expect(res.status).toBe(200);
		expect(await res.json()).toMatchObject({ skipped: true });
	});
	it('answers 500 on an invalid config', async () => {
		mockEnv.SERVICE_SLI_CONFIG = '[{"id":"x"}]';
		expect((await call({ 'x-cron-secret': 's3cret' })).status).toBe(500);
	});
});
