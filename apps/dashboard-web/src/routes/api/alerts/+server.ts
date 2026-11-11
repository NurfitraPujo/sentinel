import type { RequestHandler } from './$types';
import { json, isHttpError } from '@sveltejs/kit';
import { db } from '$lib/server/db';
import { alertConfigs, projectMembers, projects, organizationMembers, projectEnvironments } from '$lib/db/schema';
import { requireAuth } from '$lib/server/auth';
import { eq, and, or, inArray, isNull } from 'drizzle-orm';
import { hasPermission, type Role, type OrgRole } from '$lib/rbac';
import { createNatsPublisher } from '$lib/db/queries/apikeys';
import { log } from '$lib/server/observability/log';
import { CHANNEL_TARGET_KEY, channelTargetOf } from '$lib/alerts';
import { resolveEnvironmentScope } from '$lib/server/environment-access';
import { resolveEnvironmentUnion } from '$lib/server/environment-union';

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
		log.error('alert_config.changed.publish_failed', {
			projectId: projectId || '(org-wide)',
			configId,
			note: 'write already committed to DB',
			error: err,
		});
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
	environments: string[] | null;
	createdAt: Date | null;
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
		// sentinel-project-environments §4.7. `null` means EVERY environment — today's behaviour and
		// what every pre-feature row carries — and is returned as `null`, never as `[]`: an empty
		// array is a config that can never fire, which is why the request side answers `[]` with a
		// 400. The response always carries the MERGED, stored value, because §4.7 tells the client to
		// re-render its picker from it (flow E11: "the picker still shows what the config actually
		// holds").
		environments: row.environments ?? null,
		createdAt: row.createdAt,
	};
}

// ---------------------------------------------------------------------------
// sentinel-project-environments §4.7 — environment scoping on alert configs.
// ---------------------------------------------------------------------------

const ENVIRONMENTS_EMPTY_ERROR = 'environments must be null or a non-empty list';
const ENVIRONMENTS_FORBIDDEN_ERROR = 'environments contains an environment you do not have access to';

type ParsedEnvironments =
	| { ok: true; supplied: false }
	| { ok: true; supplied: true; value: string[] | null }
	| { ok: false; error: string };

/**
 * Reads `environments` off a request body, distinguishing THREE states that a naive `?? existing`
 * collapses into two:
 *
 *   - the key is ABSENT       -> `supplied: false`. On PUT this means "leave unchanged" (decision
 *                                32), and the scope guard must not run: a guard on a change nobody
 *                                requested would fail a save of an unrelated field. On POST it means
 *                                "apply the project default" (decision 11).
 *   - the key is `null`       -> every environment. This is what the picker's "All environments"
 *                                state sends, and it is deliberately NOT guarded — see the asymmetry
 *                                §4.7 spells out and calls intended.
 *   - a non-empty array       -> exactly those names, after the guard and the merge below.
 *
 * `[]` is a 400 and never "none" (§7): a config that can never fire is a config somebody thinks is
 * working.
 */
function parseEnvironments(body: Record<string, unknown>): ParsedEnvironments {
	if (!('environments' in body) || body.environments === undefined) {
		return { ok: true, supplied: false };
	}
	const raw = body.environments;
	if (raw === null) {
		return { ok: true, supplied: true, value: null };
	}
	if (!Array.isArray(raw) || raw.length === 0) {
		return { ok: false, error: ENVIRONMENTS_EMPTY_ERROR };
	}
	if (raw.some((name) => typeof name !== 'string' || name === '')) {
		return { ok: false, error: 'environments must be a list of environment names' };
	}
	// Names, not ids (§4.7): an org-wide config spans projects and an id belongs to one project. They
	// are deliberately NOT validated against project_environments — under decision 8 an environment
	// may be scoped before anybody declares it, and a name that never matches simply never fires.
	return { ok: true, supplied: true, value: [...new Set(raw as string[])] };
}

type ConfigScope = { organizationId: string; projectId: string | null };

