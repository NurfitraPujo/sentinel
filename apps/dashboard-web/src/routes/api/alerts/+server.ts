import type { RequestHandler } from './$types';
import { json } from '@sveltejs/kit';
import { db } from '$lib/server/db';
import { alertConfigs, projectMembers } from '$lib/db/schema';
import { requireAuth } from '$lib/server/auth';
import { eq, and, inArray } from 'drizzle-orm';
import { hasPermission, type Role } from '$lib/rbac';

export const GET: RequestHandler = async ({ locals }) => {
	try {
		const user = await requireAuth({ locals } as any);

		const userProjectMemberships = await db
			.select({
				projectId: projectMembers.projectId,
				role: projectMembers.role,
			})
			.from(projectMembers)
			.where(eq(projectMembers.userId, user.id));

		if (userProjectMemberships.length === 0) {
			return json([], { status: 200 });
		}

		const configs = await db
			.select({
				id: alertConfigs.id,
				projectId: alertConfigs.projectId,
				channel: alertConfigs.channel,
				channelTarget: alertConfigs.channelTarget,
				frequencyThreshold: alertConfigs.frequencyThreshold,
				windowSeconds: alertConfigs.windowSeconds,
				enabled: alertConfigs.enabled,
				createdAt: alertConfigs.createdAt,
			})
			.from(alertConfigs)
			.where(
				inArray(
					alertConfigs.projectId,
					userProjectMemberships.map((m) => m.projectId)
				)
			);

		return json(configs, { status: 200 });
	} catch (error) {
		if (error instanceof Error && error.message === 'Authentication required') {
			return json({ error: 'Authentication required' }, { status: 401 });
		}
		console.error('Error fetching alert configs:', error);
		return json({ error: 'Internal server error' }, { status: 500 });
	}
};

export const POST: RequestHandler = async ({ request, locals }) => {
	try {
		const user = await requireAuth({ locals } as any);

		const body = await request.json();

		if (!body.projectId || !body.channel || !body.channelTarget) {
			return json(
				{ error: 'Missing required fields: projectId, channel, channelTarget' },
				{ status: 400 }
			);
		}

		if (!['email', 'telegram'].includes(body.channel)) {
			return json({ error: 'Invalid channel. Must be email or telegram' }, { status: 400 });
		}

		const membershipResult = await db
			.select({
				projectId: projectMembers.projectId,
				role: projectMembers.role,
			})
			.from(projectMembers)
			.where(and(eq(projectMembers.userId, user.id), eq(projectMembers.projectId, body.projectId)));

		if (membershipResult.length === 0) {
			return json({ error: 'Access denied to this project' }, { status: 403 });
		}

		const role = membershipResult[0].role as Role;
		if (!hasPermission(role, 'write')) {
			return json({ error: 'Insufficient permissions to create alert configs' }, { status: 403 });
		}

		const newConfig = await db
			.insert(alertConfigs)
			.values({
				projectId: body.projectId,
				channel: body.channel,
				channelTarget: body.channelTarget,
				frequencyThreshold: body.frequencyThreshold ?? 50,
				windowSeconds: body.windowSeconds ?? 60,
				enabled: body.enabled ?? true,
			})
			.returning();

		return json(newConfig[0], { status: 201 });
	} catch (error) {
		if (error instanceof Error && error.message === 'Authentication required') {
			return json({ error: 'Authentication required' }, { status: 401 });
		}
		console.error('Error creating alert config:', error);
		return json({ error: 'Internal server error' }, { status: 500 });
	}
};

export const PUT: RequestHandler = async ({ request, locals }) => {
	try {
		const user = await requireAuth({ locals } as any);

		const body = await request.json();

		if (!body.id) {
			return json({ error: 'Missing required field: id' }, { status: 400 });
		}

		const existingConfig = await db
			.select()
			.from(alertConfigs)
			.where(eq(alertConfigs.id, body.id));

		if (existingConfig.length === 0) {
			return json({ error: 'Alert config not found' }, { status: 404 });
		}

		const membershipResult = await db
			.select({
				projectId: projectMembers.projectId,
				role: projectMembers.role,
			})
			.from(projectMembers)
			.where(
				and(
					eq(projectMembers.userId, user.id),
					eq(projectMembers.projectId, existingConfig[0].projectId)
				)
			);

		if (membershipResult.length === 0) {
			return json({ error: 'Access denied to this project' }, { status: 403 });
		}

		const role = membershipResult[0].role as Role;
		if (!hasPermission(role, 'write')) {
			return json({ error: 'Insufficient permissions to update alert configs' }, { status: 403 });
		}

		const updatedConfig = await db
			.update(alertConfigs)
			.set({
				channel: body.channel ?? existingConfig[0].channel,
				channelTarget: body.channelTarget ?? existingConfig[0].channelTarget,
				frequencyThreshold: body.frequencyThreshold ?? existingConfig[0].frequencyThreshold,
				windowSeconds: body.windowSeconds ?? existingConfig[0].windowSeconds,
				enabled: body.enabled ?? existingConfig[0].enabled,
			})
			.where(eq(alertConfigs.id, body.id))
			.returning();

		return json(updatedConfig[0], { status: 200 });
	} catch (error) {
		if (error instanceof Error && error.message === 'Authentication required') {
			return json({ error: 'Authentication required' }, { status: 401 });
		}
		console.error('Error updating alert config:', error);
		return json({ error: 'Internal server error' }, { status: 500 });
	}
};

export const DELETE: RequestHandler = async ({ request, locals }) => {
	try {
		const user = await requireAuth({ locals } as any);

		const body = await request.json();

		if (!body.id) {
			return json({ error: 'Missing required field: id' }, { status: 400 });
		}

		const existingConfig = await db
			.select()
			.from(alertConfigs)
			.where(eq(alertConfigs.id, body.id));

		if (existingConfig.length === 0) {
			return json({ error: 'Alert config not found' }, { status: 404 });
		}

		const membershipResult = await db
			.select({
				projectId: projectMembers.projectId,
				role: projectMembers.role,
			})
			.from(projectMembers)
			.where(
				and(
					eq(projectMembers.userId, user.id),
					eq(projectMembers.projectId, existingConfig[0].projectId)
				)
			);

		if (membershipResult.length === 0) {
			return json({ error: 'Access denied to this project' }, { status: 403 });
		}

		const role = membershipResult[0].role as Role;
		if (!hasPermission(role, 'delete')) {
			return json({ error: 'Insufficient permissions to delete alert configs' }, { status: 403 });
		}

		await db.delete(alertConfigs).where(eq(alertConfigs.id, body.id));

		return json({ success: true }, { status: 200 });
	} catch (error) {
		if (error instanceof Error && error.message === 'Authentication required') {
			return json({ error: 'Authentication required' }, { status: 401 });
		}
		console.error('Error deleting alert config:', error);
		return json({ error: 'Internal server error' }, { status: 500 });
	}
};