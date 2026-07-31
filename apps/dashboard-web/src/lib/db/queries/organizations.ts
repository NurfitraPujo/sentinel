import { db } from '$lib/server/db';
import {
  organizations,
  organizationMembers,
  organizationInvitations,
  userSessionPreferences,
  projects,
  projectMembers
} from '$lib/db/schema';
import { eq, and, sql, count } from 'drizzle-orm';

/**
 * Resolves effective project role given an organization role and an optional project-level override role.
 * Project override role takes precedence over organization role if present.
 */
export function resolveEffectiveProjectRole(
  orgRole: string,
  projectOverrideRole?: string | null
): string {
  if (projectOverrideRole) {
    return projectOverrideRole;
  }
  return orgRole;
}

/**
 * Fetches all organizations a user belongs to, including their org role and project count.
 */
export async function getUserOrganizations(userId: string) {
  const userOrgs = await db
    .select({
      id: organizations.id,
      name: organizations.name,
      slug: organizations.slug,
      avatarUrl: organizations.avatarUrl,
      role: organizationMembers.role,
      createdAt: organizations.createdAt,
      projectCount: count(projects.id),
    })
    .from(organizationMembers)
    .innerJoin(organizations, eq(organizationMembers.organizationId, organizations.id))
    .leftJoin(projects, eq(projects.organizationId, organizations.id))
    .where(eq(organizationMembers.userId, userId))
    .groupBy(
      organizations.id,
      organizations.name,
      organizations.slug,
      organizations.avatarUrl,
      organizationMembers.role,
      organizations.createdAt
    );

  return userOrgs;
}

export const RESERVED_SLUGS = ['admin', 'settings', 'api', 'auth', 'docs', 'billing', 'support'];

/**
 * Creates a new organization and assigns the creator as 'owner'.
 */
export async function createOrganization(
  userId: string,
  data: { name: string; slug: string; avatarUrl?: string }
) {
  if (RESERVED_SLUGS.includes(data.slug.toLowerCase())) {
    throw new Error(`The slug '${data.slug}' is reserved and cannot be used for an organization.`);
  }

  const [newOrg] = await db
    .insert(organizations)
    .values({
      name: data.name,
      slug: data.slug,
      avatarUrl: data.avatarUrl,
    })
    .returning();

  await db.insert(organizationMembers).values({
    organizationId: newOrg.id,
    userId: userId,
    role: 'owner',
  });

  await updateUserLastActiveOrg(userId, newOrg.id);

  return newOrg;
}

/**
 * Updates or creates user session preference for last active organization.
 */
export async function updateUserLastActiveOrg(userId: string, orgId: string) {
  await db
    .insert(userSessionPreferences)
    .values({
      userId,
      lastActiveOrganizationId: orgId,
      updatedAt: new Date(),
    })
    .onConflictDoUpdate({
      target: userSessionPreferences.userId,
      set: {
        lastActiveOrganizationId: orgId,
        updatedAt: new Date(),
      },
    });
}

/**
 * Fetches user's session preference for last active organization.
 */
export async function getUserLastActiveOrg(userId: string) {
  const [pref] = await db
    .select()
    .from(userSessionPreferences)
    .where(eq(userSessionPreferences.userId, userId));

  return pref?.lastActiveOrganizationId ?? null;
}

/**
 * Adds or updates a member in an organization.
 */
export async function upsertOrganizationMember(
  organizationId: string,
  userId: string,
  role: 'owner' | 'admin' | 'engineer' | 'support' | 'viewer'
) {
  const [member] = await db
    .insert(organizationMembers)
    .values({
      organizationId,
      userId,
      role,
    })
    .onConflictDoUpdate({
      target: [organizationMembers.userId, organizationMembers.organizationId],
      set: { role },
    })
    .returning();

  return member;
}

/**
 * Removes a member from an organization.
 */
export async function removeOrganizationMember(organizationId: string, userId: string) {
  await db
    .delete(organizationMembers)
    .where(
      and(
        eq(organizationMembers.organizationId, organizationId),
        eq(organizationMembers.userId, userId)
      )
    );
}

/**
 * Creates an invitation for a user to join an organization.
 * Cleans up any existing pending invitations for the same email in the organization first.
 */
export async function createOrganizationInvitation(
  organizationId: string,
  email: string,
  role: 'owner' | 'admin' | 'engineer' | 'support' | 'viewer',
  token: string,
  expiresAt: Date
) {
  await deletePendingInvitationsByEmail(organizationId, email);

  const [invite] = await db
    .insert(organizationInvitations)
    .values({
      organizationId,
      email,
      role,
      token,
      status: 'pending',
      expiresAt,
    })
    .returning();

  return invite;
}

/**
 * Retrieves an invitation by token with its associated organization details.
 */
export async function getInvitationByToken(token: string) {
  const [result] = await db
    .select({
      invitation: organizationInvitations,
      organization: organizations,
    })
    .from(organizationInvitations)
    .leftJoin(organizations, eq(organizationInvitations.organizationId, organizations.id))
    .where(eq(organizationInvitations.token, token));

  if (!result || !result.organization) {
    return null;
  }

  return {
    invitation: result.invitation,
    organization: result.organization,
  };
}

/**
 * Deletes an invitation record by ID.
 */
export async function deleteInvitationById(id: string) {
  await db
    .delete(organizationInvitations)
    .where(eq(organizationInvitations.id, id));
}

/**
 * Deletes pending invitations for a specific email within an organization.
 */
export async function deletePendingInvitationsByEmail(organizationId: string, email: string) {
  await db
    .delete(organizationInvitations)
    .where(
      and(
        eq(organizationInvitations.organizationId, organizationId),
        eq(organizationInvitations.email, email),
        eq(organizationInvitations.status, 'pending')
      )
    );
}

