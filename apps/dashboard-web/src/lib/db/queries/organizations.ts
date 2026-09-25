import { db } from '$lib/server/db';
import {
  organizations,
  organizationMembers,
  organizationInvitations,
  userSessionPreferences,
  projects,
  projectMembers,
  projectEnvironmentGrants,
  users
} from '$lib/db/schema';
import { eq, and, sql, count, lt } from 'drizzle-orm';
import crypto from 'crypto';

/**
 * D06: invitation tokens are looked up and stored only as sha256(token) hex digests. The raw token
 * exists only in the emailed URL and (briefly) in transit server-side; it must never be written to
 * the database or logged.
 */
export function hashInvitationToken(token: string): string {
  return crypto.createHash('sha256').update(token).digest('hex');
}

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

// D08: explicit rank used ONLY to decide whether an invitation redemption is allowed to change an
// existing membership's role. Higher number = more privileged. This is deliberately a separate,
// narrower table from src/lib/rbac.ts's permission map -- it exists purely to answer "does the invited
// role outrank the role this user already holds", not "what can this role do".
const ORG_ROLE_RANK: Record<'owner' | 'admin' | 'engineer' | 'support' | 'viewer', number> = {
  viewer: 0,
  support: 1,
  engineer: 2,
  admin: 3,
  owner: 4,
};

/** Exported so tests (and any future caller) can reason about the rank without duplicating it. */
export function outranks(candidate: OrgRoleValue, incumbent: OrgRoleValue): boolean {
  return ORG_ROLE_RANK[candidate] > ORG_ROLE_RANK[incumbent];
}

// A db-or-tx executor: db.transaction's callback argument and the top-level `db` export expose the
// same select/insert query-builder surface in drizzle-orm, so functions below accept either.
type Executor = Pick<typeof db, 'select' | 'insert'>;

/**
 * Adds a member to an organization, or -- if a membership row already exists -- upgrades its role to
 * the invited role IF AND ONLY IF the invited role outranks (by ORG_ROLE_RANK) the role the member
 * already holds. An existing membership is NEVER downgraded by this call: accepting a 'viewer' invite
 * as an existing 'owner' leaves the caller an 'owner' (D08). Returns the resulting row.
 *
 * Reads the existing row and decides the target role in application code, then writes a concrete
 * value (rather than a SQL CASE expression) -- this is a read-then-write, not a single atomic
 * statement, so a caller needing cross-request concurrency safety on THIS specific write must supply
 * a transaction executor and hold whatever lock that requires. In this codebase the only place that
 * matters is claimInvitation, and its concurrency safety comes from the invitation claim's own
 * atomic conditional UPDATE (D07) making a second concurrent call never reach this function at all
 * for the same token -- not from this function itself.
 */
