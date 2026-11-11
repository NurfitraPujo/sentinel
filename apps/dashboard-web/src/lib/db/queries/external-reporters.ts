import { and, eq, sql } from 'drizzle-orm';
import { db } from '$lib/server/db';
import { externalReporters } from '$lib/db/schema';

/**
 * sentinel-user-report-issue contract §4.6 (OQ-6): the `external_reporters` identity table.
 *
 * A separate table with its own FK, never a nullable `reporter_id` on `manual_issue_reports` and
 * never a synthesised `"user"` row -- the org-member report path is not touched by this feature.
 * `UNIQUE (project_id, external_id)` is the identity rule, which is exactly what makes intake an
 * UPSERT rather than an insert: the same `u_8f3c41` from two different instrumented products is
 * two different people, and the same one reporting twice is one row seen twice.
 *
 * `organization_id` is denormalised NOT NULL for the reason `attachments.org_id` gives: a tenancy
 * check must never depend on walking a join.
 */

export interface UpsertExternalReporterInput {
	/** From the credential, never the request body (B7). */
	organizationId: string;
	/** From the credential, or from `X-Project-Key` resolved within the credential's org (§2). */
	projectId: string;
	/** The service's own opaque user id. Attacker-controlled DATA, never identity (§2). */
	externalId: string;
	displayName?: string | null;
	email?: string | null;
}

export interface ExternalReporterRow {
	id: string;
	organizationId: string;
	projectId: string;
	externalId: string;
	displayName: string | null;
	email: string | null;
	firstSeenAt: Date;
	lastSeenAt: Date;
}

/**
 * Creates the reporter row for `(project_id, external_id)`, or refreshes the existing one.
 *
 * On conflict: `last_seen_at` always advances (§4.6 -- "makes a flooding reporter visible, and
 * gives retention something to sweep on"), and `display_name`/`email` are updated only when this
 * call actually supplies one. A caller that omits them must not blank out what an earlier report
 * knew; `COALESCE(excluded.…, external_reporters.…)` is what keeps an omission from being read as
 * a deletion, matching §3's "an omitted key and an explicit null are the same thing: absent".
 *
 * `first_seen_at` is never touched on conflict -- it is the whole point of the column.
 */
export async function upsertExternalReporter(
	input: UpsertExternalReporterInput
): Promise<ExternalReporterRow> {
	const displayName = normalizeOptional(input.displayName);
	const email = normalizeOptional(input.email);

	const [row] = await db
		.insert(externalReporters)
		.values({
			organizationId: input.organizationId,
			projectId: input.projectId,
			externalId: input.externalId,
			displayName,
			email,
		})
		.onConflictDoUpdate({
			target: [externalReporters.projectId, externalReporters.externalId],
			set: {
				lastSeenAt: sql`now()`,
				displayName: sql`coalesce(excluded.display_name, ${externalReporters.displayName})`,
				email: sql`coalesce(excluded.email, ${externalReporters.email})`,
			},
		})
		.returning();

	if (!row) {
		throw new Error('Failed to upsert external reporter');
	}

	return row as ExternalReporterRow;
}

/** Reads one reporter by its identity pair. Used by tests and by §4.2's read-back. */
export async function findExternalReporter(
	projectId: string,
	externalId: string
): Promise<ExternalReporterRow | null> {
	const [row] = await db
		.select()
		.from(externalReporters)
		.where(and(eq(externalReporters.projectId, projectId), eq(externalReporters.externalId, externalId)));
	return (row as ExternalReporterRow | undefined) ?? null;
}

function normalizeOptional(value: string | null | undefined): string | null {
	if (typeof value !== 'string') return null;
	const trimmed = value.trim();
	return trimmed.length === 0 ? null : trimmed;
}