/**
 * §4.7's scope guard, and it is TWO rules because a config has two scopes (decision 26).
 *
 * A project-scoped config is checked against the caller's grant for that project; an ORG-WIDE config
 * has no project for `resolveEnvironmentScope` to be keyed by, so it is checked against §4.3a's
 * union over the caller's accessible projects — the same list the picker is populated from, so the
 * server enforces exactly what the control already shows.
 *
 * **The guarded set is `supplied \ existing`, not `supplied` (decision 48).** The merge below
 * preserves stored names that were not offerable to this caller and the response returns the merged
 * array, so a narrowed member legitimately sees — and posts back — a name outside their grant.
 * Guarding `supplied` would answer a change they were entitled to make with a 400 they had no way to
 * avoid from the picker. A caller may always re-submit what the server just showed them; only a name
 * NEW to this config and outside their scope is a 400.
 */
async function environmentsWithinCallerScope(
	userId: string,
	scope: ConfigScope,
	supplied: string[],
	existing: string[] | null
): Promise<boolean> {
	const stored = existing ?? [];
	const novel = supplied.filter((name) => !stored.includes(name));
	if (novel.length === 0) {
		return true;
	}

	if (scope.projectId !== null) {
		const granted = await resolveEnvironmentScope(userId, scope.projectId);
		// null = no grant row = unrestricted. Absence means all (§2.2), and inverting it here would
		// lock every ordinary caller out of scoping a config at all.
		if (granted === null) {
			return true;
		}
		return novel.every((name) => granted.includes(name));
	}

	const union = await resolveEnvironmentUnion(userId, scope.organizationId);
	return novel.every((name) => union.includes(name));
}

/**
 * The names §4.1 (`includeHidden=false`, scope-filtered) or §4.3a's union would have OFFERED this
 * caller for this config's scope — i.e. the names their picker could have shown.
 *
 * This is a different question from the guard's, and the difference is the point: the guard asks
 * what the caller may ADD, this asks what they could have DESELECTED.
 */
async function offerableEnvironmentNames(userId: string, scope: ConfigScope): Promise<string[]> {
	if (scope.projectId === null) {
		return resolveEnvironmentUnion(userId, scope.organizationId);
	}

	const [rows, granted] = await Promise.all([
		db
			.select({ name: projectEnvironments.name })
			.from(projectEnvironments)
			.where(
				and(eq(projectEnvironments.projectId, scope.projectId), eq(projectEnvironments.hidden, false))
			),
		resolveEnvironmentScope(userId, scope.projectId),
	]);

	const names = rows.map((row) => row.name);
	return granted === null ? names : names.filter((name) => granted.includes(name));
}

/**
 * §4.7's merge: the server MERGES a supplied array, it does not replace with it.
 *
 *     stored = supplied  u  { name in existing : name was not offerable to this caller }
 *
 * A config may legitimately carry an environment the caller's picker could not have offered — one
 * that is unregistered (§4.7 permits scoping a name nobody has declared), one that is hidden (§4.2
 * removes it from the selector, decision 43), or one outside the caller's grant. A client that posts
 * back what its picker showed would silently drop those on any unrelated save, and the config would
 * quietly start paging for everything it had been scoped away from.
 *
 * Names the caller COULD have been offered and did not send are removed — that is a real
 * deselection and it must work.
 */
async function mergeEnvironments(
	userId: string,
	scope: ConfigScope,
	supplied: string[],
	existing: string[] | null
): Promise<string[]> {
	if (existing === null || existing.length === 0) {
		return supplied;
	}
	const offerable = await offerableEnvironmentNames(userId, scope);
	const preserved = existing.filter((name) => !offerable.includes(name));
	return [...new Set([...supplied, ...preserved])];
}

