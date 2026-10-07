import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * sentinel-user-report-issue §4.4a / §4.4b — the two new HTTP surfaces, driven directly.
 *
 * The chain's own transitions are proved against real Postgres in
 * `src/lib/server/escalation.sweep.integration.test.ts` (§11's boundary). What is proved HERE is
 * everything about the two routes that is decided before any row is touched:
 *
 *   - the cron guard, which must be `retention/+server.ts`'s statement for statement — a missing
 *     header is `401` BEFORE the configuration is consulted, an unset `CRON_SECRET` is a `500`
 *     that refuses rather than a silent success, and a wrong secret is the same `401` a missing
 *     one gets. A guard that let an unauthenticated caller drive reassignment would be a way to
 *     move somebody else's pager.
 *   - §4.4a's one fixed code on the ack route: `403` if the caller is not the current assignee —
 *     and, ahead of it, that another tenant's issue id is a `404` rather than a `403`, so the
 *     status code cannot be used to learn that a report exists and who is on it.
 *
 * The escalation MODULE is mocked here on purpose. Its behaviour is asserted where it can be
 * asserted honestly (a real database); a mock of it in this file would only prove the route calls
 * it, which is exactly what these tests are for.
 */

const escalation = {
	sweepEscalations: vi.fn(),
	sendEscalationEmails: vi.fn(),
	getLatestEscalation: vi.fn(),
	acknowledgeEscalation: vi.fn(),
};

vi.mock('$lib/server/escalation', async (importOriginal) => {
	const actual = await importOriginal<typeof import('$lib/server/escalation')>();
	return { ...actual, ...escalation };
});

const access = { requireReportAccessForIssue: vi.fn() };
vi.mock('$lib/server/report-access', () => access);

const dynamicEnv: Record<string, string | undefined> = {};
vi.mock('$env/dynamic/private', () => ({ env: dynamicEnv }));

const { POST: SWEEP } = await import('./cron/oncall-escalation/+server');
const { POST: ACK } = await import('./issues/[issueId=uuid]/escalation/ack/+server');

const ISSUE = '33333333-3333-4333-8333-333333333333';
const ASSIGNEE = 'usr_on_call';
const SCHEDULE = '44444444-4444-4444-8444-444444444444';

function cronRequest(secret: string | null) {
	const headers: Record<string, string> = {};
	if (secret !== null) headers['x-cron-secret'] = secret;
	return {
		request: new Request('http://x/api/cron/oncall-escalation', { method: 'POST', headers }),
		url: new URL('http://x/api/cron/oncall-escalation'),
	} as never;
}

function locals(userId: string | null) {
	return { auth: async () => (userId ? { user: { id: userId } } : null) } as never;
}

function escalationRow(overrides: Record<string, unknown> = {}) {
	return {
		id: '55555555-5555-4555-8555-555555555555',
		issueId: ISSUE,
		scheduleId: SCHEDULE,
		assigneeId: ASSIGNEE,
		hop: 1,
		escalatedAt: new Date('2026-09-05T09:00:00Z'),
		ackDeadlineAt: new Date('2026-09-05T09:15:00Z'),
		acknowledgedAt: null,
		exhausted: false,
		...overrides,
	};
}

const EMPTY_SWEEP = {
	due: 0,
	reassigned: 0,
	exhausted: 0,
	stoppedByReassignment: 0,
	skipped: 0,
	contended: 0,
	notified: [],
};

beforeEach(() => {
	vi.clearAllMocks();
	for (const key of Object.keys(dynamicEnv)) delete dynamicEnv[key];
	dynamicEnv.CRON_SECRET = 'shhh';
	escalation.sweepEscalations.mockResolvedValue(EMPTY_SWEEP);
	escalation.sendEscalationEmails.mockResolvedValue(undefined);
	access.requireReportAccessForIssue.mockResolvedValue({
		issueId: ISSUE,
		projectId: 'p',
		organizationId: 'o',
		role: 'engineer',
	});
});

describe('§4.4b POST /api/cron/oncall-escalation — the guard', () => {
	it('401s a request with no x-cron-secret, without ever running the sweep', async () => {
		const res = await SWEEP(cronRequest(null));
		expect(res.status).toBe(401);
		expect(escalation.sweepEscalations).not.toHaveBeenCalled();
	});

	it('401s a wrong secret with the same status a missing one gets', async () => {
		const res = await SWEEP(cronRequest('wrong'));
		expect(res.status).toBe(401);
		expect(escalation.sweepEscalations).not.toHaveBeenCalled();
	});

	it('500s when CRON_SECRET is unset, rather than accepting anything', async () => {
		// The dangerous shape this rules out is "no secret configured" collapsing into "no secret
		// required": an unauthenticated caller would then be able to drive reassignment.
		delete dynamicEnv.CRON_SECRET;
		const res = await SWEEP(cronRequest('anything'));
		expect(res.status).toBe(500);
		expect(await res.json()).toEqual({ error: 'Cron endpoint not configured' });
		expect(escalation.sweepEscalations).not.toHaveBeenCalled();
	});

	it('runs the sweep and reports its counters on a correct secret', async () => {
		escalation.sweepEscalations.mockResolvedValue({
			...EMPTY_SWEEP,
			due: 3,
			reassigned: 2,
			exhausted: 1,
			notified: [{ issueId: ISSUE, userId: ASSIGNEE }],
		});
		const res = await SWEEP(cronRequest('shhh'));
		expect(res.status).toBe(200);
		const body = await res.json();
		expect(body.success).toBe(true);
		expect(body.result).toMatchObject({ due: 3, reassigned: 2, exhausted: 1 });
		expect(escalation.sweepEscalations).toHaveBeenCalledTimes(1);
	});

	it('sends the escalated emails AFTER the sweep, never inside it', async () => {
		const calls: string[] = [];
		escalation.sweepEscalations.mockImplementation(async () => {
			calls.push('sweep');
			return { ...EMPTY_SWEEP, notified: [{ issueId: ISSUE, userId: ASSIGNEE }] };
		});
		escalation.sendEscalationEmails.mockImplementation(async () => {
			calls.push('email');
		});
		await SWEEP(cronRequest('shhh'));
		// notify.ts §8: an SMTP failure must never roll back the mutation it describes, which is
		// only true if the send happens after the transaction has committed.
		expect(calls).toEqual(['sweep', 'email']);
		expect(escalation.sendEscalationEmails).toHaveBeenCalledWith(
			[{ issueId: ISSUE, userId: ASSIGNEE }],
			'http://x'
		);
	});

	it('500s rather than throwing when the sweep fails', async () => {
		escalation.sweepEscalations.mockRejectedValue(new Error('boom'));
		const res = await SWEEP(cronRequest('shhh'));
		expect(res.status).toBe(500);
	});
});

describe('§4.4a POST /api/issues/{issueId}/escalation/ack', () => {
	const params = { issueId: ISSUE } as never;

	it('401s an unauthenticated caller', async () => {
		await expect(ACK({ params, locals: locals(null) } as never)).rejects.toMatchObject({
			status: 401,
		});
		expect(escalation.acknowledgeEscalation).not.toHaveBeenCalled();
	});

	it('404s an issue the caller cannot reach, before any assignee check', async () => {
		// requireReportAccessForIssue 404s another tenant's issue AND every `system_error` issue id
		// (§9 strict separation). Running it first is what stops the 403 below from telling a
		// stranger that a report exists and that they are not on it.
		const notFound = Object.assign(new Error('Not found'), { status: 404 });
		access.requireReportAccessForIssue.mockRejectedValue(notFound);
		await expect(ACK({ params, locals: locals('someone') } as never)).rejects.toMatchObject({
			status: 404,
		});
		expect(escalation.getLatestEscalation).not.toHaveBeenCalled();
	});

	it('404s a report that has no escalation at all', async () => {
		escalation.getLatestEscalation.mockResolvedValue(null);
		await expect(ACK({ params, locals: locals(ASSIGNEE) } as never)).rejects.toMatchObject({
			status: 404,
		});
		expect(escalation.acknowledgeEscalation).not.toHaveBeenCalled();
	});

	it('403s a caller who is not the current assignee — §4.4a s one fixed code', async () => {
		escalation.getLatestEscalation.mockResolvedValue(escalationRow());
		await expect(ACK({ params, locals: locals('someone_else') } as never)).rejects.toMatchObject({
			status: 403,
		});
		// And it writes nothing: an acknowledgement by a bystander must not stop somebody else's
		// pager.
		expect(escalation.acknowledgeEscalation).not.toHaveBeenCalled();
	});

	it('403s a member with a write role who simply is not the assignee', async () => {
		// §4.4's rule is stricter than any org role: being an owner does not make you the person
		// carrying this report.
		access.requireReportAccessForIssue.mockResolvedValue({
			issueId: ISSUE,
			projectId: 'p',
			organizationId: 'o',
			role: 'owner',
		});
		escalation.getLatestEscalation.mockResolvedValue(escalationRow());
		await expect(ACK({ params, locals: locals('the_owner') } as never)).rejects.toMatchObject({
			status: 403,
		});
	});

	it('200s the assignee and stamps acknowledged_at', async () => {
		const at = new Date('2026-09-05T09:05:00Z');
		escalation.getLatestEscalation.mockResolvedValue(escalationRow());
		escalation.acknowledgeEscalation.mockResolvedValue(escalationRow({ acknowledgedAt: at }));

		const res = await ACK({ params, locals: locals(ASSIGNEE) } as never);
		expect(res.status).toBe(200);
		const body = await res.json();
		expect(body.success).toBe(true);
		expect(body.escalation.acknowledgedAt).toBe(at.toISOString());
		expect(body.escalation.hop).toBe(1);
		expect(escalation.acknowledgeEscalation).toHaveBeenCalledWith(ISSUE, ASSIGNEE);
	});

	it('is idempotent: a second press is a 200, not a conflict', async () => {
		const at = new Date('2026-09-05T09:05:00Z');
		escalation.getLatestEscalation.mockResolvedValue(escalationRow({ acknowledgedAt: at }));
		// The conditional UPDATE matches nothing the second time, deliberately — it must not move
		// the timestamp.
		escalation.acknowledgeEscalation.mockResolvedValue(null);

		const res = await ACK({ params, locals: locals(ASSIGNEE) } as never);
		expect(res.status).toBe(200);
		expect((await res.json()).escalation.acknowledgedAt).toBe(at.toISOString());
	});
});
