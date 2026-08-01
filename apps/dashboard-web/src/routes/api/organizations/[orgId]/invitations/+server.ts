import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { createOrganizationInvitation, listOrganizationInvitations } from '$lib/db/queries/organizations';
import { db } from '$lib/server/db';
import { organizationMembers, users, organizations } from '$lib/db/schema';
import { eq, and, sql } from 'drizzle-orm';
import crypto from 'crypto';
import { sendInvitationEmail } from '$lib/server/email';
import { requireOrgMembership } from '../keys/_shared';
import { log } from '$lib/server/observability/log';

const VALID_ROLES = ['owner', 'admin', 'engineer', 'support', 'viewer'] as const;
type Role = typeof VALID_ROLES[number];

const EMAIL_REGEX = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

// D07: an admin can now see what invitations are outstanding, mirroring the create endpoint's own
// gate. Never includes tokenHash -- listOrganizationInvitations's column list is the enforcement
// point for that, not this handler.
export const GET: RequestHandler = async ({ params, locals }) => {
  const session = await locals.auth();
  if (!session?.user?.id) {
    throw error(401, 'Unauthorized');
  }

  const { orgId } = params;
  if (!orgId) {
    throw error(400, 'Missing organizationId');
  }

  const membership = await requireOrgMembership(session.user.id, orgId);
  if (!membership || !['owner', 'admin'].includes(membership.role)) {
    throw error(403, 'Forbidden: Only owners and admins can view invitations');
  }

  const invitations = await listOrganizationInvitations(orgId);
  return json(invitations);
};

export const POST: RequestHandler = async ({ params, request, locals, url }) => {
  const session = await locals.auth();
  if (!session?.user?.id) {
    throw error(401, 'Unauthorized');
  }

  const { orgId } = params;
  if (!orgId) {
    throw error(400, 'Missing organizationId');
  }

  let body: any;
  try {
    body = await request.json();
  } catch {
    throw error(400, 'Invalid JSON body');
  }

  const { email, role } = body ?? {};

  if (!email || typeof email !== 'string' || !role || typeof role !== 'string') {
    throw error(400, 'Valid email and role string are required');
  }

  if (!VALID_ROLES.includes(role as Role)) {
    throw error(400, `Invalid role. Must be one of: ${VALID_ROLES.join(', ')}`);
  }

  const normalizedEmail = email.trim().toLowerCase();
  if (!EMAIL_REGEX.test(normalizedEmail)) {
    throw error(400, 'Invalid email address format');
  }

  // Caller permission check using shared helper
  const membership = await requireOrgMembership(session.user.id, orgId);
  if (!membership || !['owner', 'admin'].includes(membership.role)) {
    throw error(403, 'Forbidden: Only owners and admins can issue invitations');
  }

  // Admins cannot issue owner invitations
  if (membership.role === 'admin' && role === 'owner') {
    throw error(403, 'Forbidden: Only owners can issue owner invitations');
  }

  // D08/D30: case-insensitive membership check -- users.email keeps provider casing, so this must
  // compare lower(email), matching idx_user_email_lower_unique, or a member stored as
  // "Bob@X.com" is invisible to a lookup for "bob@x.com".
  const [existingMember] = await db
    .select()
    .from(organizationMembers)
    .innerJoin(users, eq(organizationMembers.userId, users.id))
    .where(
      and(
        eq(organizationMembers.organizationId, orgId),
        eq(sql`lower(${users.email})`, normalizedEmail)
      )
    );

  if (existingMember) {
    throw error(400, 'User is already a member of this organization');
  }

  const token = crypto.randomBytes(32).toString('hex');
  const expiresAt = new Date(Date.now() + 7 * 24 * 60 * 60 * 1000); // 7 days

  const invite = await createOrganizationInvitation(orgId, normalizedEmail, role as Role, token, expiresAt);

  // Fetch organization name for email dispatch
  const [org] = await db
    .select({ name: organizations.name })
    .from(organizations)
    .where(eq(organizations.id, orgId));

  // D06: the raw token appears ONLY in this URL (built here, sent by email, never persisted) and
  // never in the response body below -- the DB row (and therefore `invite`) only ever has
  // tokenHash, but we still enumerate the response fields explicitly rather than spreading `invite`
  // so an added DB column can't silently start leaking.
  const inviteUrl = `${url.origin}/invitations/${token}`;

  // D41: the send used to be fire-and-forget with `.catch(() => {})`, and the response was an
  // unconditional 201 — so a caller could not tell "invitation created and emailed" from
  // "invitation created, email silently failed or was never configured". The row is created either
  // way (the copy-paste invite link in InviteMemberModal works without email), so this stays a 201;
  // what changes is that the outcome is now REPORTED rather than swallowed, and logged on failure.
  let delivered = false;
  try {
    delivered = await sendInvitationEmail(
      normalizedEmail,
      inviteUrl,
      org?.name ?? 'Sentinel Organization'
    );
  } catch (err) {
    log.error('invitation.email_failed', {
      organizationId: orgId,
      invitationId: invite.id,
      error: err instanceof Error ? err.message : String(err),
    });
  }

  return json(
    {
      id: invite.id,
      email: invite.email,
      role: invite.role,
      status: invite.status,
      expiresAt: invite.expiresAt,
      // false when EMAIL_SERVER is unset or the send failed: the invitation exists and its link is
      // still valid, but nothing was delivered and the inviter must pass the link on themselves.
      delivered,
    },
    { status: 201 }
  );
};
