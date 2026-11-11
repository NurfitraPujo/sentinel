import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import {
  createOrganizationInvitation,
  listOrganizationInvitations,
  type InvitationEnvironmentScope,
} from '$lib/db/queries/organizations';
import { db } from '$lib/server/db';
import { organizationMembers, users, organizations, projects } from '$lib/db/schema';
import { eq, and, sql } from 'drizzle-orm';
import crypto from 'crypto';
import { sendInvitationEmail } from '$lib/server/email';
import { requireOrgMembership } from '../keys/_shared';
import { log } from '$lib/server/observability/log';
import { validateGrantEnvironments } from '../projects/_shared';
import { hasPermission, type OrgRole } from '$lib/rbac';

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

/**
 * sentinel-project-environments §4.10a — the server half of decision 28's control.
 *
 * Absent or `[]` means unrestricted: the default, and what every invitation sent today means. It
 * returns `null` for that case, which is what the `environment_scopes` column stores.
 *
 * Each entry is validated **exactly as §4.10's `PUT` validates a grant** -- non-empty, and every
 * name present in that project's registry, every row of it, `hidden` included (decision 49). The
 * rule stands on §4.10's own ground: a grant naming an environment that does not exist is a grant
 * that does nothing and looks like it does something. (§4.10's `PUT` is explicitly NOT
 * environment-scope-guarded, so there is deliberately **no scope check on the inviter** -- decision
 * 39 dropped it, because a narrowed project admin refused `production` here could grant exactly
 * that through the `PUT` one screen later.)
 *
 * A duplicate `projectId` is a `400` rather than a last-wins or a union:
 * `project_environment_grants` is `PRIMARY KEY (user_id, project_id)`, so two entries naming one
 * project would raise 23505 inside the membership transaction and the acceptance would fail
 * outright -- and either resolution silently stores something the inviter did not obviously ask for
 * on a screen where the duplicate was almost certainly a UI mistake.
 *
 * `manage_members` on each named project is required on top of the permission the invitation
 * already needs: scoping is the §4.10 action, deferred, so it takes the §4.10 permission.
 */
async function validateEnvironmentScopes(
  orgId: string,
  inviterRole: string,
  value: unknown
): Promise<InvitationEnvironmentScope[] | null> {
  if (value === undefined || value === null) return null;
  if (!Array.isArray(value)) {
    throw error(400, 'environmentScopes must be an array');
  }
  if (value.length === 0) return null;

  // Scoping is the §4.10 action, deferred; it takes the §4.10 permission. Under rbac.ts's role
  // table `manage_members` is org owner/admin, which this route already requires -- checked
  // explicitly rather than assumed, so a widened role table cannot silently open this.
  if (!hasPermission(inviterRole as OrgRole, 'manage_members')) {
    throw error(403, 'Forbidden: manage_members permission required to manage environment access');
  }

  const seen = new Set<string>();
  const resolved: InvitationEnvironmentScope[] = [];

  for (const entry of value) {
    const projectId = (entry as { projectId?: unknown } | null)?.projectId;
    if (typeof projectId !== 'string' || projectId.length === 0) {
      throw error(400, 'environmentScopes entries require a projectId');
    }
    if (seen.has(projectId)) {
      throw error(400, 'environmentScopes names the same project twice');
    }
    seen.add(projectId);

    // Decision 56: an invitation's environmentScopes may name only that organization's projects.
    const [project] = await db
      .select({ id: projects.id })
      .from(projects)
      .where(and(eq(projects.id, projectId), eq(projects.organizationId, orgId)));
    if (!project) {
      throw error(404, 'Project not found in this organization');
    }

    const environments = await validateGrantEnvironments(
      projectId,
      (entry as { environments?: unknown }).environments
    );
    resolved.push({ projectId, environments });
  }

  return resolved;
}

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

  const { email, role, environmentScopes } = body ?? {};

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

  // sentinel-project-environments §4.10a, decision 28: the optional environment scope.
  // Choosing nothing is the normal path and writes NULL -- a newly added member gets no
  // project_environment_grants row and is unrestricted in every project they can reach, present and
  // future. Nothing anywhere narrows a person by machine.
  // §4.10a and §2.2: an organization `owner` cannot be scoped, the same message and the same reason
  // as §4.10's -- the person who can undo a narrowing must never be the person narrowed out. The
  // invited role is on the invitation, so this is checkable here rather than at acceptance.
  if (role === 'owner' && Array.isArray(environmentScopes) && environmentScopes.length > 0) {
    throw error(422, 'An organization owner cannot be restricted to specific environments');
  }
  const scopes = await validateEnvironmentScopes(orgId, membership.role, environmentScopes);

  const token = crypto.randomBytes(32).toString('hex');
  const expiresAt = new Date(Date.now() + 7 * 24 * 60 * 60 * 1000); // 7 days

  // D31 (residual): recorded so claimInvitation can re-check at redemption time that the inviter
  // still holds authority to grant `role` -- not just that they did when this request was made.
  const invite = await createOrganizationInvitation(
    orgId,
    normalizedEmail,
    role as Role,
    token,
    expiresAt,
    session.user.id,
    scopes
  );

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
