import type { RequestHandler } from './$types';
import { json } from '@sveltejs/kit';
import { db } from '$lib/server/db';
import { alertConfigs, projectMembers } from '$lib/db/schema';
import { requireAuth } from '$lib/server/auth';
import { eq, and, inArray } from 'drizzle-orm';
import { hasPermission, type Role } from '$lib/rbac';
import { createNatsPublisher } from '$lib/db/queries/apikeys';

// processor-go only reloads alert_configs on a hardcoded 5-minute ticker, so a config created or
// changed in the UI would not take effect for up to 5 minutes without this. Publishing is
// best-effort: the ticker remains the correctness backstop, so a publish failure must never fail
// the request that already committed the DB write — it only costs the fast path, not correctness.
// Contract (must match the processor-side subscriber exactly, per BUGS.md B5's cross-boundary-drift
// lesson): subject "alert_config.changed", payload { projectId, configId } (configId is '' for a
// bulk/unknown case, never omitted).
async function publishAlertConfigChanged(projectId: string, configId: string) {
	try {
		await createNatsPublisher().publish('alert_config.changed', { projectId, configId });
	} catch (err) {
		console.error(
			`alert_config.changed publish failed for project ${projectId}, config ${configId} (write already committed to DB):`,
			err
		);
	}
}

// alert_configs.channel_config is a JSONB blob (1716508800_init.sql), not the flat
// channel_target/window_seconds columns this route used to reference — see schema.ts for the full note.
// This API's external shape (channelTarget as a plain string, windowSeconds as a number) is preserved here
// so callers (src/routes/settings/alerts) don't need to change; only the DB-facing mapping does.
type AlertConfigRow = {
	id: string;
	projectId: string;
	channel: string;
	channelConfig: unknown;
	frequencyThreshold: number;
	frequencyWindowSeconds: number;
	enabled: boolean;
	createdAt: Date | null;
};

// channel_config is a cross-boundary payload: this route WRITES it and apps/processor-go READS it, with
// no compiler and no shared type between them (BUGS.md B5 — changing one side requires changing the other
// in the same edit). The processor's senders read a per-channel key, NOT a generic one:
//
//   apps/processor-go/alerts/notify.go:78    alertCfg.ChannelConfig["to"]        for channel 'email'
//   apps/processor-go/alerts/notify.go:100   alertCfg.ChannelConfig["chat_id"]   for channel 'telegram'
//
// This route used to write `{ target: ... }` for every channel, which no sender ever looks up — so an
// alert config created through the dashboard resolved to an empty destination and could never deliver,
// while the row looked perfectly well-formed in the database. `alert_configs.channel` is constrained to
// exactly 'email' | 'telegram', so these two cases are the whole space.
const CHANNEL_TARGET_KEY: Record<string, string> = {
	email: 'to',
	telegram: 'chat_id',
};

// channelConfigFor builds the stored shape from this API's single external `channelTarget` string.
function channelConfigFor(channel: string, target: string): Record<string, unknown> {
	const key = CHANNEL_TARGET_KEY[channel];
	if (!key) {
		// An unknown channel cannot be routed. Fail loudly here rather than storing a row the processor
		// will silently fail to deliver — the DB CHECK should already have prevented this.
		throw new Error(`unsupported alert channel ${JSON.stringify(channel)}`);
	}
	return { [key]: target };
}

// channelTargetOf reads the target back out, accepting the per-channel key and falling back to the legacy
// `target` key so rows written before this fix still render in the UI instead of appearing blank.
function channelTargetOf(channel: string, channelConfig: Record<string, unknown>): string {
	const key = CHANNEL_TARGET_KEY[channel];
	const candidates = [key ? channelConfig[key] : undefined, channelConfig.target];
	for (const value of candidates) {
		if (typeof value === 'string' && value !== '') {
			return value;
		}
	}
	return '';
}

function toApiShape(row: AlertConfigRow) {
	const channelConfig = (row.channelConfig ?? {}) as Record<string, unknown>;
	return {
		id: row.id,
		projectId: row.projectId,
		channel: row.channel,
		channelTarget: channelTargetOf(row.channel, channelConfig),
		frequencyThreshold: row.frequencyThreshold,
		windowSeconds: row.frequencyWindowSeconds,
		enabled: row.enabled,
		createdAt: row.createdAt,
	};
}

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
				channelConfig: alertConfigs.channelConfig,
				frequencyThreshold: alertConfigs.frequencyThreshold,
				frequencyWindowSeconds: alertConfigs.frequencyWindowSeconds,
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

		return json(configs.map(toApiShape), { status: 200 });
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
				channelConfig: channelConfigFor(body.channel, body.channelTarget),
				frequencyThreshold: body.frequencyThreshold ?? 50,
				frequencyWindowSeconds: body.windowSeconds ?? 60,
				enabled: body.enabled ?? true,
			})
			.returning();

		await publishAlertConfigChanged(newConfig[0].projectId, newConfig[0].id);

		return json(toApiShape(newConfig[0]), { status: 201 });
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

		const existingChannelConfig = (existingConfig[0].channelConfig ?? {}) as Record<string, unknown>;
		const updatedConfig = await db
			.update(alertConfigs)
			.set({
				channel: body.channel ?? existingConfig[0].channel,
				channelConfig:
					body.channelTarget !== undefined
						? channelConfigFor(body.channel ?? existingConfig[0].channel, body.channelTarget)
						: existingChannelConfig,
				frequencyThreshold: body.frequencyThreshold ?? existingConfig[0].frequencyThreshold,
				frequencyWindowSeconds: body.windowSeconds ?? existingConfig[0].frequencyWindowSeconds,
				enabled: body.enabled ?? existingConfig[0].enabled,
			})
			.where(eq(alertConfigs.id, body.id))
			.returning();

		await publishAlertConfigChanged(updatedConfig[0].projectId, updatedConfig[0].id);

		return json(toApiShape(updatedConfig[0]), { status: 200 });
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

		await publishAlertConfigChanged(existingConfig[0].projectId, existingConfig[0].id);

		return json({ success: true }, { status: 200 });
	} catch (error) {
		if (error instanceof Error && error.message === 'Authentication required') {
			return json({ error: 'Authentication required' }, { status: 401 });
		}
		console.error('Error deleting alert config:', error);
		return json({ error: 'Internal server error' }, { status: 500 });
	}
};
