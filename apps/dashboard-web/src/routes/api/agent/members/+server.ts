import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { authenticateAgentRequest } from '$lib/server/agent-auth';
import {
	createOrganizationInvitation,
	listOrganizationMembers,
	type OrgRoleValue,
	ORG_ROLES,
} from '$lib/db/queries/organizations';
import { db } from '$lib/server/db';
import { organizationMembers, users, organizations } from '$lib/db/schema';
import { eq, and, sql } from 'drizzle-orm';
import crypto from 'crypto';
import { sendInvitationEmail } from '$lib/server/email';
import { log } from '$lib/server/observability/log';
import { writeAgentAuditLog } from '$lib/server/agent-audit';

const EMAIL_REGEX = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

// GET /api/agent/members -- lists the calling key's own organization's members.
// B7: organizationId from AgentAuthContext, never a param.
export const GET: RequestHandler = async ({ request, url }) => {
	const ctx = await authenticateAgentRequest(request);

	const roleParam = url.searchParams.get('role');
	let members = await listOrganizationMembers(ctx.organizationId);

	if (roleParam) {
		members = members.filter((m) => m.role === roleParam);
	}

	return json({ members });
};

// POST /api/agent/members -- invites a user to the calling key's organization.
export const POST: RequestHandler = async ({ request, url }) => {
	const ctx = await authenticateAgentRequest(request);

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

	if (!ORG_ROLES.includes(role as OrgRoleValue)) {
		throw error(400, `Invalid role. Must be one of: ${ORG_ROLES.join(', ')}`);
	}

	const normalizedEmail = email.trim().toLowerCase();
	if (!EMAIL_REGEX.test(normalizedEmail)) {
		throw error(400, 'Invalid email address format');
	}

	// Case-insensitive check whether user is already a member
	const [existingMember] = await db
		.select()
		.from(organizationMembers)
		.innerJoin(users, eq(organizationMembers.userId, users.id))
		.where(
			and(
				eq(organizationMembers.organizationId, ctx.organizationId),
				eq(sql`lower(${users.email})`, normalizedEmail)
			)
		);

	if (existingMember) {
		throw error(400, 'User is already a member of this organization');
	}

	const token = crypto.randomBytes(32).toString('hex');
	const expiresAt = new Date(Date.now() + 7 * 24 * 60 * 60 * 1000); // 7 days

	const invite = await createOrganizationInvitation(
		ctx.organizationId,
		normalizedEmail,
		role as OrgRoleValue,
		token,
		expiresAt,
		ctx.agentId
	);

	// Fetch organization name for email dispatch
	const [org] = await db
		.select({ name: organizations.name })
		.from(organizations)
		.where(eq(organizations.id, ctx.organizationId));

	const inviteUrl = `${url.origin}/invitations/${token}`;

	let delivered = false;
	try {
		delivered = await sendInvitationEmail(
			normalizedEmail,
			inviteUrl,
			org?.name ?? 'Sentinel Organization'
		);
	} catch (err) {
		log.error('invitation.email_failed', {
			organizationId: ctx.organizationId,
			invitationId: invite.id,
			error: err instanceof Error ? err.message : String(err),
		});
	}

	await writeAgentAuditLog(ctx, 'agent.member_invited', 'invitation', invite.id, {
		email: normalizedEmail,
		role,
	});

	return json(
		{
			id: invite.id,
			email: invite.email,
			role: invite.role,
			status: invite.status,
			expiresAt: invite.expiresAt,
			delivered,
		},
		{ status: 201 }
	);
};
