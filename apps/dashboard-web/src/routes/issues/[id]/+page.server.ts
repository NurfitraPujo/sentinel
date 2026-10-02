import type { PageServerLoad } from './$types';
import { error } from '@sveltejs/kit';
import { requireProjectAccess } from '$lib/server/issue-access';
import {
	resolveEnvironmentScope,
	resolveEnvironmentScopes,
} from '$lib/server/environment-access';
import { resolveEnvironmentDisplayName } from '$lib/db/queries/environments';
import { issueQueries } from '$lib/server/queries/issue-queries';
import { getIssueRelations } from '$lib/db/queries/issues';
import { listPickerReleases, type PickerRelease } from '$lib/db/queries/releases';
import { getRetentionDays } from '$lib/server/retention-window';
import { detailTrendWindow } from '$lib/server/trend-window';
import { cleanOccurrenceRow } from '$lib/server/json-safe';
import { buildTagFacets } from '$lib/tag-facets';
import { loadTraceLinks } from '$lib/server/trace-links';

// sentinel-occurrence-detail-panels §3.2: the shape of an occurrence id. Tested against the lower-cased
// parameter, so a malformed value (S4) never reaches a query, where Postgres would answer 22P02 → 500.
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

export const load: PageServerLoad = async ({ params, locals, url }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const issueId = params.id;
	const issue = await issueQueries.getIssueById(issueId);

	if (!issue) {
		throw error(404, 'Issue not found');
	}

	// sentinel-project-environments §4.5a layer 1, decisions 35 and 46 — the PROJECT dimension, and
	// it changes here by SWAPPING THE HELPER, not by editing the old one.
	//
	// This was `checkProjectAccess(userId, projectId, 'viewer')`, which resolves access through
	// `project_members` only — a table nothing in this application writes — so it threw `403` for
	// every user in any real deployment, the opposite failure from `/`'s open read on the same
	// cause. `requireProjectAccess` already resolves the widened population (`isIssueWriteRole` on
	// the org role, with a `project_members` row as the read-only alternative path) and computes no
	// `roleHierarchy` at all, which is why widening the old helper's comparison was refused: org
	// roles are not project roles, `roleHierarchy['engineer']` is `undefined`, and the fix would
	// have been a silent no-op.
	//
	// It THROWS rather than returning a boolean, so the error strings on this page become the
	// helper's own (contract §7). Four are reachable at `'read'` and all four are on the PROJECT
	// dimension — including its `404 Project not found`, which is NOT the environment-dimension
	// `404` below.
	const { organizationId } = await requireProjectAccess(session.user.id, issue.projectId, 'read');

	// §4.5a layer 2 — the ENVIRONMENT dimension. `404`, never `403` (decision 20): a `403` here
	// tells the caller that a production issue with this id exists, which is the fact the boundary
	// was drawn to withhold.
	const scope = await resolveEnvironmentScope(session.user.id, issue.projectId);
	if (scope !== null && !scope.includes(issue.environment)) {
		throw error(404, 'Issue not found');
	}

	// sentinel-releases-snooze §4.1, decision 26 — the resolve dialog's picker (flow D5): the
	// project's 50 newest releases, `first_seen DESC`, and `[]` for a caller whose environment scope
	// is NON-NULL. The issue is theirs to see (the clause above let it through); the project's
	// release list is not — a release is a project-level fact with no environment column (§3), so
	// the list would name versions from environments the grant withholds. The dialog already
	// handles `[]` (select disabled, *next release* and *type a version* still offered), which is
	// why this is one clause here and not a scoped query (decision 36).
	const releases: PickerRelease[] =
		scope === null ? await listPickerReleases(issue.projectId) : [];

	// §4.1's `canWrite`: `true` iff `requireProjectAccess(…, 'write')` would pass — computed by
	// CALLING it and catching its `403`, so this is the same answer `PATCH /api/issues/{id}/status`
	// gives (`requireIssueAccess` at `'write'` is the project half plus the environment half already
	// passed above), not a re-derivation from a role string. It decides only whether the control
	// block renders (§2, a UX nicety); the endpoint re-checks and never trusts it. Anything other
	// than a `403` is a real failure and propagates — `'read'` passed above, so a `404` here is not
	// "no access", it is the project vanishing between two statements.
	let canWrite = true;
	try {
		await requireProjectAccess(session.user.id, issue.projectId, 'write', { organizationId });
	} catch (e) {
		if ((e as { status?: unknown } | null)?.status !== 403) throw e;
		canWrite = false;
	}

	const project = await issueQueries.getProjectById(issue.projectId);
	// sentinel-issue-trend §4.2, decision 11 — AFTER both access checks above, keyed on the same
	// `issueId` the loader has just been allowed to read (§2). One PK range query over the 30 UTC
	// calendar days ending today, bounded at both ends, densified by the reader into `hourly` (24)
	// and `daily` (30). The window is taken from one instant so the two headings agree on "now".
	const { windowStart, windowEnd } = detailTrendWindow(new Date());
	const trend = await issueQueries.trendForIssue(issueId, windowStart, windowEnd);
	// sentinel-issue-tag-facets §7.1 — AFTER both access checks, ONE read of the issue_tag_values rollup
	// (every row belongs to this issue, whose environment the checks above already passed).
	const tagFacets = buildTagFacets(await issueQueries.getTagFacets(issueId));
	const occurrences = await issueQueries.getOccurrencesByIssueId(issueId);
	// sentinel-occurrence-detail-panels §3.2: the selected occurrence, read AFTER both access checks
	// above, so their errors and order are unchanged. `url?.` is binding: a loader test that builds its
	// event without a `url` (page.server.test.ts's loadAs) must reach S1, not throw. The heavy JSONB
	// bodies are read for this ONE occurrence only (§3.1, §3.3).
	const raw = url?.searchParams.get('occurrence') ?? null;
	const defaultId = occurrences[0]?.id ?? null;
	let selectedOccurrenceId: string | null = defaultId;
	let occurrenceDetail: Awaited<ReturnType<typeof issueQueries.getOccurrenceDetail>> | null = null;
	let detailRead = false;
	if (raw) {
		const id = raw.toLowerCase();
		if (occurrences.some((o) => o.id === id)) {
			// S2: a listed occurrence.
			selectedOccurrenceId = id;
		} else if (UUID_RE.test(id)) {
			const scoped = await issueQueries.getOccurrenceDetail(issueId, id);
			if (scoped) {
				// S3a: an occurrence of THIS issue older than the newest 100. It renders selected, whatever
				// its level, and is not read again.
				selectedOccurrenceId = id;
				occurrenceDetail = scoped;
				detailRead = true;
			} else if (await issueQueries.occurrenceExistsOutsideIssue(issueId, id)) {
				// S3b: another issue's (or project's) occurrence. Out of scope; the message says nothing
				// about where the id lives.
				throw error(404, 'Occurrence not found');
			}
			// S3c: unknown (never stored, or swept). The default, no error.
		}
		// S4: not UUID-shaped. The default, and the value never reaches a query.
	}
	if (!detailRead && selectedOccurrenceId !== null) {
		// Only a v2 row (non-null `level`, T8) has panels, so a pre-v2 selection reads nothing. A null
		// result (the row vanished between the two reads) is not an error: the page renders no panels.
		const listed = occurrences.find((o) => o.id === selectedOccurrenceId);
		if (listed && listed.level !== null && listed.level !== undefined) {
			occurrenceDetail = await issueQueries.getOccurrenceDetail(issueId, selectedOccurrenceId);
		}
	}
	// sentinel-cross-repo-trace-view D5 — AFTER both access checks. Only read when some occurrence
	// carries a trace id (the card links and the header link need the org slug); the related-errors
	// count is for the SELECTED occurrence alone, never one query per card.
	const selectedListed = occurrences.find((o) => o.id === selectedOccurrenceId);
	const selectedTraceId = selectedListed?.traceId ?? occurrenceDetail?.traceId ?? null;
	const anyTrace = selectedTraceId !== null || occurrences.some((o) => o.traceId);
	const traceLinks = anyTrace
		? await loadTraceLinks({
				userId: session.user.id,
				organizationId,
				projectId: issue.projectId,
				selectedTraceId,
			})
		: { orgSlug: null, related: null };
	// Flow `D2`: `Occurrence History ({occurrences.length})` presented a page size as a total —
	// `getOccurrencesByIssueId` caps at 100. The heading takes this real `COUNT(*)`; the list below
	// says it is showing the most recent n.
	const occurrenceTotal = await issueQueries.countOccurrencesByIssueId(issueId);
	// §4.5a: the registry's display name, falling back to the raw name. Decisions 8+9 make the
	// fallback the normal path for a new environment, not an error case.
	const environmentDisplayName = await resolveEnvironmentDisplayName(
		issue.projectId,
		issue.environment
	);
	// §2.2, decision 37: the relations panel is guarded on the RELATED issue too, per project — an
	// out-of-scope relation comes back as a redacted placeholder rather than being dropped or 404'd.
	const relations = await getIssueRelations(
		issueId,
		await resolveEnvironmentScopes(session.user.id)
	);

	return {
		issue,
		project: project ?? null,
		// §3.4: every JSONB value passes through dropProtoKeys, so no stored `__proto__` key can make
		// SvelteKit's serialiser throw (a 500 for the whole page, `metadata` included, before this).
		occurrences: occurrences.map(cleanOccurrenceRow),
		selectedOccurrenceId,
		occurrenceDetail: occurrenceDetail ? cleanOccurrenceRow(occurrenceDetail) : null,
		occurrenceTotal,
		environmentDisplayName,
		// sentinel-cross-repo-trace-view D5: the slug the trace links are built from (null when no
		// occurrence has a trace) and the selected occurrence's other-project count (null: none to show).
		traceOrgSlug: traceLinks.orgSlug,
		relatedTraceErrors: traceLinks.related,
		relations,
		// sentinel-issue-trend §4.2 — top level beside `issue`: `{ hourly: TrendPoint[24], daily:
		// TrendPoint[30] }`, dense, oldest first, the last entry of each the current partial slot.
		trend,
		// sentinel-issue-tag-facets §7.1 — top level beside `trend`: the tag distribution blocks
		// (`TagFacetBlock[]`), `[]` for an issue with no rollup rows (the panel then renders nothing).
		tagFacets,
		// sentinel-affected-users §4.2, decision 21 — top level beside `issue`, the same number `/`
		// returns and the retention sweep uses: `Affected users (last {retentionDays} days)`. The two
		// counts themselves need no loader change; `getIssueById` selects the whole row.
		retentionDays: getRetentionDays(),
		// sentinel-releases-snooze §4.1 — top level beside `issue`: the picker's `{ version,
		// firstSeen }[]` (≤ 50, `[]` for a scoped caller — decision 26) and whether the status
		// control block renders. The four new `issue` columns need no loader change: `getIssueById`
		// selects the whole row.
		releases,
		canWrite,
	};
};
