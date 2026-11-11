import { describe, it, expect, vi, beforeEach } from 'vitest';

// Chainable Drizzle-query double, following the pattern in
// src/routes/api/organizations/keys.test.ts: each awaited chain resolves via the queued `then`
// implementation below, in the order the route code issues its queries.
function makeDbMock() {
	const dbMock: any = {
		select: vi.fn(),
		from: vi.fn(),
		innerJoin: vi.fn(),
		where: vi.fn(),
		then: vi.fn(),
	};
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.innerJoin.mockReturnValue(dbMock);
	dbMock.where.mockReturnValue(dbMock);
	// `clearAllMocks` clears call records but NOT queued `mockImplementationOnce` entries, so an
	// un-consumed queued resolution leaks into the NEXT test and answers the wrong query, making
	// results order-dependent. `mockReset` on the queue-bearing mock drops the queue; the base
	// implementation is re-established on the next line.
	dbMock.then.mockReset();
	dbMock.then.mockImplementation((resolve: any) => resolve([]));
	return dbMock;
}

const dbMock = makeDbMock();

// The route imports '$lib/server/db'; the shared helper (src/lib/server/issue-access.ts) imports
// the same file via a relative './db' specifier. Both resolve to this module.
vi.mock('$lib/server/db', () => ({ db: dbMock }));

vi.mock('$lib/db/schema', () => ({
	issues: { id: 'id', projectId: 'projectId', environment: 'environment' },
	projects: { id: 'id', organizationId: 'organizationId' },
	organizationMembers: { organizationId: 'organizationId', userId: 'userId', role: 'role' },
	projectMembers: { projectId: 'projectId', userId: 'userId', role: 'role' },
	// sentinel-project-environments §2.2: `requireIssueAccess` now resolves the caller's environment
	// scope as its last step. The grant read falls through to the db double's base resolution (`[]`)
	// in every test below that does not queue one, which is "no grant row" — unrestricted, the
	// absence-means-all rule — so the role assertions here are unaffected by it. The guard's own
	// behaviour is asserted in `src/lib/server/issue-access.environment.test.ts`.
	projectEnvironmentGrants: {
		userId: 'userId',
		projectId: 'projectId',
		environments: 'environments',
	},
}));

const issueQueries = {
	updateIssueStatus: vi.fn(),
};
vi.mock('$lib/db/queries/issues', () => issueQueries);

// sentinel-user-report-issue §4.4: a status change BY THE ASSIGNEE is an implicit acknowledgement
// and stops the escalation clock. Mocked rather than added to the `$lib/db/schema` stub above:
// what the stamp writes is a database claim, proved in
// `src/lib/server/escalation.sweep.integration.test.ts`; what this route owns is calling it.
const acknowledgeEscalationQuietly = vi.fn();
vi.mock('$lib/server/escalation', () => ({ acknowledgeEscalationQuietly }));

const { PATCH } = await import('./[issueId=uuid]/status/+server');

function locals(session: { id: string } | null) {
	return { auth: async () => (session ? { user: { id: session.id } } : null) } as any;
}

function patchRequest(body: unknown) {
	return new Request('http://x', { method: 'PATCH', body: JSON.stringify(body) });
}

const issueRow = {
	issueId: 'issue-1',
	projectId: 'project-1',
	environment: 'production',
	organizationId: 'org-1',
};

// sentinel-releases-snooze §4.2: the route always forwards the two validated snooze conditions to
// updateIssueStatus; a body without them (every pre-feature call) forwards both as null.
const NO_SNOOZE = { snoozedUntil: null, snoozeOccurrences: null };

