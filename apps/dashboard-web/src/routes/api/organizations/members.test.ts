import { describe, it, expect, vi, beforeEach } from 'vitest';

function makeDbMock() {
  const dbMock: any = {
    select: vi.fn(),
    from: vi.fn(),
    where: vi.fn(),
    innerJoin: vi.fn(),
    then: vi.fn(),
  };
  dbMock.select.mockReturnValue(dbMock);
  dbMock.from.mockReturnValue(dbMock);
  dbMock.where.mockReturnValue(dbMock);
  dbMock.innerJoin.mockReturnValue(dbMock);
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
        .mockImplementationOnce((resolve: any) => resolve([{ id: 'mem-1', userId: 'user-target', role: 'owner' }])) // target lookup
        .mockImplementationOnce((resolve: any) => resolve([{ count: 1 }])); // owner count = 1

      const request = new Request('http://x', { method: 'PATCH', body: JSON.stringify({ role: 'admin' }) });
      await expect(
        PATCH({ params: { orgId: 'org-1', memberId: 'mem-1' }, request, locals: locals({ id: 'user-caller' }) } as any)
      ).rejects.toMatchObject({ status: 400 });
      expect(orgQueries.upsertOrganizationMember).not.toHaveBeenCalled();
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
      expect(orgQueries.upsertOrganizationMember).toHaveBeenCalledWith('org-1', 'user-target', 'admin');
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
        .mockImplementationOnce((resolve: any) => resolve([{ count: 1 }]));

      await expect(
        DELETE({ params: { orgId: 'org-1', memberId: 'mem-target' }, locals: locals({ id: 'user-owner-caller' }) } as any)
      ).rejects.toMatchObject({ status: 400 });
      expect(orgQueries.removeOrganizationMember).not.toHaveBeenCalled();
    });

    it('200s and revokes member access', async () => {
      dbMock.then
        .mockImplementationOnce((resolve: any) => resolve([{ role: 'owner' }]))
        .mockImplementationOnce((resolve: any) => resolve([{ id: 'mem-target', userId: 'user-target', role: 'engineer' }]));

      const res = await DELETE({ params: { orgId: 'org-1', memberId: 'mem-target' }, locals: locals({ id: 'user-caller' }) } as any);
      expect(res.status).toBe(200);
      const body = await res.json();
      expect(body).toEqual({ success: true, memberId: 'mem-target', userId: 'user-target' });
      expect(orgQueries.removeOrganizationMember).toHaveBeenCalledWith('org-1', 'user-target');
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

    it('201s and returns invitation object for valid request', async () => {
      dbMock.then
        .mockImplementationOnce((resolve: any) => resolve([{ role: 'owner' }]))
        .mockImplementationOnce((resolve: any) => resolve([]))
        .mockImplementationOnce((resolve: any) => resolve([{ name: 'Acme Corp' }]));

      orgQueries.createOrganizationInvitation.mockResolvedValueOnce({
        id: 'inv-1',
        email: 'newuser@company.com',
        role: 'engineer',
        token: 'token123',
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
      expect(body).toEqual({
        id: 'inv-1',
        email: 'newuser@company.com',
        role: 'engineer',
        token: 'token123',
      });
      expect(orgQueries.createOrganizationInvitation).toHaveBeenCalledWith(
        'org-1',
        'newuser@company.com',
        'engineer',
        expect.any(String),
        expect.any(Date)
      );
      // The endpoint generates its own 256-bit (32-byte -> 64 hex char) token via
      // crypto.randomBytes internally and ignores the mocked query result's `token`
      // field for URL construction, so we assert the URL SHAPE rather than a fixed
      // value. NOTE: P1-3 will hash the token at rest — revisit this assertion then.
      expect(sendInvitationEmailMock).toHaveBeenCalledWith(
        'newuser@company.com',
        expect.stringMatching(/^http:\/\/localhost:5173\/invitations\/[0-9a-f]{64}$/),
        'Acme Corp'
      );
    });
  });
});
