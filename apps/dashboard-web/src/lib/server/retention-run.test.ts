import { describe, it, expect, vi } from 'vitest';
import { sql } from 'drizzle-orm';
import { issues } from '$lib/db/schema';
import { openRetentionRun, RetentionDeadlineError, RetentionConnectionLost } from './retention-run';

// sentinel-issue-tag-facets spec §5 "One run, one connection, one deadline" rule 2 and §10.2 "retention
// route" cases 1-8: run.db refuses a statement when the connection is lost, past the deadline or under a
// 1 ms cap, and otherwise awaits the set_config pair before it sends each statement.

interface Call {
	kind: 'tagged' | 'unsafe';
	text: string;
	values?: unknown[];
}

function stubClient(pairError?: Error) {
	const calls: Call[] = [];
	const tagged = vi.fn(async (strings: TemplateStringsArray, ...values: unknown[]) => {
		const text = strings.join('$');
		calls.push({ kind: 'tagged', text, values });
		if (pairError && text.includes('set_config')) throw pairError;
		return [];
	});
	const unsafe = vi.fn((text: string) => {
		calls.push({ kind: 'unsafe', text });
		const q = Promise.resolve([] as unknown[]) as unknown as Promise<unknown[]> & { values: () => Promise<unknown[]> };
		q.values = () => Promise.resolve([]);
		return q;
	});
	const client = Object.assign(tagged, { unsafe, options: { parsers: {}, serializers: {} } });
	return { client, calls, unsafe };
}

const isPair = (c: Call) => c.kind === 'tagged' && c.text.includes("set_config('statement_timeout'") && c.text.includes("set_config('lock_timeout'");
const texts = (calls: Call[]) => calls.map((c) => (isPair(c) ? 'PAIR' : c.text));