export async function upsertOrganizationMember(
  organizationId: string,
  userId: string,
  role: OrgRoleValue,
  executor: Executor = db
) {
  const [existing] = await executor
    .select()
    .from(organizationMembers)
    .where(
      and(
        eq(organizationMembers.organizationId, organizationId),
        eq(organizationMembers.userId, userId)
      )
    );

  const targetRole = existing && !outranks(role, existing.role as OrgRoleValue) ? (existing.role as OrgRoleValue) : role;

  const [member] = await executor
    .insert(organizationMembers)
    .values({ organizationId, userId, role: targetRole })
    .onConflictDoUpdate({
      target: [organizationMembers.userId, organizationMembers.organizationId],
      set: { role: targetRole },
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

export const ORG_ROLES = ['owner', 'admin', 'engineer', 'support', 'viewer'] as const;
export type OrgRoleValue = (typeof ORG_ROLES)[number];

function isOrgRole(value: unknown): value is OrgRoleValue {
  return typeof value === 'string' && (ORG_ROLES as readonly string[]).includes(value);
}

/**
 * D42: deletes PENDING invitations for `organizationId` whose expiry has already passed. Callers
 * that already hold a transaction should pass it as `executor` so the reap happens atomically with
 * whatever invitation write triggered it, per the plan's "DELETE in the same transaction as any
 * invitation write" option -- there is no separate cron route in this file's scope.
 */
export async function reapExpiredInvitations(
  organizationId: string,
  executor: Pick<typeof db, 'delete'> = db
) {
  await executor
    .delete(organizationInvitations)
    .where(
      and(
        eq(organizationInvitations.organizationId, organizationId),
        eq(organizationInvitations.status, 'pending'),
        lt(organizationInvitations.expiresAt, new Date())
      )
    );
}

/**
 * D42: reaps expired PENDING invitations across EVERY organization, returning the number deleted.
 *
 * `reapExpiredInvitations` above is scoped to a single org and only fires when that org happens to
 * issue another invitation — so an organization that stops inviting never reaps, and its expired
 * rows accumulate indefinitely. This is the unconditional sweep, driven by the existing
 * `POST /api/cron/retention` job rather than a new endpoint with its own schedule and secret.
 *
 * Now that tokens are stored hashed (D06) an unreaped row is no longer a credential at rest, so
 * this is hygiene rather than a security control — but an expired invitation still blocks
 * re-inviting the same address cleanly, and the table otherwise grows without bound.
 */
// sentinel-issue-tag-facets §5 rule 3: the retention route passes its run's connection as `exec`.
export async function reapAllExpiredInvitations(exec: typeof db = db): Promise<number> {
  const deleted = await exec
    .delete(organizationInvitations)
    .where(
      and(
        eq(organizationInvitations.status, 'pending'),
        lt(organizationInvitations.expiresAt, new Date())
      )
    )
    .returning({ id: organizationInvitations.id });

  return deleted.length;
}

/**
 * Creates an invitation for a user to join an organization. `token` is the RAW, never-persisted
 * token; only its sha256 hash (D06) is written to the row. Cleans up any existing pending invitation
 * for the same email, and reaps this org's other expired pending invitations (D42), all in the same
 * transaction as the insert.
 */
/**
 * sentinel-project-environments §4.10a, decision 28: one entry of an invitation's optional
 * environment scope, as stored in `organization_invitations.environment_scopes` (JSONB) and read
 * back at acceptance. NULL on the column -- not `[]` -- is what "unrestricted" is stored as, which
 * is what every invitation sent before this feature means.
 */
export interface InvitationEnvironmentScope {
  projectId: string;
  environments: string[];
}

export async function createOrganizationInvitation(
  organizationId: string,
  email: string,
  role: OrgRoleValue,
  token: string,
  expiresAt: Date,
  invitedBy?: string,
  environmentScopes?: InvitationEnvironmentScope[] | null
) {
  const tokenHash = hashInvitationToken(token);

  return await db.transaction(async (tx) => {
    await tx
      .delete(organizationInvitations)
      .where(
        and(
          eq(organizationInvitations.organizationId, organizationId),
          eq(organizationInvitations.email, email),
          eq(organizationInvitations.status, 'pending')
        )
      );

    await reapExpiredInvitations(organizationId, tx);

    const [invite] = await tx
      .insert(organizationInvitations)
      .values({
        organizationId,
        email,
        role,
        tokenHash,
        status: 'pending',
        expiresAt,
        invitedBy,
        // §4.10a: absent means unrestricted and stores NULL. `?? null` rather than leaving the key
        // off, so re-inviting somebody WITHOUT a scope cannot inherit a previous row's value.
        environmentScopes: environmentScopes ?? null,
      })
      .returning();

    return invite;
  });
}

/**
 * Retrieves a PENDING invitation by its raw token (hashed before lookup -- D06) with its associated
 * organization details. Returns null for a token that does not exist, is already used/revoked, or
 * has expired, since none of those are valid to act on; callers that need to distinguish those cases
 * for messaging should use `claimInvitation`, which returns a reason.
 */
export async function getInvitationByToken(token: string) {
  const tokenHash = hashInvitationToken(token);
  const [result] = await db
    .select({
      invitation: organizationInvitations,
      organization: organizations,
    })
    .from(organizationInvitations)
    .leftJoin(organizations, eq(organizationInvitations.organizationId, organizations.id))
    .where(eq(organizationInvitations.tokenHash, tokenHash));

  if (!result || !result.organization) {
    return null;
  }

  if (result.invitation.status !== 'pending' || new Date(result.invitation.expiresAt) < new Date()) {
    return null;
  }

  return {
    invitation: result.invitation,
    organization: result.organization,
  };
}

export type ClaimInvitationResult =
  | { ok: true; invitation: typeof organizationInvitations.$inferSelect; organization: typeof organizations.$inferSelect; member: typeof organizationMembers.$inferSelect }
  | { ok: false; reason: 'not_found' | 'already_used' | 'expired' | 'inviter_no_longer_authorized' };

// D31 (residual): mirrors the authority rule enforced at invitation CREATION time
// (routes/api/organizations/[orgId]/invitations/+server.ts POST) -- only owner/admin may invite at
// all, and only owner may invite an owner. Re-applied at redemption against the inviter's CURRENT
// role, not their role at the time they sent the invite.
function inviterCanStillGrant(inviterRole: string, grantedRole: OrgRoleValue): boolean {
  if (!['owner', 'admin'].includes(inviterRole)) return false;
  if (grantedRole === 'owner' && inviterRole !== 'owner') return false;
  return true;
}

// D31 (residual): a sentinel thrown INSIDE the db.transaction below, caught OUTSIDE it. Throwing
// (rather than an early `return` from the callback) is what makes Drizzle roll back the
// status='accepted' UPDATE that already ran -- a plain `return` from a transaction callback commits
// whatever the callback did so far, which would have burned the token on a refused redemption even
// though no membership was granted. Rolling back instead leaves the invitation 'pending', so it can
// still be redeemed later if the inviter's authority is restored, without ever being replayable in
// a way that grants membership on this failed attempt.
class InviterNoLongerAuthorizedError extends Error {}

/**
 * D07: atomically claims an invitation and provisions membership. This is ONE conditional UPDATE
 * (`... WHERE token_hash=$1 AND status='pending' AND expires_at > now() RETURNING *`) plus the
 * member upsert, both inside a single db.transaction -- so two concurrent redemptions of the same
 * token can never both succeed, and a failure partway through cannot leave the token both consumed
 * and unredeemed (or vice versa). The row is never deleted; status='accepted' is the permanent record.
 *
 * D31: the role on the claimed row is re-validated against the ORG_ROLES allowlist here, at the
 * moment of redemption, rather than cast. A stale or tampered role value refuses instead of granting
 * an unmodelled role.
 */
export async function claimInvitation(rawToken: string, userId: string): Promise<ClaimInvitationResult> {
  const tokenHash = hashInvitationToken(rawToken);

  try {
    return await claimInvitationTx(tokenHash, userId);
  } catch (err) {
    if (err instanceof InviterNoLongerAuthorizedError) {
      return { ok: false, reason: 'inviter_no_longer_authorized' };
    }
    throw err;
  }
}

async function claimInvitationTx(tokenHash: string, userId: string): Promise<ClaimInvitationResult> {
  return await db.transaction(async (tx) => {
    const [claimed] = await tx
      .update(organizationInvitations)
      .set({ status: 'accepted', acceptedAt: new Date() })
      .where(
        and(
          eq(organizationInvitations.tokenHash, tokenHash),
          eq(organizationInvitations.status, 'pending'),
          sql`${organizationInvitations.expiresAt} > now()`
        )
      )
      .returning();

    if (!claimed) {
      const [existing] = await tx
        .select({ status: organizationInvitations.status, expiresAt: organizationInvitations.expiresAt })
        .from(organizationInvitations)
        .where(eq(organizationInvitations.tokenHash, tokenHash));

      if (!existing) return { ok: false, reason: 'not_found' };
      if (existing.status !== 'pending') return { ok: false, reason: 'already_used' };
      return { ok: false, reason: 'expired' };
    }

    if (!isOrgRole(claimed.role)) {
      throw new Error(`Invitation ${claimed.id} has an unrecognized role '${claimed.role}'; refusing to grant it.`);
    }

    // D31 (residual): the invitation captured a role that was valid to grant AT CREATION TIME, but
    // an owner/admin invite can sit pending for up to 7 days. If the inviter has since been
    // demoted or removed, their outstanding grant must not still be honored -- a pending 'owner'
    // invite from a now-viewer should not mint a new owner. `invited_by` is nullable (rows created
    // before this column existed, or an inviter whose account was deleted) and unrecordable
    // authority is treated the same as *lost* authority: refuse, don't guess. Throwing here (see
    // InviterNoLongerAuthorizedError above) rolls back the status='accepted' write above, so the
    // invitation stays 'pending' rather than being burned on a refused attempt.
    if (!claimed.invitedBy) {
      throw new InviterNoLongerAuthorizedError();
    }
    const [inviterMembership] = await tx
      .select({ role: organizationMembers.role })
      .from(organizationMembers)
      .where(
        and(
          eq(organizationMembers.organizationId, claimed.organizationId),
          eq(organizationMembers.userId, claimed.invitedBy)
        )
      );
    if (!inviterMembership || !inviterCanStillGrant(inviterMembership.role, claimed.role)) {
      throw new InviterNoLongerAuthorizedError();
    }

    const [organization] = await tx.select().from(organizations).where(eq(organizations.id, claimed.organizationId));
    if (!organization) {
      throw new Error(`Invitation ${claimed.id} references missing organization ${claimed.organizationId}`);
    }

    // D08: never downgrades an existing membership -- see upsertOrganizationMember's doc comment.
    // Running it against `tx` keeps it inside the same transaction as the claim above, so a failure
    // here rolls back the status='accepted' claim too (the token is not burned for nothing).
    const member = await upsertOrganizationMember(claimed.organizationId, userId, claimed.role, tx);

    // sentinel-project-environments §4.10a, decision 28: one project_environment_grants row per
    // entry, IN THE SAME TRANSACTION as the membership. `environment_scopes` is NULL for every
    // invitation sent without a scope -- which is every invitation sent today -- and nothing is
    // written then, so the accepted member is unrestricted. That absence is the claim that matters.
    await applyInvitationEnvironmentScopes(tx, claimed, userId);

    return { ok: true, invitation: claimed, organization, member };
  });
}

/**
 * sentinel-project-environments §4.10a — writes the invitation's environment scopes as grant rows,
 * inside the caller's membership transaction.
 *
 * Three rules, each of which comes from somewhere:
 *
 * - **Nothing is written when the column is NULL**, which is every invitation sent today. §2.2's
 *   absence-means-all rule is unchanged and nothing narrows a person by machine.
 * - **A stale entry is SKIPPED, not fatal, and "stale" means a deleted PROJECT.** An invitation that
 *   refuses to be accepted because of a settings change made after it was sent is worse than a
 *   member who is wider than intended and can be narrowed through §4.10. (There is no deleted
 *   *environment* transition in this feature: §4.1–§4.3 define no DELETE on project_environments,
 *   and the only removal is ON DELETE CASCADE from projects, which takes the grant with it.)
 * - **An UPSERT, never a plain INSERT.** `PRIMARY KEY (user_id, project_id)`, and the invitee may
 *   already hold a grant in that project -- two scoped invitations to the same person for the same
 *   organization, or a member narrowed through §4.10 after joining and then re-invited. A plain
 *   INSERT raises 23505 and aborts this whole transaction, so the person could not join at all.
 *   The invitation's scope wins because the inviter made an explicit choice on it.
 *
 * `created_by` is the inviter (`invited_by`), not the accepting user: a boundary with no author is
 * a boundary nobody can ask about, and the person who chose the narrowing is the inviter.
 */
async function applyInvitationEnvironmentScopes(
  tx: Executor,
  claimed: typeof organizationInvitations.$inferSelect,
  acceptingUserId: string
): Promise<void> {
  const raw = claimed.environmentScopes;
  if (!Array.isArray(raw) || raw.length === 0) return;

  for (const entry of raw as InvitationEnvironmentScope[]) {
    if (!entry || typeof entry.projectId !== 'string') continue;
    if (!Array.isArray(entry.environments) || entry.environments.length === 0) continue;

    const [project] = await tx
      .select({ id: projects.id })
      .from(projects)
      .where(eq(projects.id, entry.projectId));
    if (!project) continue; // stale: the project is gone. Skip; the member is unrestricted there.

    await tx
      .insert(projectEnvironmentGrants)
      .values({
        userId: acceptingUserId,
        projectId: entry.projectId,
        environments: entry.environments,
        createdBy: claimed.invitedBy ?? acceptingUserId,
      })
      .onConflictDoUpdate({
        target: [projectEnvironmentGrants.userId, projectEnvironmentGrants.projectId],
        set: { environments: entry.environments, createdBy: claimed.invitedBy ?? acceptingUserId },
      });
  }
}

/**
 * Revokes a still-pending invitation (D07's missing revocation path). No-ops (returns undefined)
 * if the invitation is missing or already resolved, so callers can treat "nothing to revoke" and
 * "revoked" uniformly without a race against a concurrent redemption -- the UPDATE's WHERE clause is
 * the same single-statement conditional pattern claimInvitation uses.
 */
export async function revokeOrganizationInvitation(organizationId: string, invitationId: string) {
  const [revoked] = await db
    .update(organizationInvitations)
    .set({ status: 'revoked' })
    .where(
      and(
        eq(organizationInvitations.id, invitationId),
        eq(organizationInvitations.organizationId, organizationId),
        eq(organizationInvitations.status, 'pending')
      )
    )
    .returning();

  return revoked;
}

/**
 * Lists invitations for an organization (all statuses) so an admin can see what is outstanding.
 * Never returns tokenHash or any other secret-derived column -- only fields a UI legitimately needs.
 */
export async function listOrganizationInvitations(organizationId: string) {
  return await db
    .select({
      id: organizationInvitations.id,
      email: organizationInvitations.email,
      role: organizationInvitations.role,
      status: organizationInvitations.status,
      expiresAt: organizationInvitations.expiresAt,
      createdAt: organizationInvitations.createdAt,
      acceptedAt: organizationInvitations.acceptedAt,
    })
    .from(organizationInvitations)
    .where(eq(organizationInvitations.organizationId, organizationId));
}


/**
 * Creates a project inside an organization and makes the creator a project member.
 *
 * The membership row is the point: `getUserProjectAccess` (lib/server/projects.ts) reads
 * `project_members`, and the dashboard's issue list is scoped to what it returns. A project
 * created without one is invisible to the person who just created it.
 *
 * `projects.api_key` / `api_key_hash` are NOT NULL legacy columns that NOTHING authenticates
 * against — ingestion resolves a key through `project_api_keys.key_hash`
 * (apps/ingestor-go/auth/apikey.go). They are filled with an inert unique placeholder, the same
 * shape the auto-provisioned Triage inbox project uses, and the real SDK key is minted afterwards
 * through the existing key UI. Storing a usable secret here would be a credential nothing checks.
 */
export async function createProject(
	userId: string,
	organizationId: string,
	data: { name: string }
) {
	const placeholder = crypto.randomBytes(24).toString('hex');

	return await db.transaction(async (tx) => {
		const [project] = await tx
			.insert(projects)
			.values({
				organizationId,
				name: data.name,
				apiKey: `proj_${placeholder}`,
				apiKeyHash: crypto.createHash('sha256').update(placeholder).digest('hex'),
			})
			.returning();

		await tx.insert(projectMembers).values({
			projectId: project.id,
			userId,
			// 'admin' of the project they created — project_members.role is CHECKed against
			// ('admin','developer','viewer','support'); 'owner' is an ORGANIZATION role and would
			// violate the constraint.
			role: 'admin',
		});

		return project;
	});
}

/**
 * Fetches all organization members for an organization with user details.
 */
export async function listOrganizationMembers(organizationId: string) {
	const members = await db
		.select({
			id: organizationMembers.id,
			role: organizationMembers.role,
			user: {
				id: users.id,
				name: users.name,
				email: users.email,
			},
		})
		.from(organizationMembers)
		.innerJoin(users, eq(organizationMembers.userId, users.id))
		.where(eq(organizationMembers.organizationId, organizationId));

	return members.map((m) => ({
		id: m.id,
		userId: m.user.id,
		user: {
			name: m.user.name ?? m.user.email,
			email: m.user.email,
		},
		role: m.role,
	}));
}
