import type { RequestHandler } from './$types';
import { env } from '$env/dynamic/private';
import {
	authenticateSourcemapRequest,
	reportErrorResponse,
	toReportError,
	type ReportAuthContext,
} from '$lib/server/report-auth';
import { headObject, isStorageConfigured, putObject } from '$lib/server/storage';
import { bestEffortDeleteObjects } from '$lib/server/retention';
import { normalizeMinifiedPath, sha256Hex, sourcemapStorageKey } from '$lib/server/sourcemap-path';
import {
	findReferencedStorageKeys,
	readReleaseState,
	upsertReleaseSourcemaps,
	ReleaseFullError,
	type SourcemapUpsertInput,
} from '$lib/db/queries/sourcemaps';
import { recordSourcemapUpload } from '$lib/server/metrics';
import { log } from '$lib/server/observability/log';

/**
 * sentinel-sourcemaps contract §4.1 -- `POST /api/sourcemaps`: one or more source maps for one
 * release, uploaded by CI with the project's ingest key (§2.1, decision D1 hosts it here because
 * this app owns the only S3 client, the only `X-API-Key` authenticator with §7's envelope, and
 * the precedent `sentinel-user-report-issue` set).
 *
 * The shape is `report/attachments/+server.ts`'s: authenticate first (401 → 403 scope → 400/403
 * project → 429, all inside `authenticateSourcemapRequest`), then `request.formData()` with the
 * adapter's 413 preserved, then the route's own validation, every non-2xx in §7's envelope and
 * never SvelteKit's `error()`. `csrfHandle` lets the CI curl's Origin-less multipart POST reach
 * this route only because `/api/sourcemaps` is in `csrf.ts: CSRF_EXEMPT_PATHS` (§2.1).
 *
 * What is written -- three phases, no transaction open while bytes move (decision 26):
 *
 *  1. validate EVERYTHING (all-or-nothing: a CI run never lands half a release), read the
 *     release's existing rows for the request's paths, and answer the per-release pre-check
 *     (`422 release_full`, nothing written) from that read;
 *  2. `putObject` every part that is not `unchanged`, one after another, under a key derived
 *     from hashes and never from user strings (`sourcemap-path.ts: sourcemapStorageKey`, D5,
 *     decision 17). A failure here is `503 storage_unavailable` with NO row written; the objects
 *     already put stay as orphans -- permanent, counted, accepted (§4.1 *Orphan objects*);
 *  3. ONE short transaction for the upserts (`queries/sourcemaps.ts`), the per-release count
 *     re-read under a lock; object first, row second, so no row ever points at nothing. Under
 *     that lock and before the rows are written, every key this request is about to reference is
 *     `headObject`ed and re-put from the part's bytes if it is gone (decision 43, BUG-07): a
 *     concurrent request can put the SAME content-addressed key and a third one's cleanup can
 *     delete it in the seconds between this request's put and its commit; the check is the one
 *     serialised point at which "the object exists" and "the row references it" can be made true
 *     together. After commit, a `replaced` outcome deletes the previous object best-effort --
 *     but only the key the transaction itself displaced (read under the lock, never phase 1's
 *     snapshot), never a key this request put, and only if no `sourcemaps` row references it at
 *     that moment (BUG-05: keys are content-addressed, so a concurrent re-upload of older bytes
 *     recreates a key phase 1 saw, and deleting from the snapshot would leave that row pointing
 *     at nothing).
 *
 * The multipart pairing is POSITIONAL: the i-th `file` is the i-th `minified_path`
 * (`FormData.getAll` preserves part order). The route never guesses a path from the map's own
 * `file` key -- that key is a basename and collides across directories (D4).
 */

/**
 * Decision 10's defaults; the chart and compose set the same three names (§12). The per-release
 * default is 2000, not decision 10's 500: one Next.js (Turbopack) release emits ~794 server maps
 * plus ~447 client chunk maps, ~1240 in all (sentinel-sdk-js decision 26, 2026-09-16).
 */
const DEFAULT_MAX_BYTES = 50 * 1024 * 1024;
const DEFAULT_MAX_FILES_PER_REQUEST = 50;
const DEFAULT_MAX_FILES_PER_RELEASE = 2000;

