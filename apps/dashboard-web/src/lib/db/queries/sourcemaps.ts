import { and, asc, eq, inArray, sql } from 'drizzle-orm';
import { db, type Tx } from '$lib/server/db';
import { sourcemaps } from '$lib/db/schema';

/**
 * sentinel-sourcemaps contract §4.1 (decision 26) -- the `sourcemaps` table's write path, the
 * two reads `POST /api/sourcemaps` makes before it writes anything, and the dashboard's two reads
 * of it: §4.2's per-release list (`listReleaseSourcemaps`) and §4.6's grouped count for the
 * releases page (`countSourcemapsByRelease`). The processor reads this table by `(project_id,
 * release_version, minified_path)` (§4.4, Go) and is not a caller of anything here.
 *
 * Three phases, no transaction open while bytes move (decision 26): the route validates and calls
 * `readReleaseState` (phase 1, plain reads, the per-release pre-check answered from them), puts
 * every object with NO transaction open (phase 2), then calls `upsertReleaseSourcemaps` -- ONE
 * short transaction for the upserts, with the count re-read under a lock so a concurrent upload
 * that raced past the pre-check is refused here (phase 3). Object first, row second: no row ever
 * points at nothing, because a row that did would make §4.4 fetch and fail on every event.
 */

/** What phase 1 reads for one request: the release's row count, and its rows for these paths. */
export interface ReleaseSourcemapState {
	/** The release's `sourcemaps` rows BEFORE this request -- §4.1's `details.current`. */
	current: number;
	/** The existing row per requested (normalised) path; absent = no row yet for that path. */
	existing: Map<string, ExistingSourcemapRow>;
}

export interface ExistingSourcemapRow {
	sha256: string;
	storageKey: string;
}

/** One `files[]` part that phase 2 wrote and phase 3 records. */
export interface SourcemapUpsertInput {
	/** The NORMALISED path (decision 22) -- the stored string and §4.4's lookup key. */
	minifiedPath: string;
	storageKey: string;
	sizeBytes: number;
	sha256: string;
}

/**
 * §4.1 `422 release_full` from INSIDE the transaction (decision 26): a concurrent upload raced past
 * the route's pre-check, and the count re-read under the lock says the release would exceed the
 * cap. Thrown, never returned: D18 -- returning early from inside a `db.transaction` callback
 * COMMITS whatever the callback already did. `current` is the count the transaction read, which
 * is what the caller reports; the objects it already put are orphans, and the route counts them.
 */
export class ReleaseFullError extends Error {
	constructor(
		public readonly max: number,
		public readonly current: number
	) {
		super(`Release already holds ${current} of at most ${max} source maps`);
		this.name = 'ReleaseFullError';
	}
}

/**
 * Phase 1: the release's row count and its existing rows for the request's paths, read without a
 * transaction. Feeds the per-release pre-check (`current + new paths > max` → `422 release_full`
 * before the first `putObject`) and the `unchanged` decision (same row, same `sha256` → skipped).
 *
 * `paths` are the request's NORMALISED paths, already de-duplicated by the route (decision 23).
 */
export async function readReleaseState(
	projectId: string,
	releaseVersion: string,
	paths: readonly string[]
): Promise<ReleaseSourcemapState> {
	const current = await countReleaseRows(db, projectId, releaseVersion);

	const existing = new Map<string, ExistingSourcemapRow>();
	if (paths.length > 0) {
		const rows = await db
			.select({
				minifiedPath: sourcemaps.minifiedPath,
				sha256: sourcemaps.sha256,
				storageKey: sourcemaps.storageKey,
			})
			.from(sourcemaps)
			.where(
				and(
					eq(sourcemaps.projectId, projectId),
					eq(sourcemaps.releaseVersion, releaseVersion),
					inArray(sourcemaps.minifiedPath, [...paths])
				)
			);
		for (const row of rows) {
			existing.set(row.minifiedPath, { sha256: row.sha256, storageKey: row.storageKey });
		}
	}

	return { current, existing };
}

/**
 * Phase 3: ONE short transaction for the n upserts on `(project_id, release_version,
 * minified_path)` -- §4.5's unique key, §4.1's upsert target -- after every object is already in
 * storage.
 *
 * The per-release count is re-read here under a transaction-scoped advisory lock on
 * `(project_id, release_version)`. Re-reading alone would not make the contract's sentence true
 * ("a concurrent upload that races past the pre-check is refused there"): two READ COMMITTED
 * transactions can both count 499 and both insert. The lock serialises the count-then-insert per
 * release, and only per release -- uploads for different releases or projects do not queue
 * behind each other. It is held for the length of the upserts, which is milliseconds; the bytes
 * moved before this function was called.
 *
 * `hashtextextended` over the two-part key with a fixed seed, so the lock id is a function of the
 * release and nothing else; the `sourcemaps:` prefix keeps it out of the retention family's
 * `hashtext(...)` space.
 *
 * Returns the release's row count AFTER the upserts -- §4.1's `release_file_count`. Throws
 * `ReleaseFullError` (rolls back, nothing written) when the re-read count says otherwise.
 */
