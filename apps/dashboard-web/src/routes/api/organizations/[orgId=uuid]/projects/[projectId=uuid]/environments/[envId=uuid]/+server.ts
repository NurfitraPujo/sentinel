import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { and, eq } from 'drizzle-orm';
import { db } from '$lib/server/db';
import { projectEnvironments } from '$lib/db/schema';
import {
	DISPLAY_NAME_MAX_LENGTH,
	clearOtherDefaults,
	getProjectEnvironment,
	toEnvironmentApiShape,
} from '$lib/db/queries/environments';
import { WRITE_FORBIDDEN, requireProjectRoute } from '../../../_shared';

// PATCH /api/organizations/[orgId]/projects/[projectId]/environments/[envId]
// sentinel-project-environments §4.2 — rename the LABEL, hide, set default.
//
// `name` is deliberately absent from the field table and cannot be changed. It is the join key
// against issues.environment and error_occurrences.environment, which this feature does not
// rewrite; under decision 7 the name is part of issue IDENTITY, so a rename would be a
// re-identification of every issue in that environment. Rename the label, never the name.

/** Postgres unique_violation. §4.2's one race, and it must not surface as a `500`. */
const UNIQUE_VIOLATION = '23505';
const ONE_DEFAULT_INDEX = 'idx_project_environments_one_default';

function isOneDefaultCollision(err: unknown): boolean {
	const cause = err as { code?: string; constraint?: string; constraint_name?: string } | null;
	if (!cause || cause.code !== UNIQUE_VIOLATION) return false;
	const constraint = cause.constraint ?? cause.constraint_name ?? '';
	return constraint === ONE_DEFAULT_INDEX;
}

export const PATCH: RequestHandler = async ({ params, request, locals }) => {
	const { projectId } = await requireProjectRoute(locals, params, 'write', WRITE_FORBIDDEN);
	const envId = params.envId!;

	const body = await request.json().catch(() => ({}) as Record<string, unknown>);

	const updates: { displayName?: string; isDefault?: boolean; hidden?: boolean } = {};

	// §3's absent-vs-null rule: an OMITTED key means "leave unchanged". A body with no recognised
	// key at all is a `400` — a PATCH that changes nothing is a caller mistake, not a no-op.
	if (body?.displayName !== undefined) {
		if (typeof body.displayName !== 'string') {
			throw error(400, `displayName must be 1-${DISPLAY_NAME_MAX_LENGTH} characters`);
		}
		const trimmed = body.displayName.trim();
		if (trimmed.length === 0 || trimmed.length > DISPLAY_NAME_MAX_LENGTH) {
			throw error(400, `displayName must be 1-${DISPLAY_NAME_MAX_LENGTH} characters`);
		}
		updates.displayName = trimmed;
	}
	if (typeof body?.isDefault === 'boolean') {
		updates.isDefault = body.isDefault;
	}
	if (typeof body?.hidden === 'boolean') {
		// Hiding removes the row from §4.4/§4.5's selectors. It does NOT stop ingest, delete
		// anything, or exclude the environment from an unfiltered list, and it is NOT access
		// control — §4.10 is.
		updates.hidden = body.hidden;
	}

	if (Object.keys(updates).length === 0) {
		throw error(400, 'No changes supplied');
	}

	const existing = await getProjectEnvironment(projectId, envId);
	if (!existing) {
		throw error(404, 'Environment not found in this project');
	}

	let updated;
	try {
		updated = await db.transaction(async (tx) => {
			// `isDefault: true` clears every other default of this project in the SAME transaction,
			// which is what "in one write" means from the caller's side.
			if (updates.isDefault === true) {
				await clearOtherDefaults(tx, projectId, envId);
			}
			const [row] = await tx
				.update(projectEnvironments)
				.set(updates)
				.where(
					and(eq(projectEnvironments.projectId, projectId), eq(projectEnvironments.id, envId))
				)
				.returning();
			return row;
		});
	} catch (err) {
		// Two callers setting `isDefault: true` on two rows of the same project at once: the partial
		// unique index rejects the loser. Unhandled this is a `500` that reads as a defect in the
		// feature rather than as the retryable collision it is. It is the only place in this feature
		// where an ordinary double-click can hit a unique index.
		if (isOneDefaultCollision(err) || isOneDefaultCollision((err as { cause?: unknown })?.cause)) {
			throw error(409, 'Another environment was made the default; reload and try again');
		}
		throw err;
	}

	return json(toEnvironmentApiShape(updated));
};