describe('openRetentionRun (spec §5 rule 2)', () => {
	it('(1) past the deadline a statement is refused and nothing is sent', async () => {
		const { client, calls } = stubClient();
		const run = openRetentionRun(client as never, { deadlineMs: 0, isLost: () => false });
		await expect(run.db.execute(sql`SELECT 1`)).rejects.toBeInstanceOf(RetentionDeadlineError);
		expect(calls).toEqual([]);
	});

	it('(2) once the connection is lost a statement is refused and nothing is sent', async () => {
		const { client, calls } = stubClient();
		const run = openRetentionRun(client as never, { deadlineMs: 60_000, isLost: () => true });
		await expect(run.db.execute(sql`SELECT 1`)).rejects.toBeInstanceOf(RetentionConnectionLost);
		expect(calls).toEqual([]);
	});

	it('(3) every statement is preceded by the set_config pair, carrying the caps or the run bound', async () => {
		const { client, calls } = stubClient();
		const run = openRetentionRun(client as never, { deadlineMs: 60_000, isLost: () => false });
		run.setCaps({ statementMs: 250, lockMs: 100 });
		await run.db.execute(sql`SELECT 1`);
		expect(texts(calls)).toEqual(['PAIR', 'SELECT 1']);
		expect(calls[0].values).toEqual(['250', '100']);

		run.setCaps(null);
		await run.db.execute(sql`SELECT 2`);
		expect(texts(calls)).toEqual(['PAIR', 'SELECT 1', 'PAIR', 'SELECT 2']);
		for (const v of calls[2].values as string[]) {
			expect(Number(v)).toBeLessThanOrEqual(60_000);
			expect(Number(v)).toBeGreaterThan(59_000);
		}
	});

	it('(4) a transaction is BEGIN / statements / COMMIT, each behind its pair; a throw rolls back', async () => {
		const ok = stubClient();
		const run = openRetentionRun(ok.client as never, { deadlineMs: 60_000, isLost: () => false });
		await run.db.transaction(async (tx) => {
			await tx.execute(sql`SELECT 1`);
		});
		expect(texts(ok.calls)).toEqual(['PAIR', 'BEGIN', 'PAIR', 'SELECT 1', 'PAIR', 'COMMIT']);

		const bad = stubClient();
		const run2 = openRetentionRun(bad.client as never, { deadlineMs: 60_000, isLost: () => false });
		const boom = new Error('boom');
		await expect(
			run2.db.transaction(async (tx) => {
				await tx.execute(sql`SELECT 1`);
				throw boom;
			})
		).rejects.toBe(boom);
		expect(texts(bad.calls)).toEqual(['PAIR', 'BEGIN', 'PAIR', 'SELECT 1', 'ROLLBACK']);

		const lostStub = stubClient();
		let lost = false;
		const run3 = openRetentionRun(lostStub.client as never, { deadlineMs: 60_000, isLost: () => lost });
		await expect(
			run3.db.transaction(async () => {
				lost = true;
				throw boom;
			})
		).rejects.toBe(boom);
		expect(texts(lostStub.calls)).toEqual(['PAIR', 'BEGIN']);
	});

	it('(5) a statement past the deadline inside a transaction is refused, and ROLLBACK is still sent', async () => {
		const { client, calls } = stubClient();
		const run = openRetentionRun(client as never, { deadlineMs: 150, isLost: () => false });
		await expect(
			run.db.transaction(async (tx) => {
				await new Promise((r) => setTimeout(r, 100));
				await tx.execute(sql`SELECT 1`);
			})
		).rejects.toBeInstanceOf(RetentionDeadlineError);
		expect(texts(calls)).toEqual(['PAIR', 'BEGIN', 'ROLLBACK']);
	});

	it('(6) a cap below 1 ms is refused, never sent as 0 (no timeout) or negative', async () => {
		for (const caps of [
			{ statementMs: 0, lockMs: 0 },
			{ statementMs: 0.4, lockMs: 250 },
			{ statementMs: 250, lockMs: -5 },
		]) {
			const { client, calls } = stubClient();
			const run = openRetentionRun(client as never, { deadlineMs: 60_000, isLost: () => false });
			run.setCaps(caps);
			await expect(run.db.execute(sql`SELECT 1`)).rejects.toBeInstanceOf(RetentionDeadlineError);
			expect(calls).toEqual([]);
		}
	});

	const closed = () => Object.assign(new Error('write CONNECTION_CLOSED'), { code: 'CONNECTION_CLOSED' });

	it('(7) a failed pair rejects the statement with its error; the statement is never sent; no unhandled rejection', async () => {
		const seen: unknown[] = [];
		const listener = (reason: unknown) => seen.push(reason);
		process.on('unhandledRejection', listener);
		try {
			const err = closed();
			const { client, calls, unsafe } = stubClient(err);
			const run = openRetentionRun(client as never, { deadlineMs: 60_000, isLost: () => false });
			await expect(run.db.execute(sql`SELECT 1`)).rejects.toBe(err);
			expect(texts(calls)).toEqual(['PAIR']);
			expect(unsafe).not.toHaveBeenCalled();
			await new Promise((r) => setImmediate(r));
			expect(seen).toEqual([]);
		} finally {
			process.off('unhandledRejection', listener);
		}
	});

	it('(8) the same through .values() (a builder select)', async () => {
		const seen: unknown[] = [];
		const listener = (reason: unknown) => seen.push(reason);
		process.on('unhandledRejection', listener);
		try {
			const err = closed();
			const { client, calls, unsafe } = stubClient(err);
			const run = openRetentionRun(client as never, { deadlineMs: 60_000, isLost: () => false });
			await expect(run.db.select({ one: sql`1` }).from(issues)).rejects.toBe(err);
			expect(texts(calls)).toEqual(['PAIR']);
			expect(unsafe).not.toHaveBeenCalled();
			await new Promise((r) => setImmediate(r));
			expect(seen).toEqual([]);
		} finally {
			process.off('unhandledRejection', listener);
		}
	});

	it('signal aborts at the deadline', async () => {
		const { client } = stubClient();
		const run = openRetentionRun(client as never, { deadlineMs: 20, isLost: () => false });
		expect(run.signal.aborted).toBe(false);
		await new Promise((r) => setTimeout(r, 60));
		expect(run.signal.aborted).toBe(true);
		expect(run.left()).toBeLessThan(0);
	});
});