/** `error_occurrences.release_version VARCHAR(100)` and the proto's CEL bound -- bytes, not chars. */
const MAX_RELEASE_VERSION_BYTES = 100;
/** `validation.MaxFilePathLength`, the ingestor's own bound on a frame's `file`; `VARCHAR(512)`. */
const MAX_MINIFIED_PATH_CHARS = 512;

type FileOutcome = 'created' | 'replaced' | 'unchanged';

interface ValidatedPart {
	index: number;
	minifiedPath: string;
	bytes: Uint8Array;
	sha256: string;
}

/**
 * Thrown from inside the transaction by `ensureObjectsPresent` when a key this request is about
 * to commit a row to could neither be confirmed nor re-put: the transaction rolls back (D18) and
 * the route answers `503 storage_unavailable`, the same as a phase-2 put failure -- nothing is
 * written, the objects already put stay as counted orphans.
 */
class ObjectRepairError extends Error {
	constructor(
		public readonly storageKey: string,
		public readonly cause: unknown
	) {
		super(`Could not confirm object ${storageKey} before committing its row`);
		this.name = 'ObjectRepairError';
	}
}

/** The S3 SDK's "no such object" for HeadObject: `NotFound` (v3), `NoSuchKey`, or a bare 404. */
function isObjectMissing(err: unknown): boolean {
	const e = err as { name?: unknown; $metadata?: { httpStatusCode?: unknown } } | null;
	return e?.name === 'NotFound' || e?.name === 'NoSuchKey' || e?.$metadata?.httpStatusCode === 404;
}

