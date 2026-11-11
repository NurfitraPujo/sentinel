import { describe, it, expect } from 'vitest';
import { accessPredicate, projectPredicate, type AccessFilter } from './access';
import { coerceRange, enumLiteral, LiteralError, tsLiteral, uuidLiteral } from './literals';
import { createRun, DatabendUnavailableError } from './databend';
import { groupCase, sliEventMatch } from './metrics';

const P1 = '11111111-1111-1111-1111-111111111111';
const P2 = '22222222-2222-2222-2222-222222222222';
const f = (over: Partial<AccessFilter> = {}): AccessFilter => ({ scopes: [], environment: 'all', knownEnvironments: ['production', 'staging'], ...over });

describe('accessPredicate (D11)', () => {
	it('no accessible project is the constant-false predicate: nothing, not even a count', () => {
		expect(accessPredicate(f(), 'i.project_id', 'o.environment')).toBe('(1 = 0)');
		expect(projectPredicate(f(), 'i.project_id')).toBe('(1 = 0)');
	});
	it('an unrestricted project constrains the project only', () => {
		expect(accessPredicate(f({ scopes: [{ projectId: P1, environments: null }] }), 'i.project_id', 'o.environment')).toBe(`((i.project_id = '${P1}'))`);
	});
	it('a restricted scope lists exactly its environments; an empty one contributes nothing', () => {
		const p = accessPredicate(f({ scopes: [{ projectId: P1, environments: ['production'] }, { projectId: P2, environments: [] }] }), 'i.project_id', 'o.environment');
		expect(p).toContain(`o.environment IN ('production')`);
		expect(p).not.toContain(P2);
	});
	it('the environment filter ANDs with the scope, never widens it', () => {
		const p = accessPredicate(f({ environment: 'staging', scopes: [{ projectId: P1, environments: ['production'] }] }), 'i.project_id', 'o.environment');
		expect(p).toContain(`o.environment IN ('production') AND o.environment = 'staging'`);
	});
	it('an environment name outside the allow-list never reaches SQL', () => {
		expect(() => accessPredicate(f({ environment: "x' OR 1=1 --", scopes: [{ projectId: P1, environments: null }] }), 'i.project_id', 'o.environment')).toThrow(LiteralError);
		expect(() => accessPredicate(f({ scopes: [{ projectId: P1, environments: ["a'b"] }] }), 'i.project_id', 'o.environment')).toThrow(LiteralError);
	});
});

describe('literals (D14)', () => {
	it('ids must be UUIDs', () => {
		expect(uuidLiteral(P1.toUpperCase())).toBe(`'${P1}'`);
		for (const bad of ["'; DROP TABLE issues; --", 'not-a-uuid', '', P1 + ' OR 1=1']) expect(() => uuidLiteral(bad)).toThrow(LiteralError);
	});
	it('enums come from allow-lists and are escaped regardless', () => {
		expect(() => enumLiteral('x', ['y'])).toThrow(LiteralError);
		expect(enumLiteral("it's", ["it's"])).toBe("'it''s'");
	});
	it('timestamps are server-generated Dates', () => {
		expect(tsLiteral(new Date('2026-10-07T12:00:00.123Z'))).toBe("'2026-10-07 12:00:00'");
		expect(() => tsLiteral(new Date('nope'))).toThrow(LiteralError);
	});
	it('range is an enum with a default', () => {
		expect(coerceRange('28d')).toBe('28d');
		expect(coerceRange("7d'; --")).toBe('7d');
		expect(coerceRange(null)).toBe('7d');
	});
});

describe('D19 groups and D7 event matching', () => {
	it('group 1 needs request_scoped AND an http SLI service, in decision order', () => {
		const c = groupCase(['api']);
		expect(c.indexOf("'api'")).toBeLessThan(c.indexOf("'jobs'"));
		expect(c.indexOf("'jobs'")).toBeLessThan(c.indexOf("'nats'"));
		expect(c.indexOf("'nats'")).toBeLessThan(c.indexOf("'other'"));
		expect(c.indexOf("'other'")).toBeLessThan(c.indexOf("'unattributed'"));
		expect(c).toContain("o.request_scoped AND o.service IN ('api')");
		expect(groupCase([])).toContain('WHEN o.request_scoped THEN');
	});
	it('sliEventMatch follows D7', () => {
		expect(sliEventMatch({ id: 'a', kind: 'http', project: 'p', source: 's', service: 'api', windowDays: 28 })).toBe("o.request_scoped AND o.service = 'api'");
		expect(sliEventMatch({ id: 'w', kind: 'http', project: 'p', source: 's', windowDays: 28 })).toBeNull();
		expect(sliEventMatch({ id: 'j', kind: 'jobs', project: 'p', source: 's', windowDays: 28 })).toBe("o.tag_source = 'job'");
		expect(sliEventMatch({ id: 'n', kind: 'nats', project: 'p', source: 's', windowDays: 28 })).toContain("'worker_handler', 'nats_client'");
	});
});

describe('Databend client (D14)', () => {
	it('is unavailable when not configured', async () => {
		await expect(createRun(null)('select 1')).rejects.toBeInstanceOf(DatabendUnavailableError);
	});
	it('maps a refusal, an error body and a network failure to unavailable, and follows next_uri', async () => {
		const cfg = { url: 'http://db:8000', user: 'u', password: 'p', database: 'sentinel' };
		const ok = (body: unknown) => new Response(JSON.stringify(body));
		let call = 0;
		const paged = (async (u: string) => (++call === 1 ? ok({ data: [['1']], next_uri: '/v1/query/x/page/1' }) : ok({ data: [['2']], next_uri: null }))) as unknown as typeof fetch;
		expect(await createRun(cfg, paged)('select')).toEqual([['1'], ['2']]);
		await expect(createRun(cfg, (async () => new Response('', { status: 503 })) as unknown as typeof fetch)('s')).rejects.toBeInstanceOf(DatabendUnavailableError);
		await expect(createRun(cfg, (async () => ok({ error: { message: 'boom' } })) as unknown as typeof fetch)('s')).rejects.toThrow(/boom/);
		await expect(createRun(cfg, (async () => { throw new Error('ECONNREFUSED'); }) as unknown as typeof fetch)('s')).rejects.toBeInstanceOf(DatabendUnavailableError);
	});
	it('sends the SQL with basic auth and the database', async () => {
		let seen: any;
		const fetchFn = (async (_u: string, init: any) => { seen = init; return new Response(JSON.stringify({ data: [], next_uri: null })); }) as unknown as typeof fetch;
		await createRun({ url: 'http://db:8000/', user: 'sentinel_app', password: 'pw', database: 'sentinel' }, fetchFn)('select 1');
		expect(seen.headers.Authorization).toBe('Basic ' + Buffer.from('sentinel_app:pw').toString('base64'));
		expect(JSON.parse(seen.body)).toMatchObject({ sql: 'select 1', session: { database: 'sentinel' } });
	});
});
