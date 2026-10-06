import { describe, it, expect, vi, beforeEach } from 'vitest';

// sentinel-occurrence-spans TASK-02: the dashboard's pull endpoint. The service's rules (states, limit, audit) are
// span-pull.integration.test.ts's; this file owns the HTTP layer: auth, the 404-not-403 convention, the scope the request is
// served under, content type, and the status codes.

const authMock = vi.fn();
const resolveViewerSpanScope = vi.fn();
vi.mock('$lib/server/span-scope', () => ({ resolveViewerSpanScope }));
const getSpansState = vi.fn();
vi.mock('$lib/server/queries/span-queries', () => ({ getSpansState }));
const earliestTraceOccurrenceAt = vi.fn();
vi.mock('$lib/server/queries/trace-queries', () => ({ earliestTraceOccurrenceAt }));
const requestSpanPull = vi.fn();
class SpanPullError extends Error {
	constructor(public code: string, message: string, public retryAfterSeconds?: number) {
		super(message);
	}
}
const isSpanPullEnabled = vi.fn(() => true);
vi.mock('$lib/server/span-pull', () => ({ requestSpanPull, SpanPullError, isSpanPullEnabled, getSpanPullConfig: () => ({ scopes: new Map(), limitPerHour: 30 }) }));

const { GET, POST } = await import('./+server');

const ORG = '11111111-1111-4111-8111-111111111111';
const TRACE = 'a'.repeat(32);
const SCOPE = { orgId: ORG, projects: [{ id: 'p1' }], environmentScopes: new Map() };

const post = (body: unknown, headers: Record<string, string> = { 'content-type': 'application/json' }) =>
	(POST as any)({
		locals: { auth: authMock },
		request: new Request('http://localhost/api/spans/pull', { method: 'POST', headers, body: typeof body === 'string' ? body : JSON.stringify(body) }),
	});
const get = (qs: string) =>
	(GET as any)({ locals: { auth: authMock }, url: new URL(`http://localhost/api/spans/pull${qs}`) });

beforeEach(() => {
	vi.clearAllMocks();
	authMock.mockResolvedValue({ user: { id: 'user-1' } });
	resolveViewerSpanScope.mockResolvedValue(SCOPE);
	requestSpanPull.mockResolvedValue({ status: 'pending', reason: null, action: 'queued', requestId: 'r1' });
	earliestTraceOccurrenceAt.mockResolvedValue(new Date());
	getSpansState.mockResolvedValue({ status: 'none', reason: 'not_pulled' });
	isSpanPullEnabled.mockReturnValue(true);
});

describe('POST /api/spans/pull', () => {
	it('401 without a session, and nothing is requested', async () => {
		authMock.mockResolvedValue(null);
		await expect(post({ organizationId: ORG, traceId: TRACE })).rejects.toMatchObject({ status: 401 });
		expect(requestSpanPull).not.toHaveBeenCalled();
	});

	it('requests the pull AS THE SESSION USER under the scope built for that user and organization', async () => {
		const res = await post({ organizationId: ORG.toUpperCase(), traceId: TRACE.toUpperCase() });
		expect(res.status).toBe(200);
		expect(await res.json()).toEqual({ status: 'pending', reason: null, action: 'queued', enabled: true });
		expect(resolveViewerSpanScope).toHaveBeenCalledWith('user-1', ORG);
		expect(requestSpanPull).toHaveBeenCalledWith(expect.objectContaining({ traceId: TRACE, scope: SCOPE, requester: { kind: 'user', id: 'user-1' } }));
	});

	it('404 — never 403 — for a viewer with no access to the organization, a malformed trace id or organization id', async () => {
		resolveViewerSpanScope.mockResolvedValue(null);
		await expect(post({ organizationId: ORG, traceId: TRACE })).rejects.toMatchObject({ status: 404 });
		expect(requestSpanPull).not.toHaveBeenCalled();
		resolveViewerSpanScope.mockResolvedValue(SCOPE);
		await expect(post({ organizationId: ORG, traceId: 'nope' })).rejects.toMatchObject({ status: 404 });
		await expect(post({ organizationId: 'org-1', traceId: TRACE })).rejects.toMatchObject({ status: 404 });
		await expect(post({})).rejects.toMatchObject({ status: 404 });
		expect(requestSpanPull).not.toHaveBeenCalled();
	});

	it('404 when the service cannot find an occurrence the viewer may see; 403 only for an organization that is not allow-listed', async () => {
		requestSpanPull.mockRejectedValueOnce(new SpanPullError('not_found', 'Trace not found'));
		await expect(post({ organizationId: ORG, traceId: TRACE })).rejects.toMatchObject({ status: 404 });
		requestSpanPull.mockRejectedValueOnce(new SpanPullError('not_allowed', 'Span pull is not enabled for this organization'));
		await expect(post({ organizationId: ORG, traceId: TRACE })).rejects.toMatchObject({ status: 403 });
	});

	it('429 with Retry-After when the organization is over its hourly limit', async () => {
		requestSpanPull.mockRejectedValue(new SpanPullError('rate_limited', 'Too many pulls', 540));
		const res = await post({ organizationId: ORG, traceId: TRACE });
		expect(res.status).toBe(429);
		expect(res.headers.get('retry-after')).toBe('540');
		expect(await res.json()).toEqual({ message: 'Too many pulls' });
	});

	it('is CSRF-safe: a content type a cross-site <form> can send is refused before anything is read', async () => {
		await expect(post({ organizationId: ORG, traceId: TRACE }, { 'content-type': 'text/plain' })).rejects.toMatchObject({ status: 415 });
		await expect(post({ organizationId: ORG, traceId: TRACE }, { 'content-type': 'application/x-www-form-urlencoded' })).rejects.toMatchObject({ status: 415 });
		await expect(post('not json')).rejects.toMatchObject({ status: 400 });
		expect(requestSpanPull).not.toHaveBeenCalled();
	});

	it('an unexpected failure is not swallowed into a 4xx', async () => {
		requestSpanPull.mockRejectedValue(new Error('db down'));
		await expect(post({ organizationId: ORG, traceId: TRACE })).rejects.toThrow('db down');
	});
});

describe('GET /api/spans/pull (the status poll)', () => {
	it('returns the trace’s state and whether pull is offered', async () => {
		const res = await get(`?organizationId=${ORG}&traceId=${TRACE}`);
		expect(await res.json()).toEqual({ status: 'none', reason: 'not_pulled', enabled: true });
		expect(getSpansState).toHaveBeenCalledWith(TRACE, SCOPE);
	});

	it('404 for a trace the viewer can see no occurrence of: the poll is no oracle for trace ids', async () => {
		earliestTraceOccurrenceAt.mockResolvedValue(null);
		await expect(get(`?organizationId=${ORG}&traceId=${TRACE}`)).rejects.toMatchObject({ status: 404 });
		expect(getSpansState).not.toHaveBeenCalled();
	});

	it('404 without access to the organization; 401 without a session', async () => {
		resolveViewerSpanScope.mockResolvedValue(null);
		await expect(get(`?organizationId=${ORG}&traceId=${TRACE}`)).rejects.toMatchObject({ status: 404 });
		authMock.mockResolvedValue(null);
		await expect(get(`?organizationId=${ORG}&traceId=${TRACE}`)).rejects.toMatchObject({ status: 401 });
	});
});
