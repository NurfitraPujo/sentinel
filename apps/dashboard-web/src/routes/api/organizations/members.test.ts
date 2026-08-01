import { describe, it, expect, vi, beforeEach } from 'vitest';

function makeDbMock() {
  const dbMock: any = {
    select: vi.fn(),
    from: vi.fn(),
    where: vi.fn(),
    innerJoin: vi.fn(),
    limit: vi.fn(),
    for: vi.fn(),
    delete: vi.fn(),
    then: vi.fn(),
    // D32: members/[memberId]/+server.ts wraps its guards in db.transaction(async (tx) => ...).
    // The callback receives the SAME mock object as `tx`, so every chained call inside the
    // transaction (select/where/for('update')/delete/...) is driven by the identical queued
    // `then` implementations the tests set up below -- a transaction adds no extra indirection
    // for these tests to account for.
    transaction: vi.fn(async (cb: (tx: any) => unknown) => cb(dbMock)),
  };
  dbMock.select.mockReturnValue(dbMock);
  dbMock.from.mockReturnValue(dbMock);
  dbMock.where.mockReturnValue(dbMock);
  dbMock.innerJoin.mockReturnValue(dbMock);
  dbMock.limit.mockReturnValue(dbMock);
  dbMock.for.mockReturnValue(dbMock);
  dbMock.delete.mockReturnValue(dbMock);
  dbMock.then.mockImplementation((resolve: any) => resolve([]));
  return dbMock;
}

const dbMock = makeDbMock();

vi.mock('$lib/server/db', () => ({ db: dbMock }));
vi.mock('$lib/db/schema', () => ({
  organizationMembers: { id: 'id', organizationId: 'organizationId', userId: 'userId', role: 'role' },
  users: { id: 'id', email: 'email', name: 'name' },
  organizations: { id: 'id', name: 'name', slug: 'slug' },
  organizationInvitations: { id: 'id', organizationId: 'organizationId', email: 'email' },
}));

const orgQueries = {
  upsertOrganizationMember: vi.fn(),
  removeOrganizationMember: vi.fn(),
  createOrganizationInvitation: vi.fn(),
};
vi.mock('$lib/db/queries/organizations', () => orgQueries);

const sendInvitationEmailMock = vi.fn().mockResolvedValue(true);
vi.mock('$lib/server/email', () => ({
  sendInvitationEmail: (...args: any[]) => sendInvitationEmailMock(...args),
}));

const { PATCH, DELETE } = await import('./[orgId]/members/[memberId]/+server');
const { POST: POST_INVITATION } = await import('./[orgId]/invitations/+server');

function locals(session: { id: string } | null) {
  return { auth: async () => (session ? { user: { id: session.id } } : null) } as any;
}

