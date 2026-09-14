import { and, desc, eq, inArray, isNotNull, sql } from 'drizzle-orm';
import { db } from '$lib/server/db';
import { issues, releases } from '$lib/db/schema';

/**
 * sentinel-releases-snooze §4.1 and §4.4 — the two readers of the `releases` table, and the ONLY
 * two: the dashboard never writes it (the processor's `StoreEvent` upsert and the `1725000000`
 * backfill do, §4.3 / §4.5).
 *
 * Both order by `first_seen DESC`, never by parsing the version (decision 1) — that is the order
 * `idx_releases_project_first_seen (project_id, first_seen DESC)` serves, and it is why `v1.10.0`
 * can sit below `v1.9.0` when it was seen first. Neither takes an environment scope: a release is a
 * project-level fact (§3, `releases` has no environment column), and the callers decide who may see
 * the list at all — the detail loader hands a scoped caller `[]` (decision 26) and the releases
 * loader `404`s them (decision 20) — so a scope clause here would be untested code (decision 36).
 */

type Executor = Pick<typeof db, 'select'>;

/** §4.1: the resolve dialog's picker is the newest 50. */
export const DETAIL_PICKER_RELEASE_LIMIT = 50;

/** §4.4, decision 24: the page shows the 200 most recent and says so; no paging. */
export const RELEASES_PAGE_LIMIT = 200;

/** §4.1's `releases[]` — the picker needs the name and when it was first seen, nothing else. */
export interface PickerRelease {
	version: string;
	firstSeen: Date;
}

/** §4.4's `releases[]`, exactly. `camelCase`, per §3. */
export interface ReleasePageRow {
	version: string;
	firstSeen: Date;
	lastSeen: Date;
	occurrenceCount: number;
	issuesIntroduced: number;
}

/**
 * §4.1: the project's newest {@link DETAIL_PICKER_RELEASE_LIMIT} releases, `first_seen DESC`, for
 * the resolve dialog's picker (flow D5). The caller is responsible for NOT calling this for an
 * environment-scoped member — decision 26 is the loader's clause, not this function's.
 */
export async function listPickerReleases(
	projectId: string,
	executor: Executor = db
): Promise<PickerRelease[]> {
	return await executor
		.select({ version: releases.version, firstSeen: releases.firstSeen })
		.from(releases)
		.where(eq(releases.projectId, projectId))
		.orderBy(desc(releases.firstSeen))
		.limit(DETAIL_PICKER_RELEASE_LIMIT);
}

/**
 * §4.4's page data: the {@link RELEASES_PAGE_LIMIT} most recent releases with `issuesIntroduced`
 * on each, and `truncated` — `true` iff the project has more than the limit, from a `COUNT(*)`
 * beside the list.
 *
 * `issuesIntroduced` is ONE grouped `COUNT(*)` over `issues` for the whole page, never one query
 * per row: `WHERE project_id = $1 AND first_release_version IN (<the page's versions>) GROUP BY
 * first_release_version`, which is the partial index `idx_issues_project_first_release
 * (project_id, first_release_version) WHERE first_release_version IS NOT NULL` — the `IS NOT NULL`
 * is carried explicitly so the predicate matches the index's by inspection. Counted **across every
 * environment** with no scope clause (decision 36): no caller with a non-null scope reaches this
 * function (§2, decision 20). An issue moved into this project (`moveIssueToProject`) keeps its
 * `first_release_version` and counts here only if this project has a release of that name — the
 * `IN` list is the page's releases, so a version this project never saw is never looked up. A
 * release with no matching issue reads `0`, not an absent key.
 *
 * `occurrenceCount` is lifetime and `issuesIntroduced` decays with retention (§4.4's note; the
 * release note in RUNBOOK.md states the consequence).
 */
export async function listProjectReleases(
	projectId: string,
	executor: Executor = db
): Promise<{ releases: ReleasePageRow[]; truncated: boolean }> {
	const rows = await executor
		.select({
			version: releases.version,
			firstSeen: releases.firstSeen,
			lastSeen: releases.lastSeen,
			occurrenceCount: releases.occurrenceCount,
		})
		.from(releases)
		.where(eq(releases.projectId, projectId))
		.orderBy(desc(releases.firstSeen))
		.limit(RELEASES_PAGE_LIMIT);

	const [{ total }] = await executor
		.select({ total: sql<number>`count(*)::int` })
		.from(releases)
		.where(eq(releases.projectId, projectId));

	const introduced = new Map<string, number>();
	if (rows.length > 0) {
		const counts = await executor
			.select({
				version: issues.firstReleaseVersion,
				issuesIntroduced: sql<number>`count(*)::int`,
			})
			.from(issues)
			.where(
				and(
					eq(issues.projectId, projectId),
					isNotNull(issues.firstReleaseVersion),
					inArray(
						issues.firstReleaseVersion,
						rows.map((row) => row.version)
					)
				)
			)
			.groupBy(issues.firstReleaseVersion);
		for (const count of counts) {
			if (count.version !== null) introduced.set(count.version, count.issuesIntroduced);
		}
	}

	return {
		releases: rows.map((row) => ({
			version: row.version,
			firstSeen: row.firstSeen,
			lastSeen: row.lastSeen,
			occurrenceCount: row.occurrenceCount,
			issuesIntroduced: introduced.get(row.version) ?? 0,
		})),
		truncated: total > RELEASES_PAGE_LIMIT,
	};
}