export async function upsertReleaseSourcemaps(input: {
	projectId: string;
	releaseVersion: string;
	files: readonly SourcemapUpsertInput[];
	maxPerRelease: number;
}): Promise<{ releaseFileCount: number }> {
	const { projectId, releaseVersion, files, maxPerRelease } = input;

	return await db.transaction(async (tx) => {
		await tx.execute(
			sql`SELECT pg_advisory_xact_lock(hashtextextended(${`sourcemaps:${projectId}:${releaseVersion}`}, 0))`
		);

		const current = await countReleaseRows(tx, projectId, releaseVersion);

		// Which of this request's paths are new is decided here, not from the route's phase-1 read:
		// a concurrent request may have created some of them since, and each of those is now a
		// replace, not an add.
		const alreadyPresent = new Set<string>();
		if (files.length > 0) {
			const rows = await tx
				.select({ minifiedPath: sourcemaps.minifiedPath })
				.from(sourcemaps)
				.where(
					and(
						eq(sourcemaps.projectId, projectId),
						eq(sourcemaps.releaseVersion, releaseVersion),
						inArray(
							sourcemaps.minifiedPath,
							files.map((f) => f.minifiedPath)
						)
					)
				);
			for (const row of rows) alreadyPresent.add(row.minifiedPath);
		}
		const added = files.filter((f) => !alreadyPresent.has(f.minifiedPath)).length;
		if (current + added > maxPerRelease) {
			throw new ReleaseFullError(maxPerRelease, current);
		}

		for (const file of files) {
			await tx
				.insert(sourcemaps)
				.values({
					projectId,
					releaseVersion,
					minifiedPath: file.minifiedPath,
					storageKey: file.storageKey,
					sizeBytes: file.sizeBytes,
					sha256: file.sha256,
				})
				.onConflictDoUpdate({
					target: [sourcemaps.projectId, sourcemaps.releaseVersion, sourcemaps.minifiedPath],
					set: {
						storageKey: file.storageKey,
						sizeBytes: file.sizeBytes,
						sha256: file.sha256,
						uploadedAt: sql`now()`,
					},
				});
		}

		return { releaseFileCount: current + added };
	});
}

type Executor = Pick<typeof db, 'select'> | Pick<Tx, 'select'>;

async function countReleaseRows(executor: Executor, projectId: string, releaseVersion: string): Promise<number> {
	const [row] = await (executor as Pick<typeof db, 'select'>)
		.select({ count: sql<number>`count(*)::int` })
		.from(sourcemaps)
		.where(and(eq(sourcemaps.projectId, projectId), eq(sourcemaps.releaseVersion, releaseVersion)));
	return Number(row?.count ?? 0);
}

/** §4.2's `files[]` row, exactly -- `camelCase` (§3) and NO `storageKey`, see {@link listReleaseSourcemaps}. */
export interface ReleaseSourcemapRow {
	id: string;
	minifiedPath: string;
	sizeBytes: number;
	sha256: string;
	uploadedAt: Date;
}

/**
 * §4.2: the maps one release has, `minified_path ASC`, returned whole -- a release's list is
 * bounded by §4.1's per-release cap (OQ-6), so there is no paging (§3). `[]` when the release has
 * none, which is also the answer for a version this project has never seen: the route echoes the
 * version and shows an empty list either way.
 *
 * `storage_key` is deliberately not selected. §1's *NOT building* says nothing serves map bytes to
 * a browser, and the only way a client learns where the bytes live is a column that reaches a
 * response; the columns below are the whole of §4.2's row, so the key cannot leak by accident
 * (`sourcemaps.test.ts` pins the selected fields).
 *
 * `(project_id, release_version)` is `idx_sourcemaps_project_release`'s prefix, and the caller has
 * already decided who may see the list at all -- the route `404`s an environment-scoped member
 * (§2.2), the same way the releases loader does, so there is no scope clause here (decision 36's
 * reasoning, unchanged).
 */
export async function listReleaseSourcemaps(
	projectId: string,
	releaseVersion: string
): Promise<ReleaseSourcemapRow[]> {
	return await db
		.select({
			id: sourcemaps.id,
			minifiedPath: sourcemaps.minifiedPath,
			sizeBytes: sourcemaps.sizeBytes,
			sha256: sourcemaps.sha256,
			uploadedAt: sourcemaps.uploadedAt,
		})
		.from(sourcemaps)
		.where(and(eq(sourcemaps.projectId, projectId), eq(sourcemaps.releaseVersion, releaseVersion)))
		.orderBy(asc(sourcemaps.minifiedPath));
}

/**
 * §4.6's `sourcemapCount`: ONE grouped `COUNT(*)` over `sourcemaps` for the whole releases page,
 * never one query per row -- `WHERE project_id = $1 AND release_version IN (<the page's versions>)
 * GROUP BY release_version`, the shape `listProjectReleases` documents for `issuesIntroduced`
 * (releases.ts), on `idx_sourcemaps_project_release (project_id, release_version)`. The `IN` list is
 * the page's own versions (≤ `RELEASES_PAGE_LIMIT`), so a version that has maps but no `releases`
 * row is never looked up -- decision 16: map-only versions are not shown, and the loader is what
 * keeps them out, not this query.
 *
 * Returns a map keyed by version. A version with no rows is ABSENT here; the loader reads `?? 0`,
 * because §4.6 says `0`, never absent, on the page. An empty `versions` list is answered without a
 * statement -- there is nothing to count against, and `IN ()` is not SQL.
 */
export async function countSourcemapsByRelease(
	projectId: string,
	versions: readonly string[]
): Promise<Map<string, number>> {
	const counts = new Map<string, number>();
	if (versions.length === 0) return counts;

	const rows = await db
		.select({
			version: sourcemaps.releaseVersion,
			sourcemapCount: sql<number>`count(*)::int`,
		})
		.from(sourcemaps)
		.where(and(eq(sourcemaps.projectId, projectId), inArray(sourcemaps.releaseVersion, [...versions])))
		.groupBy(sourcemaps.releaseVersion);
	for (const row of rows) counts.set(row.version, Number(row.sourcemapCount));
	return counts;
}