describe('Organization Member Management Routes', () => {
  beforeEach(() => {
    vi.resetAllMocks();
    dbMock.select.mockReturnValue(dbMock);
    dbMock.from.mockReturnValue(dbMock);
    dbMock.where.mockReturnValue(dbMock);
    dbMock.innerJoin.mockReturnValue(dbMock);
    dbMock.limit.mockReturnValue(dbMock);
    dbMock.for.mockReturnValue(dbMock);
    dbMock.delete.mockReturnValue(dbMock);
    dbMock.transaction.mockImplementation(async (cb: (tx: any) => unknown) => cb(dbMock));
    dbMock.then.mockImplementation((resolve: any) => resolve([]));
    sendInvitationEmailMock.mockResolvedValue(true);
  });

  describe('PATCH /members/[memberId] (Role Update)', () => {
    it('401s when unauthenticated', async () => {
      const request = new Request('http://x', { method: 'PATCH', body: JSON.stringify({ role: 'admin' }) });
      await expect(
        PATCH({ params: { orgId: 'org-1', memberId: 'mem-1' }, request, locals: locals(null) } as any)
      ).rejects.toMatchObject({ status: 401 });
      expect(orgQueries.upsertOrganizationMember).not.toHaveBeenCalled();
    });

    it('403s on malformed JSON body when caller lacks org membership (authz runs before body parsing)', async () => {
      // No dbMock.then mock is set, so requireOrgMembership resolves to no membership.
      // The endpoint authorizes the caller BEFORE parsing the request body, so an
      // unauthorized caller gets 403 even though the body is malformed JSON.
      const request = new Request('http://x', { method: 'PATCH', body: 'invalid-json' });
      await expect(
        PATCH({ params: { orgId: 'org-1', memberId: 'mem-1' }, request, locals: locals({ id: 'user-owner' }) } as any)
      ).rejects.toMatchObject({ status: 403 });
    });

    it('400s on malformed JSON body when caller is authorized', async () => {
      dbMock.then.mockImplementationOnce((resolve: any) => resolve([{ role: 'owner' }]));
      const request = new Request('http://x', { method: 'PATCH', body: 'invalid-json' });
      await expect(
        PATCH({ params: { orgId: 'org-1', memberId: 'mem-1' }, request, locals: locals({ id: 'user-owner' }) } as any)
      ).rejects.toMatchObject({ status: 400 });
    });

    it('400s on invalid role string', async () => {
      dbMock.then.mockImplementationOnce((resolve: any) => resolve([{ role: 'owner' }]));
      const request = new Request('http://x', { method: 'PATCH', body: JSON.stringify({ role: 'super_admin' }) });
      await expect(
        PATCH({ params: { orgId: 'org-1', memberId: 'mem-1' }, request, locals: locals({ id: 'user-owner' }) } as any)
      ).rejects.toMatchObject({ status: 400 });
      expect(orgQueries.upsertOrganizationMember).not.toHaveBeenCalled();
    });

    it('403s when caller is engineer (insufficient rights)', async () => {
      dbMock.then.mockImplementationOnce((resolve: any) => resolve([{ role: 'engineer' }]));
      const request = new Request('http://x', { method: 'PATCH', body: JSON.stringify({ role: 'admin' }) });
      await expect(
        PATCH({ params: { orgId: 'org-1', memberId: 'mem-1' }, request, locals: locals({ id: 'user-caller' }) } as any)
      ).rejects.toMatchObject({ status: 403 });
      expect(orgQueries.upsertOrganizationMember).not.toHaveBeenCalled();
    });

    it('403s when an admin attempts to grant owner role', async () => {
      dbMock.then.mockImplementationOnce((resolve: any) => resolve([{ role: 'admin' }]));
      const request = new Request('http://x', { method: 'PATCH', body: JSON.stringify({ role: 'owner' }) });
      await expect(
        PATCH({ params: { orgId: 'org-1', memberId: 'mem-1' }, request, locals: locals({ id: 'user-admin' }) } as any)
      ).rejects.toMatchObject({ status: 403 });
      expect(orgQueries.upsertOrganizationMember).not.toHaveBeenCalled();
    });

    it('403s when an admin attempts to alter an existing owner role', async () => {
      dbMock.then
        .mockImplementationOnce((resolve: any) => resolve([{ role: 'admin' }])) // caller
        .mockImplementationOnce((resolve: any) => resolve([{ id: 'mem-owner', userId: 'user-owner', role: 'owner' }])); // target member

      const request = new Request('http://x', { method: 'PATCH', body: JSON.stringify({ role: 'engineer' }) });
      await expect(
        PATCH({ params: { orgId: 'org-1', memberId: 'mem-owner' }, request, locals: locals({ id: 'user-admin' }) } as any)
      ).rejects.toMatchObject({ status: 403 });
      expect(orgQueries.upsertOrganizationMember).not.toHaveBeenCalled();
    });

    it('404s when target member is not found in organization', async () => {
      dbMock.then
        .mockImplementationOnce((resolve: any) => resolve([{ role: 'owner' }])) // caller
        .mockImplementationOnce((resolve: any) => resolve([])); // target member not found

      const request = new Request('http://x', { method: 'PATCH', body: JSON.stringify({ role: 'engineer' }) });
      await expect(
        PATCH({ params: { orgId: 'org-1', memberId: 'non-existent' }, request, locals: locals({ id: 'user-owner' }) } as any)
      ).rejects.toMatchObject({ status: 404 });
      expect(orgQueries.upsertOrganizationMember).not.toHaveBeenCalled();
    });

    it('400s when attempting to demote the sole owner', async () => {
      dbMock.then
        .mockImplementationOnce((resolve: any) => resolve([{ role: 'owner' }])) // caller membership
        .mockImplementationOnce((resolve: any) => resolve([{ id: 'mem-1', userId: 'user-target', role: 'owner' }])) // target lookup (locked)
        .mockImplementationOnce((resolve: any) => resolve([{ id: 'mem-1' }])); // locked owner rows: only 1

      const request = new Request('http://x', { method: 'PATCH', body: JSON.stringify({ role: 'admin' }) });
      await expect(
        PATCH({ params: { orgId: 'org-1', memberId: 'mem-1' }, request, locals: locals({ id: 'user-caller' }) } as any)
      ).rejects.toMatchObject({ status: 400 });
      expect(orgQueries.upsertOrganizationMember).not.toHaveBeenCalled();
      // D32: the owner count must be read under `SELECT ... FOR UPDATE` inside a transaction, or
      // two concurrent demotions can both observe "2 owners" and both proceed. Asserted explicitly
      // because the mock chain returns itself for every call -- without this, deleting the lock
      // from countLockedOwners leaves this test (and the whole suite) green.
      expect(dbMock.transaction).toHaveBeenCalled();
      expect(dbMock.for).toHaveBeenCalledWith('update');
    });

    it('403s when caller attempts to change their own role', async () => {
      dbMock.then
        .mockImplementationOnce((resolve: any) => resolve([{ role: 'owner' }])) // caller membership
        .mockImplementationOnce((resolve: any) => resolve([{ id: 'mem-self', userId: 'user-caller', role: 'owner' }])); // target lookup == caller

      const request = new Request('http://x', { method: 'PATCH', body: JSON.stringify({ role: 'admin' }) });
      await expect(
        PATCH({ params: { orgId: 'org-1', memberId: 'mem-self' }, request, locals: locals({ id: 'user-caller' }) } as any)
      ).rejects.toMatchObject({ status: 400 });
      expect(orgQueries.upsertOrganizationMember).not.toHaveBeenCalled();
    });

    it('resolves the target member by id first, and never mismatches on userId collision (D33)', async () => {
      // Row A's `id` collides with row B's `userId`. The old bare `or(id = x, userId = x)` lookup
      // with no orderBy/limit made it undefined which row got hit. findOrgMember must prefer the
      // `id` match deterministically: it queries by id FIRST, and only falls back to userId if
      // that lookup misses -- so it should never even issue the userId query when the id matches.
      dbMock.then
        .mockImplementationOnce((resolve: any) => resolve([{ role: 'owner' }])) // caller
        .mockImplementationOnce((resolve: any) => resolve([{ id: 'ambiguous-key', userId: 'user-A', role: 'engineer' }])); // id match wins

      const request = new Request('http://x', { method: 'PATCH', body: JSON.stringify({ role: 'admin' }) });
      orgQueries.upsertOrganizationMember.mockResolvedValueOnce({ id: 'ambiguous-key', userId: 'user-A', role: 'admin' });

      const res = await PATCH({
        params: { orgId: 'org-1', memberId: 'ambiguous-key' },
        request,
        locals: locals({ id: 'user-caller' }),
      } as any);

      expect(res.status).toBe(200);
      // Resolved via the id match (user-A), never the colliding userId match (which would have
      // resolved to a different user).
      expect(orgQueries.upsertOrganizationMember).toHaveBeenCalledWith('org-1', 'user-A', 'admin', dbMock);
    });

    it('200s and updates role when valid', async () => {
      dbMock.then
        .mockImplementationOnce((resolve: any) => resolve([{ role: 'owner' }]))
        .mockImplementationOnce((resolve: any) => resolve([{ id: 'mem-1', userId: 'user-target', role: 'engineer' }]));

      orgQueries.upsertOrganizationMember.mockResolvedValueOnce({ id: 'mem-1', userId: 'user-target', role: 'admin' });

      const request = new Request('http://x', { method: 'PATCH', body: JSON.stringify({ role: 'admin' }) });
      const res = await PATCH({ params: { orgId: 'org-1', memberId: 'mem-1' }, request, locals: locals({ id: 'user-caller' }) } as any);
      
      expect(res.status).toBe(200);
      const body = await res.json();
      expect(body.success).toBe(true);
      expect(body.member).toEqual({ id: 'mem-1', userId: 'user-target', role: 'admin' });
      expect(orgQueries.upsertOrganizationMember).toHaveBeenCalledWith('org-1', 'user-target', 'admin', dbMock);
    });
  });

  describe('DELETE /members/[memberId] (Member Revocation)', () => {
    it('401s when unauthenticated', async () => {
      await expect(
        DELETE({ params: { orgId: 'org-1', memberId: 'mem-1' }, locals: locals(null) } as any)
      ).rejects.toMatchObject({ status: 401 });
      expect(orgQueries.removeOrganizationMember).not.toHaveBeenCalled();
    });

    it('403s when caller is non-admin/non-owner (e.g. viewer)', async () => {
      dbMock.then.mockImplementationOnce((resolve: any) => resolve([{ role: 'viewer' }]));
      await expect(
        DELETE({ params: { orgId: 'org-1', memberId: 'mem-1' }, locals: locals({ id: 'user-viewer' }) } as any)
      ).rejects.toMatchObject({ status: 403 });
      expect(orgQueries.removeOrganizationMember).not.toHaveBeenCalled();
    });

    it('403s when an admin attempts to revoke an owner', async () => {
      dbMock.then
        .mockImplementationOnce((resolve: any) => resolve([{ role: 'admin' }])) // caller
        .mockImplementationOnce((resolve: any) => resolve([{ id: 'mem-owner', userId: 'user-owner', role: 'owner' }])); // target

      await expect(
        DELETE({ params: { orgId: 'org-1', memberId: 'mem-owner' }, locals: locals({ id: 'user-admin' }) } as any)
      ).rejects.toMatchObject({ status: 403 });
      expect(orgQueries.removeOrganizationMember).not.toHaveBeenCalled();
    });

    it('404s when target member is not found in organization', async () => {
      dbMock.then
        .mockImplementationOnce((resolve: any) => resolve([{ role: 'owner' }])) // caller
        .mockImplementationOnce((resolve: any) => resolve([])); // target member not found

      await expect(
        DELETE({ params: { orgId: 'org-1', memberId: 'mem-unknown' }, locals: locals({ id: 'user-owner' }) } as any)
      ).rejects.toMatchObject({ status: 404 });
      expect(orgQueries.removeOrganizationMember).not.toHaveBeenCalled();
    });

    it('400s when caller attempts to revoke themselves', async () => {
      dbMock.then
        .mockImplementationOnce((resolve: any) => resolve([{ role: 'admin' }]))
        .mockImplementationOnce((resolve: any) => resolve([{ id: 'mem-caller', userId: 'user-caller', role: 'admin' }]));

      await expect(
        DELETE({ params: { orgId: 'org-1', memberId: 'mem-caller' }, locals: locals({ id: 'user-caller' }) } as any)
      ).rejects.toMatchObject({ status: 400 });
      expect(orgQueries.removeOrganizationMember).not.toHaveBeenCalled();
    });

    it('400s when attempting to revoke sole owner', async () => {
      dbMock.then
        .mockImplementationOnce((resolve: any) => resolve([{ role: 'owner' }]))
        .mockImplementationOnce((resolve: any) => resolve([{ id: 'mem-target', userId: 'user-target', role: 'owner' }]))
        .mockImplementationOnce((resolve: any) => resolve([{ id: 'mem-target' }])); // locked owner rows: only 1

      await expect(
        DELETE({ params: { orgId: 'org-1', memberId: 'mem-target' }, locals: locals({ id: 'user-owner-caller' }) } as any)
      ).rejects.toMatchObject({ status: 400 });
      expect(dbMock.delete).not.toHaveBeenCalled();
      // D32: same lock requirement on the revoke path.
      expect(dbMock.transaction).toHaveBeenCalled();
      expect(dbMock.for).toHaveBeenCalledWith('update');
    });

    it('200s and revokes member access', async () => {
      dbMock.then
        .mockImplementationOnce((resolve: any) => resolve([{ role: 'owner' }]))
        .mockImplementationOnce((resolve: any) => resolve([{ id: 'mem-target', userId: 'user-target', role: 'engineer' }]));

      const res = await DELETE({ params: { orgId: 'org-1', memberId: 'mem-target' }, locals: locals({ id: 'user-caller' }) } as any);
      expect(res.status).toBe(200);
      const body = await res.json();
      expect(body).toEqual({ success: true, memberId: 'mem-target', userId: 'user-target' });
      expect(dbMock.delete).toHaveBeenCalled();
    });

    // D32: concurrency regression test. Two "simultaneous" DELETEs both target the last two
    // remaining owners of an org. Each transaction must see an up-to-date owner count -- i.e. the
    // second transaction's `countLockedOwners` read must reflect the first transaction's
    // in-flight/committed delete, not a stale pre-transaction snapshot. Model this by having
    // db.transaction interleave: transaction A starts, transaction B's owner-count read is queued
    // to run AFTER A's row is gone, exactly like a real `SELECT ... FOR UPDATE` would force B to
    // block until A commits and then see the reduced count. Assert that at most one of the two
    // succeeds, and the org retains at least one owner in every observable outcome.
    it('never allows two concurrent revocations to leave an org ownerless', async () => {
      // Two rows, both owners, sharing one org.
      const ownerRows = [
        { id: 'mem-owner-1', userId: 'user-owner-1', role: 'owner' },
        { id: 'mem-owner-2', userId: 'user-owner-2', role: 'owner' },
      ];
      let remainingOwners = [...ownerRows];

      // A fake serialized transaction: real `FOR UPDATE` would make concurrent transactions run
      // their owner-count read one-after-another against the current committed state, never both
      // against the pre-delete state. This mock enforces that same serialization property so the
      // guard logic (not Postgres) is what's under test.
      let txQueue: Promise<unknown> = Promise.resolve();
      dbMock.transaction.mockImplementation((cb: (tx: any) => Promise<unknown>) => {
        const run = txQueue.then(() => cb(dbMock));
        txQueue = run.catch(() => {});
        return run;
      });

      async function deleteRequest(targetRow: { id: string; userId: string; role: string }, callerId: string) {
        // caller membership (owner, distinct row not modelled -- role check only)
        dbMock.then.mockImplementationOnce((resolve: any) => resolve([{ role: 'owner' }]));
        // target lookup: whichever row is still present
        dbMock.then.mockImplementationOnce((resolve: any) =>
          resolve(remainingOwners.some((r) => r.id === targetRow.id) ? [targetRow] : [])
        );
        // locked owner-row count: reflects current state at the moment this transaction reaches it
        dbMock.then.mockImplementationOnce((resolve: any) => resolve([...remainingOwners]));

        try {
          const res = await DELETE({
            params: { orgId: 'org-1', memberId: targetRow.id },
            locals: locals({ id: callerId }),
          } as any);
          if (res.status === 200) {
            remainingOwners = remainingOwners.filter((r) => r.id !== targetRow.id);
          }
          return { ok: true, status: res.status };
        } catch (err: any) {
          return { ok: false, status: err.status };
        }
      }

      const [resultA, resultB] = await Promise.all([
        deleteRequest(ownerRows[0], 'user-owner-2'),
        deleteRequest(ownerRows[1], 'user-owner-1'),
      ]);

      const successes = [resultA, resultB].filter((r) => r.ok && r.status === 200);
      expect(successes.length).toBeLessThanOrEqual(1);
      expect(remainingOwners.length).toBeGreaterThanOrEqual(1);

      // The serialization above is enforced by THIS TEST'S transaction mock, not by the code under
      // test -- so on its own it proves nothing about the real guard. Without these two assertions
      // the whole suite stayed green with `.for('update')` deleted from countLockedOwners
      // (verified). Postgres is what actually serializes the two transactions in production; assert
      // the code asks it to.
      expect(dbMock.transaction).toHaveBeenCalled();
      expect(dbMock.for).toHaveBeenCalledWith('update');
    });
  });

  describe('POST /invitations (Create Invitation)', () => {
    it('401s when unauthenticated', async () => {
      const request = new Request('http://x', {
        method: 'POST',
        body: JSON.stringify({ email: 'user@company.com', role: 'engineer' }),
      });
      await expect(
        POST_INVITATION({ params: { orgId: 'org-1' }, request, locals: locals(null), url: new URL('http://x') } as any)
      ).rejects.toMatchObject({ status: 401 });
      expect(orgQueries.createOrganizationInvitation).not.toHaveBeenCalled();
    });

    it('400s on malformed JSON payload', async () => {
      const request = new Request('http://x', { method: 'POST', body: 'invalid-json' });
      await expect(
        POST_INVITATION({ params: { orgId: 'org-1' }, request, locals: locals({ id: 'user-caller' }), url: new URL('http://x') } as any)
      ).rejects.toMatchObject({ status: 400 });
    });

    it('400s on invalid email format', async () => {
      const request = new Request('http://x', {
        method: 'POST',
        body: JSON.stringify({ email: 'not-an-email', role: 'engineer' }),
      });
      await expect(
        POST_INVITATION({ params: { orgId: 'org-1' }, request, locals: locals({ id: 'user-caller' }), url: new URL('http://x') } as any)
      ).rejects.toMatchObject({ status: 400 });
      expect(orgQueries.createOrganizationInvitation).not.toHaveBeenCalled();
    });

    it('400s on invalid role enum value', async () => {
      const request = new Request('http://x', {
        method: 'POST',
        body: JSON.stringify({ email: 'user@company.com', role: 'invalid_role' }),
      });
      await expect(
        POST_INVITATION({ params: { orgId: 'org-1' }, request, locals: locals({ id: 'user-caller' }), url: new URL('http://x') } as any)
      ).rejects.toMatchObject({ status: 400 });
      expect(orgQueries.createOrganizationInvitation).not.toHaveBeenCalled();
    });

    it('403s when caller is non-admin/non-owner (e.g. support)', async () => {
      dbMock.then.mockImplementationOnce((resolve: any) => resolve([{ role: 'support' }]));
      const request = new Request('http://x', {
        method: 'POST',
        body: JSON.stringify({ email: 'user@company.com', role: 'engineer' }),
      });
      await expect(
        POST_INVITATION({ params: { orgId: 'org-1' }, request, locals: locals({ id: 'user-support' }), url: new URL('http://x') } as any)
      ).rejects.toMatchObject({ status: 403 });
      expect(orgQueries.createOrganizationInvitation).not.toHaveBeenCalled();
    });

    it('403s when an admin attempts to issue an owner invitation', async () => {
      dbMock.then.mockImplementationOnce((resolve: any) => resolve([{ role: 'admin' }]));
      const request = new Request('http://x', {
        method: 'POST',
        body: JSON.stringify({ email: 'user@company.com', role: 'owner' }),
      });
      await expect(
        POST_INVITATION({ params: { orgId: 'org-1' }, request, locals: locals({ id: 'user-admin' }), url: new URL('http://x') } as any)
      ).rejects.toMatchObject({ status: 403 });
      expect(orgQueries.createOrganizationInvitation).not.toHaveBeenCalled();
    });

    it('400s when invitee is already an active organization member', async () => {
      dbMock.then
        .mockImplementationOnce((resolve: any) => resolve([{ role: 'owner' }]))
        .mockImplementationOnce((resolve: any) => resolve([{ userId: 'user-existing', email: 'existing@company.com' }]));

      const request = new Request('http://x', {
        method: 'POST',
        body: JSON.stringify({ email: 'existing@company.com', role: 'engineer' }),
      });

      await expect(
        POST_INVITATION({ params: { orgId: 'org-1' }, request, locals: locals({ id: 'user-caller' }), url: new URL('http://x') } as any)
      ).rejects.toMatchObject({ status: 400 });
      expect(orgQueries.createOrganizationInvitation).not.toHaveBeenCalled();
    });

    // Shared happy-path setup for the D41 delivery-reporting tests below.
    async function postInvitation() {
      dbMock.then
        .mockImplementationOnce((resolve: any) => resolve([{ role: 'owner' }]))
        .mockImplementationOnce((resolve: any) => resolve([]))
        .mockImplementationOnce((resolve: any) => resolve([{ name: 'Acme Corp' }]));

      orgQueries.createOrganizationInvitation.mockResolvedValueOnce({
        id: 'inv-1',
        email: 'newuser@company.com',
        role: 'engineer',
        status: 'pending',
        expiresAt: new Date('2026-01-01T00:00:00Z'),
      });

      return POST_INVITATION({
        params: { orgId: 'org-1' },
        request: new Request('http://x', {
          method: 'POST',
          body: JSON.stringify({ email: 'newuser@company.com', role: 'engineer' }),
        }),
        locals: locals({ id: 'user-caller' }),
        url: new URL('http://localhost:5173'),
      } as any);
    }

    // D41: a 201 used to be unconditional and the send was fire-and-forget with `.catch(() => {})`,
    // so "created and emailed" was indistinguishable from "created, email silently failed". These
    // fail if the endpoint goes back to swallowing the outcome.
    it('reports delivered:false when the invitation email could not be sent', async () => {
      sendInvitationEmailMock.mockResolvedValueOnce(false); // e.g. EMAIL_SERVER not configured
      const res = await postInvitation();
      expect(res.status).toBe(201);
      const body = await res.json();
      expect(body.delivered).toBe(false);
      // The invitation itself still exists -- the copy-paste link remains usable.
      expect(body.id).toBe('inv-1');
    });

    it('still 201s (not 500s) when the email transport throws, reporting delivered:false', async () => {
      sendInvitationEmailMock.mockRejectedValueOnce(new Error('SMTP unreachable'));
      const res = await postInvitation();
      expect(res.status).toBe(201);
      expect((await res.json()).delivered).toBe(false);
    });

    it('201s and returns invitation object for valid request', async () => {
      dbMock.then
        .mockImplementationOnce((resolve: any) => resolve([{ role: 'owner' }]))
        .mockImplementationOnce((resolve: any) => resolve([]))
        .mockImplementationOnce((resolve: any) => resolve([{ name: 'Acme Corp' }]));

      orgQueries.createOrganizationInvitation.mockResolvedValueOnce({
        id: 'inv-1',
        email: 'newuser@company.com',
        role: 'engineer',
        status: 'pending',
        expiresAt: new Date('2026-01-01T00:00:00Z'),
        // A real row also carries tokenHash (never the raw token) as of P1-3 (D06) -- included
        // here to prove the endpoint does NOT spread this straight into the response.
        tokenHash: 'deadbeef'.repeat(8),
      });

      const request = new Request('http://x', {
        method: 'POST',
        body: JSON.stringify({ email: '  NEWUSER@COMPANY.COM  ', role: 'engineer' }),
      });

      const res = await POST_INVITATION({
        params: { orgId: 'org-1' },
        request,
        locals: locals({ id: 'user-caller' }),
        url: new URL('http://localhost:5173'),
      } as any);

      expect(res.status).toBe(201);
      const body = await res.json();
      // P1-3 (D06): the response is built from an explicit field list, not a spread of the DB row,
      // so it can never contain tokenHash (or the raw token, which is never returned to a client at
      // all -- it only ever appears in the emailed URL).
      expect(body).toEqual({
        // D41: the response now reports whether the email actually went out. The invitation row
        // exists either way (the modal's copy-paste link works without email), but a 201 alone
        // used to be indistinguishable from a silently-swallowed send failure.
        delivered: true,
        id: 'inv-1',
        email: 'newuser@company.com',
        role: 'engineer',
        status: 'pending',
        expiresAt: '2026-01-01T00:00:00.000Z',
      });
      expect(body.tokenHash).toBeUndefined();
      expect(body.token).toBeUndefined();
      expect(orgQueries.createOrganizationInvitation).toHaveBeenCalledWith(
        'org-1',
        'newuser@company.com',
        'engineer',
        expect.any(String),
        expect.any(Date),
        // D31 (residual): the inviter's id is recorded so redemption can re-check, at claim time,
        // that they still hold authority to grant this role.
        'user-caller'
      );
      // The endpoint generates its own 256-bit (32-byte -> 64 hex char) raw token via
      // crypto.randomBytes internally; createOrganizationInvitation hashes it before persisting
      // (P1-3 / D06), so we assert the URL SHAPE rather than a fixed value, and assert the raw
      // token never appears in a query string.
      expect(sendInvitationEmailMock).toHaveBeenCalledWith(
        'newuser@company.com',
        expect.stringMatching(/^http:\/\/localhost:5173\/invitations\/[0-9a-f]{64}$/),
        'Acme Corp'
      );
      const [, invitationUrl] = sendInvitationEmailMock.mock.calls[0];
      expect(new URL(invitationUrl).search).toBe('');
    });

    it('403s when caller is non-admin/non-owner (e.g. viewer) for GET list', async () => {
      dbMock.then.mockImplementationOnce((resolve: any) => resolve([{ role: 'viewer' }]));
      const { GET: GET_INVITATIONS } = await import('./[orgId]/invitations/+server');
      await expect(
        GET_INVITATIONS({ params: { orgId: 'org-1' }, locals: locals({ id: 'user-viewer' }) } as any)
      ).rejects.toMatchObject({ status: 403 });
    });
  });
});
