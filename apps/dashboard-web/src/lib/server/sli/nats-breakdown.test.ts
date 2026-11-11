// @vitest-environment node
import { describe, it, expect, vi } from 'vitest';
import { fetchNatsBreakdown, natsBreakdownQueries } from './nats-breakdown';
import { RANGES } from '../analytics/literals';

const NOW = new Date('2026-10-20T12:00:00Z');
const vec = (rows: [string, string, number][]) => ({ status: 'success', data: { resultType: 'vector', result: rows.map(([stream, consumer, v]) => ({ metric: { stream, consumer }, value: [1, String(v)] })) } });
const respond = (body: unknown, ok = true) => ({ ok, status: ok ? 200 : 503, json: async () => body }) as Response;

describe('natsBreakdownQueries (D8)', () => {
	it('is the contract\'s topk query with a literal cluster and the range as the window', () => {
		const q = natsBreakdownQueries('example-production', '7d');
		expect(q.errors).toBe('topk(5, sum by (stream, consumer) (increase(example_core_nats_handler_runs_total{cluster="example-production", status="error"}[7d])))');
		expect(q.runs).toBe('sum by (stream, consumer) (increase(example_core_nats_handler_runs_total{cluster="example-production"}[7d]))');
	});
	it('takes its window from RANGES only, and rejects anything else', () => {
		for (const r of RANGES) expect(natsBreakdownQueries('c', r).errors).toContain(`[${r}]`);
		expect(() => natsBreakdownQueries('c', '7d]) or vector(1) #' as never)).toThrow();
	});
	it('escapes the cluster literal so it cannot leave its quotes', () => {
		expect(natsBreakdownQueries('a"} or vector(1) #', '24h').errors).toContain('cluster="a\\"} or vector(1) #"');
	});
});

describe('fetchNatsBreakdown', () => {
	const vm = (fetchFn: typeof fetch) => ({ url: 'http://vm:8428', cluster: 'example-production', fetchFn });

	it('joins errors with runs, orders by errors and computes the rate', async () => {
		const fetchFn = vi.fn(async (u: URL | RequestInfo) => {
			const q = new URL(String(u)).searchParams.get('query')!;
			return respond(q.startsWith('topk')
				? vec([['whatsmeowed', 'whatsappConsumer', 25105], ['whatsappv4', 'highThroughputConsumer', 28160]])
				: vec([['whatsmeowed', 'whatsappConsumer', 30539], ['whatsappv4', 'highThroughputConsumer', 723665], ['n', 'other', 4400000]]));
		});
		const r = await fetchNatsBreakdown(vm(fetchFn as never), '7d', NOW);
		expect(r.status).toBe('ok');
		if (r.status !== 'ok') return;
		expect(r.rows.map((x) => x.consumer)).toEqual(['highThroughputConsumer', 'whatsappConsumer']);
		expect(r.rows[0]).toMatchObject({ stream: 'whatsappv4', errors: 28160, runs: 723665 });
		expect(r.rows[1].rate).toBeCloseTo(25105 / 30539);
		expect(fetchFn).toHaveBeenCalledTimes(2); // one query for errors, one for runs
		const u = new URL(String(fetchFn.mock.calls[0][0]));
		expect(u.pathname).toBe('/api/v1/query');
		expect(u.searchParams.get('time')).toBe(String(NOW.getTime() / 1000));
	});

	it('limits to five and drops consumers with no failures', async () => {
		const errs = Array.from({ length: 7 }, (_, i) => ['s', `c${i}`, i] as [string, string, number]);
		const fetchFn = async (u: URL | RequestInfo) => respond(new URL(String(u)).searchParams.get('query')!.startsWith('topk') ? vec(errs) : vec(errs.map(([s, c]) => [s, c, 100])));
		const r = await fetchNatsBreakdown(vm(fetchFn as never), '24h', NOW);
		if (r.status !== 'ok') throw new Error('unavailable');
		expect(r.rows).toHaveLength(5);
		expect(r.rows.map((x) => x.errors)).toEqual([6, 5, 4, 3, 2]);
	});

	it('VictoriaMetrics down: unavailable, never a throw', async () => {
		const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
		expect(await fetchNatsBreakdown(vm((async () => { throw new Error('ECONNREFUSED'); }) as never), '7d', NOW)).toEqual({ status: 'unavailable' });
		expect(await fetchNatsBreakdown(vm((async () => respond({}, false)) as never), '7d', NOW)).toEqual({ status: 'unavailable' });
		expect(await fetchNatsBreakdown(vm((async () => respond({ status: 'error' })) as never), '7d', NOW)).toEqual({ status: 'unavailable' });
		warn.mockRestore();
	});

	it('missing env: unavailable without calling anything', async () => {
		const fetchFn = vi.fn();
		expect(await fetchNatsBreakdown(null, '7d', NOW)).toEqual({ status: 'unavailable' });
		expect(await fetchNatsBreakdown({ url: '', cluster: 'c', fetchFn: fetchFn as never }, '7d', NOW)).toEqual({ status: 'unavailable' });
		expect(fetchFn).not.toHaveBeenCalled();
	});
});
