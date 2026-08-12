import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { db } from '$lib/server/db';
import { organizationMembers, issues, projects, issueSubscriptions } from '$lib/db/schema';
import { eq, and, inArray } from 'drizzle-orm';
import { upsertOrganizationMember } from '$lib/db/queries/organizations';
import { requireOrgMembership } from '../../keys/_shared';

const VALID_ROLES = ['owner', 'admin', 'engineer', 'support', 'viewer'] as const;
type Role = typeof VALID_ROLES[number];

// D33: `memberId` in the URL may be either the membership row's own `id` or the target user's
// `userId` (both are used as links elsewhere in the UI). The old code matched both with a bare
// `or(...)` and no `orderBy`/`limit`, so if one row's `id` happened to equal a DIFFERENT row's
// `userId`, which row got hit was undefined. This resolves the ambiguity explicitly: try `id`
// first, and ONLY if that misses, fall back to `userId` -- never both at once. `executor` is
// threaded through so callers running inside a transaction can lock the row they find
// (`FOR UPDATE`) atomically with the rest of the guard logic (D32).
async function findOrgMember(
  executor: any,
  orgId: string,
  memberId: string,
  { lock = false }: { lock?: boolean } = {}
) {
  const byId = executor
    .select()
    .from(organizationMembers)
    .where(and(eq(organizationMembers.organizationId, orgId), eq(organizationMembers.id, memberId)));
  const [byIdRow] = lock ? await byId.for('update') : await byId;
  if (byIdRow) return byIdRow;

  const byUserId = executor
    .select()
    .from(organizationMembers)
    .where(and(eq(organizationMembers.organizationId, orgId), eq(organizationMembers.userId, memberId)))
    .limit(1);
  const [byUserIdRow] = lock ? await byUserId.for('update') : await byUserId;
  return byUserIdRow;
}

// D32: the sole-owner guard used to be count-then-write with no lock, so two concurrent
// demotions/revocations of the last two owners could both read "count = 2", both pass, and both
// proceed -- leaving the org ownerless. This locks every 'owner' membership row in the org
// (`SELECT ... FOR UPDATE`) before counting, so a second concurrent transaction touching the same
// org's owners blocks until the first commits (or rolls back), and then sees the up-to-date count.
// Aggregate functions cannot be combined with `FOR UPDATE` in Postgres, so the rows are locked and
// counted in application code rather than via `count()`.
async function countLockedOwners(tx: any, orgId: string): Promise<number> {
  const ownerRows = await tx
    .select({ id: organizationMembers.id })
    .from(organizationMembers)
    .where(and(eq(organizationMembers.organizationId, orgId), eq(organizationMembers.role, 'owner')))
    .for('update');
  return ownerRows.length;
}

export const PATCH: RequestHandler = async ({ params, request, locals }) => {
  const session = await locals.auth();
  if (!session?.user?.id) {
    throw error(401, 'Unauthorized');
  }
  const callerUserId = session.user.id;

  const { orgId, memberId } = params;
  if (!orgId || !memberId) {
    throw error(400, 'Missing organizationId or memberId');
  }

  // Caller permission check
  const callerMembership = await requireOrgMembership(callerUserId, orgId);
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

  // D32/D33: lookup, sole-owner guard, and the role write itself all happen inside one
  // transaction, with the target row (and, if relevant, every owner row) locked via
  // `SELECT ... FOR UPDATE` before any decision is made. This closes the race where two
  // concurrent PATCHes could both observe "not the sole owner" and both demote, leaving the org
  // ownerless.
  const updatedMember = await db.transaction(async (tx) => {
    const targetMember = await findOrgMember(tx, orgId, memberId, { lock: true });

    if (!targetMember) {
      throw error(404, 'Member not found in this organization');
    }

    // Symmetry with DELETE's self-revoke guard: changing your own role through this endpoint is
    // not permitted (an owner demoting themselves must go through another owner, same as revoke).
    if (targetMember.userId === callerUserId) {
      throw error(400, 'Cannot change your own organization role');
    }

    // Admins cannot alter an owner's role
    if (callerMembership.role === 'admin' && targetMember.role === 'owner') {
      throw error(403, "Forbidden: Admins cannot alter an owner's role");
    }

    // Guard against demoting the sole owner
    if (targetMember.role === 'owner' && role !== 'owner') {
      const ownerCount = await countLockedOwners(tx, orgId);
      if (ownerCount <= 1) {
        throw error(400, 'Cannot demote the sole owner of an organization');
      }
    }

    return upsertOrganizationMember(orgId, targetMember.userId, role as Role, tx);
  });

  return json({ success: true, member: updatedMember });
};

export const DELETE: RequestHandler = async ({ params, locals }) => {
  const session = await locals.auth();
  if (!session?.user?.id) {
    throw error(401, 'Unauthorized');
  }
  const callerUserId = session.user.id;

  const { orgId, memberId } = params;
  if (!orgId || !memberId) {
    throw error(400, 'Missing organizationId or memberId');
  }

  // Caller permission check
  const callerMembership = await requireOrgMembership(callerUserId, orgId);
  if (!callerMembership || !['owner', 'admin'].includes(callerMembership.role)) {
    throw error(403, 'Forbidden: Only owners and admins can revoke organization access');
  }

  // D32/D33: same reasoning as PATCH above -- lookup, sole-owner guard, and the delete itself all
  // happen inside one transaction with the relevant rows locked via `FOR UPDATE`, so two
  // concurrent revocations of the last two owners cannot both pass.
  const targetMember = await db.transaction(async (tx) => {
    const target = await findOrgMember(tx, orgId, memberId, { lock: true });

    if (!target) {
      throw error(404, 'Member not found in this organization');
    }

    // Guard against revoking caller's own access
    if (target.userId === callerUserId) {
      throw error(400, 'Cannot revoke your own organization access');
    }

    // Admins cannot revoke an owner
    if (callerMembership.role === 'admin' && target.role === 'owner') {
      throw error(403, 'Forbidden: Admins cannot revoke owners');
    }

    // Guard against revoking the sole owner
    if (target.role === 'owner') {
      const ownerCount = await countLockedOwners(tx, orgId);
      if (ownerCount <= 1) {
        throw error(400, 'Cannot revoke the sole owner of an organization');
      }
    }

    await tx
      .delete(organizationMembers)
      .where(
        and(
          eq(organizationMembers.organizationId, orgId),
          eq(organizationMembers.userId, target.userId)
        )
      );

    // R1 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): a removed member must stop receiving
    // notifications about this org's issues. `notifyIssueEvent` (notify.ts) also re-checks
    // current org membership at fan-out time as a belt-and-suspenders guard, but the row itself
    // must go too -- otherwise it lingers forever (e.g. surviving `isSubscribed` checks if the
    // user is ever re-added, or simply as dead data). Same transaction as the member removal
    // (D18): a rollback of one must roll back the other.
    const orgIssueRows = await tx
      .select({ id: issues.id })
      .from(issues)
      .innerJoin(projects, eq(projects.id, issues.projectId))
      .where(eq(projects.organizationId, orgId));
    const orgIssueIds = orgIssueRows.map((r: { id: string }) => r.id);

    if (orgIssueIds.length > 0) {
      await tx
        .delete(issueSubscriptions)
        .where(
          and(
            eq(issueSubscriptions.subscriberType, 'user'),
            eq(issueSubscriptions.subscriberId, target.userId),
            inArray(issueSubscriptions.issueId, orgIssueIds)
          )
        );
    }

    return target;
  });

  return json({ success: true, memberId: targetMember.id, userId: targetMember.userId });
};
