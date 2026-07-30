import type { RequestHandler } from './$types';
import { json } from '@sveltejs/kit';
import { db } from '$lib/server/db';
import { alertConfigs, projectMembers, projects, organizationMembers } from '$lib/db/schema';
import { requireAuth } from '$lib/server/auth';
import { eq, and, or, inArray, isNull } from 'drizzle-orm';
import { hasPermission, type Role, type OrgRole } from '$lib/rbac';
import { createNatsPublisher } from '$lib/db/queries/apikeys';

// processor-go only reloads alert_configs on a hardcoded 5-minute ticker, so a config created or
// changed in the UI would not take effect for up to 5 minutes without this. Publishing is
// best-effort: the ticker remains the correctness backstop, so a publish failure must never fail
// the request that already committed the DB write — it only costs the fast path, not correctness.
// Contract (must match the processor-side subscriber exactly, per BUGS.md B5's cross-boundary-drift
// lesson): subject "alert_config.changed", payload { projectId, configId }. configId is '' for a
// bulk/unknown case, never omitted; projectId is also '' for an organization-wide config (one that
// applies to every project in the org, not a single one) — same "unknown/bulk" convention, since the
// processor subscriber reloads its entire in-memory config set on ANY message regardless of payload
// contents, so this is a logging/debug concern, not a correctness one.
async function publishAlertConfigChanged(projectId: string, configId: string) {
	try {
		await createNatsPublisher().publish('alert_config.changed', { projectId, configId });
	} catch (err) {
		console.error(
			`alert_config.changed publish failed for project ${projectId || '(org-wide)'}, config ${configId} (write already committed to DB):`,
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
	organizationId: string;
	projectId: string | null;
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

// 'scope' is the explicit layer marker the GET response promises callers: an org-wide config's projectId
// is NULL in the DB, but making the caller infer the layer from a null check is exactly the kind of
// implicit contract that has drifted before (BUGS.md B5/B6) — so every returned row spells it out.
function toApiShape(row: AlertConfigRow) {
	const channelConfig = (row.channelConfig ?? {}) as Record<string, unknown>;
	return {
		id: row.id,
		scope: row.projectId === null ? ('organization' as const) : ('project' as const),
		organizationId: row.organizationId,
		projectId: row.projectId,
		channel: row.channel,
		channelTarget: channelTargetOf(row.channel, channelConfig),
		frequencyThreshold: row.frequencyThreshold,
		windowSeconds: row.frequencyWindowSeconds,
		enabled: row.enabled,
		createdAt: row.createdAt,
	};
}

// Org-wide alert config mutation (create/update/delete) is gated on 'manage_keys', not 'write'. An
// org-wide config routes every project's alerts in the organization — the same blast radius as an
// org-wide API key (project_api_keys with project_id NULL), which is already gated on 'manage_keys' per
// spec 008 FR-007 ("owner, admin, engineer"). Reusing that permission keeps the two org-wide-resource
// stories consistent instead of inventing a parallel bar with its own role set to reason about; it also
// correctly excludes 'support' and 'viewer', who only have 'read' and must not be able to reroute alerts
// for every project in the org.
const ORG_WIDE_ALERT_PERMISSION = 'manage_keys';

async function requireProjectAlertAccess(userId: string, projectId: string, permission: 'write' | 'delete') {
	const [membership] = await db
		.select({ role: projectMembers.role })
		.from(projectMembers)
		.where(and(eq(projectMembers.userId, userId), eq(projectMembers.projectId, projectId)));

	if (!membership) {
		return { ok: false as const, status: 403, error: 'Access denied to this project' };
	}
	if (!hasPermission(membership.role as Role, permission)) {
		return { ok: false as const, status: 403, error: 'Insufficient permissions for this alert config' };
	}
	return { ok: true as const };
}

async function requireOrgAlertAccess(userId: string, organizationId: string) {
	const [membership] = await db
		.select({ role: organizationMembers.role })
		.from(organizationMembers)
		.where(and(eq(organizationMembers.userId, userId), eq(organizationMembers.organizationId, organizationId)));

	if (!membership) {
		return { ok: false as const, status: 403, error: 'Access denied to this organization' };
	}
	if (!hasPermission(membership.role as OrgRole, ORG_WIDE_ALERT_PERMISSION)) {
		return {
			ok: false as const,
			status: 403,
			error: 'Insufficient permissions for this organization-wide alert config',
		};
	}
	return { ok: true as const };
}

// Authorizes a mutation (update/delete) against an EXISTING row, routing to the project-level or
// org-level check based on which layer the row already belongs to. The layer of an existing row is never
// taken from the request body — only from what is already stored — so a project-only member cannot
// smuggle their way into touching an org-wide config by omitting projectId in the request.
async function requireMutationAccess(
	userId: string,
	existing: { organizationId: string; projectId: string | null },
	projectPermission: 'write' | 'delete'
) {
	if (existing.projectId === null) {
		return requireOrgAlertAccess(userId, existing.organizationId);
	}
	return requireProjectAlertAccess(userId, existing.projectId, projectPermission);
}

export const GET: RequestHandler = async ({ locals }) => {
	try {
		const user = await requireAuth({ locals } as any);

		const [userProjectMemberships, userOrgMemberships] = await Promise.all([
			db
				.select({ projectId: projectMembers.projectId })
				.from(projectMembers)
				.where(eq(projectMembers.userId, user.id)),
			db
				.select({ organizationId: organizationMembers.organizationId })
				.from(organizationMembers)
				.where(eq(organizationMembers.userId, user.id)),
		]);

		const projectIds = userProjectMemberships.map((m) => m.projectId);
		const orgIds = userOrgMemberships.map((m) => m.organizationId);

		if (projectIds.length === 0 && orgIds.length === 0) {
			return json([], { status: 200 });
		}

		// Visible rows are: project-scoped configs for any project the caller is a member of, UNION
		// org-wide configs (projectId IS NULL) for any organization the caller is a member of. Read access
		// only requires membership — every role, including 'support'/'viewer', has 'read'.
		const visibility = [];
		if (projectIds.length > 0) {
			visibility.push(inArray(alertConfigs.projectId, projectIds));
		}
		if (orgIds.length > 0) {
			visibility.push(and(isNull(alertConfigs.projectId), inArray(alertConfigs.organizationId, orgIds)));
		}

		const configs = await db
			.select({
				id: alertConfigs.id,
				organizationId: alertConfigs.organizationId,
				projectId: alertConfigs.projectId,
				channel: alertConfigs.channel,
				channelConfig: alertConfigs.channelConfig,
				frequencyThreshold: alertConfigs.frequencyThreshold,
				frequencyWindowSeconds: alertConfigs.frequencyWindowSeconds,
				enabled: alertConfigs.enabled,
				createdAt: alertConfigs.createdAt,
			})
			.from(alertConfigs)
			.where(visibility.length === 1 ? visibility[0] : or(...visibility));

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

		if (!body.channel || !body.channelTarget) {
			return json(
				{ error: 'Missing required fields: channel, channelTarget' },
				{ status: 400 }
			);
		}

		if (!['email', 'telegram'].includes(body.channel)) {
			return json({ error: 'Invalid channel. Must be email or telegram' }, { status: 400 });
		}

		// projectId omitted/null means "create an organization-wide config" — it is never inferred, only
		// read directly off what the caller sent.
		const requestedProjectId: string | null =
			typeof body.projectId === 'string' && body.projectId !== '' ? body.projectId : null;

		let organizationId: string;
		let projectId: string | null;

		if (requestedProjectId === null) {
			if (!body.organizationId) {
				return json(
					{ error: 'Missing required field: organizationId (required when projectId is omitted)' },
					{ status: 400 }
				);
			}

			const access = await requireOrgAlertAccess(user.id, body.organizationId);
			if (!access.ok) {
				return json({ error: access.error }, { status: access.status });
			}

			organizationId = body.organizationId;
			projectId = null;
		} else {
			const access = await requireProjectAlertAccess(user.id, requestedProjectId, 'write');
			if (!access.ok) {
				return json({ error: access.error }, { status: access.status });
			}

			// organizationId for a project-scoped config is derived from the project itself (tenant scope
			// must come from data already established as belonging to the caller's project, never from a
			// body field — same rule as B7) rather than trusted from the request body.
			const [project] = await db
				.select({ organizationId: projects.organizationId })
				.from(projects)
				.where(eq(projects.id, requestedProjectId));

			if (!project?.organizationId) {
				return json({ error: 'Project is not associated with an organization' }, { status: 400 });
			}

			organizationId = project.organizationId;
			projectId = requestedProjectId;
		}

		const newConfig = await db
			.insert(alertConfigs)
			.values({
				organizationId,
				projectId,
				channel: body.channel,
				channelConfig: channelConfigFor(body.channel, body.channelTarget),
				frequencyThreshold: body.frequencyThreshold ?? 50,
				frequencyWindowSeconds: body.windowSeconds ?? 60,
				enabled: body.enabled ?? true,
			})
			.returning();

		await publishAlertConfigChanged(newConfig[0].projectId ?? '', newConfig[0].id);

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

		// Which layer to authorize against comes from the STORED row, not the request body — a
		// project-only member cannot touch an org-wide config by manipulating what they send.
		const access = await requireMutationAccess(user.id, existingConfig[0], 'write');
		if (!access.ok) {
			return json({ error: access.error }, { status: access.status });
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

		await publishAlertConfigChanged(updatedConfig[0].projectId ?? '', updatedConfig[0].id);

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

		const access = await requireMutationAccess(user.id, existingConfig[0], 'delete');
		if (!access.ok) {
			return json({ error: access.error }, { status: access.status });
		}

		await db.delete(alertConfigs).where(eq(alertConfigs.id, body.id));

		await publishAlertConfigChanged(existingConfig[0].projectId ?? '', existingConfig[0].id);

		return json({ success: true }, { status: 200 });
	} catch (error) {
		if (error instanceof Error && error.message === 'Authentication required') {
			return json({ error: 'Authentication required' }, { status: 401 });
		}
		console.error('Error deleting alert config:', error);
		return json({ error: 'Internal server error' }, { status: 500 });
	}
};