export const POST: RequestHandler = async ({ request }) => {
	let ctx: ReportAuthContext;
	try {
		ctx = await authenticateSourcemapRequest(request);
	} catch (err) {
		recordSourcemapUpload('rejected');
		return toReportError(err);
	}

	// A failure here is usually "not multipart", but adapter-node also rejects a body over
	// BODY_SIZE_LIMIT with a 413 at this exact point (§4.1 "the adapter's 413 -- the route wraps it
	// into §7's envelope"). A thrown status is preserved; only a shapeless failure becomes 400.
	let formData: FormData;
	try {
		formData = await request.formData();
	} catch (err) {
		recordSourcemapUpload('rejected');
		const status = (err as { status?: unknown })?.status;
		if (typeof status === 'number' && status !== 400) {
			return toReportError(err);
		}
		return reportErrorResponse(400, 'invalid_request', 'Expected multipart/form-data.', { field: 'body' });
	}

	const maxBytes = resolvePositiveInt(env.SOURCEMAP_MAX_BYTES, DEFAULT_MAX_BYTES);
	const maxFilesPerRequest = resolvePositiveInt(env.SOURCEMAP_MAX_FILES_PER_REQUEST, DEFAULT_MAX_FILES_PER_REQUEST);
	const maxFilesPerRelease = resolvePositiveInt(env.SOURCEMAP_MAX_FILES_PER_RELEASE, DEFAULT_MAX_FILES_PER_RELEASE);

	// ---- Phase 1a: the request's shape, in the contract's order. Nothing is read from storage or
	// the database until every part has passed.
	const rejected = (status: number, code: Parameters<typeof reportErrorResponse>[1], message: string, details?: Record<string, unknown>) => {
		recordSourcemapUpload('rejected');
		return reportErrorResponse(status, code, message, details);
	};

	// release_version: stored EXACTLY as sent (decision 25) -- no trim, no `v` stripping, no case
	// folding -- because it is the join key with the string the SDK sends on events. A value that
	// would change under trim (the shell `$(…)` trailing newline) is refused, not silently fixed.
	const releaseVersionRaw = formData.get('release_version');
	if (typeof releaseVersionRaw !== 'string' || releaseVersionRaw.length === 0) {
		return rejected(400, 'invalid_request', 'release_version is required (1-100 bytes).', { field: 'release_version' });
	}
	if (releaseVersionRaw !== releaseVersionRaw.trim()) {
		return rejected(400, 'invalid_request', 'release_version must not begin or end with whitespace.', {
			field: 'release_version',
			reason: 'whitespace',
		});
	}
	if (Buffer.byteLength(releaseVersionRaw, 'utf8') > MAX_RELEASE_VERSION_BYTES) {
		return rejected(400, 'invalid_request', 'release_version must be at most 100 bytes.', { field: 'release_version' });
	}
	const releaseVersion = releaseVersionRaw;

	const pathParts = formData.getAll('minified_path');
	const fileParts = formData.getAll('file');
	if (pathParts.length === 0 || pathParts.some((p) => typeof p !== 'string')) {
		return rejected(400, 'invalid_request', 'At least one minified_path text part is required.', { field: 'minified_path' });
	}
	if (fileParts.some((f) => !(f instanceof File))) {
		return rejected(400, 'invalid_request', 'Every file part must be a file.', { field: 'file' });
	}
	if (pathParts.length !== fileParts.length) {
		return rejected(400, 'invalid_request', 'minified_path and file parts must be paired one-to-one, in order.', {
			minified_path_count: pathParts.length,
			file_count: fileParts.length,
		});
	}
	if (fileParts.length > maxFilesPerRequest) {
		return rejected(400, 'too_many_files', `At most ${maxFilesPerRequest} files per request.`, {
			max: maxFilesPerRequest,
			received: fileParts.length,
		});
	}

	// Paths: normalised by §4.4's rule (decision 22), bounded after normalisation, and unique
	// after normalisation -- two parts that normalise to one path are refused whole (decision 23),
	// never last-wins and never a Postgres "cannot affect row a second time" 500.
	const normalisedPaths: string[] = [];
	const seen = new Map<string, number>();
	for (let i = 0; i < pathParts.length; i++) {
		const normalised = normalizeMinifiedPath(pathParts[i] as string);
		if (normalised.length === 0 || normalised.length > MAX_MINIFIED_PATH_CHARS) {
			return rejected(400, 'invalid_request', `minified_path ${i} must be 1-512 characters after normalisation.`, {
				field: 'minified_path',
				index: i,
			});
		}
		const first = seen.get(normalised);
		if (first !== undefined) {
			return rejected(400, 'invalid_request', `minified_path parts ${first} and ${i} normalise to the same path.`, {
				field: 'minified_path',
				duplicate: normalised,
				indexes: [first, i],
			});
		}
		seen.set(normalised, i);
		normalisedPaths.push(normalised);
	}

	// ---- Phase 1b: per-file validation, each file in order: size, then the v3 structural check
	// (decision 19 -- structural, never a full decode; a map that passes and still breaks the Go
	// decoder is the processor's `map_error`), then sha256 over the raw bytes.
	const parts: ValidatedPart[] = [];
	for (let i = 0; i < fileParts.length; i++) {
		const file = fileParts[i] as File;
		if (file.size > maxBytes) {
			return rejected(413, 'payload_too_large', `File ${i} exceeds the ${maxBytes} byte cap.`, {
				index: i,
				max_bytes: maxBytes,
			});
		}
		const bytes = new Uint8Array(await file.arrayBuffer());
		const reason = sourcemapV3Rejection(bytes);
		if (reason) {
			return rejected(422, 'invalid_sourcemap', `Part ${i} is not a version-3 sourcemap (${reason}).`, {
				index: i,
				reason,
			});
		}
		parts.push({ index: i, minifiedPath: normalisedPaths[i], bytes, sha256: sha256Hex(bytes) });
	}

	// Storage must exist before anything is read for a write that could not happen anyway. After
	// the 4xx table so a CI script with a broken invocation hears about the script, not the
	// operator's bucket.
	if (!isStorageConfigured()) {
		recordSourcemapUpload('storage_error');
		return reportErrorResponse(503, 'storage_unavailable', 'Object storage is not configured.');
	}

	// ---- Phase 1c: the release's existing rows for these paths, and the pre-check (decision 26):
	// `would_be` = rows before the request + the request's paths with no row yet; a replace does
	// not add. Answered BEFORE the first putObject, having written nothing.
	let state;
	try {
		state = await readReleaseState(ctx.projectId, releaseVersion, normalisedPaths);
	} catch (err) {
		// Not counted: §12's set has no `error` value, nothing was written and no orphan exists,
		// and `rejected` means "the caller's request was refused", which this is not.
		log.error('sourcemap_uploads.read_failed', { projectId: ctx.projectId, error: err });
		return reportErrorResponse(500, 'internal', 'Failed to read the release.');
	}
	const newPathCount = parts.filter((p) => !state.existing.has(p.minifiedPath)).length;
	if (state.current + newPathCount > maxFilesPerRelease) {
		return rejected(422, 'release_full', `Release already holds ${state.current} of at most ${maxFilesPerRelease} source maps.`, {
			max: maxFilesPerRelease,
			current: state.current,
		});
	}

	// ---- Phase 2: every object that is not `unchanged`, one after another, NO transaction open.
	const outcomes: FileOutcome[] = new Array(parts.length);
	const toUpsert: SourcemapUpsertInput[] = [];
	const bytesByKey = new Map<string, Uint8Array>();
	let objectsPut = 0;
	for (const part of parts) {
		const existing = state.existing.get(part.minifiedPath);
		if (existing && existing.sha256 === part.sha256) {
			outcomes[part.index] = 'unchanged';
			continue;
		}
		const storageKey = sourcemapStorageKey(ctx.projectId, releaseVersion, part.minifiedPath, part.sha256);
		try {
			await putObject(storageKey, part.bytes, 'application/json');
		} catch (err) {
			// §4.1: 503, no row written; the `objectsPut` objects already in the bucket are orphans.
			log.error('sourcemap_uploads.put_failed', {
				projectId: ctx.projectId,
				index: part.index,
				orphanedObjects: objectsPut,
				error: err,
			});
			recordSourcemapUpload('storage_error');
			return reportErrorResponse(503, 'storage_unavailable', 'Object storage is unavailable; retry later.');
		}
		objectsPut++;
		bytesByKey.set(storageKey, part.bytes);
		outcomes[part.index] = existing ? 'replaced' : 'created';
		toUpsert.push({
			minifiedPath: part.minifiedPath,
			storageKey,
			sizeBytes: part.bytes.byteLength,
			sha256: part.sha256,
		});
	}

	// ---- Phase 3: one short transaction. `unchanged` parts have nothing to write.
	//
	// `ensureObjectsPresent` runs INSIDE it, under the release lock, before the rows are written
	// (decision 43): each key is HEADed and, only if the object is gone, put again from the bytes
	// this request already holds -- the one case in which bytes move with a transaction open,
	// bounded to the rare repair and sanctioned by the decision. Any other storage answer aborts
	// the transaction: a row must never be committed to a key that could not be proven to exist.
	let objectsRepaired = 0;
	const ensureObjectsPresent = async (storageKeys: readonly string[]): Promise<void> => {
		for (const storageKey of storageKeys) {
			try {
				await headObject(storageKey);
				continue;
			} catch (err) {
				if (!isObjectMissing(err)) throw new ObjectRepairError(storageKey, err);
			}
			const bytes = bytesByKey.get(storageKey);
			if (bytes === undefined) throw new ObjectRepairError(storageKey, new Error('no bytes held for this key'));
			try {
				await putObject(storageKey, bytes, 'application/json');
			} catch (err) {
				throw new ObjectRepairError(storageKey, err);
			}
			objectsRepaired++;
			log.warn('sourcemap_uploads.object_repaired', { projectId: ctx.projectId, releaseVersion, storageKey });
		}
	};

	let releaseFileCount = state.current;
	let displacedKeys: string[] = [];
	if (toUpsert.length > 0) {
		try {
			({ releaseFileCount, displacedKeys } = await upsertReleaseSourcemaps({
				projectId: ctx.projectId,
				releaseVersion,
				files: toUpsert,
				maxPerRelease: maxFilesPerRelease,
				ensureObjectsPresent,
			}));
		} catch (err) {
			if (err instanceof ObjectRepairError) {
				log.error('sourcemap_uploads.repair_failed', {
					projectId: ctx.projectId,
					storageKey: err.storageKey,
					orphanedObjects: objectsPut,
					error: err.cause,
				});
				recordSourcemapUpload('storage_error');
				return reportErrorResponse(503, 'storage_unavailable', 'Object storage is unavailable; retry later.');
			}
			if (err instanceof ReleaseFullError) {
				// A concurrent upload raced past the pre-check; refused inside the transaction after
				// this request's objects were put -- tolerated exactly as a putObject failure is,
				// and the orphans counted the same way.
				log.warn('sourcemap_uploads.release_full_in_transaction', {
					projectId: ctx.projectId,
					orphanedObjects: objectsPut,
					current: err.current,
				});
				recordSourcemapUpload('storage_error');
				return reportErrorResponse(422, 'release_full', err.message, { max: err.max, current: err.current });
			}
			log.error('sourcemap_uploads.upsert_failed', {
				projectId: ctx.projectId,
				orphanedObjects: objectsPut,
				error: err,
			});
			recordSourcemapUpload('storage_error');
			return reportErrorResponse(500, 'internal', 'Failed to record the source maps.');
		}
	}

	// After commit: the objects this transaction displaced, best-effort (D3), and only those no
	// row references any more -- see the header. A key this request put is never a candidate
	// (it cannot be, the key folds the path and sha in, but the rule is stated rather than
	// relied on). A failed delete, or a reference check that could not be made, leaves the
	// object as one more permanent orphan, logged and counted; an orphan is tolerated, a row
	// pointing at nothing is not.
	const putKeys = new Set(toUpsert.map((f) => f.storageKey));
	const candidates = displacedKeys.filter((k) => !putKeys.has(k));
	if (candidates.length > 0) {
		let referenced: Set<string>;
		try {
			referenced = await findReferencedStorageKeys(candidates);
		} catch (err) {
			log.error('sourcemap_uploads.reference_check_failed', {
				projectId: ctx.projectId,
				orphanedObjects: candidates.length,
				error: err,
			});
			referenced = new Set(candidates);
			for (let i = 0; i < candidates.length; i++) recordSourcemapUpload('storage_error');
		}
		const doomed = candidates.filter((k) => !referenced.has(k));
		if (doomed.length > 0) {
			const failed = await bestEffortDeleteObjects(doomed, 'sourcemaps.replaced');
			for (let i = 0; i < failed.length; i++) recordSourcemapUpload('storage_error');
		}
	}

	for (const outcome of outcomes) recordSourcemapUpload(outcome);

	log.info('sourcemap_uploads.accepted', {
		projectId: ctx.projectId,
		keyId: ctx.keyId,
		files: parts.length,
		created: outcomes.filter((o) => o === 'created').length,
		replaced: outcomes.filter((o) => o === 'replaced').length,
		unchanged: outcomes.filter((o) => o === 'unchanged').length,
		objectsRepaired,
		releaseFileCount,
	});

	// §4.1's success body, snake_case per §3; `files[]` in request order. 200 when every file is
	// `unchanged` (the CI retry case), 201 otherwise.
	const allUnchanged = outcomes.every((o) => o === 'unchanged');
	return new Response(
		JSON.stringify({
			project_id: ctx.projectId,
			release_version: releaseVersion,
			files: parts.map((part) => ({
				minified_path: part.minifiedPath,
				sha256: part.sha256,
				size_bytes: part.bytes.byteLength,
				outcome: outcomes[part.index],
			})),
			release_file_count: releaseFileCount,
		}),
		{ status: allUnchanged ? 200 : 201, headers: { 'Content-Type': 'application/json' } }
	);
};

