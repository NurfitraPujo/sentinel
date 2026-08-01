import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { db } from '$lib/server/db';
import { organizationMembers } from '$lib/db/schema';
import { eq, and, or, count } from 'drizzle-orm';
import { upsertOrganizationMember, removeOrganizationMember } from '$lib/db/queries/organizations';
import { requireOrgMembership } from '../../keys/_shared';

const VALID_ROLES = ['owner', 'admin', 'engineer', 'support', 'viewer'] as const;
type Role = typeof VALID_ROLES[number];

export const PATCH: RequestHandler = async ({ params, request, locals }) => {
  const session = await locals.auth();
  if (!session?.user?.id) {
    throw error(401, 'Unauthorized');
  }

  const { orgId, memberId } = params;
  if (!orgId || !memberId) {
    throw error(400, 'Missing organizationId or memberId');
  }

  // Caller permission check
  const callerMembership = await requireOrgMembership(session.user.id, orgId);
  if (!callerMembership || !['owner', 'admin'].includes(callerMembership.role)) {
    throw error(403, 'Forbidden: Only owners and admins can modify member roles');
  }

  let body: any;
  try {
    body = await request.json();
  } catch {
    throw error(400, 'Invalid JSON body');
  }

  const { role } = body ?? {};

  if (!role || !VALID_ROLES.includes(role as Role)) {
    throw error(400, `Invalid role. Must be one of: ${VALID_ROLES.join(', ')}`);
  }

  // Admins cannot grant the 'owner' role
  if (callerMembership.role === 'admin' && role === 'owner') {
    throw error(403, 'Forbidden: Only owners can assign the owner role');
  }

  // Locate target member record
  const [targetMember] = await db
    .select()
    .from(organizationMembers)
    .where(
      and(
        eq(organizationMembers.organizationId, orgId),
        or(
          eq(organizationMembers.id, memberId),
          eq(organizationMembers.userId, memberId)
        )
      )
    );

  if (!targetMember) {
    throw error(404, 'Member not found in this organization');
  }

  // Admins cannot alter an owner's role
  if (callerMembership.role === 'admin' && targetMember.role === 'owner') {
    throw error(403, 'Forbidden: Admins cannot alter an owner\'s role');
  }

  // Guard against demoting the sole owner
  if (targetMember.role === 'owner' && role !== 'owner') {
    const [ownerCountResult] = await db
      .select({ count: count() })
      .from(organizationMembers)
      .where(
        and(
          eq(organizationMembers.organizationId, orgId),
          eq(organizationMembers.role, 'owner')
        )
      );

    if (ownerCountResult.count <= 1) {
      throw error(400, 'Cannot demote the sole owner of an organization');
    }
  }

  const updatedMember = await upsertOrganizationMember(orgId, targetMember.userId, role as Role);
  return json({ success: true, member: updatedMember });
};

export const DELETE: RequestHandler = async ({ params, locals }) => {
  const session = await locals.auth();
  if (!session?.user?.id) {
    throw error(401, 'Unauthorized');
  }

  const { orgId, memberId } = params;
  if (!orgId || !memberId) {
    throw error(400, 'Missing organizationId or memberId');
  }

  // Caller permission check
  const callerMembership = await requireOrgMembership(session.user.id, orgId);
  if (!callerMembership || !['owner', 'admin'].includes(callerMembership.role)) {
    throw error(403, 'Forbidden: Only owners and admins can revoke organization access');
  }

  // Locate target member record
  const [targetMember] = await db
    .select()
    .from(organizationMembers)
    .where(
      and(
        eq(organizationMembers.organizationId, orgId),
        or(
          eq(organizationMembers.id, memberId),
          eq(organizationMembers.userId, memberId)
        )
      )
    );

  if (!targetMember) {
    throw error(404, 'Member not found in this organization');
  }

  // Guard against revoking caller's own access
  if (targetMember.userId === session.user.id) {
    throw error(400, 'Cannot revoke your own organization access');
  }

  // Admins cannot revoke an owner
  if (callerMembership.role === 'admin' && targetMember.role === 'owner') {
    throw error(403, 'Forbidden: Admins cannot revoke owners');
  }

  // Guard against revoking the sole owner
  if (targetMember.role === 'owner') {
    const [ownerCountResult] = await db
      .select({ count: count() })
      .from(organizationMembers)
      .where(
        and(
          eq(organizationMembers.organizationId, orgId),
          eq(organizationMembers.role, 'owner')
        )
      );

    if (ownerCountResult.count <= 1) {
      throw error(400, 'Cannot revoke the sole owner of an organization');
    }
  }

  await removeOrganizationMember(orgId, targetMember.userId);
  return json({ success: true, memberId: targetMember.id, userId: targetMember.userId });
};
