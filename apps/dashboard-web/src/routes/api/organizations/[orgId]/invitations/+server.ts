import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { createOrganizationInvitation } from '$lib/db/queries/organizations';
import { db } from '$lib/server/db';
import { organizationMembers, users, organizations } from '$lib/db/schema';
import { eq, and } from 'drizzle-orm';
import crypto from 'crypto';
import { sendInvitationEmail } from '$lib/server/email';
import { requireOrgMembership } from '../keys/_shared';

const VALID_ROLES = ['owner', 'admin', 'engineer', 'support', 'viewer'] as const;
type Role = typeof VALID_ROLES[number];

const EMAIL_REGEX = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

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

  // Check if target email is already an active member of this organization
  const [existingMember] = await db
    .select()
    .from(organizationMembers)
    .innerJoin(users, eq(organizationMembers.userId, users.id))
    .where(
      and(
        eq(organizationMembers.organizationId, orgId),
        eq(users.email, normalizedEmail)
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

  const inviteUrl = `${url.origin}/invitations/${token}`;
  
  // Non-blocking email dispatch
  sendInvitationEmail(normalizedEmail, inviteUrl, org?.name ?? 'Sentinel Organization').catch(() => {});

  return json(invite, { status: 201 });
};