/**
 * Decision 11 (`OQ-5` (c)) — the default for a NEW config, and the notice its failure owes.
 *
 * A project-scoped config in a project that HAS an `is_default` environment is scoped to it. One in
 * a project WITHOUT a default falls back to `null` (every environment) — the alternative is a config
 * scoped to nothing, and §4.7 already refuses `[]` because such a config silently never fires — and
 * the fallback is reported on the creation response as `defaultEnvironmentApplied: false`.
 *
 * That flag is an EVENT, not a state (decision 33): it is not persisted, no column carries it, and
 * it is available exactly on the response that reports the creation. The persistent half of the
 * warning is the project-settings Environments section's visible "no default set" (§4.1), because
 * "no default is set" is a property of the PROJECT and would go stale beside the config the moment
 * somebody set one.
 *
 * An org-wide config gets `null` too, but that is not a fallback and carries no notice: there is no
 * single project and therefore no single default, and deriving one from "the default of every
 * project in the org" would produce a name list that changes as projects are added.
 */
async function defaultEnvironmentsForNewConfig(
	projectId: string | null
): Promise<{ environments: string[] | null; defaultEnvironmentApplied: boolean | null }> {
	if (projectId === null) {
		return { environments: null, defaultEnvironmentApplied: null };
	}

	const [defaultEnvironment] = await db
		.select({ name: projectEnvironments.name })
		.from(projectEnvironments)
		.where(and(eq(projectEnvironments.projectId, projectId), eq(projectEnvironments.isDefault, true)));

	if (!defaultEnvironment) {
		return { environments: null, defaultEnvironmentApplied: false };
	}
	return { environments: [defaultEnvironment.name], defaultEnvironmentApplied: true };
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
				environments: alertConfigs.environments,
				createdAt: alertConfigs.createdAt,
			})
			.from(alertConfigs)
			.where(visibility.length === 1 ? visibility[0] : or(...visibility));

		return json(configs.map(toApiShape), { status: 200 });
	} catch (error) {
		// requireAuth throws SvelteKit's HttpError now, not a bare Error whose message could be
		// string-matched. The 401 body stays byte-identical to what this endpoint has always
		// returned — `{ error: ... }`, not SvelteKit's own `{ message: ... }` shape.
		if (isHttpError(error) && error.status === 401) {
			return json({ error: 'Authentication required' }, { status: 401 });
		}
		log.error('alerts.fetch_failed', { error });
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

		const parsedEnvironments = parseEnvironments(body);
		if (!parsedEnvironments.ok) {
			return json({ error: parsedEnvironments.error }, { status: 400 });
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

		// §4.7's environment scope. Resolved AFTER authorization, because both branches below read
		// data (the caller's grant, the project's default) that only means anything once the caller is
		// established as entitled to this config's scope.
		let environments: string[] | null;
		let defaultEnvironmentApplied: boolean | null = null;
		if (parsedEnvironments.supplied) {
			environments = parsedEnvironments.value;
			// An explicit value is never overridden by a default (decision 11's last row), and `null`
			// is deliberately unguarded — see environmentsWithinCallerScope.
			if (environments !== null) {
				const allowed = await environmentsWithinCallerScope(
					user.id,
					{ organizationId, projectId },
					environments,
					// A new config has nothing stored, so `supplied \ existing` is the whole of
					// `supplied` here — decision 48's carve-out only has anything to carve on an update.
					null
				);
				if (!allowed) {
					return json({ error: ENVIRONMENTS_FORBIDDEN_ERROR }, { status: 400 });
				}
			}
		} else {
			const applied = await defaultEnvironmentsForNewConfig(projectId);
			environments = applied.environments;
			defaultEnvironmentApplied = applied.defaultEnvironmentApplied;
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
				environments,
			})
			.returning();

		await publishAlertConfigChanged(newConfig[0].projectId ?? '', newConfig[0].id);

		// decision 33: the notice rides on the response that reports the creation and nowhere
		// afterwards. It is present ONLY when the fallback actually happened, so a client cannot
		// mistake "scoped to the project default" for it — and it is not persisted, by design.
		return json(
			defaultEnvironmentApplied === false
				? { ...toApiShape(newConfig[0]), defaultEnvironmentApplied: false }
				: toApiShape(newConfig[0]),
			{ status: 201 }
		);
	} catch (error) {
		// requireAuth throws SvelteKit's HttpError now, not a bare Error whose message could be
		// string-matched. The 401 body stays byte-identical to what this endpoint has always
		// returned — `{ error: ... }`, not SvelteKit's own `{ message: ... }` shape.
		if (isHttpError(error) && error.status === 401) {
			return json({ error: 'Authentication required' }, { status: 401 });
		}
		log.error('alerts.create_failed', { error });
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

		// PUT never changes which project/org a config belongs to — an update endpoint is not a move
		// endpoint, and silently ignoring a scope-changing field (the prior behavior) returned 200 while
		// changing nothing, which the caller has no way to distinguish from success. Reject explicitly
		// instead. A field is only checked when the caller actually sent it (undefined means "unchanged").
		const requestedProjectId: string | null | undefined =
			body.projectId === undefined ? undefined : body.projectId === '' ? null : body.projectId;
		if (requestedProjectId !== undefined && requestedProjectId !== existingConfig[0].projectId) {
			return json(
				{ error: 'Cannot change an alert config’s project scope via update; delete and recreate instead' },
				{ status: 400 }
			);
		}
		if (
			body.organizationId !== undefined &&
			body.organizationId !== existingConfig[0].organizationId
		) {
			return json(
				{ error: 'Cannot change an alert config’s organization scope via update; delete and recreate instead' },
				{ status: 400 }
			);
		}

		// §4.7 is patch-like on `environments` (decision 32): an ABSENT key leaves the stored value
		// alone, whatever it is, and the scope guard does not run for it. `environmentUpdate` is
		// therefore spread into the .set() below rather than assigned a fallback — `?? existing` would
		// be right for every other field here and wrong for this one, because it cannot tell "leave
		// unchanged" from "set to every environment".
		const parsedEnvironments = parseEnvironments(body);
		if (!parsedEnvironments.ok) {
			return json({ error: parsedEnvironments.error }, { status: 400 });
		}

		const configScope = {
			organizationId: existingConfig[0].organizationId,
			projectId: existingConfig[0].projectId,
		};
		const storedEnvironments = existingConfig[0].environments ?? null;
		let environmentUpdate: { environments?: string[] | null } = {};
		if (parsedEnvironments.supplied) {
			if (parsedEnvironments.value === null) {
				environmentUpdate = { environments: null };
			} else {
				const allowed = await environmentsWithinCallerScope(
					user.id,
					configScope,
					parsedEnvironments.value,
					storedEnvironments
				);
				if (!allowed) {
					return json({ error: ENVIRONMENTS_FORBIDDEN_ERROR }, { status: 400 });
				}
				environmentUpdate = {
					environments: await mergeEnvironments(
						user.id,
						configScope,
						parsedEnvironments.value,
						storedEnvironments
					),
				};
			}
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
				...environmentUpdate,
			})
			.where(eq(alertConfigs.id, body.id))
			.returning();

		await publishAlertConfigChanged(updatedConfig[0].projectId ?? '', updatedConfig[0].id);

		return json(toApiShape(updatedConfig[0]), { status: 200 });
	} catch (error) {
		// requireAuth throws SvelteKit's HttpError now, not a bare Error whose message could be
		// string-matched. The 401 body stays byte-identical to what this endpoint has always
		// returned — `{ error: ... }`, not SvelteKit's own `{ message: ... }` shape.
		if (isHttpError(error) && error.status === 401) {
			return json({ error: 'Authentication required' }, { status: 401 });
		}
		log.error('alerts.update_failed', { error });
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
		// requireAuth throws SvelteKit's HttpError now, not a bare Error whose message could be
		// string-matched. The 401 body stays byte-identical to what this endpoint has always
		// returned — `{ error: ... }`, not SvelteKit's own `{ message: ... }` shape.
		if (isHttpError(error) && error.status === 401) {
			return json({ error: 'Authentication required' }, { status: 401 });
		}
		log.error('alerts.delete_failed', { error });
		return json({ error: 'Internal server error' }, { status: 500 });
	}
};
