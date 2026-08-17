import { describe, it, expect, vi, beforeEach } from 'vitest';
import { issues, projects, issueComments, agentIdempotencyKeys } from '$lib/db/schema';

/**
 * N9 (docs/plans/AGENT_WORKER_PLAN.md C4/C5, D21): the load-bearing acceptance test for
 * client-supplied idempotency on `createComment` -- a blocking question replayed with the SAME
 * idempotency key must return the ORIGINAL question's comment id and fire EXACTLY ONE notification
 * (a blocking question bypasses the 15-min email throttle, so a duplicate here is a double-email to
 * the reporter -- the precise defect N9 exists to close).
 *
 * A purpose-built STATEFUL `tx` double (not the queue doubles in comments.test.ts) is required:
 * idempotency is inherently cross-call, so the fake must actually store the key on the first call
 * and return it on the second. Resolution is by drizzle table IDENTITY (the imported table objects),
 * so the same `select`/`insert` surface serves the issue/project reads, the comment insert, and the
 * idempotency-key store/lookup without a brittle ordered queue.
 */

const notifyIssueEvent = vi.fn(async () => [{ userId: 'reporter-1' }]);
vi.mock('$lib/server/notify', () => ({ notifyIssueEvent }));

vi.mock('$lib/db/queries/subscriptions', () => ({ subscribe: vi.fn() }));
vi.mock('./reports', () => ({ claimDraftAttachmentsForComment: vi.fn() }));
vi.mock('$lib/server/storage', () => ({ deleteObject: vi.fn(), isStorageConfigured: vi.fn(() => false) }));
vi.mock('$lib/server/observability/log', () => ({ log: { error: vi.fn(), warn: vi.fn(), info: vi.fn() } }));

// A single stateful backing store shared across every createComment call in a test.
interface FakeState {
	idempotency: Map<string, { op: string; commentId: string | null }>;
	comments: Map<string, Record<string, unknown>>;
	waitingOn: string | null;
	commentSeq: number;
}

let state: FakeState;

function makeTx() {
	const tx: any = {
		select: (cols?: unknown) => ({
			from: (table: unknown) => ({
				where: (_cond: unknown) => {
					let rows: unknown[] = [];
					if (table === issues) {
						rows = [{ id: 'issue-1', projectId: 'proj-1', waitingOn: state.waitingOn }];
					} else if (table === projects) {
						rows = [{ organizationId: 'org-1' }];
					} else if (table === agentIdempotencyKeys) {
						const hit = [...state.idempotency.values()][0];
						rows = hit ? [{ op: hit.op, commentId: hit.commentId }] : [];
					} else if (table === issueComments) {
						// replay fetch of the original comment by id
						rows = [...state.comments.values()].slice(-1);
					}
					// support both `.where(...)` awaited directly and `.where(...).orderBy(...)`
					const result = rows;
					return {
						orderBy: () => Promise.resolve(result),
						then: (resolve: any) => resolve(result),
					};
				},
			}),
		}),
		insert: (table: unknown) => ({
			values: (values: any) => {
				if (table === issueComments) {
					const id = `comment-${++state.commentSeq}`;
					const row = {
						id,
						issueId: values.issueId,
						parentId: values.parentId ?? null,
						authorType: values.authorType,
						authorId: values.authorId,
						blocking: Boolean(values.blocking),
						bodyMd: values.bodyMd,
						createdAt: new Date(),
						editedAt: null,
					};
					state.comments.set(id, row);
					return { returning: () => Promise.resolve([row]) };
				}
				if (table === agentIdempotencyKeys) {
					const mapKey = `${values.agentId}::${values.idempotencyKey}`;
					return {
						onConflictDoNothing: () => ({
							returning: () => {
								if (state.idempotency.has(mapKey)) return Promise.resolve([]);
								state.idempotency.set(mapKey, { op: values.op, commentId: values.commentId ?? null });
								return Promise.resolve([{ id: `idem-${state.idempotency.size}` }]);
							},
						}),
					};
				}
				// issueActivity (and any other) -- bare awaited insert
				return { then: (resolve: any) => resolve(undefined) };
			},
		}),
		update: (_table: unknown) => ({
			set: (values: any) => ({
				where: () => {
					if (Object.prototype.hasOwnProperty.call(values, 'waitingOn')) {
						state.waitingOn = values.waitingOn;
					}
					return Promise.resolve(undefined);
				},
			}),
		}),
	};
	return tx;
}

vi.mock('$lib/server/db', () => ({
	db: {
		transaction: (cb: any) => cb(makeTx()),
	},
}));

const { createComment } = await import('./comments');

beforeEach(() => {
	vi.clearAllMocks();
	state = { idempotency: new Map(), comments: new Map(), waitingOn: null, commentSeq: 0 };
});

describe('N9 (D21): createComment idempotency key', () => {
	it('a blocking question replayed with the same key returns the original comment id and notifies exactly once', async () => {
		const first = await createComment({
			issueId: 'issue-1',
			authorType: 'agent',
			authorId: 'agent-1',
			bodyMd: 'Which environment reproduces this?',
			blocking: true,
			waitingOnAudience: 'reporter',
			idempotencyKey: 'job-abc',
		});

		expect(first.deduplicated).toBe(false);
		expect(first.comment.id).toBe('comment-1');
		expect(state.waitingOn).toBe('reporter');
		expect(notifyIssueEvent).toHaveBeenCalledTimes(1);

		// Simulate the answer arriving (waiting_on cleared) BEFORE the crashed agent retries -- the
		// replay must NOT resurrect waiting_on, proving it takes no side-effecting path.
		state.waitingOn = null;

		const replay = await createComment({
			issueId: 'issue-1',
			authorType: 'agent',
			authorId: 'agent-1',
			bodyMd: 'Which environment reproduces this? (reworded on retry)',
			blocking: true,
			waitingOnAudience: 'reporter',
			idempotencyKey: 'job-abc',
		});

		expect(replay.deduplicated).toBe(true);
		expect(replay.comment.id).toBe('comment-1'); // ORIGINAL question's comment id
		expect(replay.notified).toEqual([]);
		expect(state.waitingOn).toBeNull(); // not re-set
		// The whole point: still exactly one notification across both calls -> one email.
		expect(notifyIssueEvent).toHaveBeenCalledTimes(1);
	});

	it('a fresh key on a plain comment writes normally and is not deduplicated', async () => {
		const res = await createComment({
			issueId: 'issue-1',
			authorType: 'agent',
			authorId: 'agent-1',
			bodyMd: 'Looking into this now.',
			idempotencyKey: 'job-xyz',
		});
		expect(res.deduplicated).toBe(false);
		expect(res.comment.id).toBe('comment-1');
		expect(notifyIssueEvent).toHaveBeenCalledTimes(1);
	});

	it('reusing a comment key for a question (different op) is rejected', async () => {
		await createComment({
			issueId: 'issue-1',
			authorType: 'agent',
			authorId: 'agent-1',
			bodyMd: 'plain comment',
			idempotencyKey: 'shared-key',
		});

		await expect(
			createComment({
				issueId: 'issue-1',
				authorType: 'agent',
				authorId: 'agent-1',
				bodyMd: 'a question',
				blocking: true,
				waitingOnAudience: 'team',
				idempotencyKey: 'shared-key',
			})
		).rejects.toThrowError(/already used for op/);
	});
});