describe('PATCH /api/issues/:id/status', () => {
	beforeEach(() => {
		vi.clearAllMocks();
		dbMock.select.mockReturnValue(dbMock);
		dbMock.from.mockReturnValue(dbMock);
		dbMock.innerJoin.mockReturnValue(dbMock);
		dbMock.where.mockReturnValue(dbMock);
		dbMock.then.mockReset();
		dbMock.then.mockImplementation((resolve: any) => resolve([]));
		// N7d (A05-status): updateIssueStatus now returns `{changed, notified}` instead of a bare
		// notified array -- default every test to the "changed" shape so the route's destructure
		// (`const {changed, notified} = await updateIssueStatus(...)`) doesn't blow up on tests that
		// don't care about the return value at all.
		issueQueries.updateIssueStatus.mockResolvedValue({ changed: true, notified: [] });
	});

	it('401s when there is no session', async () => {
		await expect(
			PATCH({
				params: { issueId: 'issue-1' },
				request: patchRequest({ status: 'resolved' }),
				locals: locals(null),
				url: new URL('http://x'),
			} as any)
		).rejects.toMatchObject({ status: 401 });
	});

	it('400s on an invalid status', async () => {
		await expect(
			PATCH({
				params: { issueId: 'issue-1' },
				request: patchRequest({ status: 'bogus' }),
				locals: locals({ id: 'user-1' }),
				url: new URL('http://x'),
			} as any)
		).rejects.toMatchObject({ status: 400 });
	});

	// D10: an org `viewer` is not in the bulk endpoint's write allowlist
	// (owner|admin|engineer|support) and must not be able to resolve issues one at a time either.
	it('403s an org viewer attempting to resolve an issue', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) => resolve([issueRow])) // issue+project lookup
			.mockImplementationOnce((resolve: any) => resolve([{ role: 'viewer' }])); // org role

		await expect(
			PATCH({
				params: { issueId: 'issue-1' },
				request: patchRequest({ status: 'resolved' }),
				locals: locals({ id: 'user-1' }),
				url: new URL('http://x'),
			} as any)
		).rejects.toMatchObject({ status: 403 });
		expect(issueQueries.updateIssueStatus).not.toHaveBeenCalled();
	});

	it.each(['owner', 'admin', 'engineer', 'support'])(
		'allows an org %s who is also a project member to resolve an issue',
		async (role) => {
			dbMock.then
				.mockImplementationOnce((resolve: any) => resolve([issueRow])) // issue+project lookup
				.mockImplementationOnce((resolve: any) => resolve([{ role }])); // org role
			// No third resolution is queued: an org role on the write allowlist short-circuits the
			// `project_members` lookup (D17), and the grant read `requireIssueAccess` now ends with
			// falls through to the base `[]` — no grant row, unrestricted.

			const res = await PATCH({
				params: { issueId: 'issue-1' },
				request: patchRequest({ status: 'resolved' }),
				locals: locals({ id: 'user-1' }),
				url: new URL('http://x'),
			} as any);

			expect(res.status).toBe(200);
			expect(issueQueries.updateIssueStatus).toHaveBeenCalledWith(
				'issue-1',
				'resolved',
				undefined,
				'user',
				'user-1',
				NO_SNOOZE
			);
		}
	);

	// D10 item 2: org role alone is not enough — the caller must also be on the issue's project.
	// This previously asserted that an org ADMIN with no `project_members` row was refused. That
	// model was wrong and tests/e2e U13 proved it: an org admin could not link two issues in their
	// own organization, because `project_members` is populated only for per-project grants, not for
	// org-level staff. An org role on the write allowlist is itself an org-wide grant.
	it('allows an org admin with no project_members row — an org role is an org-wide grant', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) => resolve([issueRow])) // issue+project lookup
			.mockImplementationOnce((resolve: any) => resolve([{ role: 'admin' }])); // org role, no project query

		issueQueries.updateIssueStatus.mockResolvedValueOnce({ changed: true, notified: [] });

		await PATCH({
			params: { issueId: 'issue-1' },
			request: patchRequest({ status: 'resolved' }),
			locals: locals({ id: 'user-1' }),
			url: new URL('http://x'),
		} as any);

		expect(issueQueries.updateIssueStatus).toHaveBeenCalled();
	});

	// D10's actual hole, still closed: an org `viewer` gets no write access from the org role, and
	// with no project membership either, is refused outright.
	it('403s an org viewer with no project membership', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) => resolve([issueRow])) // issue+project lookup
			.mockImplementationOnce((resolve: any) => resolve([{ role: 'viewer' }])) // org role
			.mockImplementationOnce((resolve: any) => resolve([])); // no project membership row

		await expect(
			PATCH({
				params: { issueId: 'issue-1' },
				request: patchRequest({ status: 'resolved' }),
				locals: locals({ id: 'user-1' }),
				url: new URL('http://x'),
			} as any)
		).rejects.toMatchObject({ status: 403 });
		expect(issueQueries.updateIssueStatus).not.toHaveBeenCalled();
	});

	// A project grant conveys READ only — it must never become a backdoor to writing, or the
	// single-issue path would again be more permissive than the bulk one (D23).
	it('403s an org viewer who IS a project member — project grants do not convey write', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) => resolve([issueRow])) // issue+project lookup
			.mockImplementationOnce((resolve: any) => resolve([{ role: 'viewer' }])) // org role
			.mockImplementationOnce((resolve: any) => resolve([{ role: 'developer' }])); // project member

		await expect(
			PATCH({
				params: { issueId: 'issue-1' },
				request: patchRequest({ status: 'resolved' }),
				locals: locals({ id: 'user-1' }),
				url: new URL('http://x'),
			} as any)
		).rejects.toMatchObject({ status: 403 });
		expect(issueQueries.updateIssueStatus).not.toHaveBeenCalled();
	});

	// D10 item 3: resolvedInVersion is validated against the varchar(100) column instead of being
	// passed through, so an oversized value 400s rather than 500ing at the DB.
	it('400s an oversized resolvedInVersion instead of letting it reach the DB', async () => {
		await expect(
			PATCH({
				params: { issueId: 'issue-1' },
				request: patchRequest({ status: 'resolved', resolvedInVersion: 'v'.repeat(101) }),
				locals: locals({ id: 'user-1' }),
				url: new URL('http://x'),
			} as any)
		).rejects.toMatchObject({ status: 400 });
		expect(issueQueries.updateIssueStatus).not.toHaveBeenCalled();
	});

	it('accepts a resolvedInVersion at exactly the 100-char limit', async () => {
		dbMock.then
			.mockImplementationOnce((resolve: any) => resolve([issueRow]))
			.mockImplementationOnce((resolve: any) => resolve([{ role: 'owner' }]));

		const res = await PATCH({
			params: { issueId: 'issue-1' },
			request: patchRequest({ status: 'resolved', resolvedInVersion: 'v'.repeat(100) }),
			locals: locals({ id: 'user-1' }),
			url: new URL('http://x'),
		} as any);

		expect(res.status).toBe(200);
		expect(issueQueries.updateIssueStatus).toHaveBeenCalledWith(
			'issue-1',
			'resolved',
			'v'.repeat(100),
			'user',
			'user-1',
			NO_SNOOZE
		);
	});

	// sentinel-releases-snooze §4.2 (decisions 33, 38, 40) -- flows A1-A5, A11. The validators are
	// REAL here (issue-access is not mocked in this file): what these cases prove is the route's
	// refusal strings, verbatim, and what reaches updateIssueStatus on the happy paths. The
	// agent-route half of "one validator, both routes" is agent-api-spec/contract.test.ts.
	describe('snoozedUntil / snoozeOccurrences (sentinel-releases-snooze §4.2)', () => {
		const FUTURE_Z = '2099-09-20T09:00:00Z';
		const FUTURE_OFFSET = '2099-09-20T16:00:00+07:00'; // the same instant as FUTURE_Z

		/** An org admin on `issue-1` -- the shortest path through the access gate. */
		function grantAdmin() {
			dbMock.then
				.mockImplementationOnce((resolve: any) => resolve([issueRow])) // issue+project lookup
				.mockImplementationOnce((resolve: any) => resolve([{ role: 'admin' }])); // org role
		}
		function patch(body: unknown) {
			return PATCH({
				params: { issueId: 'issue-1' },
				request: patchRequest(body),
				locals: locals({ id: 'user-1' }),
				url: new URL('http://x'),
			} as any);
		}
		const lastSnooze = () => issueQueries.updateIssueStatus.mock.calls.at(-1)?.[5];

		// A1: both conditions, parsed, reach updateIssueStatus.
		it('A1: forwards both conditions to updateIssueStatus with the instant parsed', async () => {
			grantAdmin();
			const res = await patch({ status: 'ignored', snoozedUntil: FUTURE_Z, snoozeOccurrences: 100 });
			expect(res.status).toBe(200);
			expect(await res.json()).toEqual({ success: true, status: 'ignored', changed: true });
			expect(issueQueries.updateIssueStatus).toHaveBeenCalledWith('issue-1', 'ignored', undefined, 'user', 'user-1', {
				snoozedUntil: new Date(FUTURE_Z),
				snoozeOccurrences: 100,
			});
		});

		// A11: an offset is accepted on both routes and is the same instant as its Z form.
		it('A11: a future +07:00 snoozedUntil is accepted and normalised to the instant', async () => {
			grantAdmin();
			const res = await patch({ status: 'ignored', snoozedUntil: FUTURE_OFFSET });
			expect(res.status).toBe(200);
			expect(lastSnooze()).toEqual({ snoozedUntil: new Date(FUTURE_Z), snoozeOccurrences: null });
		});

		// A11 / §3: both blanks are omitted -- the indefinite ignore, exactly today's write.
		it("A11: '' on either key with status ignored is 200 and the indefinite ignore", async () => {
			grantAdmin();
			const res = await patch({ status: 'ignored', snoozedUntil: '', snoozeOccurrences: '' });
			expect(res.status).toBe(200);
			expect(lastSnooze()).toEqual({ snoozedUntil: null, snoozeOccurrences: null });
		});

		it("'' on either key with status resolved is omitted too -- not a 400", async () => {
			grantAdmin();
			const res = await patch({ status: 'resolved', snoozedUntil: '', snoozeOccurrences: '' });
			expect(res.status).toBe(200);
			expect(lastSnooze()).toEqual({ snoozedUntil: null, snoozeOccurrences: null });
		});

		// A2 + A11: ONE string for a past, an unparseable and the two wrong-shaped values
		// (decisions 33, 38 -- the last two would pass Date.parse alone).
		it.each([
			['A2: a past instant', '2020-01-01T00:00:00Z'],
			["A11: an unparseable 'tomorrow'", 'tomorrow'],
			['A11: a date-only 2099-09-20', '2099-09-20'],
			['A11: a zone-less 2099-09-20T09:00', '2099-09-20T09:00'],
			['a non-string', 12345],
		])('400 `snoozedUntil must be a future timestamp`: %s', async (_label, snoozedUntil) => {
			await expect(patch({ status: 'ignored', snoozedUntil })).rejects.toMatchObject({
				status: 400,
				body: { message: 'snoozedUntil must be a future timestamp' },
			});
			expect(issueQueries.updateIssueStatus).not.toHaveBeenCalled();
		});

		// A4: 0, over the cap, and a numeric STRING (only '' is omitted; '12' is refused).
		it.each([0, 1000001, '12'])(
			'A4: 400 `snoozeOccurrences must be an integer between 1 and 1000000`: %j',
			async (snoozeOccurrences) => {
				await expect(patch({ status: 'ignored', snoozeOccurrences })).rejects.toMatchObject({
					status: 400,
					body: { message: 'snoozeOccurrences must be an integer between 1 and 1000000' },
				});
				expect(issueQueries.updateIssueStatus).not.toHaveBeenCalled();
			}
		);

		it('accepts the bounds: 1 and 1000000', async () => {
			grantAdmin();
			await patch({ status: 'ignored', snoozeOccurrences: 1 });
			expect(lastSnooze()).toEqual({ snoozedUntil: null, snoozeOccurrences: 1 });
			grantAdmin();
			await patch({ status: 'ignored', snoozeOccurrences: 1000000 });
			expect(lastSnooze()).toEqual({ snoozedUntil: null, snoozeOccurrences: 1000000 });
		});

		// A3: a snooze key with any status but ignored -- each key has its own string.
		it('A3: 400 `snoozeOccurrences is only valid with status ignored`', async () => {
			await expect(patch({ status: 'resolved', snoozeOccurrences: 5 })).rejects.toMatchObject({
				status: 400,
				body: { message: 'snoozeOccurrences is only valid with status ignored' },
			});
			expect(issueQueries.updateIssueStatus).not.toHaveBeenCalled();
		});

		it('400 `snoozedUntil is only valid with status ignored`', async () => {
			await expect(patch({ status: 'resolved', snoozedUntil: FUTURE_Z })).rejects.toMatchObject({
				status: 400,
				body: { message: 'snoozedUntil is only valid with status ignored' },
			});
			await expect(patch({ status: 'unresolved', snoozedUntil: FUTURE_Z })).rejects.toMatchObject({
				status: 400,
				body: { message: 'snoozedUntil is only valid with status ignored' },
			});
			expect(issueQueries.updateIssueStatus).not.toHaveBeenCalled();
		});

		// The refusal happens BEFORE the access gate, like resolvedInVersion's: no db query is made
		// for an invalid body -- but a valid one still goes through it (the 401/403 matrix above is
		// unchanged: a viewer with a valid snooze body is still 403).
		it('403s an org viewer sending a valid snooze -- the access matrix is unchanged', async () => {
			dbMock.then
				.mockImplementationOnce((resolve: any) => resolve([issueRow]))
				.mockImplementationOnce((resolve: any) => resolve([{ role: 'viewer' }]))
				.mockImplementationOnce((resolve: any) => resolve([]));
			await expect(patch({ status: 'ignored', snoozeOccurrences: 100 })).rejects.toMatchObject({ status: 403 });
			expect(issueQueries.updateIssueStatus).not.toHaveBeenCalled();
		});

		it('401s with no session, whatever the snooze body', async () => {
			await expect(
				PATCH({
					params: { issueId: 'issue-1' },
					request: patchRequest({ status: 'ignored', snoozeOccurrences: 100 }),
					locals: locals(null),
					url: new URL('http://x'),
				} as any)
			).rejects.toMatchObject({ status: 401 });
		});
	});

	describe('§4.4 implicit acknowledgement', () => {
		/** An org admin resolving `issue-1`, which is the shortest path through the access gate above. */
		function resolveAsAdmin() {
			dbMock.then
				.mockImplementationOnce((resolve: any) => resolve([issueRow])) // issue+project lookup
				.mockImplementationOnce((resolve: any) => resolve([{ role: 'admin' }])); // org role
			return PATCH({
				params: { issueId: 'issue-1' },
				request: patchRequest({ status: 'resolved' }),
				locals: locals({ id: 'user-1' }),
				url: new URL('http://x'),
			} as any);
		}

		it('stamps the escalation when the caller changes the status, after the mutation committed', async () => {
			// §4.4: "the assignee claims, comments on, or changes the status of that issue … Someone
			// who is already working on it must not be reassigned for not pressing a button." Whether
			// the caller IS the assignee is decided inside `acknowledgeEscalation`'s own conditional
			// UPDATE, so this route hands it the acting user and decides nothing itself.
			issueQueries.updateIssueStatus.mockResolvedValueOnce({ changed: true, notified: [] });
			await resolveAsAdmin();
			expect(acknowledgeEscalationQuietly).toHaveBeenCalledWith('issue-1', 'user-1');
		});

		it('does not stamp when the status did not actually change', async () => {
			issueQueries.updateIssueStatus.mockResolvedValueOnce({ changed: false, notified: [] });
			await resolveAsAdmin();
			expect(acknowledgeEscalationQuietly).not.toHaveBeenCalled();
		});
	});
});