/**
 * §4.1's structural sourcemap-v3 check, in the contract's order: the bytes parse as JSON, the
 * object has `"version": 3`, a string `"mappings"` and an array `"sources"`. Returns the §7
 * `reason` or null when the part passes. Deliberately not a decode (decision 19).
 */
function sourcemapV3Rejection(bytes: Uint8Array): 'not_json' | 'not_v3' | 'no_mappings' | 'no_sources' | null {
	let parsed: unknown;
	try {
		parsed = JSON.parse(Buffer.from(bytes.buffer, bytes.byteOffset, bytes.byteLength).toString('utf8'));
	} catch {
		return 'not_json';
	}
	if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) return 'not_v3';
	const map = parsed as Record<string, unknown>;
	if (map.version !== 3) return 'not_v3';
	if (typeof map.mappings !== 'string') return 'no_mappings';
	if (!Array.isArray(map.sources)) return 'no_sources';
	return null;
}

/**
 * Resolves one of the three caps from `$env/dynamic/private` (plain integers -- §12 states the
 * syntax beside each in `values.yaml`). An unset, unparseable or non-positive value falls back to
 * decision 10's default rather than to "no cap", the rule `resolveReportUploadRpm` follows.
 */
function resolvePositiveInt(raw: string | undefined, fallback: number): number {
	if (raw === undefined || raw.trim() === '') return fallback;
	const parsed = Number(raw);
	if (!Number.isFinite(parsed) || parsed <= 0) return fallback;
	return Math.floor(parsed);
}
